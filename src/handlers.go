package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"
)

// ─── HTTP Handlers (§17-§29) ───────────────────────────────────────────────

// ─── HTTP Handlers (§17-§29) ───────────────────────────────────────────────

func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

func (p *Proxy) HandleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}

	// started_at and uptime_sec (StartedAt may be zero in tests)
	var startedAtStr string
	var uptimeSec int
	if !p.StartedAt.IsZero() {
		startedAtStr = p.StartedAt.UTC().Format(time.RFC3339)
		uptimeSec = int(time.Since(p.StartedAt).Seconds())
	}

	// api_key_valid — use cached validation status when available.
	apiKeyValid := false
	p.mu.RLock()
	if p.UserInfoCache != nil && p.UserInfoCache.data != nil &&
		time.Since(p.UserInfoCache.fetchedAt) < UserInfoCacheTTL {
		apiKeyValid = true
	}
	p.mu.RUnlock()
	if !apiKeyValid && p.Upstream != nil {
		apiKeyValid = p.ValidateApiKey()
	}

	// token_state, valid_tokens, total_tokens (KeyPool may be nil in tests)
	// Initialize as empty slice so nil KeyPool marshals to [] not null (SPEC §22)
	tokenState := []KeyState{}
	var validTokens int
	var totalTokens int
	if p.KeyPool != nil {
		tokenState = p.KeyPool.State()
		validTokens = p.KeyPool.HealthyCount()
		totalTokens = p.KeyPool.Total()
	}

	// models_count
	modelsCount := len(p.GetEffectiveModels())

	// Vision handoff cache stats
	var handoffStats HandoffCacheStats
	if p.ImageHandoffCache != nil {
		handoffStats = p.ImageHandoffCache.Stats()
	}

	WriteJSON(w, http.StatusOK, map[string]interface{}{
		"ok":              true,
		"started_at":      startedAtStr,
		"uptime_sec":      uptimeSec,
		"api_key_valid":   apiKeyValid,
		"provider":        "umans",
		"token_state":     tokenState,
		"valid_tokens":    validTokens,
		"total_tokens":    totalTokens,
		"models_count":    modelsCount,
		"runtime":         "go",
		"runtime_version": runtime.Version(),
		"version":         p.Version,
		"port":            ParseListenPort(p.Config.ListenAddr),
		"visionHandoff": map[string]interface{}{
			"enabled":      p.Config.VisionHandoffEnabled,
			"cacheEnabled": p.Config.VisionHandoffCacheEnabled,
			"cache": map[string]interface{}{
				"size":      handoffStats.Size,
				"maxSize":   handoffStats.MaxSize,
				"ttlMs":     handoffStats.TtlMs,
				"hits":      handoffStats.Hits,
				"misses":    handoffStats.Misses,
				"evictions": handoffStats.Evictions,
			},
		},
	})
}

func (p *Proxy) HandleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}
	models := p.GetEffectiveModels()
	p.catalogMu.RLock()
	modelInfo := p.ModelInfoMap
	displayNames := p.DisplayNameMap
	upstreamPricing := p.UpstreamPricing
	p.catalogMu.RUnlock()
	var data []map[string]interface{}
	for _, m := range models {
		info := modelInfo[m]
		entry := map[string]interface{}{
			"id":           m,
			"object":       "model",
			"created":      p.StartedAt.Unix(),
			"owned_by":     "umans",
			"root":         m,
			"permission":   []interface{}{},
			"display_name": displayNames[m],
		}
		if cw, ok := toInt(info.Capabilities.ContextWindow); ok && cw > 0 {
			entry["context_length"] = cw
			outputLimit := cw
			if rmt, ok := toInt(info.Capabilities.RecommendedMaxTokens); ok && rmt > 0 {
				outputLimit = rmt
			} else if mct, ok := toInt(info.Capabilities.MaxCompletionTokens); ok && mct > 0 {
				outputLimit = mct
			}
			entry["max_output_tokens"] = outputLimit
		}
		if pr, ok := upstreamPricing[m]; ok {
			pricing := map[string]interface{}{}
			if pr.Input > 0 {
				pricing["prompt"] = pr.Input / 1000000.0
			}
			if pr.Output > 0 {
				pricing["completion"] = pr.Output / 1000000.0
			}
			if len(pricing) > 0 {
				entry["pricing"] = pricing
			}
		}
		data = append(data, entry)
	}
	if data == nil {
		data = []map[string]interface{}{}
	}
	WriteJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

