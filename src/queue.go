package proxy

import (
	"log"
	"net/http"
	"runtime"
	"time"
)

// ─── Queue / Dispatch ────────────────────────────────────────────────────────

func (p *Proxy) dispatchItem(item *QueueItem) bool {
	p.queueMu.Lock()

	if p.shuttingDown.Load() {
		p.queueMu.Unlock()
		return false
	}
	if err := item.Req.Context().Err(); err != nil {
		p.queueMu.Unlock()
		return false
	}
	if tw, ok := item.Response.(*responseWriterTracker); ok && tw.IsCommitted() {
		p.queueMu.Unlock()
		return false
	}

	p.ActiveRequests++
	p.queueMu.Unlock()

	go p.runDispatch(item)
	return true
}

// runDispatch executes a request that already owns a slot. It does not re-check
// the gate or modify ActiveRequests; it only runs the proxy handler and calls
// OnRequestComplete() in every exit path via defer.
func (p *Proxy) runDispatch(item *QueueItem) {
	if item.Done != nil {
		defer close(item.Done)
	}
	defer func() {
		if rv := recover(); rv != nil {
			stack := make([]byte, 4096)
			n := runtime.Stack(stack, false)
			log.Printf("panic in runDispatch: %v\n%s", rv, stack[:n])
			if tw, ok := item.Response.(*responseWriterTracker); ok && !tw.IsCommitted() {
				if item.WriteError != nil {
					item.WriteError(item.Response, http.StatusInternalServerError,
						"internal error", "server_error", "panic")
				}
			}
		}
		p.OnRequestComplete()
	}()

	if item.Format == "anthropic" {
		p.proxyAnthropicRequest(item)
	} else {
		p.proxyChatRequest(item.Response, item.Req, item.Payload, item.Model,
			item.WriteError, item.WritePassthroughError)
	}
}

// gateLimit returns the effective concurrency gate based on the concurrency limit mode.
// Returns -1 if no gate applies (both hard_cap and limit are nil).
// Caller must NOT hold queueMu — this method does not acquire it.
func (p *Proxy) gateLimit() int {
	p.cachedGateMu.RLock()
	if p.cachedGateAt.IsZero() || time.Since(p.cachedGateAt) > 1*time.Second {
		p.cachedGateMu.RUnlock()
	} else {
		gate := p.cachedGate
		p.cachedGateMu.RUnlock()
		return gate
	}

	// Snapshot OverrideConcurrency under configMu.RLock and manualLimit/mode
	// via existing thread-safe helpers.
	p.configMu.RLock()
	override := p.Config.OverrideConcurrency
	lastConcurrency := p.LastConcurrency
	p.configMu.RUnlock()

	eff := p.GetEffectiveConcurrency(lastConcurrency, override)

	mode := p.getConcurrencyLimitMode()
	manualLimit := p.getManualLimit()

	var gate int
	switch mode {
	case "manual":
		// If manualLimit is 0 (uninitialized), fall back to soft cap.
		if manualLimit <= 0 {
			if eff.Limit != nil {
				gate = *eff.Limit
			} else if eff.HardCap != nil {
				gate = *eff.HardCap
			} else {
				gate = 1
			}
		} else {
			// Clamp to [1, hardCap] if hardCap is available.
			gate = manualLimit
			if eff.HardCap != nil && gate > *eff.HardCap {
				gate = *eff.HardCap
			}
			if gate < 1 {
				gate = 1
			}
		}
	case "hard":
		if eff.HardCap != nil {
			gate = *eff.HardCap
		} else if eff.Limit != nil {
			gate = *eff.Limit
		} else {
			gate = 1
		}
	default: // "soft" or empty
		if eff.Limit != nil {
			gate = *eff.Limit
		} else if eff.HardCap != nil {
			gate = *eff.HardCap
		} else {
			gate = 1
		}
	}

	p.cachedGateMu.Lock()
	p.cachedGate = gate
	p.cachedGateAt = time.Now()
	p.cachedGateMu.Unlock()
	return gate
}

