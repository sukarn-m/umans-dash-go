package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ─── HTTP Helpers (§17) ────────────────────────────────────────────────────

// ─── HTTP Helpers (§17) ────────────────────────────────────────────────────

func truncateBody(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...[truncated]"
}

// extractClientAPIKey reads the bearer token or X-Api-Key from the client request.
func extractClientAPIKey(req *http.Request) string {
	if req == nil {
		return ""
	}
	if xKey := strings.TrimSpace(req.Header.Get("X-Api-Key")); xKey != "" {
		return xKey
	}
	auth := strings.TrimSpace(req.Header.Get("Authorization"))
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

// setLastClientKey records a client API key that successfully reached upstream.
// Used in passthrough mode for usage/history fetching.
func (p *Proxy) setLastClientKey(key string) {
	if key == "" {
		return
	}
	p.lastClientKeyMu.Lock()
	p.lastClientKey = key
	p.lastClientKeyMu.Unlock()
}

// getLastClientKey returns the last-known-good client API key.
func (p *Proxy) getLastClientKey() string {
	p.lastClientKeyMu.RLock()
	defer p.lastClientKeyMu.RUnlock()
	return p.lastClientKey
}

// upstreamAPIKeyForDashboard returns the API key to use for upstream dashboard
// calls (usage, concurrency, history). In passthrough mode, uses the
// last-known-good client key. In smart mode, prefers the last-known-good client
// key and falls back to the pool key. In managed mode, uses the pool key.
func (p *Proxy) upstreamAPIKeyForDashboard() string {
	p.configMu.RLock()
	mode := p.Config.ApiKeyMode
	p.configMu.RUnlock()
	if mode == "passthrough" {
		return p.getLastClientKey()
	}
	// Smart mode: prefer last-known-good client key, fall back to pool
	if mode == "smart" {
		if key := p.getLastClientKey(); key != "" {
			return key
		}
	}
	// Managed mode (or smart with no client key yet): use first available key
	if p.KeyPool != nil {
		if slot, ok := p.KeyPool.Acquire(-1); ok && slot != nil {
			p.KeyPool.MarkHealthy(slot.Index)
			return slot.Key
		}
	}
	if p.Config.APIKey != "" {
		return p.Config.APIKey
	}
	if len(p.Config.APIKeys) > 0 {
		return p.Config.APIKeys[0]
	}
	return ""
}

func (p *Proxy) Authorized(req *http.Request) bool {
	// In passthrough mode, accept any request with an API key — the client's
	// key will be passed through to upstream.
	p.configMu.RLock()
	mode := p.Config.ApiKeyMode
	p.configMu.RUnlock()
	if mode == "passthrough" {
		return extractClientAPIKey(req) != ""
	}
	// In smart mode, always accept — either the client's key will be used,
	// or the proxy's own key will be used.
	if mode == "smart" {
		return true
	}
	// Managed mode: validate against configured keys.
	if len(p.Config.APIKeys) == 0 {
		return true
	}
	xKey := strings.TrimSpace(req.Header.Get("X-Api-Key"))
	if xKey != "" {
		for _, k := range p.Config.APIKeys {
			if k == xKey {
				return true
			}
		}
	}
	auth := strings.TrimSpace(req.Header.Get("Authorization"))
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	token := strings.TrimSpace(auth[7:])
	for _, k := range p.Config.APIKeys {
		if k == token {
			return true
		}
	}
	return false
}

func ReadBody(req *http.Request) (string, error) {
	data, err := io.ReadAll(io.LimitReader(req.Body, int64(MaxBodySize)+1))
	if err != nil {
		return "", err
	}
	if len(data) > MaxBodySize {
		return "", fmt.Errorf("request body too large")
	}
	return string(data), nil
}

func WriteJSON(w http.ResponseWriter, status int, payload interface{}) {
	// SSE keepalive branch: if Content-Type is already text/event-stream,
	// the response is in streaming mode and we write the payload as an SSE event.
	if w.Header().Get("Content-Type") == "text/event-stream" {
		b, err := json.Marshal(payload)
		if err != nil {
			// Send error as SSE event
			errEv := SSEEvent{
				Event: "error",
				Data:  fmt.Sprintf(`{"error":"failed to marshal JSON: %s"}`, err.Error()),
			}
			w.Write([]byte(errEv.Format()))
			safeFlush(w)
			return
		}
		w.Write([]byte("data: "))
		w.Write(b)
		w.Write([]byte("\n\n"))
		safeFlush(w)
		return
	}

	// Normal JSON mode — marshal FIRST so we can send 500 on encode failure (§17.4)
	b, err := json.Marshal(payload)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		errResp, _ := json.Marshal(map[string]interface{}{
			"error": fmt.Sprintf("failed to marshal JSON: %s", err.Error()),
		})
		w.Write(errResp)
		return
	}
	// 6.4: If the writer is a *responseWriterTracker and IsCommitted() returns
	// true, skip WriteHeader and Write entirely instead of relying on the
	// tracker's silent no-op. This prevents "superfluous response.WriteHeader"
	// log warnings.
	if tw, ok := w.(*responseWriterTracker); ok && tw.IsCommitted() {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

func readBodyText(body io.ReadCloser) (string, error) {
	if body == nil {
		return "", nil
	}
	data, err := io.ReadAll(io.LimitReader(body, int64(MaxBodySize)+1))
	if err != nil {
		return "", err
	}
	if len(data) > MaxBodySize {
		return "", fmt.Errorf("upstream response body exceeds MaxBodySize (%d bytes)", MaxBodySize)
	}
	// 5.2: Drain any remaining body data before closing for connection reuse.
	io.Copy(io.Discard, body)
	body.Close()
	return string(data), nil
}

// drainAndClose reads and discards any remaining body data before closing,
// enabling HTTP connection reuse. Safe to call on an already-closed body.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	io.Copy(io.Discard, body)
	body.Close()
}

// pipeBodyToResponse copies the upstream response body to the client,
// flushing after each write. Returns the number of bytes written.
// If 0 bytes were written (empty stream), the caller can detect this
// and retry the request.
func pipeBodyToResponse(body io.ReadCloser, w http.ResponseWriter, r *http.Request) (int, error) {
	if body == nil {
		return 0, fmt.Errorf("pipeBodyToResponse: body is nil")
	}
	defer body.Close()

	// Use the original client context (not the timeout-wrapped one) so that
	// long-running SSE streams aren't truncated when the request timeout fires.
	// The timeout only governs the pre-streaming phase; once streaming begins,
	// the stream runs until upstream EOF or client disconnect.
	ctx := r.Context()
	if tw, ok := w.(*responseWriterTracker); ok && tw.clientCtx != nil {
		ctx = tw.clientCtx
	}

	totalWritten := 0
	chunk := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return totalWritten, fmt.Errorf("pipeBodyToResponse: client disconnected: %w", ctx.Err())
		default:
		}

		n, err := body.Read(chunk)
		if n > 0 {
			if _, werr := w.Write(chunk[:n]); werr != nil {
				return totalWritten, fmt.Errorf("pipeBodyToResponse: write to client failed: %w", werr)
			}
			safeFlush(w)
			totalWritten += n
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return totalWritten, nil
			}
			return totalWritten, fmt.Errorf("pipeBodyToResponse: read from upstream: %w", err)
		}
	}
}