// HandleValidate validates the API key by calling the upstream user info
// endpoint. §17.1 Route #4.
func (p *Proxy) HandleValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}
	valid := p.ValidateApiKey()
	WriteJSON(w, http.StatusOK, map[string]interface{}{
		"valid": valid,
	})
}

// HandleApiModels returns the list of effective models plus disabled models
// and display names. Distinct from /v1/models. §17.1 Route #5.
func (p *Proxy) HandleApiModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}
	models := p.GetEffectiveModels()
	p.configMu.RLock()
	disabled := p.Config.DisabledModels
	p.configMu.RUnlock()
	p.catalogMu.RLock()
	// Deep copy displayNames to avoid racing with concurrent catalog updates.
	displayNames := make(map[string]string, len(p.DisplayNameMap))
	for k, v := range p.DisplayNameMap {
		displayNames[k] = v
	}
	p.catalogMu.RUnlock()
	WriteJSON(w, http.StatusOK, map[string]interface{}{
		"models":         models,
		"disabledModels": disabled,
		"displayNames":   displayNames,
	})
}

// HandleModelsInfo returns the raw model catalog (ModelInfoMap). §17.1 Route #17.
func (p *Proxy) HandleModelsInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}
	p.catalogMu.RLock()
	// Deep copy ModelInfoMap to avoid racing with concurrent catalog updates.
	modelInfoCopy := make(map[string]ModelInfo, len(p.ModelInfoMap))
	for k, v := range p.ModelInfoMap {
		modelInfoCopy[k] = v // ModelInfo is a value type with interface{} fields — shallow copy is sufficient
	}
	p.catalogMu.RUnlock()
	WriteJSON(w, http.StatusOK, modelInfoCopy)
}

// HandleChatCompletions is the entry point for /v1/chat/completions (§19).
// It performs auth, body reading, JSON parsing, model extraction, and
// dispatches through the concurrency queue to proxyChatRequest.
func (p *Proxy) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// §35.1: recover from panics
	defer func() {
		if rv := recover(); rv != nil {
			log.Printf("PANIC in HandleChatCompletions: %v", rv)
			if tw, ok := w.(*responseWriterTracker); ok && tw.IsCommitted() {
				return
			}
			WriteOpenAIError(w, http.StatusInternalServerError,
				"internal server error", "server_error", "")
		}
	}()

	if r.Method != http.MethodPost {
		WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}

	// §17.2: Authentication
	if !p.Authorized(r) {
		WriteOpenAIError(w, http.StatusUnauthorized, "invalid API key", "authentication_error", "")
		return
	}

	// §17.3: Read body with size limit
	bodyStr, err := ReadBody(r)
	if err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, "failed to read request body: "+err.Error(), "invalid_request_error", "")
		return
	}

	// Parse JSON body
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(bodyStr), &payload); err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON in request body: "+err.Error(), "invalid_request_error", "")
		return
	}

	// Extract model from payload
	model, _ := payload["model"].(string)
	if model == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "missing required field: model", "invalid_request_error", "")
		return
	}

	// Construct QueueItem for the concurrency queue.
	// Detach the request context so the queue item survives ServeHTTP's
	// return. The async queue dispatch may run after ServeHTTP returns and
	// its defer cancel() fires, which would cancel the original context and
	// cause every queued request to be rejected as "client disconnected".
	// We keep the timeout deadline and propagate cancellation via the tracker's
	// clientCtx for client-disconnect detection in pipeBodyToResponse.
	done := make(chan struct{})
	queueReq := r.WithContext(detachedContext(r.Context()))
	item := QueueItem{
		Format:                "openai",
		Response:              w,
		Payload:               payload,
		Model:                 model,
		WriteError:            OpenAIErrorWriter(),
		WritePassthroughError: OpenAIPassthroughErrorWriter(),
		Req:                   queueReq,
		Done:                  done,
	}

	// §5.3: Try to dispatch immediately or enqueue
	if p.TryEnqueueOrDispatch(&item) {
		// Dispatched synchronously (no gate) — runDispatch runs in a goroutine.
		// Wait for it to complete so ServeHTTP doesn't return (and finalize the
		// response) before the request is done.
		<-done
		return
	}
	// If TryEnqueueOrDispatch returned false, the request was enqueued.
	// Wait for the queue worker to dispatch and complete it.
	<-done
}

