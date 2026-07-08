// Package proxy provides a local HTTP reverse proxy for the UMANS AI API.
//
// This file contains the proxy function implementations.
package proxy

import (
	"bytes"
	"container/list"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
)

// ─── Constants ─────────────────────────────────────────────────────────────

const (
	MaxQueueSize           = 256
	QueueFullErrorCode     = "queue_full"
	MaxBodySize            = 5 * 1024 * 1024
	ConvMapMax             = 10000
	ConvMapEvictTarget     = 8000  // 80% of ConvMapMax — eviction target when map is at capacity
	UmansAPIBase           = "https://api.code.umans.ai/v1"
	APIKeyEnvVar           = "UMANS_API_KEY"
	ModelCatalogCacheTTL   = 5 * time.Minute
	UsageCacheTTL          = 5 * time.Minute
	UpstreamModelsCacheTTL = 5 * time.Minute // §8.7
	UserInfoCacheTTL       = 5 * time.Minute // §8.8
	UserInfoCacheTimeout   = 10 * time.Second // §7.2 GetUserInfo timeout

	// DefaultRetryAttempts is used when Config.RetryAttempts is nil (unset).
	// Was hardcoded MaxRetries=10 before; now default 2 per user requirement.
	DefaultRetryAttempts = 2
	// MaxRetryAttemptsCap is the upper bound for the retry slider.
	MaxRetryAttemptsCap = 10

	// DEFAULT_VISION_HANDOFF_PROMPT is the system prompt for vision handoff
	// image analysis when config.visionHandoffPrompt is empty (SPEC §11.7).
	DEFAULT_VISION_HANDOFF_PROMPT = `You are an image captioning module. Your output is fed verbatim into another model as the sole visual content of the image — it cannot see the image itself, only your text.

Produce a factual, third-person description of the image contents. Do NOT use first person ("I see..."). Do NOT address the reader. Do NOT speculate about what the user wants.

Cover:
- Type of image (screenshot, photograph, diagram, UI, log, etc.) and overall layout
- All visible elements (objects, UI widgets, people, regions) and their spatial arrangement
- Exact transcription of any visible text, code, or labels (use quotes)
- Salient technical details (file paths, error messages, colors, dimensions, filenames)

Write as a single coherent description, not a bulleted list. Be thorough but concise.`
)

// BackoffPresets maps strategy name → per-attempt delays (in seconds).
// Index 0 = delay before attempt 2, index 1 = delay before attempt 3, etc.
// If the preset has fewer entries than the retry count, the last value repeats.
var BackoffPresets = map[string][]int{
	"aggressive":   {1, 3, 5, 10, 15, 20, 25, 30, 45, 60},
	"conservative": {5, 15, 30, 45, 60, 120, 180, 240, 300},
}

var ReasoningLevelBudgets = map[string]int{
	"low":    8000,
	"medium": 16000,
	"high":   16000,
	"max":    32000,
}

// stripUmansPrefixRegex strips leading "Umans " (case-insensitive) from display names.
// Compiled once at package init, not on every call.
var stripUmansPrefixRegex = regexp.MustCompile(`(?i)^Umans\s+`)

// extractUserPromptPrefixRegex strips leading [/...] prefixes from user prompts.
// Compiled once at package init, not on every call.
var extractUserPromptPrefixRegex = regexp.MustCompile(`^\[[^\]]+\]\s*`)

// NewProxy creates and initializes a new Proxy instance.
// It initializes the error log file (§16.1) as part of startup.
func NewProxy(cfg *Config) *Proxy {
	p := &Proxy{
		Config:          cfg,
		ModelInfoMap:    map[string]ModelInfo{},
		DisplayNameMap:  map[string]string{},
		UpstreamPricing: map[string]Pricing{},
	}
	p.StartedAt = time.Now()
	if err := p.InitErrorLogFile(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to initialize error log file: %v\n", err)
	}
	// §31 step 2: Initialize ImageHandoffCache (50 entries, 24h TTL or configured).
	handoffTtl := 24 * time.Hour
	if p.Config != nil && p.Config.VisionHandoffCacheTtl.Duration > 0 {
		handoffTtl = p.Config.VisionHandoffCacheTtl.Duration
	}
	p.ImageHandoffCache = NewImageHandoffCache(50, handoffTtl)

	// Restore concurrency limit mode from config (with migration from old BurstMode).
	mode := cfg.ConcurrencyLimitMode
	if mode == "" {
		if cfg.BurstMode {
			mode = "hard"
		} else {
			mode = "soft"
		}
	}
	p.setConcurrencyLimitMode(mode)
	if cfg.ManualConcurrencyLimit > 0 {
		p.setManualLimit(cfg.ManualConcurrencyLimit)
	}
	// Initialize retry config snapshot from loaded config.
	retryAttempts := DefaultRetryAttempts
	if cfg.RetryAttempts != nil {
		retryAttempts = *cfg.RetryAttempts
	}
	p.setRetryConfig(retryAttempts, cfg.BackoffStrategy)

	// §31 step 3: Initialize KeyPool.
	if len(cfg.Keys) > 0 {
		p.KeyPool = NewKeyPool(cfg.Keys)
	} else if cfg.APIKey != "" {
		p.KeyPool = NewKeyPool([]KeyConfig{{Name: "Default", Key: cfg.APIKey}})
	}
	if p.KeyPool != nil {
		p.KeyPool.SetConfig(cfg)
	}

	// §31 step 4: Initialize Upstream client.
	upstreamBaseURL := cfg.UpstreamBaseURL
	if upstreamBaseURL == "" {
		upstreamBaseURL = UmansAPIBase
	}
	p.Upstream = NewUpstreamClient(upstreamBaseURL, cfg.APIKey, cfg.RequestTimeout.Duration)

	// §31 step 5: Set exitFn for restart shutdown.
	p.exitFn = os.Exit

	// §7.6: shared catalog client
	p.catalogClient = &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: false,
			MaxIdleConns:        32,
			MaxIdleConnsPerHost: 32,
			IdleConnTimeout:     60 * time.Second,
		},
	}

	// §31 step 6a: Validate API key (non-fatal).
	if p.Upstream != nil && cfg.APIKey != "" {
		if !p.ValidateApiKey() {
			fmt.Fprintf(os.Stderr, "warning: API key validation failed; proxy will start with empty model catalog\n")
		}
	}

	// §31 step 6: Fetch concurrency data at startup.
	// This populates LastConcurrency with concurrent_sessions, limit,
	// hard_cap, and user_id from the upstream /usage endpoint.
	// Non-fatal: if Upstream is nil or the fetch fails, the proxy
	// starts with zero-valued LastConcurrency and will retry on
	// the first HandleUsage/HandleConcurrency request.
	if p.Upstream != nil {
		p.FetchConcurrency(false)
	}

	return p
}

// ─── Config (§2) ────────────────────────────────────────────────────────────

var durationRegex = regexp.MustCompile(`^(\d+)(h|m|s)$`)

// ParseDuration parses strings like "15m", "6h", "30s" into a Duration.
// Bare numbers (pure digits with no unit suffix) are interpreted as milliseconds.
// Unparseable input returns the zero-value Duration{}.
func ParseDuration(str string) Duration {
	if str == "" {
		return Duration{}
	}
	matches := durationRegex.FindStringSubmatch(str)
	if matches == nil {
		// Bare number → milliseconds
		if n, err := strconv.Atoi(str); err == nil {
			return Duration{time.Duration(n) * time.Millisecond}
		}
		return Duration{}
	}
	val, err := strconv.Atoi(matches[1])
	if err != nil {
		return Duration{}
	}
	switch matches[2] {
	case "h":
		return Duration{time.Duration(val) * time.Hour}
	case "m":
		return Duration{time.Duration(val) * time.Minute}
	case "s":
		return Duration{time.Duration(val) * time.Second}
	}
	return Duration{}
}

// MaskToken masks an API key for display: first 10 + "..." + last 4.
func MaskToken(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 14 {
		if len(key) <= 4 {
			return key[:1] + "..."
		}
		return key[:1] + "..." + key[len(key)-2:]
	}
	return key[:10] + "..." + key[len(key)-4:]
}

