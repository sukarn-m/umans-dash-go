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

// ─── Proxy Anthropic Request (§20) ───────────────────────────────────────────

func (p *Proxy) proxyAnthropicRequest(item *QueueItem) {
	// Snapshot config fields, then release lock (see proxyChatRequest for rationale).
	p.configMu.RLock()
	cfgMode := p.Config.ApiKeyMode
	cfgMaxImages := p.Config.MaxImages
	cfgUpstreamURL := p.Config.UpstreamBaseURL
	p.configMu.RUnlock()

	w := item.Response
	r := item.Req
	payload := item.Payload
	model := item.Model
	isStream := item.IsStream
	fp := item.Fingerprint
	preferredIndex := item.PreferredKeyIndex

	// STEP 1: Key acquisition (§20.1, §14.5)
	// Note: p.KeyPool may be nil in tests or when no keys are configured.
	var slot *KeySlot
	var ok bool
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
		WriteAnthropicError(w, http.StatusUnauthorized,
			"no API key provided by client", "authentication_error")
		return
	} else if p.KeyPool != nil {
		slot, ok = p.KeyPool.Acquire(preferredIndex)
		if !ok {
			WriteAnthropicError(w, http.StatusServiceUnavailable,
				"no available API keys", "overloaded_error")
			return
		}
		currentKey = slot.Key
	}

	// STEP 2: Session tracking (§20.2, §14.5)
	var sessNum int64
	if fp != "" {
		existing := p.TouchConversation(fp)
		if existing != nil {
			existing.RequestCount++
			if slot != nil {
				existing.TokenIndex = slot.Index
			}
			p.TrackConversationSession(fp, existing, true)
			sessNum = existing.SessNum
		} else {
			p.convMu.Lock()
			p.globalSessionCounter++
			sessNum = p.globalSessionCounter
			p.convMu.Unlock()
			newSess := &ConversationSession{
				TokenIndex:   0,
				RequestCount: 1,
				SessNum:      sessNum,
			}
			if slot != nil {
				newSess.TokenIndex = slot.Index
			}
			p.TrackConversationSession(fp, newSess, true)
			// Log first prompt of this session (mirrors OpenAI path §19.2).
			userPrompt := ExtractUserPrompt(payload)
			if userPrompt != "" {
				if len(userPrompt) > 80 {
					userPrompt = userPrompt[:80] + "…"
				}
				fmt.Fprintf(os.Stderr, "[session %d] first prompt: %s\n", sessNum, userPrompt)
			}
		}
	}

	// STEP 4: Normalize thinking payload (§20.4)
	NormalizeThinkingPayload(payload)

	// STEP 5: Model resolution (same as OpenAI path)
	resolvedModel := p.ResolveModelId(model)
	payload["model"] = resolvedModel

	// STEP 5b: Limit images (§20.3) — only when NOT doing vision handoff,
	// mirroring the OpenAI path. When handoff is active, images must be
	// preserved so CollectImageParts can find and process them.
	needsHandoff := p.NeedsVisionHandoff(resolvedModel)
	if !needsHandoff {
		LimitImagesInMessages(payload, cfgMaxImages)
	}

	// STEP 6: Vision handoff (§20.5, §11)
	if needsHandoff {
		if isStream {
			p.FlushVisionHandoffKeepalive(w)
		}
		p.PerformVisionHandoff(r.Context(), payload, resolvedModel, currentKey)
	}

	// STEP 7: Marshal payload
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		WriteJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"type": "error",
			"error": map[string]interface{}{
				"type":    "api_error",
				"message": fmt.Sprintf("failed to marshal payload: %v", err),
			},
		})
		return
	}

	// STEP 8: Upstream call with retry (§20.6, §15)
	if p.Upstream == nil {
		WriteAnthropicError(w, http.StatusServiceUnavailable,
			"upstream not configured", "api_error")
		return
	}
	currentKeyIndex := -1
	if slot != nil {
		currentKeyIndex = slot.Index
	}
	var lastStatus int
	var lastErrBody string
	headersCommitted := false
	_, retryErr := p.retryLoop(func(attempt int, isLast bool) (retry bool, err error) {
		defer func() {
			if rv := recover(); rv != nil {
				stack := make([]byte, 4096)
				n := runtime.Stack(stack, false)
				log.Printf("PANIC in anthropic retry callback [attempt=%d isLast=%v headersCommitted=%v]: %v\n%s",
					attempt, isLast, headersCommitted, rv, stack[:n])
				err = fmt.Errorf("panic in anthropic retry callback: %v", rv)
				retry = false
			}
		}()

		// If we already committed headers on a previous attempt, we cannot retry.
		if headersCommitted {
			return false, fmt.Errorf("response already committed (cannot retry after headers sent)")
		}

		// Key rotation on retry (§15.3) — mirrors OpenAI path
		if attempt > 1 && !usingClientKey && p.KeyPool != nil && p.KeyPool.Total() > 1 {
			p.KeyPool.MarkUnhealthy(currentKeyIndex, lastStatus)
			newSlot, ok := p.KeyPool.Acquire(-1)
			if !ok {
				return false, fmt.Errorf("no healthy keys available for retry")
			}
			currentKeyIndex = newSlot.Index
			currentKey = newSlot.Key
		}

		resp, err := p.Upstream.Messages(r.Context(), bodyBytes, isStream, currentKey)
		if err != nil {
			lastStatus = 502
			if p.KeyPool != nil && currentKeyIndex >= 0 {
				p.KeyPool.MarkUnhealthy(currentKeyIndex, lastStatus)
			}
			_ = p.LogRetryableError(
				attempt, isLast,
				sessNum, "",
				r.Method, r.URL.String(),
				map[string][]string(r.Header), string(bodyBytes),
				cfgUpstreamURL+"/messages", "POST",
				map[string][]string{}, 502, "Bad Gateway", err.Error(),
			)
			lastErrBody = fmt.Sprintf(`{"error":{"type":"api_error","message":"upstream network error: %v"}}`, err)
			return !isLast, nil // retry if not last (isLast → stop, !isLast → retry)
		}

		if resp.StatusCode >= 400 {
			errBodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, MaxBodySize))
			drainAndClose(resp.Body)
			errBody := string(errBodyBytes)

			if IsRetryableStatus(resp.StatusCode) {
				lastStatus = resp.StatusCode
				lastErrBody = errBody
				_ = p.LogRetryableError(
					attempt, isLast,
					sessNum, "",
					r.Method, r.URL.String(),
					map[string][]string(r.Header), string(bodyBytes),
					cfgUpstreamURL+"/messages", "POST",
					map[string][]string{}, resp.StatusCode, resp.Status, lastErrBody,
				)
				return !isLast, nil // retry (isLast → stop, !isLast → retry)
			}

			// Non-retryable: write immediately — do NOT set lastStatus/lastErrBody
			WriteAnthropicPassthroughError(w, resp.StatusCode, errBody)
			if resp.StatusCode == http.StatusServiceUnavailable {
				if p.KeyPool != nil && currentKeyIndex >= 0 {
					p.KeyPool.MarkUnhealthy(currentKeyIndex, http.StatusServiceUnavailable)
				}
			}
			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
				p.BumpThrottled()
			}
			return false, nil // success (handled) — stop retrying
		}

		// Success path — pipe response
		if !usingClientKey && p.KeyPool != nil && currentKeyIndex >= 0 {
			p.KeyPool.MarkHealthy(currentKeyIndex)
		}
		if (cfgMode == "passthrough" || cfgMode == "smart") && clientKey != "" {
			p.setLastClientKey(clientKey)
		}
		ct := resp.Header.Get("Content-Type")
		if ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		headersCommitted = true
		if tw, ok := w.(*responseWriterTracker); ok && tw.IsCommitted() {
			drainAndClose(resp.Body)
			return false, nil
		}
		w.WriteHeader(resp.StatusCode)
		_, pipeErr := pipeBodyToResponse(resp.Body, w, r)
		if pipeErr != nil && !errors.Is(pipeErr, io.EOF) {
			if strings.Contains(ct, "text/event-stream") {
				writeSSEErrorEvent(w, "upstream_error", "upstream stream interrupted: "+pipeErr.Error())
			}
		}
		return false, nil // success
	})
	if retryErr != nil {
		WriteAnthropicError(w, http.StatusServiceUnavailable,
			retryErr.Error(), "api_error")
		return
	}
	// If all retries exhausted on retryable errors, return the last error
	if lastStatus != 0 && lastErrBody != "" {
		defer func() { recover() }()
		WriteAnthropicPassthroughError(w, lastStatus, lastErrBody)
		if lastStatus == http.StatusServiceUnavailable {
			if p.KeyPool != nil && currentKeyIndex >= 0 {
				p.KeyPool.MarkUnhealthy(currentKeyIndex, lastStatus)
			}
		}
		if lastStatus == http.StatusTooManyRequests || lastStatus == http.StatusServiceUnavailable {
			p.BumpThrottled()
		}
		return
	}
	return
}

// dispatchItem is used only for the no-limit fast path in TryEnqueueOrDispatch.
// It reserves a slot synchronously and spawns runDispatch.
// dispatchItem does NOT call OnRequestComplete() itself — that is exclusively
// in runDispatch's defer.