// detachedContext returns a context that is never cancelled and has no
// deadline, but carries the values from the parent. This is used for queue
// items whose dispatch may outlive the ServeHTTP call that created them.
// Without this, the context derived from r.Context() is cancelled when
// ServeHTTP returns (after the handler enqueues the item and returns),
// causing every queued request to be rejected as "client disconnected" before
// the queue worker can dispatch it. Client-disconnect detection for streaming
// is handled separately via responseWriterTracker.clientCtx in pipeBodyToResponse.
func detachedContext(parent context.Context) context.Context {
	return context.WithoutCancel(parent)
}

// HandleMessages is the Anthropic Messages pipeline (§20).
func (p *Proxy) HandleMessages(w http.ResponseWriter, r *http.Request) {
	// §35.1: recover from panics
	defer func() {
		if rv := recover(); rv != nil {
			log.Printf("PANIC in HandleMessages: %v", rv)
			if tw, ok := w.(*responseWriterTracker); ok && tw.IsCommitted() {
				return
			}
			WriteAnthropicError(w, http.StatusInternalServerError,
				"internal server error", "api_error")
		}
	}()

	// Method check
	if r.Method != http.MethodPost {
		WriteAnthropicError(w, http.StatusMethodNotAllowed,
			"method not allowed", "invalid_request_error")
		return
	}

	// §17.3: Read body
	bodyStr, err := ReadBody(r)
	if err != nil {
		WriteAnthropicError(w, http.StatusBadRequest,
			fmt.Sprintf("failed to read request body: %v", err),
			"invalid_request_error")
		return
	}

	// Parse JSON
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(bodyStr), &payload); err != nil {
		WriteAnthropicError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid JSON: %v", err),
			"invalid_request_error")
		return
	}

	// Extract model
	model, _ := payload["model"].(string)

	// Extract stream flag (default false)
	isStream := false
	if streamVal, ok := payload["stream"]; ok {
		if b, ok := streamVal.(bool); ok {
			isStream = b
		}
	}

	// Compute fingerprint (§14)
	fp := FingerprintPayload(payload)

	// Session tracking — get preferred key index (§14.5)
	preferredIndex := -1
	if fp != "" {
		session := p.TouchConversation(fp)
		if session != nil {
			preferredIndex = session.TokenIndex
		}
	}

	// Construct QueueItem
	// Detach the request context so the queue item survives ServeHTTP's
	// return (see HandleChatCompletions for rationale).
	done := make(chan struct{})
	queueReq := r.WithContext(detachedContext(r.Context()))
	item := QueueItem{
		Response:              w,
		Payload:               payload,
		Model:                 model,
		WriteError:            AnthropicErrorWriter(),
		WritePassthroughError: AnthropicPassthroughErrorWriter(),
		Format:                "anthropic",
		Req:                   queueReq,
		Fingerprint:           fp,
		PreferredKeyIndex:     preferredIndex,
		IsStream:              isStream,
		Done:                  done,
	}

	// Try to dispatch immediately or enqueue (§5)
	dispatched := p.TryEnqueueOrDispatch(&item)
	_ = dispatched
	// Wait for the queue worker (or inline dispatch) to complete the request.
	<-done
}