// ParseListenPort extracts the port from a "host:port" string.
func ParseListenPort(addr string) int {
	if addr == "" {
		return 8084
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 8084
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port == 0 {
		return 8084
	}
	return port
}

func DefaultConfig() Config {
	defRetry := DefaultRetryAttempts
	return Config{
		ListenAddr:             "127.0.0.1:8084",
		UpstreamBaseURL:        "https://api.code.umans.ai/v1",
		RequestTimeout:         ParseDuration("15m"),
		OverrideConcurrency:    0,
		MaxImages:              9,
		VisionHandoffEnabled:   false,
		VisionHandoffModel:     "umans-coder",
		VisionHandoffPrompt:    "",
		VisionHandoffCacheEnabled: false,
		VisionHandoffCacheTtl:  ParseDuration("24h"),
		VisionHandoffConcurrency: 4,
		WallpaperSource:        "bing",
		ConcurrencyLimitMode:  "soft",
		RetryAttempts:         &defRetry,
		BackoffStrategy:       "aggressive",
	}
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("UPSTREAM_BASE_URL"); v != "" {
		cfg.UpstreamBaseURL = v
	}
	if v := os.Getenv("REQUEST_TIMEOUT"); v != "" {
		cfg.RequestTimeout = ParseDuration(v)
	}
	if v := os.Getenv("UMANS_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("API_KEYS"); v != "" {
		cfg.APIKeys = strings.Split(v, ",")
		for i := range cfg.APIKeys {
			cfg.APIKeys[i] = strings.TrimSpace(cfg.APIKeys[i])
		}
	}
	if v := os.Getenv("OVERRIDE_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.OverrideConcurrency = n
		}
	}
	if v := os.Getenv("MAX_IMAGES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxImages = n
		}
	}
	if v := os.Getenv("VISION_HANDOFF_ENABLED"); v != "" {
		cfg.VisionHandoffEnabled = v != "false"
	}
	if v := os.Getenv("VISION_HANDOFF_CACHE_ENABLED"); v != "" {
		cfg.VisionHandoffCacheEnabled = strings.ToLower(v) != "false"
	}
	if v := os.Getenv("VISION_HANDOFF_CACHE_TTL"); v != "" {
		cfg.VisionHandoffCacheTtl = ParseDuration(v)
	}
	if v := os.Getenv("RETRY_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.RetryAttempts = &n
		}
	}
	if v := os.Getenv("BACKOFF_STRATEGY"); v != "" {
		if _, ok := BackoffPresets[v]; ok {
			cfg.BackoffStrategy = v
		}
	}
}

func LoadConfig() (Config, error) {
	cfg := DefaultConfig()
	path := filepath.Join(".config", "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		applyEnvOverrides(&cfg)
		applyDefaultApiKeyMode(&cfg)
		return cfg, nil
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		cfg = DefaultConfig()
		applyEnvOverrides(&cfg)
		applyDefaultApiKeyMode(&cfg)
		return cfg, err
	}
	applyEnvOverrides(&cfg)
	if cfg.RequestTimeout.Duration <= 0 {
		log.Fatal("RequestTimeout must be positive")
	}
	// Migrate: old config.json files won't have RETRY_ATTEMPTS / BACKOFF_STRATEGY.
	if cfg.RetryAttempts == nil {
		def := DefaultRetryAttempts
		cfg.RetryAttempts = &def
	} else if *cfg.RetryAttempts < 0 || *cfg.RetryAttempts > MaxRetryAttemptsCap {
		def := DefaultRetryAttempts
		cfg.RetryAttempts = &def
	}
	if cfg.BackoffStrategy == "" {
		cfg.BackoffStrategy = "aggressive"
	}
	if _, ok := BackoffPresets[cfg.BackoffStrategy]; !ok {
		cfg.BackoffStrategy = "aggressive"
	}
	applyDefaultApiKeyMode(&cfg)
	return cfg, nil
}

// applyDefaultApiKeyMode sets the default API key mode when it's not explicitly
// configured. Defaults to "smart".
func applyDefaultApiKeyMode(cfg *Config) {
	if cfg.ApiKeyMode != "" {
		return
	}
	cfg.ApiKeyMode = "smart"
}

func saveConfig(cfg Config) error {
	dir := ".config"
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// Atomic write: temp file in same dir, then rename (§PORT-COMPAT).
	tmpPath := filepath.Join(dir, "config.json.tmp")
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	finalPath := filepath.Join(dir, "config.json")
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath) // best-effort cleanup on rename failure
		return err
	}
	return nil
}

var (
	saveTimer   *time.Timer
	saveTimerMu sync.Mutex
)

func debouncedSaveConfig(cfg Config) {
	saveTimerMu.Lock()
	defer saveTimerMu.Unlock()
	if saveTimer != nil {
		saveTimer.Stop()
	}
	saveTimer = time.AfterFunc(500*time.Millisecond, func() {
		_ = saveConfig(cfg)
	})
}

// ─── Upstream Client (§7) ───────────────────────────────────────────────────

// NewUpstreamClient creates a new UpstreamClient with a keep-alive HTTP client.
// The baseURL should be the API base (e.g., "https://api.code.umans.ai/v1").
// The timeout is the default/client-level timeout; per-request timeouts are
// enforced via context.WithTimeout in each method.
func NewUpstreamClient(baseURL, apiKey string, timeout time.Duration) *UpstreamClient {
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	transport := &http.Transport{
		DisableKeepAlives:   false,
		MaxIdleConns:        128,
		MaxIdleConnsPerHost: 128,
		IdleConnTimeout:     60 * time.Second,
		ResponseHeaderTimeout: timeout,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
	return &UpstreamClient{
		baseURL:    baseURL,
		timeout:    timeout,
		apiKey:     apiKey,
		httpClient: client,
	}
}

// SetAPIKey updates the API key used for requests where no explicit key is provided.
// NOTE: This is NOT safe for concurrent use with per-request key rotation.
// Prefer passing the apiKey directly to ChatCompletions/Messages.
func (u *UpstreamClient) SetAPIKey(key string) {
	u.apiKey = key
}

// SetTimeout updates the request timeout for subsequent requests.
// Also updates the transport's ResponseHeaderTimeout to match.
func (u *UpstreamClient) SetTimeout(timeout time.Duration) {
	u.timeout = timeout
	u.httpClient.Timeout = timeout
	if tr, ok := u.httpClient.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = timeout
	}
}

// GetUserInfo fetches model/user info from upstream.
// GET {baseURL}/models/info with Authorization: Bearer and Connection: keep-alive headers.
// 10-second timeout enforced via context.
// Returns parsed JSON as json.RawMessage.
func (u *UpstreamClient) GetUserInfo(apiKey string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.baseURL+"/models/info", nil)
	if err != nil {
		return nil, fmt.Errorf("GetUserInfo: creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Connection", "keep-alive")

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetUserInfo: executing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GetUserInfo: upstream returned status %d", resp.StatusCode)
	}

	body, err := readBodyText(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("GetUserInfo: reading response body: %w", err)
	}

	var raw json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return nil, fmt.Errorf("GetUserInfo: parsing JSON: %w", err)
	}
	return raw, nil
}

// GetUsage fetches usage data from upstream.
// GET {baseURL}/usage with Authorization: Bearer *** Connection: keep-alive headers.
// 10-second timeout enforced via context.
// Returns the raw response body as json.RawMessage for the caller to unmarshal.
// On non-2xx status, returns an error (caller handles fallback to cached data).
func (u *UpstreamClient) GetUsage(apiKey string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.baseURL+"/usage", nil)
	if err != nil {
		return nil, fmt.Errorf("GetUsage: creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Connection", "keep-alive")

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetUsage: executing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GetUsage: upstream returned status %d", resp.StatusCode)
	}

	body, err := readBodyText(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("GetUsage: reading response body: %w", err)
	}

	var raw json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return nil, fmt.Errorf("GetUsage: parsing JSON: %w", err)
	}
	return raw, nil
}

// ChatCompletions sends a chat completion request to upstream.
// POST {baseURL}/chat/completions
// Headers: Authorization: Bearer {apiKey}, Content-Type: application/json,
// Accept: text/event-stream (if stream) or application/json, Connection: keep-alive.
// Timeout: u.timeout (set from config.requestTimeout at construction).
// Returns raw *http.Response. Caller must defer resp.Body.Close().
func (u *UpstreamClient) ChatCompletions(ctx context.Context, body []byte, isStream bool, apiKey string) (*http.Response, error) {
	return u.doPost(ctx, "/chat/completions", body, isStream, u.timeout, apiKey)
}

// Messages sends an Anthropic Messages API request to upstream.
// POST {baseURL}/messages
// Same headers as ChatCompletions.
// Timeout: u.timeout (set from config.requestTimeout at construction).
// Returns raw *http.Response. Caller must defer resp.Body.Close().
func (u *UpstreamClient) Messages(ctx context.Context, body []byte, isStream bool, apiKey string) (*http.Response, error) {
	return u.doPost(ctx, "/messages", body, isStream, u.timeout, apiKey)
}

// doPost is the shared POST implementation for ChatCompletions and Messages.
// It constructs and sends a POST request with the appropriate headers and
// per-request timeout. The caller is responsible for closing resp.Body.
func (u *UpstreamClient) doPost(ctx context.Context, path string, body []byte, isStream bool, timeout time.Duration, apiKey string) (*http.Response, error) {
	var cancel context.CancelFunc
	if !isStream && timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: creating request: %w", path, err)
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	if isStream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("Connection", "keep-alive")

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: executing request: %w", path, err)
	}

	// Wrap body to call cancel on Close, ensuring context resources are released.
	// The cancel function may be nil for streaming requests (no WithTimeout created).
	if cancel != nil {
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	}
	return resp, nil
}

// cancelOnClose wraps an io.ReadCloser and calls the provided cancel function
// when Close() is called. This ensures context resources are released when the
// caller finishes reading the response body.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	c.cancel()
	return c.ReadCloser.Close()
}

// ─── KeyPool (§3) ───────────────────────────────────────────────────────────

