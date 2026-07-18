package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// ─── Reasoning / Schema / Payload Normalization (§12-§13) ─────────────────────

// ─── Reasoning Helpers ──────────────────────────────────────────────────────

func ParseLevels(raw interface{}) []string {
	if arr, ok := raw.([]interface{}); ok {
		var result []string
		for _, v := range arr {
			if s, ok := v.(string); ok {
				s = strings.TrimSpace(s)
				if s != "" {
					result = append(result, s)
				}
			}
		}
		return result
	}
	if s, ok := raw.(string); ok {
		return strings.Fields(s)
	}
	return nil
}

func InferReasoningModeFromCapabilities(reasoningCaps interface{}) *bool {
	if reasoningCaps == nil {
		return nil
	}
	caps, ok := reasoningCaps.(map[string]interface{})
	if !ok {
		return nil
	}
	if supported, ok := caps["supported"].(bool); ok && supported {
		t := true
		return &t
	}
	if levels := ParseLevels(caps["levels"]); len(levels) > 0 {
		t := true
		return &t
	}
	return nil
}

func BuildReasoningVariants(reasoningCaps interface{}) map[string]interface{} {
	if reasoningCaps == nil {
		return nil
	}
	caps, ok := reasoningCaps.(map[string]interface{})
	if !ok {
		return nil
	}
	supported, _ := caps["supported"].(bool)
	if !supported {
		return nil
	}
	if canDisable, ok := caps["can_disable"].(bool); ok && !canDisable {
		return nil
	}
	levels := ParseLevels(caps["levels"])
	variants := map[string]interface{}{}
	for _, lvl := range levels {
		if lvl == "none" {
			continue
		}
		budget, ok := ReasoningLevelBudgets[lvl]
		if !ok {
			continue
		}
		variants[lvl] = map[string]interface{}{
			"thinking": map[string]interface{}{
				"type":          "enabled",
				"budget_tokens": budget,
			},
		}
	}
	if len(variants) == 0 {
		return nil
	}
	return variants
}

// ApplyAutoThink implements §13.7: after model resolution, if the model's
// reasoning capabilities indicate supported==true and can_disable==false,
// set payload.thinking to {type: "adaptive"}.
func ApplyAutoThink(payload map[string]interface{}, reasoningCaps interface{}) {
	if reasoningCaps == nil {
		return
	}
	caps, ok := reasoningCaps.(map[string]interface{})
	if !ok {
		return
	}
	supported, _ := caps["supported"].(bool)
	if !supported {
		return
	}
	canDisable, has := caps["can_disable"].(bool)
	if has && !canDisable {
		payload["thinking"] = map[string]interface{}{
			"type": "adaptive",
		}
	}
}

// ─── Model Catalog (§8) ────────────────────────────────────────────────────

func (p *Proxy) ApplyCatalogData(data map[string]interface{}) {
	newModelInfo := map[string]ModelInfo{}
	newDisplayNames := map[string]string{}
	for id, info := range data {
		infoMap, ok := info.(map[string]interface{})
		if !ok {
			continue
		}
		mi := ModelInfo{}
		if dn, ok := infoMap["display_name"].(string); ok {
			mi.DisplayName = dn
			newDisplayNames[id] = stripUmansPrefix(dn)
		}
		if caps, ok := infoMap["capabilities"].(map[string]interface{}); ok {
			mi.Capabilities = Capabilities{
				ContextWindow:        caps["context_window"],
				RecommendedMaxTokens: caps["recommended_max_tokens"],
				MaxCompletionTokens:  caps["max_completion_tokens"],
				SupportsTools:        caps["supports_tools"],
				SupportsVision:       caps["supports_vision"],
				Reasoning:            caps["reasoning"],
			}
		}
		newModelInfo[id] = mi
	}
	p.catalogMu.Lock()
	p.ModelInfoMap = newModelInfo
	p.DisplayNameMap = newDisplayNames
	p.catalogMu.Unlock()
}

func stripUmansPrefix(name string) string {
	return stripUmansPrefixRegex.ReplaceAllString(name, "")
}

func (p *Proxy) GetModelInfo(id string) ModelInfo {
	p.catalogMu.RLock()
	defer p.catalogMu.RUnlock()
	return p.ModelInfoMap[id]
}