func (p *Proxy) HandleConfigGet(w http.ResponseWriter, r *http.Request) {
	p.configMu.RLock()
	defer p.configMu.RUnlock()
	WriteJSON(w, http.StatusOK, map[string]interface{}{
		"listenAddr":                p.Config.ListenAddr,
		"upstreamBaseURL":           p.Config.UpstreamBaseURL,
		"apiKey":                    MaskToken(p.Config.APIKey),
		"enabledModels":             p.Config.EnabledModels,
		"modelDisplayNames":         p.Config.ModelDisplayNames,
		"overrideConcurrency":       p.Config.OverrideConcurrency,
		"maxImages":                 p.Config.MaxImages,
		"disabledModels":            p.Config.DisabledModels,
		"visionHandoffEnabled":      p.Config.VisionHandoffEnabled,
		"visionHandoffModel":        p.Config.VisionHandoffModel,
		"visionHandoffPrompt":       p.Config.VisionHandoffPrompt,
		"visionHandoffCacheEnabled": p.Config.VisionHandoffCacheEnabled,
		"wallpaperSource":           p.Config.WallpaperSource,
		"apikeyMode":                p.Config.ApiKeyMode,
		"concurrencyLimitMode":      p.getConcurrencyLimitMode(),
		"manualConcurrencyLimit":    p.getManualLimit(),
		"slotFreeDelay":             p.Config.SlotFreeDelay,
		"retryAttempts":             p.getRetryAttempts(),
		"backoffStrategy":           p.getBackoffStrategy(),
		"requestTimeout":            p.Config.RequestTimeout.DurationMs() / 1000,
	})
}