// NewKeyPool creates a new key pool from the given keys.
func NewKeyPool(keys []KeyConfig) *KeyPool {
	entries := make([]*keyEntry, len(keys))
	for i, k := range keys {
		entries[i] = &keyEntry{Key: k.Key, Name: k.Name, Healthy: true, CooldownMs: 30000}
	}
	return &KeyPool{entries: entries}
}

// SetConfig associates a Config with the pool so that Acquire can set the
// active API key per spec §3.2. Optional — if not called, Acquire skips
// the config update (useful for standalone testing).
func (p *KeyPool) SetConfig(cfg *Config) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.config = cfg
}

// Acquire returns a healthy key, round-robin. preferredIndex >= 0 tries that key first.
func (p *KeyPool) Acquire(preferredIndex int) (*KeySlot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) == 0 {
		return nil, false
	}
	now := time.Now()
	if preferredIndex >= 0 && preferredIndex < len(p.entries) {
		e := p.entries[preferredIndex]
		if e.Healthy || now.Sub(e.LastError) >= time.Duration(e.CooldownMs)*time.Millisecond {
			e.Healthy = true
			if p.config != nil {
				p.config.APIKey = e.Key
			}
			return &KeySlot{Key: e.Key, Name: e.Name, Index: preferredIndex}, true
		}
	}
	for i := 0; i < len(p.entries); i++ {
		idx := p.index % len(p.entries)
		p.index++
		e := p.entries[idx]
		if e.Healthy || now.Sub(e.LastError) >= time.Duration(e.CooldownMs)*time.Millisecond {
			e.Healthy = true
			if p.config != nil {
				p.config.APIKey = e.Key
			}
			return &KeySlot{Key: e.Key, Name: e.Name, Index: idx}, true
		}
	}
	return nil, false
}

// MarkUnhealthy marks a key as unhealthy with a cooldown based on status code.
func (p *KeyPool) MarkUnhealthy(index, status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if index < 0 || index >= len(p.entries) {
		return
	}
	e := p.entries[index]
	e.Healthy = false
	e.LastError = time.Now()
	if status >= 503 {
		e.CooldownMs = 60000
	} else if status >= 502 {
		e.CooldownMs = 30000
	} else {
		e.CooldownMs = 10000
	}
}

// MarkHealthy resets a key to healthy status.
func (p *KeyPool) MarkHealthy(index int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if index < 0 || index >= len(p.entries) {
		return
	}
	p.entries[index].Healthy = true
	p.entries[index].LastError = time.Time{}
}

// HealthyCount returns the number of healthy or cooldown-expired keys.
func (p *KeyPool) HealthyCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	count := 0
	for _, e := range p.entries {
		if e.Healthy || now.Sub(e.LastError) >= time.Duration(e.CooldownMs)*time.Millisecond {
			count++
		}
	}
	return count
}

// Total returns the total number of keys in the pool.
func (p *KeyPool) Total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// State returns the public state of all keys.
func (p *KeyPool) State() []KeyState {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	states := make([]KeyState, len(p.entries))
	for i, e := range p.entries {
		cool := int64(0)
		status := "none"
		if e.Key != "" {
			if !e.Healthy {
				cool = e.CooldownMs - now.Sub(e.LastError).Milliseconds()
				if cool <= 0 {
					cool = 0
					status = "active"
				} else {
					status = "cooldown"
				}
			} else {
				status = "active"
			}
		}
		states[i] = KeyState{
			Name:              e.Name,
			Status:            status,
			Healthy:           status == "active",
			RemainingCooldown: cool,
			Token:             MaskToken(e.Key),
		}
	}
	return states
}

// ─── ImageHandoffCache (§11) ────────────────────────────────────────────────

// NewImageHandoffCache creates a new LRU cache for vision handoff descriptions.
func NewImageHandoffCache(maxSize int, ttl time.Duration) *ImageHandoffCache {
	return &ImageHandoffCache{
		maxSize: maxSize,
		ttl:     ttl,
		lru:     list.New(),
		lookup:  make(map[string]*list.Element),
	}
}

// Get returns a cached description if it exists and hasn't expired.
func (c *ImageHandoffCache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.lookup[key]
	if !ok {
		c.misses++
		return "", false
	}
	entry := elem.Value.(*handoffCacheEntry)
	if time.Since(entry.time) > c.ttl {
		c.lru.Remove(elem)
		delete(c.lookup, key)
		c.misses++
		return "", false
	}
	// LRU: move to front
	c.lru.MoveToFront(elem)
	c.hits++
	return entry.desc, true
}

// Set stores a description in the cache, evicting the oldest entry if at capacity.
func (c *ImageHandoffCache) Set(key, desc string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.lookup[key]; ok {
		entry := elem.Value.(*handoffCacheEntry)
		entry.desc = desc
		entry.time = time.Now()
		c.lru.MoveToFront(elem)
		return
	}
	entry := &handoffCacheEntry{hash: key, desc: desc, time: time.Now()}
	elem := c.lru.PushFront(entry)
	c.lookup[key] = elem
	if c.lru.Len() > c.maxSize {
		back := c.lru.Back()
		if back != nil {
			c.lru.Remove(back)
			delete(c.lookup, back.Value.(*handoffCacheEntry).hash)
			c.evictions++
		}
	}
}

// Stats returns current cache statistics.
func (c *ImageHandoffCache) Stats() HandoffCacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return HandoffCacheStats{
		Size:      c.lru.Len(),
		MaxSize:   c.maxSize,
		TtlMs:     c.ttl.Milliseconds(),
		Hits:      c.hits,
		Misses:    c.misses,
		Evictions: c.evictions,
	}
}

// Resize updates the cache's max size and TTL.
func (c *ImageHandoffCache) Resize(maxSize int, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxSize = maxSize
	c.ttl = ttl
}

