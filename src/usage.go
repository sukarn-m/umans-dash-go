package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"
)

// ─── Concurrency / Usage (§6) ─────────────────────────────────────────────────

// ─── Concurrency/Usage (§6) ─────────────────────────────────────────────────

// FetchUsage fetches usage data from the upstream /usage endpoint.
// SPEC §6.1: 10s timeout, 5-min cache TTL (UsageCacheTTL).
// fresh=true bypasses the cache and forces an upstream fetch.
// On non-OK response, returns cached data (if any) or nil.
// When the usage window changes (window.started_at), resets throttledCount
// to 0 and updates throttledWindowStart (§6.1 throttle reset).
func (p *Proxy) FetchUsage(fresh bool) *UsageData {
	// Check cache first (unless fresh=true)
	if !fresh {
		p.mu.RLock()
		if p.usageCache != nil && time.Since(p.usageCacheFetchedAt) < UsageCacheTTL {
			data := p.usageCache
			p.mu.RUnlock()
			return data
		}
		p.mu.RUnlock()
	}

	// Need an UpstreamClient. If p.Upstream is nil (test mode), return cached/nil.
	if p.Upstream == nil {
		p.mu.RLock()
		data := p.usageCache
		p.mu.RUnlock()
		return data
	}

	// Fetch from upstream — use the dashboard API key
	dashKey := p.upstreamAPIKeyForDashboard()
	raw, err := p.Upstream.GetUsage(dashKey)
	if err != nil {
		// On error/non-OK: return cached data or nil (§6.1)
		log.Printf("FetchUsage: upstream fetch failed: %v", err)
		p.mu.RLock()
		data := p.usageCache
		p.mu.RUnlock()
		return data
	}

	// Parse the full response (includes concurrent_sessions, limits, user_id)
	var fullResp struct {
		Usage  UsageInfo    `json:"usage"`
		Window *WindowInfo  `json:"window"`
		Plan   PlanInfo     `json:"plan"`
		Limits *UsageLimits `json:"limits"`
		UserID string       `json:"user_id"`
	}
	if err := json.Unmarshal(raw, &fullResp); err != nil {
		log.Printf("FetchUsage: failed to parse response: %v", err)
		p.mu.RLock()
		data := p.usageCache
		p.mu.RUnlock()
		return data
	}

	data := &UsageData{
		Usage:              fullResp.Usage,
		Window:             fullResp.Window,
		Plan:               fullResp.Plan,
		ConcurrentSessions: fullResp.Usage.ConcurrentSessions,
		Limits:             fullResp.Limits,
		UserID:             fullResp.UserID,
	}

	// §6.1: Throttle reset on window change
	var newWindow string
	if data.Window != nil {
		newWindow = data.Window.StartedAt
	}

	// Step 1: Read old ThrottledWindow under queueMu.RLock (snapshot)
	// Both ThrottledWindow and ThrottledCount are co-located under queueMu.
	p.queueMu.RLock()
	oldWindow := p.ThrottledWindow
	p.queueMu.RUnlock()

	// Step 2: If window changed, reset ThrottledCount under queueMu.Lock()
	if newWindow != "" && newWindow != oldWindow {
		p.queueMu.Lock()
		p.ThrottledCount = 0
		p.ThrottledWindow = newWindow
		p.queueMu.Unlock()
	}

	// Step 3: Write cache fields under p.mu.Lock()
	p.mu.Lock()
	p.usageCache = data
	p.usageCacheFetchedAt = time.Now()
	p.UsageData = data
	p.mu.Unlock()

	return data
}