func (p *Proxy) HandleConfigPost(w http.ResponseWriter, r *http.Request) {
	body, err := ReadBody(r)
	if err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "invalid JSON"})
		return
	}

	needSignalQueue := false

	p.configMu.Lock()
	if v, ok := data["apiKey"].(string); ok {
		p.Config.APIKey = v
	}
	if v, ok := data["apiKeys"].([]interface{}); ok {
		keys := make([]string, 0, len(v))
		for _, k := range v {
			if s, ok := k.(string); ok {
				keys = append(keys, s)
			}
		}
		p.Config.APIKeys = keys
	}
	restartRequired := false
	if v, ok := data["listenAddr"].(string); ok {
		if v != p.Config.ListenAddr {
			restartRequired = true
		}
		p.Config.ListenAddr = v
	}
	if v, ok := data["enabledModels"].([]interface{}); ok {
		models := make([]string, 0, len(v))
		for _, m := range v {
			if s, ok := m.(string); ok {
				models = append(models, s)
			}
		}
		p.Config.EnabledModels = models
	}
	if v, ok := data["modelDisplayNames"].(map[string]interface{}); ok {
		names := make(map[string]string)
		for k, val := range v {
			if s, ok := val.(string); ok {
				names[k] = s
			}
		}
		p.Config.ModelDisplayNames = names
	}
	if v, ok := data["wallpaperSource"].(string); ok {
		switch v {
		case "none", "bing", "wallhaven":
			p.Config.WallpaperSource = v
			p.invalidateDashboardCache()
		default:
			p.configMu.Unlock()
			WriteJSON(w, http.StatusBadRequest, map[string]interface{}{
				"error": "wallpaperSource must be one of: none, bing, wallhaven",
			})
			return
		}
	}
	if v, ok := data["overrideConcurrency"].(float64); ok {
		if v < 0 {
			p.configMu.Unlock()
			WriteJSON(w, http.StatusBadRequest, map[string]interface{}{
				"error": "overrideConcurrency must be >= 0",
			})
			return
		}
		p.Config.OverrideConcurrency = int(v)
	}
	if v, ok := data["maxImages"].(float64); ok {
		if v < 0 {
			p.configMu.Unlock()
			WriteJSON(w, http.StatusBadRequest, map[string]interface{}{
				"error": "maxImages must be >= 0",
			})
			return
		}
		p.Config.MaxImages = int(v)
	}
	if v, ok := data["disabledModels"].([]interface{}); ok {
		models := make([]string, 0, len(v))
		for _, m := range v {
			if s, ok := m.(string); ok {
				models = append(models, s)
			}
		}
		p.Config.DisabledModels = models
	}
	if v, ok := data["visionHandoffEnabled"].(bool); ok {
		p.Config.VisionHandoffEnabled = v
	}
	if v, ok := data["visionHandoffModel"].(string); ok {
		p.Config.VisionHandoffModel = v
	}
	if v, ok := data["visionHandoffPrompt"].(string); ok {
		p.Config.VisionHandoffPrompt = v
	}
	if v, ok := data["visionHandoffCacheEnabled"].(bool); ok {
		p.Config.VisionHandoffCacheEnabled = v
	}
	if v, ok := data["apikeyMode"].(string); ok {
		if v == "managed" || v == "passthrough" || v == "smart" {
			p.Config.ApiKeyMode = v
		}
	}
	if mode, ok := data["concurrencyLimitMode"].(string); ok {
		if mode == "soft" || mode == "hard" || mode == "manual" {
			p.Config.ConcurrencyLimitMode = mode
			p.setConcurrencyLimitMode(mode)
			// Sync deprecated BurstMode for rollback safety.
			p.Config.BurstMode = (mode == "hard")
			// When switching to manual mode with manualLimit == 0,
			// initialize it to the current soft cap (or hard cap if no soft cap).
			if mode == "manual" && p.getManualLimit() == 0 {
				override := p.Config.OverrideConcurrency
				eff := p.GetEffectiveConcurrency(p.LastConcurrency, override)
				var defaultLimit int
				if eff.Limit != nil {
					defaultLimit = *eff.Limit
				} else if eff.HardCap != nil {
					defaultLimit = *eff.HardCap
				} else {
					defaultLimit = 1
				}
				p.Config.ManualConcurrencyLimit = defaultLimit
				p.setManualLimit(defaultLimit)
			}
			// Any mode change can change the gate, so signal the queue
			// to dispatch waiting items or clear stale state.
			needSignalQueue = true
		}
		// §6.4: invalidate cached gate on concurrency mode change
		p.invalidateGateCache()
	}
	if v, ok := data["manualConcurrencyLimit"].(float64); ok {
		limit := int(v)
		if limit < 1 {
			limit = 1
		}
		p.Config.ManualConcurrencyLimit = limit
		p.setManualLimit(limit)
		// §6.4: invalidate cached gate on manual limit change
		p.invalidateGateCache()
		needSignalQueue = true
	}
	if v, ok := data["slotFreeDelay"].(float64); ok {
		delay := int(v)
		if delay < 0 {
			delay = 0
		}
		p.Config.SlotFreeDelay = delay
	}
	if v, ok := data["retryAttempts"].(float64); ok {
		attempts := int(v)
		if attempts < 0 {
			attempts = 0
		}
		if attempts > MaxRetryAttemptsCap {
			attempts = MaxRetryAttemptsCap
		}
		p.Config.RetryAttempts = &attempts
		p.setRetryConfig(attempts, p.getBackoffStrategy())
	}
	if v, ok := data["backoffStrategy"].(string); ok {
		if _, ok2 := BackoffPresets[v]; ok2 {
			p.Config.BackoffStrategy = v
			p.setRetryConfig(p.getRetryAttempts(), v)
		}
	}
	if v, ok := data["requestTimeout"].(float64); ok {
		secs := int(v)
		if secs < 30 {
			secs = 30
		}
		if secs > 1800 { // 30 minutes
			secs = 1800
		}
		p.Config.RequestTimeout = Duration{time.Duration(secs) * time.Second}
		if p.Upstream != nil {
			p.Upstream.SetTimeout(p.Config.RequestTimeout.Duration)
			p.Upstream.CloseIdleConnections()
		}
	}
	if v, ok := data["keys"].([]interface{}); ok {
		keys := make([]KeyConfig, 0, len(v))
		for _, k := range v {
			if m, ok := k.(map[string]interface{}); ok {
				kc := KeyConfig{}
				if name, ok := m["name"].(string); ok {
					kc.Name = name
				}
				if key, ok := m["key"].(string); ok {
					kc.Key = key
				}
				if session, ok := m["session"].(string); ok {
					kc.Session = session
				}
				keys = append(keys, kc)
			}
		}
		p.Config.Keys = keys
		p.KeyPool = NewKeyPool(keys)
	}

	// Snapshot response data while holding the lock.
	respData := map[string]interface{}{
		"success":                   true,
		"listenAddr":                p.Config.ListenAddr,
		"upstreamBaseURL":           p.Config.UpstreamBaseURL,
		"apiKey":                    MaskToken(p.Config.APIKey),
		"enabledModels":             p.Config.EnabledModels,
		"modelDisplayNames":         p.Config.ModelDisplayNames,
		"overrideConcurrency":       p.Config.OverrideConcurrency,
		"maxImages":                 p.Config.MaxImages,
		"disabledModels":            p.Config.DisabledModels,
		"visionHandoffEnabled":      p.Config.VisionHandoffEnabled,
		"visionHandoffModel":        p.Config.VisionHandoffModel,
		"visionHandoffPrompt":       p.Config.VisionHandoffPrompt,
		"visionHandoffCacheEnabled": p.Config.VisionHandoffCacheEnabled,
		"wallpaperSource":           p.Config.WallpaperSource,
		"apikeyMode":                p.Config.ApiKeyMode,
		"concurrencyLimitMode":      p.getConcurrencyLimitMode(),
		"manualConcurrencyLimit":    p.getManualLimit(),
		"slotFreeDelay":             p.Config.SlotFreeDelay,
		"retryAttempts":             p.getRetryAttempts(),
		"backoffStrategy":           p.getBackoffStrategy(),
		"requestTimeout":            p.Config.RequestTimeout.DurationMs() / 1000,
		"restartRequired":           restartRequired,
	}
	debouncedSaveConfig(*p.Config)
	p.invalidateDashboardCache()

	// Release configMu BEFORE calling signalQueue to avoid lock-order inversion
	// with queue paths that hold queueMu and then call gateLimit() (which needs
	// configMu.RLock()).
	p.configMu.Unlock()

	if needSignalQueue {
		p.signalQueue()
	}

	WriteJSON(w, http.StatusOK, respData)
}