func (p *Proxy) GetModelDisplayName(id string) string {
	p.catalogMu.RLock()
	defer p.catalogMu.RUnlock()
	return p.DisplayNameMap[id]
}

func (p *Proxy) GetOrderedModelIds() []string {
	p.catalogMu.RLock()
	defer p.catalogMu.RUnlock()
	out := make([]string, 0, len(p.ModelInfoMap))
	for id := range p.ModelInfoMap {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		di := p.DisplayNameMap[out[i]]
		if di == "" {
			di = out[i]
		}
		dj := p.DisplayNameMap[out[j]]
		if dj == "" {
			dj = out[j]
		}
		di = strings.ToLower(di)
		dj = strings.ToLower(dj)
		if di != dj {
			return di < dj
		}
		return out[i] < out[j]
	})
	return out
}

func (p *Proxy) GetEffectiveModels() []string {
	p.configMu.RLock()
	disabled := p.Config.DisabledModels
	enabled := p.Config.EnabledModels
	p.configMu.RUnlock()

	catalogIds := p.GetOrderedModelIds()
	var all []string
	if len(catalogIds) > 0 {
		all = catalogIds
	} else {
		all = enabled
	}
	if len(disabled) == 0 {
		return all
	}
	disabledSet := map[string]bool{}
	for _, d := range disabled {
		disabledSet[d] = true
	}
	var result []string
	for _, m := range all {
		if !disabledSet[m] {
			result = append(result, m)
		}
	}
	return result
}

// ─── Model Catalog fetch/cache (§8) ─────────────────────────────────────────

// FetchModelCatalog fetches the model catalog from the upstream API.
// SPEC §8.1: GET {baseURL}/models/info, optional Authorization header,
// 15s timeout, returns map[string]interface{}.
func (p *Proxy) FetchModelCatalog() (map[string]interface{}, error) {
	baseURL := p.Config.UpstreamBaseURL
	if baseURL == "" {
		baseURL = UmansAPIBase
	}

	url := strings.TrimRight(baseURL, "/") + "/models/info"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Connection", "keep-alive")

	// Optional Authorization header — only set if API key is configured
	if p.Config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.Config.APIKey)
	}

	resp, err := p.catalogClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch model catalog: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := readBodyText(resp.Body)
		return nil, fmt.Errorf("fetch model catalog: HTTP %d: %s", resp.StatusCode, body)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode model catalog: %w", err)
	}

	return result, nil
}