func writeSSEHeaders(w http.ResponseWriter) {
	// If the response is already a streaming SSE response, we don't need to do
	// anything. This is a convenience check only; the tracker path below is the
	// authoritative guard.
	if w.Header().Get("Content-Type") == "text/event-stream" {
		return
	}

	// Commit the SSE headers. If the writer is our tracker, use its atomic
	// commit method to avoid bypassing the written/hijacked/aborted guards.
	if tw, ok := w.(*responseWriterTracker); ok {
		if tw.commitSSE() {
			safeFlush(w)
		}
		return
	}

	// Fallback for non-tracker writers (tests).
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	safeFlush(w)
}

// safeFlush flushes the ResponseWriter if it implements http.Flusher.
// It is panic-safe — recovers from any flush error (e.g. closed/hijacked connection).
func safeFlush(w http.ResponseWriter) {
	defer func() { recover() }()
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// writeSSEErrorEvent writes an SSE error event to an already-committed SSE stream.
// This is used when the upstream pipe breaks mid-stream after headers have been sent
// and the HTTP status code can no longer be changed. The error is sent as a
// data event containing a JSON error object, followed by [DONE], so that
// well-behaved SSE clients can detect the failure.
// This function is panic-safe — it recovers from any panic (e.g. from writing
// to a closed/hijacked connection) and silently swallows the error.
func writeSSEErrorEvent(w http.ResponseWriter, errType, message string) {
	defer func() { recover() }()
	// 4.5: Skip writing the SSE error if the response writer has been hijacked
	// or Abort() has already been called. Do NOT skip merely because headers
	// have been committed; SSE error events are expected on an already-committed
	// stream.
	if tw, ok := w.(*responseWriterTracker); ok {
		tw.mu.Lock()
		hijacked := tw.hijacked
		aborted := tw.aborted
		tw.mu.Unlock()
		if hijacked || aborted {
			return
		}
	}
	errJSON, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
		},
	})
	w.Write([]byte("data: "))
	w.Write(errJSON)
	w.Write([]byte("\n\n"))
	w.Write([]byte("data: [DONE]\n\n"))
	safeFlush(w)
}