// sha256Hash returns the hex-encoded SHA-256 hash of the input.
func sha256Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// md5Hash returns the hex-encoded MD5 hash of the input.
// Used for conversation fingerprinting (§14) and usage history cache keys.
func md5Hash(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

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
	defer p.configMu.RUnlock()

	if !p.Config.VisionHandoffEnabled {
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
	if p.Config.VisionHandoffCacheEnabled && p.ImageHandoffCache != nil {
		hash := sha256Hash(dataURI)
		p.ImageHandoffCache.Set(hash, desc)
	}
}

// analyzeImageViaHandoff sends a single image to the vision handoff model and
// returns a text description. It makes a non-streaming chatCompletions call
// with the image as an image_url content part (SPEC §11.4).
func (p *Proxy) analyzeImageViaHandoff(ctx context.Context, dataURI, handoffModel, apiKey string) string {
	// Check handoff cache first (§11.4)
	if p.Config.VisionHandoffCacheEnabled && p.ImageHandoffCache != nil {
		hash := sha256Hash(dataURI)
		if cached, ok := p.ImageHandoffCache.Get(hash); ok {
			return cached
		}
	}

	systemPrompt := p.Config.VisionHandoffPrompt
	if systemPrompt == "" {
		systemPrompt = DEFAULT_VISION_HANDOFF_PROMPT
	}

	handoffReq := map[string]interface{}{
		"model": handoffModel,
		"stream": false,
		"messages": []interface{}{
			map[string]interface{}{
				"role": "system",
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

	resp, err := p.Upstream.ChatCompletions(ctx, bodyBytes, false, apiKey)
	if err != nil {
		return fmt.Sprintf("[Image analysis failed: upstream request error: %s]", err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyText, _ := readBodyText(resp.Body)
		return fmt.Sprintf("[Image analysis failed: upstream returned status %d: %s]", resp.StatusCode, bodyText)
	}

	var respData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		return fmt.Sprintf("[Image analysis failed: failed to decode response: %s]", err.Error())
	}

	choices, ok := respData["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return "[Image analysis failed: no choices in response]"
	}
	firstChoice, ok := choices[0].(map[string]interface{})
	if !ok {
		return "[Image analysis failed: invalid choice format]"
	}
	message, ok := firstChoice["message"].(map[string]interface{})
	if !ok {
		return "[Image analysis failed: no message in choice]"
	}
	content := message["content"]

	switch c := content.(type) {
	case string:
		if c == "" {
			return "[Image analysis failed: empty content]"
		}
		p.cacheHandoffDescription(dataURI, c)
		return c
	case []interface{}:
		var sb strings.Builder
		for _, part := range c {
			if p, ok := part.(map[string]interface{}); ok {
				if t, _ := p["type"].(string); t == "text" {
					if text, ok := p["text"].(string); ok {
						sb.WriteString(text)
					}
				}
			}
		}
		if sb.Len() == 0 {
			return "[Image analysis failed: no text content in response array]"
		}
		desc := sb.String()
		p.cacheHandoffDescription(dataURI, desc)
		return desc
	default:
		return "[Image analysis failed: unexpected content type]"
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

// ─── Conversation Tracking (§14) ─────────────────────────────────────────────

// ensureConversationMap lazily initializes conversation tracking state.
// Must be called under convMu.
func (p *Proxy) ensureConversationMap() {
	if p.conversationMap == nil {
		p.conversationMap = make(map[string]*ConversationSession)
		p.convLRU = list.New()
		p.convLRUIndex = make(map[string]*list.Element)
	}
}

// TouchConversation returns the ConversationSession for the given fingerprint,
// moving it to the most-recently-used position in the LRU. Returns nil if the
// fingerprint is empty or not found in the map. (Spec §14.3)
func (p *Proxy) TouchConversation(fingerprint string) *ConversationSession {
	if fingerprint == "" {
		return nil
	}
	p.convMu.Lock()
	defer p.convMu.Unlock()
	p.ensureConversationMap()
	elem, ok := p.convLRUIndex[fingerprint]
	if !ok {
		return nil
	}
	p.convLRU.MoveToFront(elem)
	return p.conversationMap[fingerprint]
}

// TrackConversationSession stores or updates a conversation session in the map.
// If alreadyTouched is true, the fingerprint is assumed to already be at the
// most-recently-used position, so only the session value is set. Otherwise the
// entry is moved to most-recently-used. If the map is at capacity (ConvMapMax)
// and the fingerprint is new, entries are evicted from the LRU tail until the
// size reaches ConvMapEvictTarget. (Spec §14.4)
func (p *Proxy) TrackConversationSession(fingerprint string, session *ConversationSession, alreadyTouched bool) {
	if fingerprint == "" {
		return
	}
	p.convMu.Lock()
	defer p.convMu.Unlock()
	p.ensureConversationMap()

	_, exists := p.conversationMap[fingerprint]

	// Eviction: only if at capacity AND fingerprint is new (Spec §14.4 lines 685-686)
	if !exists && len(p.conversationMap) >= ConvMapMax {
		for len(p.conversationMap) > ConvMapEvictTarget {
			back := p.convLRU.Back()
			if back == nil {
				break
			}
			oldFp := back.Value.(string)
			p.convLRU.Remove(back)
			delete(p.conversationMap, oldFp)
			delete(p.convLRUIndex, oldFp)
		}
	}

	// Set the session value
	p.conversationMap[fingerprint] = session

	// Update LRU position (Spec §14.4 lines 687-688)
	// Line 687: If alreadyTouched → just set the fingerprint (LRU already
	// updated by a prior TouchConversation call, so skip the move).
	// Line 688: Otherwise → move to most-recently-used.
	if !alreadyTouched {
		if elem, ok := p.convLRUIndex[fingerprint]; ok {
			p.convLRU.MoveToFront(elem)
		} else {
			elem := p.convLRU.PushFront(fingerprint)
			p.convLRUIndex[fingerprint] = elem
		}
	}
}

// ─── Error Logging (§16) ────────────────────────────────────────────────────

func RedactHeaders(headers map[string][]string) map[string][]string {
	result := map[string][]string{}
	for k, v := range headers {
		lower := strings.ToLower(k)
		sensitive := lower == "authorization" || lower == "x-api-key" || lower == "cookie" || lower == "set-cookie" || lower == "api-key"
		if !sensitive {
			if strings.Contains(lower, "auth") || strings.Contains(lower, "token") || strings.Contains(lower, "key") || strings.Contains(lower, "password") || strings.Contains(lower, "secret") {
				sensitive = true
			}
		}
		if sensitive {
			result[k] = []string{"[REDACTED]"}
		} else {
			result[k] = v
		}
	}
	return result
}

func RedactBodyJson(body string) string {
	if body == "" {
		return ""
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return body
	}
	walked := redactWalk(parsed)
	b, err := json.MarshalIndent(walked, "", "  ")
	if err != nil {
		return "[unserializable]"
	}
	return string(b)
}

func redactWalk(o interface{}) interface{} {
	if m, ok := o.(map[string]interface{}); ok {
		result := map[string]interface{}{}
		for k, v := range m {
			lower := strings.ToLower(k)
			if lower == "api_key" || lower == "apikey" || strings.Contains(lower, "token") || strings.Contains(lower, "password") || strings.Contains(lower, "secret") || strings.Contains(lower, "authorization") {
				result[k] = "[REDACTED]"
			} else if k == "messages" {
				if arr, ok := v.([]interface{}); ok {
					result[k] = redactWalk(arr)
				} else {
					result[k] = redactWalk(v)
				}
			} else if k == "content" {
				if s, ok := v.(string); ok && len(s) > 2000 {
					result[k] = s[:2000] + "...[truncated]"
				} else {
					result[k] = redactWalk(v)
				}
			} else {
				result[k] = redactWalk(v)
			}
		}
		return result
	}
	if arr, ok := o.([]interface{}); ok {
		result := make([]interface{}, len(arr))
		for i, v := range arr {
			result[i] = redactWalk(v)
		}
		return result
	}
	return o
}

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

// ─── Error Logging (§16) ───────────────────────────────────────────────────

// ErrorLogRecord represents a single error log entry per SPEC §15.5.
type ErrorLogRecord struct {
	Timestamp string           `json:"timestamp"`
	ErrorType string           `json:"error_type"`
	Stage     string           `json:"stage"`
	Attempt   int              `json:"attempt"`
	SessNum   int64            `json:"sessNum"`
	SlotName  string           `json:"slotName"`
	Request   ErrorLogRequest  `json:"request"`
	Upstream  ErrorLogUpstream `json:"upstream"`
}

type ErrorLogRequest struct {
	Method  string              `json:"method"`
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
}

type ErrorLogUpstream struct {
	URL        string              `json:"url"`
	Method     string              `json:"method"`
	Headers    map[string][]string `json:"headers"`
	Status     int                 `json:"status"`
	StatusText string              `json:"statusText"`
	Body       string              `json:"body"`
}

// InitErrorLogFile creates the .logs/ directory (if needed) and opens a session-level
// error log file named errors-{ISO-timestamp}.log. Colons and dots in the timestamp
// are replaced with dashes. Idempotent: safe to call multiple times.
// SPEC §16.1
func (p *Proxy) InitErrorLogFile() error {
	p.errorLogMu.Lock()
	defer p.errorLogMu.Unlock()

	if p.errorLogInitDone {
		return nil
	}

	// Create .logs/ directory (perm 0755)
	if err := os.MkdirAll(".logs", 0755); err != nil {
		return fmt.Errorf("InitErrorLogFile: creating .logs directory: %w", err)
	}

	// Build filename: errors-{ISO-timestamp}.log
	// RFC3339Nano for uniqueness, then replace colons and dots with dashes
	ts := time.Now().Format(time.RFC3339Nano)
	ts = strings.ReplaceAll(ts, ":", "-")
	ts = strings.ReplaceAll(ts, ".", "-")

	filename := filepath.Join(".logs", "errors-"+ts+".log")

	f, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("InitErrorLogFile: opening error log file: %w", err)
	}

	p.errorLogFile = f
	p.errorLogInitDone = true
	return nil
}

// LogHttpError appends an error record to the session error log file.
// Format: "--- HTTP ERROR ---\n{json}\n\n"
// SPEC §16.4
func (p *Proxy) LogHttpError(record ErrorLogRecord) error {
	p.errorLogMu.Lock()
	defer p.errorLogMu.Unlock()

	if p.errorLogFile == nil {
		return nil // not initialized — non-fatal
	}

	jsonBytes, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		jsonBytes = []byte("[unserializable]")
	}

	entry := "--- HTTP ERROR ---\n" + string(jsonBytes) + "\n\n"
	if _, err := p.errorLogFile.WriteString(entry); err != nil {
		return fmt.Errorf("LogHttpError: writing to error log: %w", err)
	}
	return nil
}

// LogRetryableError is a convenience wrapper that constructs and logs an error record
// for a retryable HTTP error (500/503). SPEC §15.5
func (p *Proxy) LogRetryableError(
	attempt int, isLast bool,
	sessNum int64, slotName string,
	reqMethod, reqURL string,
	reqHeaders map[string][]string, reqBody string,
	upstreamURL, upstreamMethod string,
	upstreamHeaders map[string][]string,
	upstreamStatus int, upstreamStatusText, upstreamBody string,
) error {
	stage := "retryable_attempt"
	if isLast {
		stage = "final_attempt"
	}
	record := ErrorLogRecord{
		Timestamp: time.Now().Format(time.RFC3339),
		ErrorType: "upstream_http_error",
		Stage:     stage,
		Attempt:   attempt,
		SessNum:   sessNum,
		SlotName:  slotName,
		Request: ErrorLogRequest{
			Method:  reqMethod,
			URL:     reqURL,
			Headers: RedactHeaders(reqHeaders),
			Body:    RedactBodyJson(truncateBody(reqBody, 4096)),
		},
		Upstream: ErrorLogUpstream{
			URL:        upstreamURL,
			Method:     upstreamMethod,
			Headers:    RedactHeaders(upstreamHeaders),
			Status:     upstreamStatus,
			StatusText: upstreamStatusText,
			Body:       RedactBodyJson(truncateBody(upstreamBody, 4096)),
		},
	}
	return p.LogHttpError(record)
}

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
	return string(data), nil
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

	if info, ok := p.ModelInfoMap[resolvedModel]; ok {
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
func (p *Proxy) proxyAnthropicRequest(item QueueItem) {
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

	// STEP 5: Limit images (§20.3)
	LimitImagesInMessages(payload, cfgMaxImages)

	// STEP 5b: Model resolution (same as OpenAI path)
	resolvedModel := p.ResolveModelId(model)
	payload["model"] = resolvedModel

	// STEP 6: Vision handoff (§20.5, §11)
	if p.NeedsVisionHandoff(resolvedModel) {
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
			resp.Body.Close()
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

// dispatchItem sends an item to the appropriate pipeline function.
// proxyChatRequest/proxyAnthropicRequest are full implementations handling the
// complete request lifecycle (key acquisition, normalization, retries, etc.).
// The defer OnRequestComplete() ensures activeRequests is decremented exactly
// once per dispatched item.
func (p *Proxy) dispatchItem(item *QueueItem) {
	defer func() {
		if rv := recover(); rv != nil {
			stack := make([]byte, 4096)
			n := runtime.Stack(stack, false)
			log.Printf("PANIC recovered in dispatchItem: %v\n%s", rv, stack[:n])
		}
		p.OnRequestComplete()
	}()

	// Lightweight guard: if the response became committed/aborted after the slot
	// was reserved, the slot will still be released by the defer above.
	if tw, ok := item.Response.(*responseWriterTracker); ok && tw.IsCommitted() {
		return
	}

	if item.Format == "anthropic" {
		p.proxyAnthropicRequest(*item)
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

	eff := p.GetEffectiveConcurrency(p.LastConcurrency)

	p.concurrencyLimitMu.RLock()
	mode := p.concurrencyLimitMode
	manualLimit := p.manualLimit
	p.concurrencyLimitMu.RUnlock()

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

// TryEnqueueOrDispatch attempts to dispatch a request immediately if under the
// concurrency gate, or enqueues it if capacity exists.
//
// Contract:
//   - Returns true if the request was dispatched directly. ActiveRequests is
//     already incremented; OnRequestComplete() is called by dispatchItem.
//   - Returns false if the request was enqueued or rejected. The caller must
//     NOT call OnRequestComplete() in this case.
func (p *Proxy) TryEnqueueOrDispatch(item *QueueItem) bool {
	p.queueMu.Lock()

	if p.shuttingDown.Load() {
		p.queueMu.Unlock()
		if item != nil && item.WriteError != nil && item.Response != nil {
			item.WriteError(item.Response, http.StatusServiceUnavailable,
				"server is shutting down", "server_error", "shutting_down")
		}
		return false
	}

	gate := p.gateLimit()

	// No gate → dispatch immediately
	if gate < 0 {
		p.ActiveRequests++
		p.queueMu.Unlock()
		p.dispatchItem(item)
		return true
	}

	// Under gate → dispatch immediately
	if p.ActiveRequests < gate {
		p.ActiveRequests++
		p.queueMu.Unlock()
		// Guard: if the client already disconnected/committed, release the slot.
		if tw, ok := item.Response.(*responseWriterTracker); ok && tw.IsCommitted() {
			p.OnRequestComplete()
			return true
		}
		p.dispatchItem(item)
		return true
	}

	// At or over gate → try to enqueue
	if len(p.requestQueue) >= MaxQueueSize {
		// Queue full → reject with 503
		p.ThrottledCount++
		p.queueMu.Unlock()
		if item != nil && item.WriteError != nil && item.Response != nil {
			WriteQueueFullError(item.Response, item.Format)
		}
		return false
	}

	// Enqueue
	p.requestQueue = append(p.requestQueue, *item)
	p.QueueLen = len(p.requestQueue)
	p.queueMu.Unlock()
	return false
}

// GetQueueLength returns the current queue length in a thread-safe manner.
func (p *Proxy) GetQueueLength() int {
	p.queueMu.Lock()
	defer p.queueMu.Unlock()
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

// OnRequestComplete decrements the active request count and processes the queue.
// If SlotFreeDelay > 0, it sleeps that many seconds before freeing the slot.
func (p *Proxy) OnRequestComplete() {
	// Configurable delay before freeing the slot (avoids upstream rate limits).
	if delay := p.getSlotFreeDelay(); delay > 0 {
		time.Sleep(time.Duration(delay) * time.Second)
	}
	p.queueMu.Lock()
	if p.ActiveRequests > 0 {
		p.ActiveRequests--
	}
	p.queueMu.Unlock()
	p.ProcessQueue()
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
	}
	p.requestQueue = nil
	p.QueueLen = 0
}

// ProcessQueue dispatches queued requests while there is capacity.
func (p *Proxy) ProcessQueue() int {
	p.queueMu.Lock()

	if p.shuttingDown.Load() {
		p.queueMu.Unlock()
		return 0
	}

	gate := p.gateLimit()

	if gate < 0 {
		p.queueMu.Unlock()
		return 0
	}

	dispatched := 0
	for len(p.requestQueue) > 0 && p.ActiveRequests < gate {
		item := p.requestQueue[0]
		p.requestQueue = p.requestQueue[1:]
		p.QueueLen = len(p.requestQueue)

		// Drop disconnected or already committed/aborted items silently.
		if item.Req.Context().Err() != nil {
			if tw, ok := item.Response.(*responseWriterTracker); ok && !tw.IsCommitted() {
				if item.WriteError != nil {
					item.WriteError(item.Response, http.StatusGatewayTimeout,
						"queue timeout: client disconnected", "server_error", "queue_timeout")
				}
			}
			continue
		}
		if tw, ok := item.Response.(*responseWriterTracker); ok && tw.IsCommitted() {
			continue
		}

		p.ActiveRequests++
		dispatched++

		go p.dispatchItem(&item)
	}
	p.queueMu.Unlock()
	return dispatched
}

// triggerProcessQueue debounces ProcessQueue triggers from config changes.
func (p *Proxy) triggerProcessQueue() {
	p.pqTriggerMu.Lock()
	defer p.pqTriggerMu.Unlock()
	if p.pqTriggerTimer != nil {
		p.pqTriggerTimer.Stop()
	}
	p.pqTriggerTimer = time.AfterFunc(100*time.Millisecond, func() {
		p.ProcessQueue()
	})
}

// stopProcessQueueTrigger stops any pending ProcessQueue trigger.
func (p *Proxy) stopProcessQueueTrigger() {
	p.pqTriggerMu.Lock()
	defer p.pqTriggerMu.Unlock()
	if p.pqTriggerTimer != nil {
		p.pqTriggerTimer.Stop()
		p.pqTriggerTimer = nil
	}
}

// ResetQueue clears all queue state.
func (p *Proxy) ResetQueue() {
	p.queueMu.Lock()
	defer p.queueMu.Unlock()
	p.requestQueue = nil
	p.QueueLen = 0
	p.ActiveRequests = 0
	p.ThrottledCount = 0
}

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
		Usage              UsageInfo     `json:"usage"`
		Window             *WindowInfo  `json:"window"`
		Plan               PlanInfo      `json:"plan"`
		Limits             *UsageLimits  `json:"limits"`
		UserID             string        `json:"user_id"`
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

	// Step 1: Read old ThrottledWindow under p.mu.RLock (snapshot)
	p.mu.RLock()
	oldWindow := p.ThrottledWindow
	p.mu.RUnlock()

	// Step 2: If window changed, reset ThrottledCount under queueMu.Lock()
	// (ThrottledCount lives under queueMu, NOT p.mu)
	if newWindow != "" && newWindow != oldWindow {
		p.queueMu.Lock()
		p.ThrottledCount = 0
		p.queueMu.Unlock()
	}

	// Step 3: Write ThrottledWindow + cache fields under p.mu.Lock()
	p.mu.Lock()
	if newWindow != "" && newWindow != oldWindow {
		p.ThrottledWindow = newWindow
	}
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
func (p *Proxy) GetEffectiveConcurrency(lastConcurrency ConcurrencyData) ConcurrencyData {
	override := p.Config.OverrideConcurrency

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
		if pr, ok := p.UpstreamPricing[m]; ok {
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
	displayNames := p.DisplayNameMap
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
	modelInfo := p.ModelInfoMap
	p.catalogMu.RUnlock()
	WriteJSON(w, http.StatusOK, modelInfo)
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

	// Construct QueueItem for the concurrency queue
	item := QueueItem{
		Format:                "openai",
		Response:              w,
		Payload:               payload,
		Model:                 model,
		WriteError:            OpenAIErrorWriter(),
		WritePassthroughError: OpenAIPassthroughErrorWriter(),
		Req:                   r,
	}

	// §5.3: Try to dispatch immediately or enqueue
	if p.TryEnqueueOrDispatch(&item) {
		return
	}
	// If TryEnqueueOrDispatch returned false, the request was either enqueued
	// (will be dispatched later by ProcessQueue → dispatchItem, which
	// handles ActiveRequests increment/decrement) or rejected with a 503
	// (WriteQueueFullError already called inside TryEnqueueOrDispatch).
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
	item := QueueItem{
		Response:              w,
		Payload:               payload,
		Model:                 model,
		WriteError:            AnthropicErrorWriter(),
		WritePassthroughError: AnthropicPassthroughErrorWriter(),
		Format:                "anthropic",
		Req:                   r,
		Fingerprint:           fp,
		PreferredKeyIndex:     preferredIndex,
		IsStream:              isStream,
	}

	// Try to dispatch immediately or enqueue (§5)
	dispatched := p.TryEnqueueOrDispatch(&item)
	if dispatched {
		return
	}
}

func (p *Proxy) HandleConfigGet(w http.ResponseWriter, r *http.Request) {
	p.configMu.RLock()
	defer p.configMu.RUnlock()
	WriteJSON(w, http.StatusOK, map[string]interface{}{
		"listenAddr":               p.Config.ListenAddr,
		"upstreamBaseURL":          p.Config.UpstreamBaseURL,
		"apiKey":                   MaskToken(p.Config.APIKey),
		"enabledModels":            p.Config.EnabledModels,
		"modelDisplayNames":        p.Config.ModelDisplayNames,
		"overrideConcurrency":      p.Config.OverrideConcurrency,
		"maxImages":                p.Config.MaxImages,
		"disabledModels":           p.Config.DisabledModels,
		"visionHandoffEnabled":     p.Config.VisionHandoffEnabled,
		"visionHandoffModel":       p.Config.VisionHandoffModel,
		"visionHandoffPrompt":      p.Config.VisionHandoffPrompt,
		"visionHandoffCacheEnabled": p.Config.VisionHandoffCacheEnabled,
		"wallpaperSource":           p.Config.WallpaperSource,
		"apikeyMode":                p.Config.ApiKeyMode,
		"concurrencyLimitMode":      p.getConcurrencyLimitMode(),
		"manualConcurrencyLimit":     p.getManualLimit(),
		"slotFreeDelay":              p.Config.SlotFreeDelay,
		"retryAttempts":              p.getRetryAttempts(),
		"backoffStrategy":            p.getBackoffStrategy(),
		"requestTimeout":             p.Config.RequestTimeout.DurationMs() / 1000,
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
	p.configMu.Lock()
	defer p.configMu.Unlock()
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
			WriteJSON(w, http.StatusBadRequest, map[string]interface{}{
				"error": "wallpaperSource must be one of: none, bing, wallhaven",
			})
			return
		}
	}
	if v, ok := data["overrideConcurrency"].(float64); ok {
		if v < 0 {
			WriteJSON(w, http.StatusBadRequest, map[string]interface{}{
				"error": "overrideConcurrency must be >= 0",
			})
			return
		}
		p.Config.OverrideConcurrency = int(v)
	}
	if v, ok := data["maxImages"].(float64); ok {
		if v < 0 {
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
				eff := p.GetEffectiveConcurrency(p.LastConcurrency)
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
			// Any mode change can change the gate, so always process the queue
			// to dispatch waiting items or clear stale state.
			p.triggerProcessQueue()
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
		p.triggerProcessQueue()
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
	debouncedSaveConfig(*p.Config)
	p.invalidateDashboardCache()

	WriteJSON(w, http.StatusOK, map[string]interface{}{
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
		"concurrencyLimitMode":   p.getConcurrencyLimitMode(),
		"manualConcurrencyLimit": p.getManualLimit(),
		"slotFreeDelay":            p.Config.SlotFreeDelay,
		"retryAttempts":            p.getRetryAttempts(),
		"backoffStrategy":          p.getBackoffStrategy(),
		"requestTimeout":           p.Config.RequestTimeout.DurationMs() / 1000,
		"restartRequired":           restartRequired,
	})
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

	eff := p.GetEffectiveConcurrency(lastConcurrency)
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
	p.shuttingDown.Store(true)
	p.stopProcessQueueTrigger()

	// Reject queued items under queueMu
	p.queueMu.Lock()
	p.rejectQueueLocked()
	p.queueMu.Unlock()

	// Proxy-level drain: poll ActiveRequests until 0 or 5-second timeout.
	pollTimeout := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
pollLoop:
	for {
		p.queueMu.RLock()
		active := p.ActiveRequests
		p.queueMu.RUnlock()
		if active <= 0 {
			break pollLoop
		}
		select {
		case <-pollTimeout:
			break pollLoop
		case <-ticker.C:
		}
	}
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
func endOfTodayUTC() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, time.UTC)
}

// serveWallpaperImage writes image data to the response with image/jpeg content type
// and optional Expires header. Used by both wallpaper handlers.
func serveWallpaperImage(w http.ResponseWriter, data []byte, expires *time.Time) {
	serveWallpaperImageTyped(w, data, expires, "image/jpeg")
}

// serveWallpaperImageTyped writes image data to the response with an optional
// Expires header and a caller-specified content type.
func serveWallpaperImageTyped(w http.ResponseWriter, data []byte, expires *time.Time, contentType string) {
	w.Header().Set("Content-Type", contentType)
	if expires != nil {
		w.Header().Set("Expires", expires.UTC().Format(http.TimeFormat))
	}
	w.Write(data)
}

// downloadImage fetches an image from the given URL with the specified User-Agent
// and timeout. Returns the raw image bytes, or nil on error.
func downloadImage(imageURL, userAgent string, timeout time.Duration) []byte {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest("GET", imageURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}
	return data
}

// saveCacheFile writes data to the cache file, creating the directory if needed.
// Returns true on success, false on error. Does not return 500 on failure.
func saveCacheFile(cacheFile string, data []byte) bool {
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0755); err != nil {
		return false
	}
	if err := os.WriteFile(cacheFile, data, 0644); err != nil {
		return false
	}
	return true
}

// isJPEG checks whether data starts with the JPEG magic bytes (0xFF 0xD8).
// Used only for freshly downloaded images, NOT for cached files (tests use fake data).
func isJPEG(data []byte) bool {
	return len(data) >= 2 && data[0] == 0xFF && data[1] == 0xD8
}

// isValidImage checks whether data starts with a recognized image format magic
// bytes (JPEG, PNG, or WebP). Used for validating freshly downloaded wallpapers.
func isValidImage(data []byte) bool {
	if len(data) < 2 {
		return false
	}
	// JPEG: FF D8
	if data[0] == 0xFF && data[1] == 0xD8 {
		return true
	}
	// PNG: 89 50 4E 47
	if len(data) >= 4 && data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47 {
		return true
	}
	// WebP: 52 49 46 46 ?? ?? ?? ?? 57 45 42 50 (RIFF....WEBP)
	if len(data) >= 12 && data[0] == 0x52 && data[1] == 0x49 && data[2] == 0x46 && data[3] == 0x46 &&
		data[8] == 0x57 && data[9] == 0x45 && data[10] == 0x42 && data[11] == 0x50 {
		return true
	}
	return false
}

// imageContentType returns the MIME content type for a valid image, or
// "image/jpeg" as fallback.
func imageContentType(data []byte) string {
	if len(data) >= 4 && data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47 {
		return "image/png"
	}
	if len(data) >= 12 && data[0] == 0x52 && data[1] == 0x49 && data[2] == 0x46 && data[3] == 0x46 &&
		data[8] == 0x57 && data[9] == 0x45 && data[10] == 0x42 && data[11] == 0x50 {
		return "image/webp"
	}
	return "image/jpeg"
}

// HandleRestart is the real restart handler routed from /api/restart.
// It responds with success and then exits the process with code 42 after
// a short delay so the HTTP response is flushed. §17.1 Route #13.
func (p *Proxy) HandleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "Restarting...",
	})
	// §29.2, §35.2: after 500ms, close HTTP server and exit(42).
	if p.httpServer == nil && p.exitFn == nil {
		return
	}
	time.AfterFunc(500*time.Millisecond, p.triggerRestart)
}

// upgradePeapixResolution replaces the resolution suffix in a peapix image URL
// with _3840 for UHD (3840x2160) output. Falls back to the original URL if the
// pattern doesn't match.
func upgradePeapixResolution(imageURL string) string {
	// Peapix URLs look like: https://img.peapix.com/<hash>_1920.jpg
	// Replace the _NNNN suffix before .jpg with _3840.
	re := regexp.MustCompile(`_\d+\.(jpg|jpeg|png)$`)
	return re.ReplaceAllString(imageURL, "_3840.$1")
}

// HandleBingWallpaper proxies the daily Bing wallpaper via peapix.com.
// Cache TTL: 24 hours (daily). Falls back to cached file on error.
func (p *Proxy) HandleBingWallpaper(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}
	cacheFile := ".cache/wallpaper.jpg"
	expires := endOfTodayUTC()

	// Fast path: check cache WITHOUT lock (read-only stat + read).
	// If file exists and was modified today (UTC), serve it immediately.
	if info, err := os.Stat(cacheFile); err == nil {
		today := time.Now().UTC().Format("2006-01-02")
		cachedDate := info.ModTime().UTC().Format("2006-01-02")
		if cachedDate == today {
			if data, err := os.ReadFile(cacheFile); err == nil {
				serveWallpaperImage(w, data, &expires)
				return
			}
		}
	}

	// Cache miss or expired — acquire lock to prevent duplicate fetches.
	p.wallpaperMu.Lock()
	defer p.wallpaperMu.Unlock()

	// Double-check: another goroutine may have refreshed the cache while we
	// waited for the lock.
	if info, err := os.Stat(cacheFile); err == nil {
		today := time.Now().UTC().Format("2006-01-02")
		cachedDate := info.ModTime().UTC().Format("2006-01-02")
		if cachedDate == today {
			if data, err := os.ReadFile(cacheFile); err == nil {
				serveWallpaperImage(w, data, &expires)
				return
			}
		}
	}

	// Fetch from peapix.com/bing/feed
	peapixURL := "https://peapix.com/bing/feed"
	peapixUA := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

	apiClient := &http.Client{Timeout: 15 * time.Second}
	apiReq, err := http.NewRequest("GET", peapixURL, nil)
	if err != nil {
		if data, err := os.ReadFile(cacheFile); err == nil {
			serveWallpaperImage(w, data, &expires)
			return
		}
		WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "wallpaper not available"})
		return
	}
	apiReq.Header.Set("User-Agent", peapixUA)

	apiResp, err := apiClient.Do(apiReq)
	if err != nil {
		if data, err := os.ReadFile(cacheFile); err == nil {
			serveWallpaperImage(w, data, &expires)
			return
		}
		WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "wallpaper not available"})
		return
	}
	defer apiResp.Body.Close()

	if apiResp.StatusCode != http.StatusOK {
		if data, err := os.ReadFile(cacheFile); err == nil {
			serveWallpaperImage(w, data, &expires)
			return
		}
		WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "wallpaper not available"})
		return
	}

	body, err := io.ReadAll(apiResp.Body)
	if err != nil {
		if data, err := os.ReadFile(cacheFile); err == nil {
			serveWallpaperImage(w, data, &expires)
			return
		}
		WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "wallpaper not available"})
		return
	}

	// Parse JSON: expect array of objects with fullUrl, imageUrl, or url field.
	var feed []map[string]interface{}
	if err := json.Unmarshal(body, &feed); err != nil {
		if data, err := os.ReadFile(cacheFile); err == nil {
			serveWallpaperImage(w, data, &expires)
			return
		}
		WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "wallpaper not available"})
		return
	}
	if len(feed) == 0 {
		if data, err := os.ReadFile(cacheFile); err == nil {
			serveWallpaperImage(w, data, &expires)
			return
		}
		WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "wallpaper not available"})
		return
	}

	// Extract image URL from first item: fullUrl, imageUrl, or url.
	var imageURL string
	if u, ok := feed[0]["fullUrl"].(string); ok && u != "" {
		imageURL = u
	} else if u, ok := feed[0]["imageUrl"].(string); ok && u != "" {
		imageURL = u
	} else if u, ok := feed[0]["url"].(string); ok && u != "" {
		imageURL = u
	}
	if imageURL == "" {
		if data, err := os.ReadFile(cacheFile); err == nil {
			serveWallpaperImage(w, data, &expires)
			return
		}
		WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "wallpaper not available"})
		return
	}

	// Upgrade to UHD (3840) variant for large screens — replaces _1920, _640, etc.
	imageURL = upgradePeapixResolution(imageURL)

	// Download the image with 30s timeout and browser User-Agent.
	imageData := downloadImage(imageURL, peapixUA, 30*time.Second)
	if imageData == nil || !isJPEG(imageData) {
		if data, err := os.ReadFile(cacheFile); err == nil {
			serveWallpaperImage(w, data, &expires)
			return
		}
		WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "wallpaper not available"})
		return
	}

	// Save to cache.
	saveCacheFile(cacheFile, imageData)

	// Serve the fresh image.
	serveWallpaperImage(w, imageData, &expires)
}

