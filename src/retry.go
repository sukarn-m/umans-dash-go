package proxy

import (
	"time"
)

// ─── Retry Logic (§15) ─────────────────────────────────────────────────────

// ─── Retry Logic (§15) ─────────────────────────────────────────────────────

// getRetryDelay returns the delay to sleep BEFORE attempt N+1, given that
// attempt N just failed. Uses the configured BackoffPresets table.
// attempt is 1-based: attempt=1 → delay before attempt 2.
// If attempt exceeds the preset length, the last preset value repeats.
func (p *Proxy) getRetryDelay(attempt int) time.Duration {
	preset, ok := BackoffPresets[p.getBackoffStrategy()]
	if !ok || len(preset) == 0 {
		preset = BackoffPresets["aggressive"]
	}
	if len(preset) == 0 {
		return 1 * time.Second
	}
	idx := attempt - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(preset) {
		idx = len(preset) - 1
	}
	return time.Duration(preset[idx]) * time.Second
}

// retryLoop executes fn up to p.getRetryAttempts() times with preset-driven
// delays. Signature/semantics unchanged from the old free function:
//   - err non-nil → abort, return (false, err)
//   - retry=false → success, return (true, nil)
//   - retry=true and attempt < max → sleep getRetryDelay(attempt), continue
//   - retry=true and attempt == max (isLast) → return (true, nil)
func (p *Proxy) retryLoop(fn func(attempt int, isLast bool) (bool, error)) (bool, error) {
	maxAttempts := p.getRetryAttempts()
	if maxAttempts <= 0 {
		// Retries disabled — run exactly once, isLast=true.
		_, err := fn(1, true)
		return err == nil, err
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		retry, err := fn(attempt, attempt == maxAttempts)
		if err != nil {
			return false, err
		}
		if !retry {
			return true, nil
		}
		if attempt < maxAttempts {
			time.Sleep(p.getRetryDelay(attempt))
		}
	}
	return true, nil
}

// IsRetryableStatus returns true for HTTP statuses that should trigger a retry
// per SPEC §15.2: 500, 503, and 502 (network errors are treated as 502).
// Per §15.4, all other HTTP errors (400, 401, 404, 429, etc.) are non-retryable
// and should be returned to the client immediately.
func IsRetryableStatus(status int) bool {
	return status == 500 || status == 503 || status == 502
}

// Key Rotation on Retry (SPEC §15.3)
//
// Key rotation is logic that lives inside the retry callback (the §19
// proxyChatRequest caller's responsibility), not a standalone function. §15
// provides the infrastructure (RetryLoop, IsRetryableStatus, KeyPool methods)
// that the callback uses. The expected pseudocode for the §19 caller:
//
//	 RetryLoop(func(attempt int, isLast bool) (bool, error) {
//	     // ── Key rotation (§15.3) ──
//	     // On retries after the first attempt, if the pool has more than one
//	     // key, mark the previous key unhealthy and acquire a fresh one.
//	     if attempt > 1 && p.KeyPool.Total() > 1 {
//	         p.KeyPool.MarkUnhealthy(currentKeyIndex, lastStatus)
//	         slot, ok := p.KeyPool.Acquire(-1) // round-robin, no preference
//	         if !ok {
//	             return false, fmt.Errorf("no healthy keys available")
//	         }
//	         currentKeyIndex = slot.Index
//	         p.Upstream.SetAPIKey(slot.Key)
//	     }
//
//	     // ── Execute upstream request (§19 logic) ──
//	     resp, err := p.Upstream.ChatCompletions(body, isStream)
//
//	     // ── Network error (treated as 502, §15.2) ──
//	     if err != nil {
//	         lastStatus = 502
//	         p.LogHttpError(ErrorLogRecord{...}) // stage: "retryable_attempt" or "final_attempt"
//	         return true, nil // retry
//	     }
//
//	     // ── Check status code ──
//	     if IsRetryableStatus(resp.StatusCode) {
//	         lastStatus = resp.StatusCode
//	         p.LogHttpError(ErrorLogRecord{...})
//	         resp.Body.Close()
//	         return true, nil // retry
//	     }
//
//	     // ── Non-retryable (§15.4) or success ──
//	     return false, nil // no retry — resp is returned to caller
//	 })