// FetchUsageHistory fetches usage history from upstream /usage/history with a
// 5-min cache (§6.1). Supports from, to, granularity, and scope query params.
// Cache key includes all params. On failure, returns cached data or an empty
// structure.
func (p *Proxy) FetchUsageHistory(from, to, granularity, scope string, fresh bool) interface{} {
	cacheKey := md5Hash(fmt.Sprintf("%v", map[string]string{"from": from, "to": to, "granularity": granularity, "scope": scope}))

	// Check cache
	if !fresh {
		p.mu.RLock()
		if p.usageHistoryCache != nil && p.usageHistoryCache.key == cacheKey && time.Since(p.usageHistoryCache.fetchedAt) < UsageCacheTTL {
			data := p.usageHistoryCache.data
			p.mu.RUnlock()
			return data
		}
		p.mu.RUnlock()
	}

	// Need an UpstreamClient
	if p.Upstream == nil {
		p.mu.RLock()
		data := p.usageHistoryCache
		p.mu.RUnlock()
		if data != nil {
			return data.data
		}
		return map[string]interface{}{
			"granularity": granularity,
			"from":        from,
			"to":          to,
			"account_id":  nil,
			"buckets":     []interface{}{},
		}
	}

	// Build URL with query params
	params := url.Values{}
	if from != "" {
		params.Set("from", from)
	}
	if to != "" {
		params.Set("to", to)
	}
	if granularity != "" {
		params.Set("granularity", granularity)
	}
	if scope != "" {
		params.Set("scope", scope)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.Config.UpstreamBaseURL+"/usage/history?"+params.Encode(), nil)
	if err != nil {
		p.mu.RLock()
		data := p.usageHistoryCache
		p.mu.RUnlock()
		if data != nil {
			return data.data
		}
		return map[string]interface{}{
			"granularity": granularity,
			"from":        from,
			"to":          to,
			"account_id":  nil,
			"buckets":     []interface{}{},
		}
	}
	req.Header.Set("Authorization", "Bearer "+p.Config.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := p.Upstream.httpClient.Do(req)
	if err != nil {
		p.mu.RLock()
		data := p.usageHistoryCache
		p.mu.RUnlock()
		if data != nil {
			return data.data
		}
		return map[string]interface{}{
			"granularity": granularity,
			"from":        from,
			"to":          to,
			"account_id":  nil,
			"buckets":     []interface{}{},
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		p.mu.RLock()
		data := p.usageHistoryCache
		p.mu.RUnlock()
		if data != nil {
			return data.data
		}
		return map[string]interface{}{
			"granularity": granularity,
			"from":        from,
			"to":          to,
			"account_id":  nil,
			"buckets":     []interface{}{},
		}
	}

	body, err := readBodyText(resp.Body)
	if err != nil {
		p.mu.RLock()
		data := p.usageHistoryCache
		p.mu.RUnlock()
		if data != nil {
			return data.data
		}
		return map[string]interface{}{
			"granularity": granularity,
			"from":        from,
			"to":          to,
			"account_id":  nil,
			"buckets":     []interface{}{},
		}
	}

	var data interface{}
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		p.mu.RLock()
		cached := p.usageHistoryCache
		p.mu.RUnlock()
		if cached != nil {
			return cached.data
		}
		return map[string]interface{}{
			"granularity": granularity,
			"from":        from,
			"to":          to,
			"account_id":  nil,
			"buckets":     []interface{}{},
		}
	}

	// Cache the result
	p.mu.Lock()
	p.usageHistoryCache = &usageHistoryCacheEntry{
		data:      data,
		fetchedAt: time.Now(),
		key:       cacheKey,
	}
	p.mu.Unlock()

	return data
}

// FetchConcurrency fetches usage data and extracts concurrency information.
// SPEC §6.2: extracts concurrent_sessions, limits.concurrency.limit,
// limits.concurrency.hard_cap, and user_id from the usage response.
// Stores the result in LastConcurrency and invalidates the effective
// concurrency cache.
func (p *Proxy) FetchConcurrency(fresh bool) {
	data := p.FetchUsage(fresh)
	if data == nil {
		return
	}

	// Extract concurrency fields
	concurrent := data.ConcurrentSessions // §6.2: concurrent = usage.concurrent_sessions ?? 0

	var limit *int
	var hardCap *int

	if data.Limits != nil && data.Limits.Concurrency != nil {
		// Copy pointers so we don't alias the cached UsageData's internals
		if data.Limits.Concurrency.Limit != nil {
			v := *data.Limits.Concurrency.Limit
			limit = &v
		}
		if data.Limits.Concurrency.HardCap != nil {
			v := *data.Limits.Concurrency.HardCap
			hardCap = &v
		}
	}

	p.mu.Lock()
	p.LastConcurrency = ConcurrencyData{
		Concurrent: concurrent,
		Limit:      limit,
		HardCap:    hardCap,
		UserID:     data.UserID, // A1: extract user_id
	}
	// A2: invalidate effective concurrency cache
	p.effectiveConcurrencyCache = nil
	// §6.4: invalidate cached gate limit
	p.invalidateGateCache()
	p.mu.Unlock()
}

// GetEffectiveConcurrency computes the effective concurrency limits.
// SPEC §6.3: If overrideConcurrency > 0, hard_cap = min(override, apiHardCap)
// (or just override if apiHardCap is nil), overridden = true.
// Otherwise uses API values directly, overridden = false.
// Result is cached in effectiveConcurrencyCache (invalidated by FetchConcurrency).
// Cache key includes Limit, HardCap, and OverrideConcurrency values.
//
// IMPORTANT: Cache is only used in production (p.Upstream != nil). In test mode
// (p.Upstream == nil), test helpers like setLastConcurrency() modify
// p.LastConcurrency directly without invalidating the cache, so we must
// always recompute to avoid returning stale cached results.
//
// The override parameter is passed in by the caller (snapshot under configMu.RLock)
// to avoid recursive configMu.RLock when called from gateLimit() and self-deadlock
// when called from HandleConfigPost (which holds configMu.Lock()).
func (p *Proxy) GetEffectiveConcurrency(lastConcurrency ConcurrencyData, override int) ConcurrencyData {
	// Check cache first (§6.3) — only in production mode
	if p.Upstream != nil {
		p.mu.RLock()
		cached := p.effectiveConcurrencyCache
		if cached != nil {
			// Compare cache key: Limit, HardCap, OverrideConcurrency
			limitMatch := (lastConcurrency.Limit == nil && cached.Limit == nil) ||
				(lastConcurrency.Limit != nil && cached.Limit != nil && *lastConcurrency.Limit == *cached.Limit)
			hardCapMatch := (lastConcurrency.HardCap == nil && cached.HardCap == nil) ||
				(lastConcurrency.HardCap != nil && cached.HardCap != nil && *lastConcurrency.HardCap == *cached.HardCap)
			overrideMatch := cached.Overridden == (override > 0)
			if limitMatch && hardCapMatch && overrideMatch {
				result := *cached
				p.mu.RUnlock()
				return result
			}
		}
		p.mu.RUnlock()
	}

	// Compute the result (existing logic)
	result := lastConcurrency
	if override > 0 {
		result.Overridden = true
		if lastConcurrency.HardCap != nil {
			min := override
			if *lastConcurrency.HardCap < min {
				min = *lastConcurrency.HardCap
			}
			hc := min
			result.HardCap = &hc
		} else {
			hc := override
			result.HardCap = &hc
		}
	}

	// Cache the result (§6.3) — only in production mode
	if p.Upstream != nil {
		p.mu.Lock()
		p.effectiveConcurrencyCache = &result
		p.mu.Unlock()
	}

	return result
}

// BumpThrottled increments the throttled count.
// SPEC §6.4: Called when the queue is full and a 503 response is returned.
func (p *Proxy) BumpThrottled() {
	p.queueMu.Lock()
	p.ThrottledCount++
	p.queueMu.Unlock()
}