// invalidateGateCache resets the cached gate limit.
func (p *Proxy) invalidateGateCache() {
	p.cachedGateMu.Lock()
	p.cachedGate = 0
	p.cachedGateAt = time.Time{}
	p.cachedGateMu.Unlock()
}

// getConcurrencyLimitMode returns the current concurrency limit mode (thread-safe).
func (p *Proxy) getConcurrencyLimitMode() string {
	p.concurrencyLimitMu.RLock()
	defer p.concurrencyLimitMu.RUnlock()
	return p.concurrencyLimitMode
}

// setConcurrencyLimitMode updates the concurrency limit mode (thread-safe).
func (p *Proxy) setConcurrencyLimitMode(mode string) {
	p.concurrencyLimitMu.Lock()
	p.concurrencyLimitMode = mode
	p.concurrencyLimitMu.Unlock()
}

// getManualLimit returns the current manual concurrency limit (thread-safe).
func (p *Proxy) getManualLimit() int {
	p.concurrencyLimitMu.RLock()
	defer p.concurrencyLimitMu.RUnlock()
	return p.manualLimit
}

// setManualLimit updates the manual concurrency limit (thread-safe).
// Clamps to >= 1.
func (p *Proxy) setManualLimit(limit int) {
	if limit < 1 {
		limit = 1
	}
	p.concurrencyLimitMu.Lock()
	p.manualLimit = limit
	p.concurrencyLimitMu.Unlock()
}

// getRetryAttempts returns the configured retry count (thread-safe).
// NOTE: The `< 0` guard (not `<= 0`) is required because RetryAttempts is a
// *int — a user-explicit 0 must pass through as 0 (no retries). See §2.9
// for the full rationale on the *int pointer design.
func (p *Proxy) getRetryAttempts() int {
	p.retryConfigMu.RLock()
	defer p.retryConfigMu.RUnlock()
	n := p.retryAttempts
	if n < 0 {
		n = DefaultRetryAttempts
	}
	if n > MaxRetryAttemptsCap {
		n = MaxRetryAttemptsCap
	}
	return n
}

// getBackoffStrategy returns the configured backoff preset name (thread-safe).
// Returns "aggressive" if empty.
func (p *Proxy) getBackoffStrategy() string {
	p.retryConfigMu.RLock()
	defer p.retryConfigMu.RUnlock()
	if p.backoffStrategy == "" {
		return "aggressive"
	}
	return p.backoffStrategy
}

// setRetryConfig snapshots the retry config fields under retryConfigMu.
// Called from NewProxy and HandleConfigPost.
func (p *Proxy) setRetryConfig(attempts int, strategy string) {
	p.retryConfigMu.Lock()
	p.retryAttempts = attempts
	p.backoffStrategy = strategy
	p.retryConfigMu.Unlock()
}

// getVisionHandoffLimit reads Config.VisionHandoffConcurrency under configMu.RLock.
func (p *Proxy) getVisionHandoffLimit() int {
	p.configMu.RLock()
	defer p.configMu.RUnlock()
	return p.Config.VisionHandoffConcurrency
}

// trySignalQueueLocked schedules one ProcessQueue pass. Caller must hold queueMu (write).
func (p *Proxy) trySignalQueueLocked() {
	if p.queuePending || p.shuttingDown.Load() {
		return
	}
	p.queuePending = true
	p.queueCond.Signal()
}

// signalQueue schedules one ProcessQueue pass.
func (p *Proxy) signalQueue() {
	p.queueMu.Lock()
	defer p.queueMu.Unlock()
	p.trySignalQueueLocked()
}

// queueWorker runs in a background goroutine and processes the queue when signaled.
func (p *Proxy) queueWorker() {
	defer p.workerWG.Done()
	for {
		p.queueMu.Lock()
		for !p.queuePending && !p.shuttingDown.Load() {
			p.queueCond.Wait()
		}
		if p.shuttingDown.Load() {
			p.queuePending = false
			p.queueMu.Unlock()
			return
		}
		p.queuePending = false
		p.queueMu.Unlock()
		p.ProcessQueue()
	}
}