func (p *Proxy) HandleKeysGet(w http.ResponseWriter, r *http.Request) {
	p.configMu.RLock()
	defer p.configMu.RUnlock()
	var safe []map[string]interface{}
	var maskedKeys []map[string]interface{}
	for _, k := range p.Config.Keys {
		safe = append(safe, map[string]interface{}{
			"name":         k.Name,
			"token_masked": MaskToken(k.Key),
			"has_token":    k.Key != "",
			"has_session":  k.Session != "",
		})
		maskedKeys = append(maskedKeys, map[string]interface{}{
			"name":    k.Name,
			"key":     MaskToken(k.Key),
			"session": k.Session,
		})
	}
	if safe == nil {
		safe = []map[string]interface{}{}
	}
	if maskedKeys == nil {
		maskedKeys = []map[string]interface{}{}
	}
	WriteJSON(w, http.StatusOK, map[string]interface{}{
		"keys": maskedKeys,
		"safe": safe,
	})
}

func (p *Proxy) HandleKeysPost(w http.ResponseWriter, r *http.Request) {
	body, err := ReadBody(r)
	if err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "invalid JSON"})
		return
	}
	action, _ := data["action"].(string)
	switch action {
	case "add":
		name, _ := data["name"].(string)
		if name == "" {
			name = fmt.Sprintf("Key %d", len(p.Config.Keys)+1)
		}
		key, _ := data["key"].(string)
		p.Config.Keys = append(p.Config.Keys, KeyConfig{Name: name, Key: key, Session: ""})
		if p.Config.APIKey == "" && key != "" {
			p.Config.APIKey = key
		}
		p.KeyPool = NewKeyPool(p.Config.Keys)
		debouncedSaveConfig(*p.Config)

		WriteJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"keys":    maskKeysForResponse(p.Config.Keys),
		})
	case "update":
		idxFloat, _ := data["index"].(float64)
		idx := int(idxFloat)
		if idx < 0 || idx >= len(p.Config.Keys) {
			WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "Key not found"})
			return
		}
		if name, ok := data["name"].(string); ok {
			p.Config.Keys[idx].Name = name
		}
		if key, ok := data["key"].(string); ok {
			p.Config.Keys[idx].Key = key
		}
		if session, ok := data["session"].(string); ok {
			p.Config.Keys[idx].Session = session
		}
		if idx == 0 {
			p.Config.APIKey = p.Config.Keys[0].Key
		}
		p.KeyPool = NewKeyPool(p.Config.Keys)
		debouncedSaveConfig(*p.Config)

		WriteJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"keys":    maskKeysForResponse(p.Config.Keys),
		})
	case "delete":
		idxFloat, _ := data["index"].(float64)
		idx := int(idxFloat)
		if idx < 0 || idx >= len(p.Config.Keys) {
			WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "Key not found"})
			return
		}
		p.Config.Keys = append(p.Config.Keys[:idx], p.Config.Keys[idx+1:]...)
		if len(p.Config.Keys) == 0 {
			p.Config.Keys = []KeyConfig{{Name: "Key 1", Key: "", Session: ""}}
		}
		if idx == 0 {
			p.Config.APIKey = ""
			if len(p.Config.Keys) > 0 {
				p.Config.APIKey = p.Config.Keys[0].Key
			}
		}
		p.KeyPool = NewKeyPool(p.Config.Keys)
		debouncedSaveConfig(*p.Config)

		WriteJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"keys":    maskKeysForResponse(p.Config.Keys),
		})
	default:
		WriteJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "Unknown action"})
	}
}