// GetCatalogData returns cached model catalog data, fetching from upstream if stale.
// SPEC §8.2: 5-min cache, dedup concurrent fetches, stale-cache fallback,
// populates ModelInfoMap and DisplayNameMap.
func (p *Proxy) GetCatalogData() (map[string]interface{}, error) {
	result, err := p.catalogGroup.Do("catalog", func() (interface{}, error) {
		// Fast path: check for valid cached data under read lock
		p.mu.RLock()
		if p.CatalogCache != nil && time.Since(p.CatalogCache.fetchedAt) < ModelCatalogCacheTTL {
			data := p.CatalogCache.data
			p.mu.RUnlock()
			return data, nil
		}
		p.mu.RUnlock()

		// Save stale cache reference for fallback (outside the lock but before fetch)
		p.mu.RLock()
		staleCache := p.CatalogCache
		p.mu.RUnlock()

		data, err := p.FetchModelCatalog()
		p.mu.Lock()
		if err != nil {
			if staleCache != nil {
				stale := *staleCache
				p.CatalogCache = &stale
				p.mu.Unlock()
				return stale.data, nil
			}
			p.CatalogCache = &catalogCache{
				data:      nil,
				fetchedAt: time.Now(),
				fetchErr:  err,
			}
			p.mu.Unlock()
			return nil, err
		}
		p.CatalogCache = &catalogCache{
			data:      data,
			fetchedAt: time.Now(),
			fetchErr:  nil,
		}
		p.mu.Unlock()
		p.ApplyCatalogData(data)
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	return result.(map[string]interface{}), nil
}

// GetAllCatalogModels returns all catalog model IDs (without disabled filter),
// sorted by display name then ID. Falls back to Config.EnabledModels if catalog is empty.
// SPEC §8.6
func (p *Proxy) GetAllCatalogModels() []string {
	catalogIds := p.GetOrderedModelIds()
	if len(catalogIds) > 0 {
		return catalogIds
	}
	// Fall back to Config.EnabledModels when catalog is empty
	if p.Config != nil {
		return p.Config.EnabledModels
	}
	return nil
}

// FetchUpstreamModels fetches the upstream /models endpoint for pricing data.
// SPEC §8.7: GET {baseURL}/models, 5-min cache, 10s timeout,
// returns the data array ([]interface{}).
func (p *Proxy) FetchUpstreamModels() ([]interface{}, error) {
	// Check cache first
	p.mu.RLock()
	if p.UpstreamModelsCache != nil && time.Since(p.UpstreamModelsCache.fetchedAt) < UpstreamModelsCacheTTL {
		data := p.UpstreamModelsCache.data
		p.mu.RUnlock()
		return data, nil
	}
	p.mu.RUnlock()

	// Perform HTTP fetch
	baseURL := p.Config.UpstreamBaseURL
	if baseURL == "" {
		baseURL = UmansAPIBase
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	url := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create upstream models request: %w", err)
	}
	req.Header.Set("Connection", "keep-alive")

	if p.Config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.Config.APIKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch upstream models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := readBodyText(resp.Body)
		return nil, fmt.Errorf("fetch upstream models: HTTP %d: %s", resp.StatusCode, body)
	}

	// Response format: { "data": [ { "id": "...", "pricing": {...} }, ... ] }
	var result struct {
		Data []interface{} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode upstream models: %w", err)
	}

	// Cache the result
	p.mu.Lock()
	p.UpstreamModelsCache = &upstreamModelsCacheEntry{
		data:      result.Data,
		fetchedAt: time.Now(),
		fetchErr:  nil,
	}
	p.mu.Unlock()

	return result.Data, nil
}

// ValidateApiKey validates the API key by calling the upstream user info endpoint.
// SPEC §8.8: calls upstream GetUserInfo(), stores in userInfoCache (5-min TTL),
// calls ApplyCatalogData(data), returns bool.
func (p *Proxy) ValidateApiKey() bool {
	// No API key configured — nothing to validate
	if p.Config.APIKey == "" {
		return false
	}
	// Check cache first — if we validated recently, return cached result
	p.mu.RLock()
	if p.UserInfoCache != nil && time.Since(p.UserInfoCache.fetchedAt) < UserInfoCacheTTL {
		valid := p.UserInfoCache.data != nil
		p.mu.RUnlock()
		return valid
	}
	p.mu.RUnlock()

	// Fetch user info (equivalent to upstream.GetUserInfo())
	// Per §7.2, GetUserInfo() hits GET {baseURL}/models/info
	baseURL := p.Config.UpstreamBaseURL
	if baseURL == "" {
		baseURL = UmansAPIBase
	}

	url := strings.TrimRight(baseURL, "/") + "/models/info"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ValidateApiKey: *** request: %v\n", err)
		return false
	}
	req.Header.Set("Connection", "keep-alive")

	if p.Config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.Config.APIKey)
	}

	resp, err := p.catalogClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ValidateApiKey: *** failed: %v\n", err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := readBodyText(resp.Body)
		fmt.Fprintf(os.Stderr, "ValidateApiKey: *** %d: %s\n", resp.StatusCode, body)
		return false
	}

	// Parse the response — this is the "user info" which is the same as catalog data
	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		fmt.Fprintf(os.Stderr, "ValidateApiKey: decode failed: %v\n", err)
		return false
	}

	// Store in userInfoCache
	p.mu.Lock()
	p.UserInfoCache = &userInfoCacheEntry{
		data:      data,
		fetchedAt: time.Now(),
	}
	p.mu.Unlock()

	// Call ApplyCatalogData to populate model info from the user info response
	p.ApplyCatalogData(data)

	return true
}

// ─── Vision Handoff (§11) ──────────────────────────────────────────────────

func (p *Proxy) NeedsVisionHandoff(resolvedModel string) bool {
	p.configMu.RLock()
	enabled := p.Config.VisionHandoffEnabled
	p.configMu.RUnlock()
	if !enabled {
		return false
	}
	p.catalogMu.RLock()
	info, ok := p.ModelInfoMap[resolvedModel]
	p.catalogMu.RUnlock()
	if !ok {
		return false
	}
	sv, ok := info.Capabilities.SupportsVision.(string)
	if !ok {
		return false
	}
	return sv == "via-handoff"
}