// TryEnqueueOrDispatch attempts to dispatch a request immediately if under the
// TryEnqueueOrDispatch attempts to dispatch a request immediately if there is no
// concurrency gate, or enqueues it for the queue worker to dispatch when a gate
// is in effect.
//
// Contract:
//   - Returns true if the request was dispatched directly. ActiveRequests is
//     already incremented; OnRequestComplete() is called by runDispatch.
//   - Returns false if the request was enqueued or rejected. The caller must
//     NOT call OnRequestComplete() in this case.
func (p *Proxy) TryEnqueueOrDispatch(item *QueueItem) bool {
	if p.shuttingDown.Load() {
		if item.WriteError != nil {
			item.WriteError(item.Response, http.StatusServiceUnavailable,
				"service unavailable", "server_error", "shutting_down")
		}
		if item.Done != nil {
			close(item.Done)
		}
		return false
	}

	p.queueMu.Lock()
	gate := p.gateLimit()

	// No gate → dispatch immediately (queue is drained in ProcessQueue).
	if gate < 0 {
		p.queueMu.Unlock()
		if p.dispatchItem(item) {
			return true
		}
		// dispatchItem returned false: shutdown, cancelled, or committed.
		// Do NOT re-enqueue; the item was not consumed.
		if item.Done != nil {
			close(item.Done)
		}
		return false
	}

	if len(p.requestQueue) >= MaxQueueSize {
		p.ThrottledCount++
		p.queueMu.Unlock()
		WriteQueueFullError(item.Response, item.Format)
		if item.Done != nil {
			close(item.Done)
		}
		return false
	}

	p.requestQueue = append(p.requestQueue, item)
	p.QueueLen = len(p.requestQueue)
	p.trySignalQueueLocked()
	p.queueMu.Unlock()
	return false
}

// GetQueueLength returns the current queue length in a thread-safe manner.
func (p *Proxy) GetQueueLength() int {
	p.queueMu.RLock()
	defer p.queueMu.RUnlock()
	return len(p.requestQueue)
}

// getThrottledCount returns the current throttled count under queueMu.RLock.
func (p *Proxy) getThrottledCount() int {
	p.queueMu.RLock()
	defer p.queueMu.RUnlock()
	return p.ThrottledCount
}

// getActiveRequests returns the current active request count under queueMu.RLock.
func (p *Proxy) getActiveRequests() int {
	p.queueMu.RLock()
	defer p.queueMu.RUnlock()
	return p.ActiveRequests
}

// getQueueLen returns the current queue length field under queueMu.RLock.
func (p *Proxy) getQueueLen() int {
	p.queueMu.RLock()
	defer p.queueMu.RUnlock()
	return p.QueueLen
}

// getSlotFreeDelay returns the configured slot-free delay in seconds (thread-safe).
// Reads Config.SlotFreeDelay under configMu.RLock.
func (p *Proxy) getSlotFreeDelay() int {
	p.configMu.RLock()
	defer p.configMu.RUnlock()
	return p.Config.SlotFreeDelay
}

// OnRequestComplete decrements the active request count and signals the queue.
// If SlotFreeDelay > 0, it waits that many seconds before freeing the slot.
// The delay goroutine listens on queueDone so shutdown can cancel pending delays.
func (p *Proxy) OnRequestComplete() {
	delay := p.getSlotFreeDelay()

	if delay <= 0 || p.shuttingDown.Load() {
		p.queueMu.Lock()
		if p.ActiveRequests > 0 {
			p.ActiveRequests--
		}
		p.queueMu.Unlock()
		p.signalQueue()
		return
	}

	go func() {
		select {
		case <-time.After(time.Duration(delay) * time.Second):
		case <-p.queueDone:
			// Shutdown cancelled the delay; release immediately.
		}
		p.queueMu.Lock()
		if p.ActiveRequests > 0 {
			p.ActiveRequests--
		}
		p.queueMu.Unlock()
		p.signalQueue()
	}()
}

