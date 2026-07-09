package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
)

// ─── Proxy Chat Request (§19) ────────────────────────────────────────────────

func (p *Proxy) proxyChatRequest(w http.ResponseWriter, r *http.Request,
	payload map[string]interface{}, model string,
	writeError ErrorWriter, writePassthroughError PassthroughErrorWriter) {

	// Snapshot the config fields we need, then release the lock.
	// Holding RLock for the entire upstream request blocks HandleConfigPost
	// (which needs Lock) for the full duration of LLM completions.
	p.configMu.RLock()
	cfgMode := p.Config.ApiKeyMode
	cfgMaxImages := p.Config.MaxImages
	cfgUpstreamURL := p.Config.UpstreamBaseURL
	p.configMu.RUnlock()

	// ═══════════════════════════════════════════════════════════════════════
	// Step 1: Key Acquisition (§19.1, §14.5)
	// ═══════════════════════════════════════════════════════════════════════

	fingerprint := FingerprintPayload(payload)

	// Touch existing conversation session (if any) for key affinity
	var existingSession *ConversationSession
	if fingerprint != "" {
		existingSession = p.TouchConversation(fingerprint)
	}

	// Determine preferred key index from existing session
	preferredIndex := -1
	if existingSession != nil {
		preferredIndex = existingSession.TokenIndex
	}

	// Acquire a key from the pool
	// Note: p.KeyPool may be nil in tests or when no keys are configured.
	var slot *KeySlot
	var currentKeyIndex int
	usingClientKey := false
	clientKey := extractClientAPIKey(r)
	currentKey := ""
	mode := cfgMode
	if (mode == "passthrough" || mode == "smart") && clientKey != "" {
		// Pass-through or smart with a client key: use the client's key directly
		usingClientKey = true
		currentKey = clientKey
	} else if mode == "passthrough" {
		// Pure passthrough with no client key — reject
		if writeError != nil {
			writeError(w, http.StatusUnauthorized,
				"no API key provided by client", "authentication_error", "")
		}
		return
	} else if p.KeyPool != nil {
		slot, _ = p.KeyPool.Acquire(preferredIndex)
		if slot == nil {
			if writeError != nil {
				writeError(w, http.StatusServiceUnavailable,
					"no healthy API keys available", "api_error", "")
			}
			return
		}
		currentKeyIndex = slot.Index
		currentKey = slot.Key
	}

	// slotName is used for error logging; empty if no slot
	slotName := ""
	if slot != nil {
		slotName = slot.Name
	}

	// ═══════════════════════════════════════════════════════════════════════
	// Step 2: Session Tracking (§19.2, §14.5)
	// ═══════════════════════════════════════════════════════════════════════

	var sessNum int64
	var session *ConversationSession
	if fingerprint != "" {
		if existingSession != nil {
			session = existingSession
			session.RequestCount++
			session.TokenIndex = currentKeyIndex
			sessNum = session.SessNum
		} else {
			p.convMu.Lock()
			p.globalSessionCounter++
			sessNum = p.globalSessionCounter
			p.convMu.Unlock()

			session = &ConversationSession{
				TokenIndex:   currentKeyIndex,
				RequestCount: 1,
				SessNum:      sessNum,
			}
		}

		p.TrackConversationSession(fingerprint, session, existingSession != nil)

		if session.RequestCount == 1 {
			userPrompt := ExtractUserPrompt(payload)
			if userPrompt != "" {
				if len(userPrompt) > 80 {
					userPrompt = userPrompt[:80] + "…"
				}
				fmt.Fprintf(os.Stderr, "[session %d] first prompt: %s\n", sessNum, userPrompt)
			}
		}
	}

	// ═══════════════════════════════════════════════════════════════════════
	// Step 3: StripReasoningContent (§19.3, §13.1)
	// ═══════════════════════════════════════════════════════════════════════

	StripReasoningContent(payload)

	// ═══════════════════════════════════════════════════════════════════════
	// Step 4: (Response cache removed — only ImageHandoffCache remains)
	// ═══════════════════════════════════════════════════════════════════════

	isStream, _ := payload["stream"].(bool)

	// ═══════════════════════════════════════════════════════════════════════
	// Step 5: Model Resolution (§19.5, §11.2)
	// ═══════════════════════════════════════════════════════════════════════

	resolvedModel := p.ResolveModelId(model)
	payload["model"] = resolvedModel

	// ═══════════════════════════════════════════════════════════════════════
	// Step 6: Tool Schema Normalization (§19.6, §12)
	// ═══════════════════════════════════════════════════════════════════════

	if tools, ok := payload["tools"].([]interface{}); ok && len(tools) > 0 {
		NormalizeToolSchemas(tools)
	}

	// ═══════════════════════════════════════════════════════════════════════
	// Step 7: Image Limiting (§19.7, §13.3)
	// ═══════════════════════════════════════════════════════════════════════

	needsHandoff := p.NeedsVisionHandoff(resolvedModel)
	if !needsHandoff {
		LimitImagesInMessages(payload, cfgMaxImages)
	}

	// ═══════════════════════════════════════════════════════════════════════
	// Step 8: Auto-Think (§19.8, §13.7)
	// ═══════════════════════════════════════════════════════════════════════

	// 6.2: Guard catalog map reads under catalogMu.RLock()
	p.catalogMu.RLock()
	info, ok := p.ModelInfoMap[resolvedModel]
	p.catalogMu.RUnlock()
	if ok {
		ApplyAutoThink(payload, info.Capabilities.Reasoning)
	}

	// ═══════════════════════════════════════════════════════════════════════
	// Step 9: NormalizeThinkingPayload (§19.9, §13.2)
	// ═══════════════════════════════════════════════════════════════════════

	NormalizeThinkingPayload(payload)

	// ═══════════════════════════════════════════════════════════════════════
	// Step 10: Vision Handoff SSE Keepalive (§19.10, §11.6)
	// ═══════════════════════════════════════════════════════════════════════

	if needsHandoff && isStream {
		p.FlushVisionHandoffKeepalive(w)
	}

	// ═══════════════════════════════════════════════════════════════════════
	// Step 11: PerformVisionHandoff (§19.11, §11.5)
	// ═══════════════════════════════════════════════════════════════════════

	if needsHandoff {
		p.PerformVisionHandoff(r.Context(), payload, resolvedModel, currentKey)
	}

	// ═══════════════════════════════════════════════════════════════════════
	// Step 12: Retry Loop with Upstream Forwarding (§19.12, §15)
	// ═══════════════════════════════════════════════════════════════════════

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		if writeError != nil {
			writeError(w, http.StatusInternalServerError,
				"failed to serialize request payload: "+err.Error(), "api_error", "")
		}
		return
	}

	var lastStatus int

	headersCommitted := false // tracks whether SSE headers have been sent to client
	_, retryErr := p.retryLoop(func(attempt int, isLast bool) (retry bool, err error) {
		defer func() {
			if rv := recover(); rv != nil {
				stack := make([]byte, 4096)
				n := runtime.Stack(stack, false)
				log.Printf("PANIC in retry callback [attempt=%d isLast=%v headersCommitted=%v]: %v\n%s",
					attempt, isLast, headersCommitted, rv, stack[:n])
				err = fmt.Errorf("panic in retry callback: %v", rv)
				retry = false
			}
		}()

		// If we already committed SSE headers on a previous attempt, we cannot
		// retry — the response is already in flight.
		if headersCommitted {
			return false, fmt.Errorf("response already committed (cannot retry after SSE headers sent)")
		}

		// ── Key rotation on retry (§15.3) ──
		if attempt > 1 && !usingClientKey && p.KeyPool != nil && p.KeyPool.Total() > 1 {
			p.KeyPool.MarkUnhealthy(currentKeyIndex, lastStatus)
			newSlot, ok := p.KeyPool.Acquire(-1)
			if !ok {
				return false, fmt.Errorf("no healthy keys available for retry")
			}
			currentKeyIndex = newSlot.Index
			slotName = newSlot.Name
			currentKey = newSlot.Key
		}

		// ── Execute upstream request (§19.12b) ──
		if p.Upstream == nil {
			if writeError != nil {
				writeError(w, http.StatusServiceUnavailable,
					"no upstream client configured", "api_error", "")
			}
			return false, nil
		}

		resp, err := p.Upstream.ChatCompletions(r.Context(), bodyBytes, isStream, currentKey)

		// ── Network error → treated as 502 (§15.2) ──
		if err != nil {
			lastStatus = 502
			if p.KeyPool != nil {
				p.KeyPool.MarkUnhealthy(currentKeyIndex, lastStatus)
			}

			_ = p.LogRetryableError(
				attempt, isLast,
				sessNum, slotName,
				r.Method, r.URL.String(),
				map[string][]string(r.Header), string(bodyBytes),
				cfgUpstreamURL+"/chat/completions", "POST",
				map[string][]string{}, 502, "Bad Gateway", err.Error(),
			)

			if isLast {
				if writePassthroughError != nil {
					writePassthroughError(w, http.StatusBadGateway,
						`{"error":{"message":"upstream network error: `+err.Error()+`","type":"upstream_error"}}`)
				}
				return false, nil
			}
			return true, nil
		}

		// ── Check status code (§19.12c-f) ──
		if IsRetryableStatus(resp.StatusCode) {
			lastStatus = resp.StatusCode
			if p.KeyPool != nil {
				p.KeyPool.MarkUnhealthy(currentKeyIndex, lastStatus)
			}

			errBody, _ := readBodyText(resp.Body)
			resp.Body.Close()

			_ = p.LogRetryableError(
				attempt, isLast,
				sessNum, slotName,
				r.Method, r.URL.String(),
				map[string][]string(r.Header), string(bodyBytes),
				cfgUpstreamURL+"/chat/completions", "POST",
				map[string][]string(resp.Header), resp.StatusCode, resp.Status, errBody,
			)

			if isLast {
				if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests {
					p.BumpThrottled()
				}
				if writePassthroughError != nil {
					writePassthroughError(w, resp.StatusCode, errBody)
				}
				return false, nil
			}
			return true, nil

		} else if resp.StatusCode >= 400 {
			// ── Non-retryable HTTP errors (§15.4): pass through immediately ──
			errBody, _ := readBodyText(resp.Body)
			resp.Body.Close()

			if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests {
				p.BumpThrottled()
			}
			if writePassthroughError != nil {
				writePassthroughError(w, resp.StatusCode, errBody)
			}
			return false, nil
		}

		// ── Success: HTTP 2xx (§19.12d) ──
		if !usingClientKey && p.KeyPool != nil {
			p.KeyPool.MarkHealthy(currentKeyIndex)
		}
		// In passthrough/smart mode, record the client's key as last-known-good
		if (cfgMode == "passthrough" || cfgMode == "smart") && clientKey != "" {
			p.setLastClientKey(clientKey)
		}

		contentType := resp.Header.Get("Content-Type")

		if strings.Contains(contentType, "text/event-stream") {
			// ── SSE streaming response ──
			// Peek at the first chunk to detect empty streams before committing
			// headers to the client. If upstream returns 200 + text/event-stream
			// but sends 0 data bytes, treat it as a retryable error.
			peekBuf := make([]byte, 4096)
			peekN, peekErr := resp.Body.Read(peekBuf)

			if peekN == 0 && (peekErr == nil || errors.Is(peekErr, io.EOF)) {
				// Empty SSE stream — retry if possible
				resp.Body.Close()
				lastStatus = 502
				if p.KeyPool != nil {
					p.KeyPool.MarkUnhealthy(currentKeyIndex, lastStatus)
				}
				_ = p.LogRetryableError(
					attempt, isLast,
					sessNum, slotName,
					r.Method, r.URL.String(),
					map[string][]string(r.Header), string(bodyBytes),
					cfgUpstreamURL+"/chat/completions", "POST",
					map[string][]string(resp.Header), 502, "Bad Gateway",
					"empty SSE stream (0 bytes received)",
				)
				if isLast {
					if writePassthroughError != nil {
						writePassthroughError(w, http.StatusBadGateway,
							`{"error":{"message":"upstream returned empty stream","type":"upstream_error"}}`)
					}
					return false, nil
				}
				return true, nil
			}

			// We have data — commit SSE headers and write the peeked bytes
			headersCommitted = true
			writeSSEHeaders(w)
			if peekN > 0 {
				w.Write(peekBuf[:peekN])
				safeFlush(w)
			}
			// Pipe the rest of the body
			_, pipeErr := pipeBodyToResponse(resp.Body, w, r)
			if pipeErr != nil && !errors.Is(pipeErr, io.EOF) {
				// Upstream pipe broke mid-stream after headers were committed.
				// We can't retry (headers already sent) — write an SSE error
				// event so the client knows the stream was interrupted.
				writeSSEErrorEvent(w, "upstream_error", "upstream stream interrupted: "+pipeErr.Error())
			}
			return false, nil // done — cannot retry after committing SSE headers
		} else {
			// ── JSON response ──
			respBody, readErr := readBodyText(resp.Body)
			resp.Body.Close()

			if readErr != nil {
				if writeError != nil {
					writeError(w, http.StatusBadGateway,
						"failed to read upstream response: "+readErr.Error(),
						"upstream_error", "")
				}
				return false, nil
			}

			if isStream {
				// Streaming was requested but upstream returned JSON.
				// Wrap as a single SSE chunk + [DONE]
				headersCommitted = true
				writeSSEHeaders(w)
				w.Write([]byte("data: "))
				w.Write([]byte(respBody))
				w.Write([]byte("\n\n"))
				w.Write([]byte("data: [DONE]\n\n"))
				safeFlush(w)
			} else {
				// Non-streaming JSON response
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(respBody))
			}
		}

		return false, nil
	})

	if retryErr != nil && writeError != nil {
		defer func() { recover() }()
		if w.Header().Get("Content-Type") == "text/event-stream" {
			// SSE headers already committed — can't change status code.
			// Write an SSE error event so the client knows the stream failed.
			writeSSEErrorEvent(w, "api_error", retryErr.Error())
		} else {
			writeError(w, http.StatusServiceUnavailable,
				retryErr.Error(), "api_error", "")
		}
	}
}

// proxyAnthropicRequest handles an Anthropic-format messages request (§20).