func (p *Proxy) ResolveModelId(requestedModel string) string {
	if requestedModel == "" {
		return ""
	}
	if strings.HasPrefix(requestedModel, "umans-") {
		return requestedModel
	}
	p.configMu.RLock()
	defer p.configMu.RUnlock()
	prefixed := "umans-" + requestedModel
	for _, m := range p.GetEffectiveModels() {
		if m == prefixed {
			return prefixed
		}
	}
	for _, m := range p.GetEffectiveModels() {
		if m == requestedModel {
			return m
		}
	}
	return requestedModel
}

func CollectImageParts(payload map[string]interface{}) []ImagePart {
	var parts []ImagePart
	var walkContentArray func(content []interface{})
	walkContentArray = func(content []interface{}) {
		for i, part := range content {
			p, ok := part.(map[string]interface{})
			if !ok {
				continue
			}
			if pType, _ := p["type"].(string); pType == "image_url" {
				if imgURL, ok := p["image_url"].(map[string]interface{}); ok {
					if url, ok := imgURL["url"].(string); ok && url != "" {
						parts = append(parts, ImagePart{Container: content, Index: i, DataURI: url})
					}
				}
			} else if pType == "image" {
				if source, ok := p["source"].(map[string]interface{}); ok {
					if srcType, _ := source["type"].(string); srcType == "base64" {
						mediaType, _ := source["media_type"].(string)
						data, _ := source["data"].(string)
						if mediaType != "" && data != "" {
							parts = append(parts, ImagePart{Container: content, Index: i, DataURI: fmt.Sprintf("data:%s;base64,%s", mediaType, data)})
						}
					} else if srcType == "url" {
						if url, _ := source["url"].(string); url != "" {
							parts = append(parts, ImagePart{Container: content, Index: i, DataURI: url})
						}
					}
				}
			}
			if nested, ok := p["content"].([]interface{}); ok {
				walkContentArray(nested)
			}
		}
	}
	if sys, ok := payload["system"].([]interface{}); ok {
		walkContentArray(sys)
	}
	if msgs, ok := payload["messages"].([]interface{}); ok {
		for _, m := range msgs {
			msg, ok := m.(map[string]interface{})
			if !ok {
				continue
			}
			if content, ok := msg["content"].([]interface{}); ok {
				walkContentArray(content)
			}
		}
	}
	return parts
}

// cacheHandoffDescription stores a description in the handoff cache if enabled.
func (p *Proxy) cacheHandoffDescription(dataURI, desc string) {
	p.configMu.RLock()
	cacheEnabled := p.Config.VisionHandoffCacheEnabled
	p.configMu.RUnlock()
	if cacheEnabled && p.ImageHandoffCache != nil {
		hash := sha256Hash(dataURI)
		p.ImageHandoffCache.Set(hash, desc)
	}
}