// HandleWallhavenWallpaper proxies a random top-listed wallpaper from wallhaven.cc.
// Cache TTL: 1 hour. Falls back to cached file on error.
func (p *Proxy) HandleWallhavenWallpaper(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}
	cacheFile := ".cache/wallpaper-haven.jpg"

	// Fast path: check cache WITHOUT lock (read-only stat + read).
	// If file exists and is < 1 hour old, serve it immediately.
	if info, err := os.Stat(cacheFile); err == nil {
		if time.Since(info.ModTime()) < time.Hour {
			if data, err := os.ReadFile(cacheFile); err == nil {
				serveWallpaperImage(w, data, nil) // no Expires for wallhaven (GAP 5)
				return
			}
		}
	}

	// Cache miss or expired — acquire lock.
	p.wallpaperMu.Lock()
	defer p.wallpaperMu.Unlock()

	// Double-check after acquiring lock.
	if info, err := os.Stat(cacheFile); err == nil {
		if time.Since(info.ModTime()) < time.Hour {
			if data, err := os.ReadFile(cacheFile); err == nil {
				serveWallpaperImage(w, data, nil)
				return
			}
		}
	}

	// Fetch from wallhaven.cc API — filter for >=1440p wallpapers
	wallhavenURL := "https://wallhaven.cc/api/v1/search?categories=100&purity=100&topRange=1M&sorting=toplist&order=desc&page=3&atleast=2560x1440"
	wallhavenUA := "umans-proxy/1.0"
	browserUA := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

	// Helper closure: fallback to stale cache, else 404.
	fallback := func() {
		if data, err := os.ReadFile(cacheFile); err == nil {
			serveWallpaperImage(w, data, nil)
			return
		}
		WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "wallpaper not available"})
	}

	apiClient := &http.Client{Timeout: 15 * time.Second}
	apiReq, err := http.NewRequest("GET", wallhavenURL, nil)
	if err != nil {
		fallback()
		return
	}
	apiReq.Header.Set("User-Agent", wallhavenUA)

	apiResp, err := apiClient.Do(apiReq)
	if err != nil {
		fallback()
		return
	}
	defer apiResp.Body.Close()

	if apiResp.StatusCode != http.StatusOK {
		fallback()
		return
	}

	body, err := io.ReadAll(apiResp.Body)
	if err != nil {
		fallback()
		return
	}

	// Parse JSON: expect { "data": [ { "path": "https://..." }, ... ] }
	var result struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		fallback()
		return
	}
	if len(result.Data) == 0 {
		fallback()
		return
	}

	// Pick a random entry from the data array.
	pick := result.Data[rand.Intn(len(result.Data))]
	imageURL, ok := pick["path"].(string)
	if !ok || imageURL == "" {
		fallback()
		return
	}

	// Download the image with 30s timeout and browser User-Agent.
	imageData := downloadImage(imageURL, browserUA, 30*time.Second)
	if imageData == nil || !isValidImage(imageData) {
		fallback()
		return
	}

	// Save to cache.
	saveCacheFile(cacheFile, imageData)

	// Serve the fresh image.
	serveWallpaperImageTyped(w, imageData, nil, imageContentType(imageData))
}