// rejectQueueLocked writes a 503 to every queued item and clears the queue.
// Caller must hold queueMu.
func (p *Proxy) rejectQueueLocked() {
	for _, item := range p.requestQueue {
		if tw, ok := item.Response.(*responseWriterTracker); ok && tw.IsCommitted() {
			continue
		}
		if item.WriteError != nil {
			item.WriteError(item.Response, http.StatusServiceUnavailable,
				"server is shutting down", "server_error", "shutting_down")
		}
		if item.Done != nil {
			close(item.Done)
		}
	}
	p.requestQueue = nil
	p.QueueLen = 0
}

// ProcessQueue dispatches queued requests while there is capacity.
// It is the only dispatcher when a concurrency limit is in effect.
// It reserves slots under queueMu, removes the corresponding items from the
// queue, then launches goroutines that already own their slots.
//
// If gateLimit() returns < 0 (no limit in effect), the queue is drained under
// queueMu and all queued items are dispatched immediately. This prevents
// already-queued items from being stranded when the limit is removed at runtime.
func (p *Proxy) ProcessQueue() int {
	if p.shuttingDown.Load() {
		return 0
	}

	p.queueMu.Lock()

	// If no limit is in effect, drain the entire queue immediately.
	gate := p.gateLimit()
	if gate < 0 {
		if len(p.requestQueue) == 0 {
			p.queueMu.Unlock()
			return 0
		}
		toDispatch := make([]*QueueItem, len(p.requestQueue))
		copy(toDispatch, p.requestQueue)
		p.requestQueue = nil
		p.QueueLen = 0
		for range toDispatch {
			p.ActiveRequests++
		}
		p.queueMu.Unlock()
		for _, item := range toDispatch {
			go p.runDispatch(item)
		}
		return len(toDispatch)
	}

	var toDispatch []*QueueItem
	var cancelled []*QueueItem

	for i := 0; i < len(p.requestQueue); i++ {
		// Re-check shutdown and gate inside the loop.
		if p.shuttingDown.Load() {
			break
		}
		currentGate := p.gateLimit()
		if currentGate != gate {
			// Gate changed; stop this pass and let the next signal re-evaluate.
			break
		}
		if p.ActiveRequests >= currentGate {
			// No free slots. The next completion will signal a new pass.
			break
		}

		item := p.requestQueue[i]
		if item.Req.Context().Err() != nil {
			cancelled = append(cancelled, item)
			continue
		}

		// Reserve the slot while holding queueMu.
		p.ActiveRequests++
		toDispatch = append(toDispatch, item)
	}

	// Remove dispatched/cancelled items from the head of the queue.
	processed := len(toDispatch) + len(cancelled)
	if processed > 0 {
		p.requestQueue = p.requestQueue[processed:]
	}
	p.QueueLen = len(p.requestQueue)
	p.queueMu.Unlock()

	// Write cancellation errors outside queueMu.
	for _, item := range cancelled {
		if tw, ok := item.Response.(*responseWriterTracker); ok && !tw.IsCommitted() {
			if item.WriteError != nil {
				item.WriteError(item.Response, http.StatusGatewayTimeout,
					"queue timeout: client disconnected", "server_error", "client_cancelled")
			}
		}
		if item.Done != nil {
			close(item.Done)
		}
	}

	// Dispatch reserved slots outside queueMu.
	for _, item := range toDispatch {
		go p.runDispatch(item)
	}

	if len(toDispatch) > 0 {
		p.signalQueue()
	}
	return len(toDispatch)
}

// ResetQueue clears all queue state.
func (p *Proxy) ResetQueue() {
	p.queueMu.Lock()
	defer p.queueMu.Unlock()

	// ResetQueue clears queue accounting. It is only safe when there are no
	// in-flight requests whose goroutines will later call OnRequestComplete().
	// If called while requests are active, ActiveRequests accounting will diverge.
	p.ActiveRequests = 0
	p.requestQueue = nil
	p.QueueLen = 0
	p.ThrottledCount = 0
}