// analyzeImageViaHandoff sends a single image to the vision handoff model and
// returns a text description. It makes a streaming chatCompletions call
// with the image as an image_url content part (SPEC §11.4).
//
// Streaming is used instead of non-streaming to avoid upstream context
// cancellation on long-running vision requests. The upstream provider
// (api.code.umans.ai) cancels non-streaming contexts after ~90-120s, which
// causes 502 "context canceled" errors on large images. Streaming keeps
// the connection alive and avoids this timeout.
func (p *Proxy) analyzeImageViaHandoff(ctx context.Context, dataURI, handoffModel, apiKey string) string {
	// Check handoff cache first (§11.4) — snapshot config under lock to avoid
	// data races with HandleConfigPost.
	p.configMu.RLock()
	cacheEnabled := p.Config.VisionHandoffCacheEnabled
	systemPrompt := p.Config.VisionHandoffPrompt
	p.configMu.RUnlock()

	if cacheEnabled && p.ImageHandoffCache != nil {
		hash := sha256Hash(dataURI)
		if cached, ok := p.ImageHandoffCache.Get(hash); ok {
			return cached
		}
	}

	if systemPrompt == "" {
		systemPrompt = DEFAULT_VISION_HANDOFF_PROMPT
	}

	handoffReq := map[string]interface{}{
		"model":  handoffModel,
		"stream": true,
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "system",
				"content": systemPrompt,
			},
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{
						"type": "text",
						"text": "Describe this image.",
					},
					map[string]interface{}{
						"type": "image_url",
						"image_url": map[string]interface{}{
							"url": dataURI,
						},
					},
				},
			},
		},
	}

	bodyBytes, err := json.Marshal(handoffReq)
	if err != nil {
		return fmt.Sprintf("[Image analysis failed: failed to marshal request: %s]", err.Error())
	}

	resp, err := p.Upstream.ChatCompletions(ctx, bodyBytes, true, apiKey)
	if err != nil {
		return fmt.Sprintf("[Image analysis failed: upstream request error: %s]", err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyText, _ := readBodyText(resp.Body)
		return fmt.Sprintf("[Image analysis failed: upstream returned status %d: %s]", resp.StatusCode, bodyText)
	}

	// Parse SSE stream and accumulate content from delta chunks.
	var contentBuilder strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		choices, ok := chunk["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			continue
		}
		firstChoice, ok := choices[0].(map[string]interface{})
		if !ok {
			continue
		}
		delta, ok := firstChoice["delta"].(map[string]interface{})
		if !ok {
			// Non-streaming-style response (message instead of delta) —
			// fall back to extracting from message.content.
			if msg, ok := firstChoice["message"].(map[string]interface{}); ok {
				extractContent(msg["content"], &contentBuilder)
			}
			continue
		}
		extractContent(delta["content"], &contentBuilder)
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Sprintf("[Image analysis failed: error reading SSE stream: %s]", err.Error())
	}

	desc := contentBuilder.String()
	if desc == "" {
		return "[Image analysis failed: empty content from stream]"
	}

	p.cacheHandoffDescription(dataURI, desc)
	return desc
}

// extractContent appends text from a streaming delta content field to the
// provided builder. Content can be a string or an array of content parts
// (each with type "text" and a "text" field).
func extractContent(content interface{}, builder *strings.Builder) {
	switch c := content.(type) {
	case string:
		builder.WriteString(c)
	case []interface{}:
		for _, part := range c {
			if p, ok := part.(map[string]interface{}); ok {
				if t, _ := p["type"].(string); t == "text" {
					if text, ok := p["text"].(string); ok {
						builder.WriteString(text)
					}
				}
			}
		}
	}
}

// ─── Tool Schema Normalization (§12) ────────────────────────────────────────

func NormalizeToolSchemas(tools []interface{}) {
	if !hasSchemaRefsOrDefs(tools) {
		return
	}
	for _, tool := range tools {
		t, ok := tool.(map[string]interface{})
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]interface{})
		if !ok {
			continue
		}
		params, ok := fn["parameters"].(map[string]interface{})
		if !ok {
			continue
		}
		fn["parameters"] = normalizeSchemaMap(params, extractDefinitions(params), 12)
	}
}

// hasSchemaRefsOrDefs returns true if any tool's parameters contain $ref, $defs,
// or definitions at the top level (per SPEC §12.1 / §19 step 6 guard condition).
func hasSchemaRefsOrDefs(tools []interface{}) bool {
	for _, tool := range tools {
		t, ok := tool.(map[string]interface{})
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]interface{})
		if !ok {
			continue
		}
		params, ok := fn["parameters"].(map[string]interface{})
		if !ok {
			continue
		}
		if _, has := params["$ref"]; has {
			return true
		}
		if _, has := params["$defs"]; has {
			return true
		}
		if _, has := params["definitions"]; has {
			return true
		}
	}
	return false
}