// PerformVisionHandoff replaces images with text descriptions from the handoff
// model (SPEC §11.5). It accepts optional pre-collected image parts via variadic
// to avoid re-scanning the payload when parts have already been collected.
func (p *Proxy) PerformVisionHandoff(ctx context.Context, payload map[string]interface{}, resolvedModel string, apiKey string, preCollectedParts ...[]ImagePart) int {
	if !p.NeedsVisionHandoff(resolvedModel) {
		return 0
	}

	var parts []ImagePart
	if len(preCollectedParts) > 0 && preCollectedParts[0] != nil {
		parts = preCollectedParts[0]
	} else {
		parts = CollectImageParts(payload)
	}

	if len(parts) == 0 {
		return 0
	}

	handoffModel := p.Config.VisionHandoffModel
	if handoffModel == "" {
		handoffModel = "umans-coder"
	}

	limit := p.Config.VisionHandoffConcurrency
	if limit <= 0 {
		limit = 4
	}
	sem := make(chan struct{}, limit)

	descriptions := make([]string, len(parts))
	var wg sync.WaitGroup
	for i := range parts {
		wg.Add(1)
		go func(idx int, dataURI string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				descriptions[idx] = "[Image analysis cancelled]"
				return
			}
			defer func() { <-sem }()
			if p.Upstream == nil {
				descriptions[idx] = p.HandoffResponse
				return
			}
			descriptions[idx] = p.analyzeImageViaHandoff(ctx, dataURI, handoffModel, apiKey)
		}(i, parts[i].DataURI)
	}
	wg.Wait()

	for i, ip := range parts {
		label := "[Image content — analyzed by vision module, shown as text because the active model cannot see images:]\n"
		if len(parts) > 1 {
			label = fmt.Sprintf("[Image %d content — analyzed by vision module, shown as text because the active model cannot see images:]\n", i+1)
		}
		content := ip.Container.([]interface{})
		content[ip.Index] = map[string]interface{}{
			"type": "text",
			"text": label + descriptions[i],
		}
	}

	return len(parts)
}