// maskKeysForResponse builds a masked copy of the keys for API responses,
// mirroring HandleKeysGet's maskedKeys format. Never leaks raw key values.
func maskKeysForResponse(keys []KeyConfig) []map[string]interface{} {
	masked := make([]map[string]interface{}, len(keys))
	for i, k := range keys {
		masked[i] = map[string]interface{}{
			"name":    k.Name,
			"key":     MaskToken(k.Key),
			"session": k.Session,
		}
	}
	return masked
}

func (p *Proxy) HandleUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}
	// §28.1: ?fresh=1 bypasses cache
	fresh := r.URL.Query().Get("fresh") == "1"

	// Attempt to fetch fresh usage data if Upstream is available.
	// If Upstream is nil (test mode), FetchUsage returns cached/nil data.
	if p.Upstream != nil {
		p.FetchUsage(fresh)
	}

	// Snapshot shared fields under RLock before writing HTTP response.
	// FetchUsage (if called above) has already released its write lock,
	// so RLock cannot deadlock.
	var usage interface{}
	var window interface{}
	var plan interface{}
	p.mu.RLock()
	usageData := p.UsageData
	p.mu.RUnlock()
	throttled := p.getThrottledCount()
	if usageData != nil {
		usage = usageData.Usage
		window = usageData.Window
		plan = usageData.Plan
	}
	WriteJSON(w, http.StatusOK, map[string]interface{}{
		"usage":     usage,
		"window":    window,
		"plan":      plan,
		"throttled": throttled,
	})
}

func (p *Proxy) HandleConcurrency(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}
	// §28.2: ?fresh=1 bypasses cache
	fresh := r.URL.Query().Get("fresh") == "1"

	// Attempt to fetch fresh concurrency data if Upstream is available.
	if p.Upstream != nil {
		p.FetchConcurrency(fresh)
	}

	// Snapshot shared fields under RLock before computing effective
	// concurrency and writing HTTP response.
	// FetchConcurrency (if called above) has already released its write
	// lock, so RLock cannot deadlock.
	// GetEffectiveConcurrency takes its own locks internally, so it must
	// be called AFTER RUnlock to avoid deadlock.
	p.mu.RLock()
	lastConcurrency := p.LastConcurrency
	p.mu.RUnlock()
	activeRequests := p.getActiveRequests()
	queueLen := p.getQueueLen()

	p.configMu.RLock()
	override := p.Config.OverrideConcurrency
	p.configMu.RUnlock()
	eff := p.GetEffectiveConcurrency(lastConcurrency, override)
	resp := map[string]interface{}{
		"concurrent": eff.Concurrent,
		"active":     activeRequests,
		"queued":     queueLen,
		"user_id":    eff.UserID,
		"overridden": eff.Overridden,
	}
	// §28.2: limit and hard_cap are always-present fields.
	// When upstream doesn't provide them, emit null (nil *int
	// serializes to JSON null).
	resp["limit"] = eff.Limit
	resp["hard_cap"] = eff.HardCap
	resp["concurrency_limit_mode"] = p.getConcurrencyLimitMode()
	resp["manual_limit"] = p.getManualLimit()
	WriteJSON(w, http.StatusOK, resp)
}