func extractDefinitions(schema map[string]interface{}) map[string]interface{} {
	merged := map[string]interface{}{}
	if defs, ok := schema["definitions"].(map[string]interface{}); ok {
		for k, v := range defs {
			merged[k] = v
		}
	}
	if defs, ok := schema["$defs"].(map[string]interface{}); ok {
		for k, v := range defs {
			merged[k] = v
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func normalizeSchemaMap(node map[string]interface{}, defs map[string]interface{}, maxDepth int) map[string]interface{} {
	if maxDepth <= 0 {
		return deepCloneMap(node)
	}
	// Merge local definitions into a child-scoped copy of defs (SPEC §12.2).
	// Copy-on-write prevents local defs from leaking into sibling scopes.
	localDefs := extractDefinitions(node)
	if localDefs != nil {
		childDefs := make(map[string]interface{}, len(defs)+len(localDefs))
		for k, v := range defs {
			childDefs[k] = v
		}
		for k, v := range localDefs {
			childDefs[k] = v
		}
		defs = childDefs
	}
	if resolved := TryResolveRef(node, defs); resolved != nil {
		return normalizeSchemaMap(resolved, defs, maxDepth-1)
	}
	normalized := map[string]interface{}{}
	for key, value := range node {
		if key == "definitions" || key == "$defs" || key == "nullable" {
			continue
		}
		normalized[key] = normalizeSchemaValue(value, defs, maxDepth-1)
	}
	SimplifyNullableCombinator(normalized, "anyOf")
	SimplifyNullableCombinator(normalized, "oneOf")
	NormalizeTypeField(normalized)
	NormalizeEnumField(normalized)
	if normalized["const"] == nil {
		delete(normalized, "const")
	}
	return normalized
}

func normalizeSchemaValue(value interface{}, defs map[string]interface{}, maxDepth int) interface{} {
	if m, ok := value.(map[string]interface{}); ok {
		return normalizeSchemaMap(m, defs, maxDepth)
	}
	if arr, ok := value.([]interface{}); ok {
		result := make([]interface{}, len(arr))
		for i, v := range arr {
			result[i] = normalizeSchemaValue(v, defs, maxDepth)
		}
		return result
	}
	return value
}

func TryResolveRef(node map[string]interface{}, defs map[string]interface{}) map[string]interface{} {
	if defs == nil {
		return nil
	}
	ref, ok := node["$ref"].(string)
	if !ok {
		return nil
	}
	if len(node) != 1 {
		return nil
	}
	var name string
	if strings.HasPrefix(ref, "#/definitions/") {
		name = ref[len("#/definitions/"):]
	} else if strings.HasPrefix(ref, "#/$defs/") {
		name = ref[len("#/$defs/"):]
	} else {
		return nil
	}
	def, ok := defs[name]
	if !ok {
		return nil
	}
	if m, ok := def.(map[string]interface{}); ok {
		return deepCloneMap(m)
	}
	return nil
}

func SimplifyNullableCombinator(schema map[string]interface{}, key string) {
	rawOptions, ok := schema[key].([]interface{})
	if !ok {
		return
	}
	var filtered []interface{}
	for _, opt := range rawOptions {
		if !isNullSchema(opt) {
			filtered = append(filtered, opt)
		}
	}
	if len(filtered) == 0 {
		delete(schema, key)
		return
	}
	if len(filtered) == 1 {
		if m, ok := filtered[0].(map[string]interface{}); ok {
			delete(schema, key)
			for k, v := range m {
				schema[k] = v
			}
			return
		}
	}
	schema[key] = filtered
}

func isNullSchema(schema interface{}) bool {
	m, ok := schema.(map[string]interface{})
	if !ok {
		return false
	}
	if t, _ := m["type"].(string); t == "null" {
		return true
	}
	if m["const"] == nil {
		if _, has := m["const"]; has {
			return true
		}
	}
	if enum, ok := m["enum"].([]interface{}); ok && len(enum) == 1 && enum[0] == nil {
		return true
	}
	return false
}

func NormalizeTypeField(schema map[string]interface{}) {
	rawType, ok := schema["type"]
	if !ok {
		return
	}
	if _, ok := rawType.(string); ok {
		return
	}
	arr, ok := rawType.([]interface{})
	if !ok {
		return
	}
	var nonNull []string
	for _, t := range arr {
		if s, ok := t.(string); ok && s != "null" && strings.TrimSpace(s) != "" {
			nonNull = append(nonNull, s)
		}
	}
	if len(nonNull) == 0 {
		delete(schema, "type")
	} else {
		schema["type"] = nonNull[0]
	}
}

func NormalizeEnumField(schema map[string]interface{}) {
	enum, ok := schema["enum"].([]interface{})
	if !ok {
		return
	}
	seen := map[string]bool{}
	var filtered []interface{}
	for _, entry := range enum {
		if entry == nil {
			continue
		}
		key := enumDedupKey(entry)
		if seen[key] {
			continue
		}
		seen[key] = true
		filtered = append(filtered, entry)
	}
	if len(filtered) == 0 {
		delete(schema, "enum")
	} else {
		schema["enum"] = filtered
	}
}

// enumDedupKey produces a typeof:JSON key for enum deduplication (SPEC §12.6).
// Falls back to fmt.Sprintf if json.Marshal fails (defensive — values from
// JSON unmarshal are always serializable, but channels/funcs could appear in
// hand-constructed maps).
func enumDedupKey(entry interface{}) string {
	b, err := json.Marshal(entry)
	if err != nil {
		return fmt.Sprintf("%T:%v", entry, entry)
	}
	return fmt.Sprintf("%T:%s", entry, b)
}

func deepCloneMap(m map[string]interface{}) map[string]interface{} {
	b, _ := json.Marshal(m)
	var result map[string]interface{}
	json.Unmarshal(b, &result)
	return result
}

// ─── Payload Normalization (§13) ───────────────────────────────────────────

func StripReasoningContent(payload map[string]interface{}) {
	msgs, ok := payload["messages"].([]interface{})
	if !ok {
		return
	}
	for _, m := range msgs {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "assistant" {
			delete(msg, "reasoning_content")
			delete(msg, "reasoningContent")
		}
	}
}

func NormalizeThinkingPayload(payload map[string]interface{}) {
	thinking, ok := payload["thinking"].(map[string]interface{})
	if !ok {
		return
	}
	if bt, has := thinking["budgetTokens"]; has {
		if _, hasSnake := thinking["budget_tokens"]; !hasSnake {
			thinking["budget_tokens"] = bt
			delete(thinking, "budgetTokens")
		}
	}
}

func LimitImagesInMessages(payload map[string]interface{}, maxImages int) {
	if maxImages <= 0 {
		return
	}
	type imgPart struct {
		content []interface{}
		index   int
		time    int
	}
	var imageParts []imgPart
	var walkContentArray func(content []interface{}, time int)
	walkContentArray = func(content []interface{}, time int) {
		for i, part := range content {
			p, ok := part.(map[string]interface{})
			if !ok {
				continue
			}
			if pType, _ := p["type"].(string); pType == "image_url" || pType == "image" {
				imageParts = append(imageParts, imgPart{content: content, index: i, time: time})
			}
			if nested, ok := p["content"].([]interface{}); ok {
				walkContentArray(nested, time)
			}
		}
	}
	if sys, ok := payload["system"].([]interface{}); ok {
		walkContentArray(sys, -1)
	}
	if msgs, ok := payload["messages"].([]interface{}); ok {
		for mi, m := range msgs {
			msg, ok := m.(map[string]interface{})
			if !ok {
				continue
			}
			if content, ok := msg["content"].([]interface{}); ok {
				walkContentArray(content, mi)
			}
		}
	}
	if len(imageParts) <= maxImages {
		return
	}
	sort.Slice(imageParts, func(i, j int) bool {
		return imageParts[i].time < imageParts[j].time
	})
	for i := 0; i < len(imageParts)-maxImages; i++ {
		ip := imageParts[i]
		ip.content[ip.index] = map[string]interface{}{
			"type": "text",
			"text": "(Image previously shared)",
		}
	}
}

func FingerprintPayload(payload map[string]interface{}) string {
	msgs, ok := payload["messages"].([]interface{})
	if !ok {
		return ""
	}
	var userText string
	found := false
	for _, m := range msgs {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "user" {
			userText = MsgText(msg)
			found = true
			break
		}
	}
	if !found {
		return ""
	}
	hash := md5Hash(userText)
	if len(hash) < 12 {
		return hash
	}
	return hash[:12]
}

func MsgText(m map[string]interface{}) string {
	content, ok := m["content"]
	if !ok {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	if arr, ok := content.([]interface{}); ok {
		for _, part := range arr {
			p, ok := part.(map[string]interface{})
			if !ok {
				continue
			}
			if t, _ := p["type"].(string); t == "text" {
				if text, ok := p["text"].(string); ok {
					return text
				}
			}
		}
	}
	return ""
}

func ExtractUserPrompt(payload map[string]interface{}) string {
	msgs, ok := payload["messages"].([]interface{})
	if !ok {
		return ""
	}
	var userText string
	for _, m := range msgs {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "user" {
			userText = MsgText(msg)
		}
	}
	return extractUserPromptPrefixRegex.ReplaceAllString(userText, "")
}