// dashboardTemplateData holds values injected into the HTML/JS templates at serve time.
type dashboardTemplateData struct {
	Version              string
	BackoffPresets       string // JSON-encoded
	DefaultRetryAttempts int
	DefaultRequestTimeout int // seconds
}

// renderDashboard renders the dashboard HTML template with injected values.
// Caches the rendered output on first call. Falls back to raw bytes on parse error.
func (p *Proxy) renderDashboard() []byte {
	p.dashMu.Lock()
	defer p.dashMu.Unlock()
	if p.dashRendered != nil {
		return p.dashRendered
	}
	if len(p.DashboardHTML) == 0 {
		return nil
	}
	tmpl, err := template.New("dashboard").Parse(string(p.DashboardHTML))
	if err != nil {
		// Template parse error — serve raw HTML
		p.dashRendered = p.DashboardHTML
		return p.dashRendered
	}
	presetsJSON, _ := json.Marshal(BackoffPresets)
	data := dashboardTemplateData{
		Version:               p.Version,
		BackoffPresets:        string(presetsJSON),
		DefaultRetryAttempts:  DefaultRetryAttempts,
		DefaultRequestTimeout: int(p.Config.RequestTimeout.Duration.Seconds()),
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		p.dashRendered = p.DashboardHTML
		return p.dashRendered
	}
	p.dashRendered = buf.Bytes()
	return p.dashRendered
}