func (p *Proxy) HandleUsageHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}
	fresh := r.URL.Query().Get("fresh") == "1"
	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	granularity := r.URL.Query().Get("granularity")
	if granularity == "" {
		granularity = "day"
	}
	scope := r.URL.Query().Get("scope")
	data := p.FetchUsageHistory(from, to, granularity, scope, fresh)
	WriteJSON(w, http.StatusOK, data)
}

func (p *Proxy) HandleUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}
	// Snapshot LastConcurrency.UserID under RLock (same pattern as
	// HandleConcurrency) so we read a consistent value.
	p.mu.RLock()
	userID := p.LastConcurrency.UserID
	p.mu.RUnlock()
	WriteJSON(w, http.StatusOK, map[string]interface{}{
		"loggedIn": true,
		"email":    "",
		"user_id":  userID,
	})
}

// triggerRestart closes the HTTP server (graceful, 5s timeout) and then
// exits with code 42, so the external process manager (e.g. systemd) can
// restart the process. It is invoked by HandleRestart via
// time.AfterFunc(500ms) so the HTTP response is flushed to the client
// before the server shuts down (§29.2, §35.2).
func (p *Proxy) triggerRestart() {
	p.Shutdown()
	if p.exitFn != nil {
		p.exitFn(42)
	} else {
		os.Exit(42)
	}
}

// flushErrorLog flushes and closes the error log file (§35.2: "flush logs").
// Safe to call multiple times; no-op if error log is not initialized.
func (p *Proxy) flushErrorLog() {
	p.errorLogMu.Lock()
	defer p.errorLogMu.Unlock()

	if p.errorLogFile != nil {
		_ = p.errorLogFile.Sync()
		_ = p.errorLogFile.Close()
		p.errorLogFile = nil
		p.errorLogInitDone = false
	}
}

// Shutdown performs graceful shutdown (§35.2):
// 1. Mark the proxy as shutting down and reject new queued requests.
// 2. Stop any pending ProcessQueue trigger.
// 3. Reject queued items with 503.
// 4. Poll ActiveRequests until it reaches 0 or a 5-second timeout elapses.
// 5. Close the HTTP server with a 5-second drain timeout.
// 6. Flush and close the error log file.
// 7. Call exitFn(0) (or os.Exit(0) if exitFn is nil).
func (p *Proxy) Shutdown() {
	// 1. Stop accepting new work and signal queue worker to exit.
	p.shuttingDown.Store(true)

	// 2. Wake the queue worker so it observes shutdown and exits.
	p.queueMu.Lock()
	p.queuePending = false
	p.queueMu.Unlock()
	p.queueShutdownOnce.Do(func() { close(p.queueDone) })
	p.queueCond.Broadcast()
	p.workerWG.Wait()

	// 3. Reject any queued items. No new items can arrive because handlers
	//    check shuttingDown first and TryEnqueueOrDispatch rejects when true.
	p.queueMu.Lock()
	p.rejectQueueLocked()
	p.queueMu.Unlock()

	// 4. Drain active requests until zero or deadline.
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
drainLoop:
	for {
		p.queueMu.RLock()
		active := p.ActiveRequests
		p.queueMu.RUnlock()
		if active <= 0 {
			break drainLoop
		}
		select {
		case <-deadline:
			break drainLoop
		case <-ticker.C:
		}
	}

	// 5. Close the HTTP server.
	if p.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.httpServer.Shutdown(ctx)
	}
	p.flushErrorLog()
	if p.exitFn != nil {
		p.exitFn(0)
	} else {
		os.Exit(0)
	}
}

// HandleSignal processes an OS signal for graceful shutdown (§35.2).
func (p *Proxy) HandleSignal(sig os.Signal) {
	log.Printf("Received signal %v — initiating graceful shutdown", sig)
	p.Shutdown()
}

// SetHTTPServer assigns the *http.Server so that Shutdown/triggerRestart
// can gracefully drain connections (§35.2).
func (p *Proxy) SetHTTPServer(srv *http.Server) {
	p.httpServer = srv
}

// endOfTodayUTC returns the expiry time for daily-cached wallpapers:
// end of the current day in UTC (23:59:59).