// FlushVisionHandoffKeepalive sends SSE headers (if not already sent) and a
// keepalive comment before vision handoff analysis begins (SPEC §11.6).
// Returns true if SSE headers were flushed by this call.
func (p *Proxy) FlushVisionHandoffKeepalive(w http.ResponseWriter) bool {
	if w.Header().Get("Content-Type") == "text/event-stream" {
		w.Write([]byte(": keepalive — analyzing image via vision handoff\n\n"))
		safeFlush(w)
		return false
	}
	writeSSEHeaders(w)
	w.Write([]byte(": keepalive — analyzing image via vision handoff\n\n"))
	safeFlush(w)
	return true
}

func WriteOpenAIError(w http.ResponseWriter, status int, message, errType, code string) {
	errObj := map[string]interface{}{
		"message": message,
		"type":    errType,
	}
	if code != "" {
		errObj["code"] = code
	}
	WriteJSON(w, status, map[string]interface{}{"error": errObj})
}

func WriteAnthropicError(w http.ResponseWriter, status int, message, errType string) {
	if errType == "" {
		typeMap := map[int]string{
			400: "invalid_request_error", 401: "authentication_error",
			403: "permission_error", 404: "not_found_error",
			429: "rate_limit_error", 500: "api_error", 503: "overloaded_error",
		}
		errType = typeMap[status]
		if errType == "" {
			errType = "api_error"
		}
	}
	WriteJSON(w, status, map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
		},
	})
}

func WritePassthroughError(w http.ResponseWriter, status int, body string) {
	trimmed := strings.TrimSpace(body)
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
		msg := trimmed
		errType := "upstream_error"
		code := ""
		if e, ok := parsed["error"].(map[string]interface{}); ok {
			if m, ok := e["message"].(string); ok {
				msg = m
			}
			if t, ok := e["type"].(string); ok {
				errType = t
			}
			if c, ok := e["code"].(string); ok {
				code = c
			}
		} else if m, ok := parsed["message"].(string); ok {
			// §17.4: fallback to top-level "message" field
			msg = m
		}
		WriteOpenAIError(w, status, msg, errType, code)
		return
	}
	WriteOpenAIError(w, status, trimmed, "upstream_error", "")
}

func WriteAnthropicPassthroughError(w http.ResponseWriter, status int, body string) {
	trimmed := strings.TrimSpace(body)
	msg := trimmed
	errType := "api_error"
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
		if e, ok := parsed["error"].(map[string]interface{}); ok {
			if m, ok := e["message"].(string); ok {
				msg = m
			}
			if t, ok := e["type"].(string); ok {
				errType = t
			}
		} else if m, ok := parsed["message"].(string); ok {
			// §17.4: fallback to top-level "message" field
			msg = m
		}
	}
	WriteAnthropicError(w, status, msg, errType)
}

// WriteQueueFullError writes a 429 Too Many Requests queue_full error in the
// appropriate format, with a Retry-After header to discourage immediate retries.
func WriteQueueFullError(w http.ResponseWriter, format string) {
	w.Header().Set("Retry-After", "2")
	if format == "anthropic" {
		WriteAnthropicError(w, http.StatusTooManyRequests,
			"The server is overloaded. Please retry later.", "overloaded_error")
	} else {
		WriteOpenAIError(w, http.StatusTooManyRequests,
			"The server is overloaded. Please retry later.",
			"server_error", QueueFullErrorCode)
	}
}

// OpenAIErrorWriter returns an ErrorWriter that writes errors in OpenAI format.
func OpenAIErrorWriter() ErrorWriter {
	return WriteOpenAIError
}

// AnthropicErrorWriter returns an ErrorWriter that writes errors in Anthropic format.
// It adapts WriteAnthropicError (4 params) to the ErrorWriter signature (5 params)
// by discarding the code parameter, since Anthropic errors don't have a code field.
func AnthropicErrorWriter() ErrorWriter {
	return func(w http.ResponseWriter, status int, message, errType, code string) {
		WriteAnthropicError(w, status, message, errType)
	}
}

// OpenAIPassthroughErrorWriter returns a PassthroughErrorWriter for OpenAI format.
func OpenAIPassthroughErrorWriter() PassthroughErrorWriter {
	return WritePassthroughError
}

// AnthropicPassthroughErrorWriter returns a PassthroughErrorWriter for Anthropic format.
func AnthropicPassthroughErrorWriter() PassthroughErrorWriter {
	return WriteAnthropicPassthroughError
}

// proxyChatRequest handles an OpenAI-format chat completion request (§19).
// It executes the full 12-step pipeline: key acquisition, session tracking,
// payload normalization, cache check, model resolution, tool schema normalization,
// image limiting, auto-think, thinking payload normalization, vision handoff
// preparation, vision handoff execution, and the retry loop with upstream
// forwarding.