// renderDashboardJS renders the dashboard JS template with injected values.
// Caches the rendered output on first call. Falls back to raw bytes on parse error.
func (p *Proxy) renderDashboardJS() []byte {
	p.dashMu.Lock()
	defer p.dashMu.Unlock()
	if p.dashJsRendered != nil {
		return p.dashJsRendered
	}
	if len(p.DashboardJS) == 0 {
		return nil
	}
	tmpl, err := template.New("dashboardJS").Parse(string(p.DashboardJS))
	if err != nil {
		p.dashJsRendered = p.DashboardJS
		return p.dashJsRendered
	}
	presetsJSON, _ := json.Marshal(BackoffPresets)
	data := dashboardTemplateData{
		Version:               p.Version,
		BackoffPresets:        string(presetsJSON),
		DefaultRetryAttempts:  DefaultRetryAttempts,
		DefaultRequestTimeout: int(p.Config.RequestTimeout.Duration.Seconds()),
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		p.dashJsRendered = p.DashboardJS
		return p.dashJsRendered
	}
	p.dashJsRendered = buf.Bytes()
	return p.dashJsRendered
}

// invalidateDashboardCache clears cached dashboard render outputs.
func (p *Proxy) invalidateDashboardCache() {
	p.dashMu.Lock()
	p.dashRendered = nil
	p.dashJsRendered = nil
	p.dashMu.Unlock()
}

// ServeHTTP implements http.Handler for the Proxy. It wraps HandleRequest
// with panic recovery (§35.1) and per-request timeout enforcement (§35.3).
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// §35.3: Determine timeout, default 300s.
	timeout := p.Config.RequestTimeout.Duration
	if timeout <= 0 {
		timeout = 300 * time.Second
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	tw := &responseWriterTracker{ResponseWriter: w, clientCtx: r.Context()}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if rv := recover(); rv != nil {
				stack := make([]byte, 4096)
				n := runtime.Stack(stack, false)
				stack = stack[:n]
				log.Printf("PANIC recovered in HTTP handler [path=%s method=%s]: %v\n%s",
					r.URL.Path, r.Method, rv, stack)

				_ = p.LogHttpError(ErrorLogRecord{
					Timestamp: time.Now().UTC().Format(time.RFC3339),
					ErrorType: "panic",
					Stage:     "handler",
					Request: ErrorLogRequest{
						Method: r.Method,
						URL:    r.URL.String(),
					},
				})

				tw.mu.Lock()
				alreadyWritten := tw.written
				if !alreadyWritten {
					tw.hijacked = true
				}
				tw.mu.Unlock()

				if !alreadyWritten {
					WriteOpenAIError(tw, http.StatusInternalServerError,
						"internal server error", "server_error", "")
				} else if tw.Header().Get("Content-Type") == "text/event-stream" {
					errEv := SSEEvent{
						Event: "error",
						Data:  `{"error":"internal server error"}`,
					}
					tw.Write([]byte(errEv.Format()))
					tw.Flush()
				}
			}
		}()
		p.HandleRequest(tw, r.WithContext(ctx))
	}()

	select {
	case <-done:
	case <-ctx.Done():
		// Timeout fired. If the response is already streaming (SSE), don't
		// kill it — let the upstream stream continue. The timeout applies to
		// the pre-streaming phase (queue + connect + first byte), not to the
		// streaming duration. Only hijack if nothing has been written yet.
		tw.mu.Lock()
		alreadyWritten := tw.written
		if !alreadyWritten {
			// Mark aborted so any concurrent writer (including queued dispatch
			// that starts after this point) becomes a no-op.
			tw.aborted = true
			tw.hijacked = true
		}
		tw.mu.Unlock()

		if !alreadyWritten {
			WriteOpenAIError(tw, http.StatusGatewayTimeout,
				"request timeout", "server_error", "timeout")
		}
		// If already streaming, do NOT inject a timeout error into the stream.
		// Wait for the handler goroutine to finish naturally (upstream EOF or error).
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
}

func (p *Proxy) HandleRequest(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Dashboard and healthz don't require auth
	if path != "/" && path != "/dashboard" && path != "/healthz" {
		if len(p.Config.APIKeys) > 0 && !p.Authorized(r) {
			WriteOpenAIError(w, http.StatusUnauthorized, "invalid proxy api key", "authentication_error", "")
			return
		}
	}

	switch {
	case path == "/" || path == "/dashboard":
		// Snapshot wallpaper source under configMu.RLock to avoid reading a
		// partially-updated value while HandleConfigPost is mutating Config.
		p.configMu.RLock()
		wallpaperSource := p.Config.WallpaperSource
		p.configMu.RUnlock()

		html := p.renderDashboard()
		if html == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		// Determine wallpaper style and build injection CSS
		var inject string
		p.wallpaperCssMu.RLock()
		cachedCss := p.wallpaperCss
		cachedMod := p.wallpaperCssMod
		cachedSrc := p.wallpaperCssSrc
		p.wallpaperCssMu.RUnlock()

		switch wallpaperSource {
		case "bing", "wallhaven":
			wpPath := ".cache/wallpaper.jpg"
			if wallpaperSource == "wallhaven" {
				wpPath = ".cache/wallpaper-haven.jpg"
			}
			info, err := os.Stat(wpPath)
			if err == nil {
				mod := info.ModTime()
				if cachedCss != "" && cachedSrc == wallpaperSource && cachedMod.Equal(mod) {
					inject = cachedCss
				} else {
					data, err := os.ReadFile(wpPath)
					if err == nil {
						b64 := base64.StdEncoding.EncodeToString(data)
						ct := imageContentType(data)
						inject = fmt.Sprintf(`<style>body{background-image:url('data:%s;base64,%s')!important;background-size:cover!important;background-position:center!important;background-repeat:no-repeat!important;background-attachment:fixed!important;background-color:#070912}</style>`, ct, b64)
						p.wallpaperCssMu.Lock()
						p.wallpaperCss = inject
						p.wallpaperCssMod = mod
						p.wallpaperCssSrc = wallpaperSource
						p.wallpaperCssMu.Unlock()
					} else {
						inject = `<style>body{background:#0d1117}</style>`
					}
				}
			} else {
				// No cached wallpaper file → fall back to dark background
				inject = `<style>body{background:#0d1117}</style>`
			}
		default: // "none" or empty
			inject = `<style>body{background:#0d1117}</style>`
		}

		// Inject before </head>; fall back to appending if no </head> found
		rendered := string(html)
		if strings.Contains(rendered, "</head>") {
			rendered = strings.Replace(rendered, "</head>", inject+"</head>", 1)
		} else {
			rendered += inject
		}

		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(rendered))
	case path == "/dashboard.js":
		js := p.renderDashboardJS()
		if js == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		w.Write(js)
	case path == "/healthz":
		p.HandleHealthz(w, r)
	case path == "/v1/models/info":
		p.HandleModelsInfo(w, r)
	case path == "/v1/models":
		p.HandleModels(w, r)
	case path == "/api/validate":
		p.HandleValidate(w, r)
	case path == "/api/models":
		p.HandleApiModels(w, r)
	case path == "/v1/chat/completions":
		p.HandleChatCompletions(w, r)
	case path == "/v1/messages" || path == "/messages":
		p.HandleMessages(w, r)
	case path == "/api/config":
		if r.Method == http.MethodGet {
			p.HandleConfigGet(w, r)
		} else if r.Method == http.MethodPost {
			p.HandleConfigPost(w, r)
		} else {
			WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		}
	case path == "/api/keys":
		if r.Method == http.MethodGet {
			p.HandleKeysGet(w, r)
		} else if r.Method == http.MethodPost {
			p.HandleKeysPost(w, r)
		} else {
			WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		}
	case path == "/api/umans/usage" && r.Method == http.MethodGet:
		p.HandleUsage(w, r)
	case path == "/api/umans/concurrency" && r.Method == http.MethodGet:
		p.HandleConcurrency(w, r)
	case path == "/api/umans/usage-history":
		p.HandleUsageHistory(w, r)
	case path == "/api/umans/user":
		p.HandleUser(w, r)
	case path == "/api/restart":
		p.HandleRestart(w, r)
	case path == "/api/bg":
		p.HandleBingWallpaper(w, r)
	case path == "/api/bg-wallhaven":
		p.HandleWallhavenWallpaper(w, r)
	case path == "/api/sleev" || path == "/api/bg-freegen":
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("Not Found"))
	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("Not Found"))
	}
}
