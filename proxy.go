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
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ─── Constants ─────────────────────────────────────────────────────────────

const (
	MaxRetries             = 10
	RetryDelayMs           = 3000
	MaxQueueSize           = 256
	QueueFullErrorCode     = "queue_full"
	MaxBodySize            = 5 * 1024 * 1024
	ConvMapMax             = 10000
	ConvMapEvictTarget     = 8000  // 80% of ConvMapMax — eviction target when map is at capacity
	UmansAPIBase           = "https://api.code.umans.ai/v1"
	APIKeyEnvVar           = "UMANS_API_KEY"
	ModelsDevCatalogURL    = "https://models.dev/api.json"
	ModelCatalogCacheTTL   = 5 * time.Minute
	ModelsDevCacheTTL      = 5 * time.Minute
	UsageCacheTTL          = 5 * time.Minute
	OpencodeConfigCacheTTL = 5 * time.Minute
	UpstreamModelsCacheTTL = 5 * time.Minute // §8.7
	UserInfoCacheTTL       = 5 * time.Minute // §8.8
	UserInfoCacheTimeout   = 10 * time.Second // §7.2 GetUserInfo timeout

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

var (
	modelsDevCache     map[string]interface{}
	modelsDevCacheTime time.Time
	modelsDevCacheMu   sync.Mutex
)

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

	// §31 step 7: Fetch models.dev catalog (non-fatal).
	getModelsDevCatalog()

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
	return Config{
		ListenAddr:             "127.0.0.1:8084",
		UpstreamBaseURL:        "https://api.code.umans.ai/v1",
		RequestTimeout:         ParseDuration("300s"),
		OverrideConcurrency:    0,
		MaxImages:              9,
		VisionHandoffEnabled:   false,
		VisionHandoffModel:     "umans-coder",
		VisionHandoffPrompt:    "",
		VisionHandoffCacheEnabled: false,
		VisionHandoffCacheTtl:  ParseDuration("24h"),
		WallpaperSource:        "bing",
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
}

func LoadConfig() (Config, error) {
	cfg := DefaultConfig()
	path := filepath.Join(".config", "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		applyEnvOverrides(&cfg)
		return cfg, nil
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		cfg = DefaultConfig()
		applyEnvOverrides(&cfg)
		return cfg, err
	}
	applyEnvOverrides(&cfg)
	if cfg.RequestTimeout.Duration <= 0 {
		log.Fatal("RequestTimeout must be positive")
	}
	return cfg, nil
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

var (
	opencodeTimer    *time.Timer
	opencodeTimerMu  sync.Mutex
	opencodeRunning  bool
)

var (
	opencodeDiscoveryCache     []string
	opencodeDiscoveryCacheHome string
	opencodeDiscoveryCachedAt  time.Time
	opencodeDiscoveryMu        sync.Mutex
)

func debouncedSetupOpencodeConfig(p *Proxy) {
	opencodeTimerMu.Lock()
	defer opencodeTimerMu.Unlock()
	if opencodeTimer != nil {
		opencodeTimer.Stop()
	}
	listenAddr := p.Config.ListenAddr
	opencodeTimer = time.AfterFunc(500*time.Millisecond, func() {
		opencodeTimerMu.Lock()
		if opencodeRunning {
			opencodeTimerMu.Unlock()
			return
		}
		opencodeRunning = true
		opencodeTimerMu.Unlock()

		defer func() {
			opencodeTimerMu.Lock()
			opencodeRunning = false
			opencodeTimerMu.Unlock()
		}()

		homeDir, _ := os.UserHomeDir()
		port := ParseListenPort(listenAddr)
		firstRun := p.SetupOpencodeConfig(homeDir, port)
		if firstRun {
			openBrowser(fmt.Sprintf("http://localhost:%d/", port))
		}
	})
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if cmd != nil {
		_ = cmd.Start()
	}
}

// ─── Upstream Client (§7) ───────────────────────────────────────────────────

// NewUpstreamClient creates a new UpstreamClient with a keep-alive HTTP client.
// The baseURL should be the API base (e.g., "https://api.code.umans.ai/v1").
// The timeout is the default/client-level timeout; per-request timeouts are
// enforced via context.WithTimeout in each method.
func NewUpstreamClient(baseURL, apiKey string, timeout time.Duration) *UpstreamClient {
	transport := &http.Transport{
		DisableKeepAlives:   false,
		MaxIdleConns:        128,
		MaxIdleConnsPerHost: 128,
		IdleConnTimeout:     60 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second,
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

// SetAPIKey updates the API key used for subsequent requests.
// This supports per-request key rotation from the key pool (§3).
func (u *UpstreamClient) SetAPIKey(key string) {
	u.apiKey = key
}

// GetUserInfo fetches model/user info from upstream.
// GET {baseURL}/models/info with Authorization: Bearer and Connection: keep-alive headers.
// 10-second timeout enforced via context.
// Returns parsed JSON as json.RawMessage.
func (u *UpstreamClient) GetUserInfo() (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.baseURL+"/models/info", nil)
	if err != nil {
		return nil, fmt.Errorf("GetUserInfo: creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+u.apiKey)
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
func (u *UpstreamClient) GetUsage() (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.baseURL+"/usage", nil)
	if err != nil {
		return nil, fmt.Errorf("GetUsage: creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+u.apiKey)
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
// Headers: Authorization: Bearer, Content-Type: application/json,
// Accept: text/event-stream (if stream) or application/json, Connection: keep-alive.
// Timeout: u.timeout (set from config.requestTimeout at construction).
// Returns raw *http.Response. Caller must defer resp.Body.Close().
func (u *UpstreamClient) ChatCompletions(body []byte, isStream bool) (*http.Response, error) {
	return u.doPost("/chat/completions", body, isStream, u.timeout)
}

// Messages sends an Anthropic Messages API request to upstream.
// POST {baseURL}/messages
// Same headers as ChatCompletions.
// Timeout: u.timeout (set from config.requestTimeout at construction).
// Returns raw *http.Response. Caller must defer resp.Body.Close().
func (u *UpstreamClient) Messages(body []byte, isStream bool) (*http.Response, error) {
	return u.doPost("/messages", body, isStream, u.timeout)
}

// doPost is the shared POST implementation for ChatCompletions and Messages.
// It constructs and sends a POST request with the appropriate headers and
// per-request timeout. The caller is responsible for closing resp.Body.
func (u *UpstreamClient) doPost(path string, body []byte, isStream bool, timeout time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.baseURL+path, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%s: creating request: %w", path, err)
	}

	req.Header.Set("Authorization", "Bearer "+u.apiKey)
	req.Header.Set("Content-Type", "application/json")
	if isStream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("Connection", "keep-alive")

	resp, err := u.httpClient.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%s: executing request: %w", path, err)
	}

	// Wrap body to call cancel on Close, ensuring context resources are released.
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
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

// ─── Models.dev (§9) ───────────────────────────────────────────────────────

func DeriveModelsDevId(umansId string) string {
	return strings.TrimPrefix(umansId, "umans-")
}

func UmansIdCandidates(umansId string) []string {
	base := DeriveModelsDevId(umansId)
	if base == umansId {
		return []string{umansId}
	}
	return []string{umansId, base}
}

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

func BuildModelsDevIndex(catalog map[string]interface{}) map[string]ModelsDevEntry {
	index := map[string]ModelsDevEntry{}
	priorityOrder := []string{"umans-ai", "openai", "anthropic", "google", "mistral", "meta", "xai", "deepseek", "moonshotai", "zhipuai", "alibaba", "nvidia", "cohere", "minimax", "stepfun", "xiaomi"}
	seen := map[string]bool{}
	for _, p := range priorityOrder {
		seen[p] = true
	}
	var allProviders []string
	for p := range catalog {
		if !seen[p] {
			allProviders = append(allProviders, p)
		}
	}
	sort.Strings(allProviders)
	for _, providerID := range append(priorityOrder, allProviders...) {
		provider, ok := catalog[providerID].(map[string]interface{})
		if !ok {
			continue
		}
		models, ok := provider["models"].(map[string]interface{})
		if !ok {
			continue
		}
		for modelID, model := range models {
			if _, exists := index[modelID]; !exists {
				index[modelID] = ModelsDevEntry{ProviderID: providerID, ModelID: modelID, Model: model}
			}
		}
	}
	return index
}

func fetchModelsDevCatalog() (map[string]interface{}, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", ModelsDevCatalogURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev catalog returned status %d", resp.StatusCode)
	}
	body := io.LimitReader(resp.Body, 50*1024*1024) // 50 MB cap
	var catalog map[string]interface{}
	if err := json.NewDecoder(body).Decode(&catalog); err != nil {
		return nil, fmt.Errorf("failed to decode models.dev catalog: %w", err)
	}
	return catalog, nil
}

func getModelsDevCatalog() map[string]interface{} {
	modelsDevCacheMu.Lock()
	defer modelsDevCacheMu.Unlock()
	if modelsDevCache != nil && time.Since(modelsDevCacheTime) < ModelsDevCacheTTL {
		return modelsDevCache
	}
	catalog, err := fetchModelsDevCatalog()
	if err != nil {
		log.Printf("models.dev catalog fetch failed (non-fatal): %v", err)
		return nil
	}
	modelsDevCache = catalog
	modelsDevCacheTime = time.Now()
	return catalog
}

func findModelsDevEntry(catalog map[string]interface{}, umansId string) *ModelsDevEntry {
	if catalog == nil {
		return nil
	}
	candidates := UmansIdCandidates(umansId)
	canonicalProviders := []string{
		"openai", "anthropic", "google", "mistral", "meta",
		"xai", "deepseek", "moonshotai", "zhipuai", "alibaba",
		"nvidia", "cohere", "minimax", "stepfun", "xiaomi",
	}

	// Tier 1: umans-ai provider, direct model match
	if provider, ok := catalog["umans-ai"].(map[string]interface{}); ok {
		if models, ok := provider["models"].(map[string]interface{}); ok {
			for _, candidate := range candidates {
				if model, ok := models[candidate].(map[string]interface{}); ok {
					return &ModelsDevEntry{
						ProviderID: "umans-ai",
						ModelID:    candidate,
						Model:      model,
					}
				}
			}
		}
	}

	// Tier 2: canonical providers, direct model match
	for _, providerID := range canonicalProviders {
		provider, ok := catalog[providerID].(map[string]interface{})
		if !ok {
			continue
		}
		models, ok := provider["models"].(map[string]interface{})
		if !ok {
			continue
		}
		for _, candidate := range candidates {
			if model, ok := models[candidate].(map[string]interface{}); ok {
				return &ModelsDevEntry{
					ProviderID: providerID,
					ModelID:    candidate,
					Model:      model,
				}
			}
		}
	}

	// Tier 3: scan all providers for matching candidate ID
	for providerID, providerRaw := range catalog {
		provider, ok := providerRaw.(map[string]interface{})
		if !ok {
			continue
		}
		models, ok := provider["models"].(map[string]interface{})
		if !ok {
			continue
		}
		for _, candidate := range candidates {
			if model, ok := models[candidate].(map[string]interface{}); ok {
				return &ModelsDevEntry{
					ProviderID: providerID,
					ModelID:    candidate,
					Model:      model,
				}
			}
		}
	}

	// Tier 4: match by nested model.id field
	for providerID, providerRaw := range catalog {
		provider, ok := providerRaw.(map[string]interface{})
		if !ok {
			continue
		}
		models, ok := provider["models"].(map[string]interface{})
		if !ok {
			continue
		}
		for modelKey, modelRaw := range models {
			model, ok := modelRaw.(map[string]interface{})
			if !ok {
				continue
			}
			if modelID, ok := model["id"].(string); ok {
				for _, candidate := range candidates {
					if modelID == candidate {
						return &ModelsDevEntry{
							ProviderID: providerID,
							ModelID:    modelKey,
							Model:      model,
						}
					}
				}
			}
		}
	}

	return nil
}

func resolveReasoningMode(devEntry *ModelsDevEntry, reasoningCaps interface{}) *bool {
	if devEntry != nil {
		if model, ok := devEntry.Model.(map[string]interface{}); ok {
			if rawOpts, ok := model["reasoning_options"]; ok {
				opts := ParseLevels(rawOpts)
				if len(opts) > 0 {
					t := true
					return &t
				}
			}
		}
	}
	if mode := InferReasoningModeFromCapabilities(reasoningCaps); mode != nil {
		return mode
	}
	t := true
	return &t
}

// ─── Model Catalog (§8) ────────────────────────────────────────────────────

func (p *Proxy) ApplyCatalogData(data map[string]interface{}) {
	p.catalogMu.Lock()
	defer p.catalogMu.Unlock()
	if p.ModelInfoMap == nil {
		p.ModelInfoMap = map[string]ModelInfo{}
	}
	if p.DisplayNameMap == nil {
		p.DisplayNameMap = map[string]string{}
	}
	for id, info := range data {
		infoMap, ok := info.(map[string]interface{})
		if !ok {
			continue
		}
		mi := ModelInfo{}
		if dn, ok := infoMap["display_name"].(string); ok {
			mi.DisplayName = dn
			p.DisplayNameMap[id] = stripUmansPrefix(dn)
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
		p.ModelInfoMap[id] = mi
	}
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
	var ids []string
	for id := range p.ModelInfoMap {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		di := p.DisplayNameMap[ids[i]]
		if di == "" {
			di = ids[i]
		}
		dj := p.DisplayNameMap[ids[j]]
		if dj == "" {
			dj = ids[j]
		}
		di = strings.ToLower(di)
		dj = strings.ToLower(dj)
		if di != dj {
			return di < dj
		}
		return ids[i] < ids[j]
	})
	return ids
}

func (p *Proxy) GetEffectiveModels() []string {
	catalogIds := p.GetOrderedModelIds()
	var all []string
	if len(catalogIds) > 0 {
		all = catalogIds
	} else {
		all = p.Config.EnabledModels
	}
	if len(p.DisabledModels) == 0 {
		return all
	}
	disabledSet := map[string]bool{}
	for _, d := range p.DisabledModels {
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

	client := &http.Client{
		Timeout: 15 * time.Second,
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

	resp, err := client.Do(req)
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
	// Fast path: check for valid cached data under read lock
	p.mu.RLock()
	if p.CatalogCache != nil && time.Since(p.CatalogCache.fetchedAt) < ModelCatalogCacheTTL {
		data := p.CatalogCache.data
		p.mu.RUnlock()
		return data, nil
	}
	fetching := p.CatalogFetching
	var doneCh chan struct{}
	if fetching {
		doneCh = p.CatalogFetchDone
	}
	p.mu.RUnlock()

	// If a fetch is already in progress, wait for it
	if fetching && doneCh != nil {
		<-doneCh // block until fetch completes
		p.mu.RLock()
		if p.CatalogCache != nil {
			data, err := p.CatalogCache.data, p.CatalogCache.fetchErr
			p.mu.RUnlock()
			return data, err
		}
		p.mu.RUnlock()
		return nil, fmt.Errorf("catalog fetch returned no data")
	}

	// We are the fetcher — acquire write lock and set up singleflight
	p.mu.Lock()
	// Double-check after acquiring write lock (another goroutine may have fetched)
	if p.CatalogCache != nil && time.Since(p.CatalogCache.fetchedAt) < ModelCatalogCacheTTL {
		data := p.CatalogCache.data
		p.mu.Unlock()
		return data, nil
	}
	// Check again if someone else started fetching
	if p.CatalogFetching {
		doneCh = p.CatalogFetchDone
		p.mu.Unlock()
		<-doneCh
		p.mu.RLock()
		if p.CatalogCache != nil {
			data, err := p.CatalogCache.data, p.CatalogCache.fetchErr
			p.mu.RUnlock()
			return data, err
		}
		p.mu.RUnlock()
		return nil, fmt.Errorf("catalog fetch returned no data")
	}
	// Mark ourselves as the fetcher
	p.CatalogFetching = true
	p.CatalogFetchDone = make(chan struct{})
	// Save stale cache reference for fallback
	var staleCache *catalogCache
	if p.CatalogCache != nil {
		stale := *p.CatalogCache
		staleCache = &stale
	}
	p.mu.Unlock()

	// Perform the fetch (outside the lock)
	data, err := p.FetchModelCatalog()

	p.mu.Lock()
	// Store result (or error) in cache
	if err != nil {
		// On failure, fall back to stale cache if available
		if staleCache != nil {
			p.CatalogCache = staleCache
		} else {
			p.CatalogCache = &catalogCache{
				data:      nil,
				fetchedAt: time.Now(),
				fetchErr:  err,
			}
		}
	} else {
		p.CatalogCache = &catalogCache{
			data:      data,
			fetchedAt: time.Now(),
			fetchErr:  nil,
		}
	}
	// Signal all waiters
	p.CatalogFetching = false
	close(p.CatalogFetchDone)
	p.CatalogFetchDone = nil
	result := p.CatalogCache
	p.mu.Unlock()

	// Populate ModelInfoMap and DisplayNameMap OUTSIDE the lock to avoid deadlock
	if err == nil && data != nil {
		p.ApplyCatalogData(data)
	}

	if result != nil {
		return result.data, result.fetchErr
	}
	return nil, err
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

	client := &http.Client{
		Timeout: UserInfoCacheTimeout,
	}

	url := strings.TrimRight(baseURL, "/") + "/models/info"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ValidateApiKey: create request: %v\n", err)
		return false
	}
	req.Header.Set("Connection", "keep-alive")

	if p.Config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.Config.APIKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ValidateApiKey: request failed: %v\n", err)
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
func (p *Proxy) analyzeImageViaHandoff(dataURI, handoffModel string) string {
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

	resp, err := p.Upstream.ChatCompletions(bodyBytes, false)
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
	return md5Hash(userText)[:12]
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

// GetRetryDelay computes the escalating retry delay per SPEC §15.1.
// Formula: RETRY_DELAY_MS + (3000 * (attempt - 1)) milliseconds.
// Attempt 1→2: 3s, 2→3: 6s, 3→4: 9s, ...
func GetRetryDelay(attempt int) time.Duration {
	return time.Duration(RetryDelayMs+3000*(attempt-1)) * time.Millisecond
}

// RetryLoop executes fn up to MaxRetries times with escalating delays per SPEC §15.1.
// The callback receives {attempt (1-based), isLast} and returns {retry, err}.
//   - If err is non-nil, the loop aborts immediately and returns (false, err).
//   - If retry is false, the loop returns (true, nil) — success.
//   - If retry is true and attempt < MaxRetries, it sleeps GetRetryDelay(attempt)
//     before the next attempt.
//   - If retry is true and attempt == MaxRetries (isLast), the loop returns
//     (true, nil).
//
// This mirrors the test-local testRetryLoop exactly, but without the test-only
// testRetryDelay override — production always uses real delays.
func RetryLoop(fn func(attempt int, isLast bool) (bool, error)) (bool, error) {
	for attempt := 1; attempt <= MaxRetries; attempt++ {
		retry, err := fn(attempt, attempt == MaxRetries)
		if err != nil {
			return false, err
		}
		if !retry {
			return true, nil
		}
		if attempt < MaxRetries {
			time.Sleep(GetRetryDelay(attempt))
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
			Body:    RedactBodyJson(reqBody),
		},
		Upstream: ErrorLogUpstream{
			URL:        upstreamURL,
			Method:     upstreamMethod,
			Headers:    RedactHeaders(upstreamHeaders),
			Status:     upstreamStatus,
			StatusText: upstreamStatusText,
			Body:       RedactBodyJson(upstreamBody),
		},
	}
	return p.LogHttpError(record)
}

// ─── HTTP Helpers (§17) ────────────────────────────────────────────────────

func (p *Proxy) Authorized(req *http.Request) bool {
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
	var buf []byte
	chunk := make([]byte, 4096)
	total := 0
	for {
		n, err := req.Body.Read(chunk)
		if n > 0 {
			total += n
			if total > MaxBodySize {
				return "", fmt.Errorf("request body too large")
			}
			buf = append(buf, chunk[:n]...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", err
		}
	}
	return string(buf), nil
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
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			return
		}
		w.Write([]byte("data: "))
		w.Write(b)
		w.Write([]byte("\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
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
	var buf []byte
	chunk := make([]byte, 4096)
	total := 0
	for {
		n, err := body.Read(chunk)
		if n > 0 {
			total += n
			if total > MaxBodySize {
				return "", fmt.Errorf("upstream response body exceeds MaxBodySize (%d bytes)", MaxBodySize)
			}
			buf = append(buf, chunk[:n]...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", fmt.Errorf("readBodyText: reading upstream body: %w", err)
		}
	}
	return string(buf), nil
}

func pipeBodyToResponse(body io.ReadCloser, w http.ResponseWriter, r *http.Request) error {
	if body == nil {
		return fmt.Errorf("pipeBodyToResponse: body is nil")
	}
	defer body.Close()

	ctx := r.Context()
	flusher, _ := w.(http.Flusher)

	chunk := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("pipeBodyToResponse: client disconnected: %w", ctx.Err())
		default:
		}

		n, err := body.Read(chunk)
		if n > 0 {
			if _, werr := w.Write(chunk[:n]); werr != nil {
				return fmt.Errorf("pipeBodyToResponse: write to client failed: %w", werr)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("pipeBodyToResponse: read from upstream: %w", err)
		}
	}
}

func writeSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// FlushVisionHandoffKeepalive sends SSE headers (if not already sent) and a
// keepalive comment before vision handoff analysis begins (SPEC §11.6).
// Returns true if SSE headers were flushed by this call.
func (p *Proxy) FlushVisionHandoffKeepalive(w http.ResponseWriter) bool {
	if w.Header().Get("Content-Type") == "text/event-stream" {
		w.Write([]byte(": keepalive — analyzing image via vision handoff\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return false
	}
	writeSSEHeaders(w)
	w.Write([]byte(": keepalive — analyzing image via vision handoff\n\n"))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
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

// WriteQueueFullError writes a 503 queue_full error in the appropriate format.
func WriteQueueFullError(w http.ResponseWriter, format string) {
	if format == "anthropic" {
		WriteAnthropicError(w, http.StatusServiceUnavailable,
			"The server is overloaded. Please retry later.", "overloaded_error")
	} else {
		WriteOpenAIError(w, http.StatusServiceUnavailable,
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

	p.configMu.RLock()
	defer p.configMu.RUnlock()

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
	if p.KeyPool != nil {
		slot, _ = p.KeyPool.Acquire(preferredIndex)
		if slot == nil {
			if writeError != nil {
				writeError(w, http.StatusServiceUnavailable,
					"no healthy API keys available", "api_error", "")
			}
			return
		}
		currentKeyIndex = slot.Index
	}

	// Set the acquired key on the upstream client
	if slot != nil && p.Upstream != nil {
		p.Upstream.SetAPIKey(slot.Key)
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
		LimitImagesInMessages(payload, p.Config.MaxImages)
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
		p.PerformVisionHandoff(payload, resolvedModel)
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

	_, retryErr := RetryLoop(func(attempt int, isLast bool) (bool, error) {
		// ── Key rotation on retry (§15.3) ──
		if attempt > 1 && p.KeyPool != nil && p.KeyPool.Total() > 1 {
			p.KeyPool.MarkUnhealthy(currentKeyIndex, lastStatus)
			newSlot, ok := p.KeyPool.Acquire(-1)
			if !ok {
				return false, fmt.Errorf("no healthy keys available for retry")
			}
			currentKeyIndex = newSlot.Index
			slotName = newSlot.Name
			if p.Upstream != nil {
				p.Upstream.SetAPIKey(newSlot.Key)
			}
		}

		// ── Execute upstream request (§19.12b) ──
		if p.Upstream == nil {
			if writeError != nil {
				writeError(w, http.StatusServiceUnavailable,
					"no upstream client configured", "api_error", "")
			}
			return false, nil
		}

		resp, err := p.Upstream.ChatCompletions(bodyBytes, isStream)

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
				p.Config.UpstreamBaseURL+"/chat/completions", "POST",
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
				p.Config.UpstreamBaseURL+"/chat/completions", "POST",
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
		if p.KeyPool != nil {
			p.KeyPool.MarkHealthy(currentKeyIndex)
		}

		contentType := resp.Header.Get("Content-Type")

		if strings.Contains(contentType, "text/event-stream") {
			// ── SSE streaming response ──
			if w.Header().Get("Content-Type") != "text/event-stream" {
				writeSSEHeaders(w)
			}
			pipeBodyToResponse(resp.Body, w, r)
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
				if w.Header().Get("Content-Type") != "text/event-stream" {
					writeSSEHeaders(w)
				}
				w.Write([]byte("data: "))
				w.Write([]byte(respBody))
				w.Write([]byte("\n\n"))
				w.Write([]byte("data: [DONE]\n\n"))
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
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
		if w.Header().Get("Content-Type") != "text/event-stream" {
			writeError(w, http.StatusServiceUnavailable,
				retryErr.Error(), "api_error", "")
		}
	}
}

// proxyAnthropicRequest handles an Anthropic-format messages request (§20).
func (p *Proxy) proxyAnthropicRequest(item QueueItem) {
	p.configMu.RLock()
	defer p.configMu.RUnlock()

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
	if p.KeyPool != nil {
		slot, ok = p.KeyPool.Acquire(preferredIndex)
		if !ok {
			WriteAnthropicError(w, http.StatusServiceUnavailable,
				"no available API keys", "overloaded_error")
			return
		}
	}

	// STEP 2: Set upstream API key
	if slot != nil && p.Upstream != nil {
		p.Upstream.SetAPIKey(slot.Key)
	}

	// STEP 3: Session tracking (§20.2, §14.5)
	var sessNum int64
	if fp != "" {
		existing := p.TouchConversation(fp)
		if existing != nil {
			existing.RequestCount++
			existing.TokenIndex = slot.Index
			p.TrackConversationSession(fp, existing, true)
			sessNum = existing.SessNum
		} else {
			p.convMu.Lock()
			p.globalSessionCounter++
			sessNum = p.globalSessionCounter
			p.convMu.Unlock()
			newSess := &ConversationSession{
				TokenIndex:   slot.Index,
				RequestCount: 1,
				SessNum:      sessNum,
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
	LimitImagesInMessages(payload, p.Config.MaxImages)

	// STEP 5b: Model resolution (same as OpenAI path)
	resolvedModel := p.ResolveModelId(model)
	payload["model"] = resolvedModel

	// STEP 6: Vision handoff (§20.5, §11)
	if p.NeedsVisionHandoff(resolvedModel) {
		if isStream {
			p.FlushVisionHandoffKeepalive(w)
		}
		p.PerformVisionHandoff(payload, resolvedModel)
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

	// STEP 8: Upstream call (§20.6)
	if p.Upstream == nil {
		WriteAnthropicError(w, http.StatusServiceUnavailable,
			"upstream not configured", "api_error")
		return
	}
	resp, err := p.Upstream.Messages(bodyBytes, isStream)
	if err != nil {
		// STEP 9: Network error → 502 (§20.9)
		errBody := fmt.Sprintf(`{"error":{"type":"api_error","message":"upstream network error: %v"}}`, err)
		WriteAnthropicPassthroughError(w, http.StatusBadGateway, errBody)
		if p.KeyPool != nil {
			p.KeyPool.MarkUnhealthy(slot.Index, http.StatusBadGateway)
		}
		return
	}
	// NOTE: no defer resp.Body.Close() here — the success path is handled by
	// pipeBodyToResponse (which has its own defer body.Close()), and the HTTP
	// error path closes resp.Body explicitly below.

	// STEP 10: HTTP error (§20.7)
	if resp.StatusCode >= 400 {
		errBodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, MaxBodySize))
		resp.Body.Close()
		errBody := string(errBodyBytes)

		WriteAnthropicPassthroughError(w, resp.StatusCode, errBody)

		// §20 line 913: Mark unhealthy ONLY on 503, NOT on 500
		if resp.StatusCode == http.StatusServiceUnavailable {
			if p.KeyPool != nil {
				p.KeyPool.MarkUnhealthy(slot.Index, http.StatusServiceUnavailable)
			}
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			p.BumpThrottled()
		}

		return
	}

	// STEP 11: Success (§20.8)
	if p.KeyPool != nil {
		p.KeyPool.MarkHealthy(slot.Index)
	}

	// Set headers (§20 line 916)
	ct := resp.Header.Get("Content-Type")
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(resp.StatusCode)

	// Pipe body to response (handles both streaming and non-streaming)
	_ = pipeBodyToResponse(resp.Body, w, r)
}

// dispatchQueuedItem sends a queued item to the appropriate pipeline function.
// proxyChatRequest/proxyAnthropicRequest are full implementations handling the
// complete request lifecycle (key acquisition, normalization, retries, etc.).
// The defer OnRequestComplete() ensures activeRequests is decremented regardless.
func (p *Proxy) dispatchQueuedItem(item QueueItem) {
	defer func() {
		p.OnRequestComplete()
	}()

	if item.Format == "anthropic" {
		p.proxyAnthropicRequest(item)
	} else {
		p.proxyChatRequest(item.Response, item.Req, item.Payload, item.Model,
			item.WriteError, item.WritePassthroughError)
	}
}

// gateLimit returns the effective concurrency gate (hard_cap ?? limit).
// Returns -1 if no gate applies (both hard_cap and limit are nil).
// Caller must NOT hold queueMu — this method does not acquire it.
func (p *Proxy) gateLimit() int {
	eff := p.GetEffectiveConcurrency(p.LastConcurrency)
	if eff.HardCap != nil {
		return *eff.HardCap
	}
	if eff.Limit != nil {
		return *eff.Limit
	}
	return -1
}

// TryEnqueueOrDispatch attempts to dispatch a request immediately if under the
// concurrency gate, or enqueues it if capacity exists.
//
// Contract:
//   - Returns true if the caller should proceed with direct dispatch.
//     The caller MUST increment ActiveRequests before dispatching and MUST
//     call OnRequestComplete() when done (which decrements and processes the queue).
//   - Returns false if the request was enqueued or rejected (503).
//     The caller must NOT increment ActiveRequests in this case; ProcessQueue
//     handles incrementing when it dequeues and dispatches.
func (p *Proxy) TryEnqueueOrDispatch(item *QueueItem) bool {
	p.queueMu.Lock()
	defer p.queueMu.Unlock()

	gate := p.gateLimit()

	// No gate → dispatch immediately
	if gate < 0 {
		return true
	}

	// Under gate → dispatch immediately
	if p.ActiveRequests < gate {
		return true
	}

	// At or over gate → try to enqueue
	if len(p.requestQueue) >= MaxQueueSize {
		// Queue full → reject with 503
		p.ThrottledCount++
		if item != nil && item.WriteError != nil && item.Response != nil {
			WriteQueueFullError(item.Response, item.Format)
		}
		return false
	}

	// Enqueue
	p.requestQueue = append(p.requestQueue, *item)
	p.QueueLen = len(p.requestQueue)
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

// OnRequestComplete decrements the active request count and processes the queue.
func (p *Proxy) OnRequestComplete() {
	p.queueMu.Lock()
	if p.ActiveRequests > 0 {
		p.ActiveRequests--
	}
	p.queueMu.Unlock()
	p.ProcessQueue()
}

// ProcessQueue dispatches queued requests while there is capacity.
func (p *Proxy) ProcessQueue() int {
	p.queueMu.Lock()

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
		p.ActiveRequests++
		dispatched++

		go p.dispatchQueuedItem(item)
	}
	p.queueMu.Unlock()
	return dispatched
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

	// Fetch from upstream
	raw, err := p.Upstream.GetUsage()
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
		Window             WindowInfo    `json:"window"`
		Plan               PlanInfo      `json:"plan"`
		ConcurrentSessions int           `json:"concurrent_sessions"`
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
		ConcurrentSessions: fullResp.ConcurrentSessions,
		Limits:             fullResp.Limits,
		UserID:             fullResp.UserID,
	}

	// §6.1: Throttle reset on window change
	newWindow := data.Window.StartedAt

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
	p.mu.Unlock()
}

// GetEffectiveConcurrency computes the effective concurrency limits.
// SPEC §6.3: If overrideConcurrency > 0, hard_cap = min(override, apiHardCap)
// (or just override if apiHardCap is nil), overridden = true.
// Otherwise uses API values directly, overridden = false.
// Result is cached in effectiveConcurrencyCache (invalidated by FetchConcurrency).
//
// IMPORTANT: Cache is only used in production (p.Upstream != nil). In test mode
// (p.Upstream == nil), test helpers like setLastConcurrency() modify
// p.LastConcurrency directly without invalidating the cache, so we must
// always recompute to avoid returning stale cached results.
func (p *Proxy) GetEffectiveConcurrency(lastConcurrency ConcurrencyData) ConcurrencyData {
	// Check cache first (§6.3) — only in production mode
	if p.Upstream != nil {
		p.mu.RLock()
		if p.effectiveConcurrencyCache != nil {
			cached := *p.effectiveConcurrencyCache
			p.mu.RUnlock()
			return cached
		}
		p.mu.RUnlock()
	}

	// Compute the result (existing logic)
	result := lastConcurrency
	if p.Config.OverrideConcurrency > 0 {
		result.Overridden = true
		if lastConcurrency.HardCap != nil {
			min := p.Config.OverrideConcurrency
			if *lastConcurrency.HardCap < min {
				min = *lastConcurrency.HardCap
			}
			hc := min
			result.HardCap = &hc
		} else {
			hc := p.Config.OverrideConcurrency
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

// ─── Opencode Config (§24) ─────────────────────────────────────────────────

func DiscoverOpencodeConfigs(homeDir string) []string {
	opencodeDiscoveryMu.Lock()
	defer opencodeDiscoveryMu.Unlock()
	if opencodeDiscoveryCache != nil && opencodeDiscoveryCacheHome == homeDir && time.Since(opencodeDiscoveryCachedAt) < OpencodeConfigCacheTTL {
		return opencodeDiscoveryCache
	}

	var result []string
	seen := map[string]bool{}
	addIfExists := func(p string) {
		if _, err := os.Stat(p); err == nil {
			if !seen[p] {
				result = append(result, p)
				seen[p] = true
			}
		}
	}

	if runtime.GOOS == "windows" {
		// Scan C:\Users\* for opencode configs
		usersDir := `C:\Users`
		entries, err := os.ReadDir(usersDir)
		if err == nil {
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				userPath := filepath.Join(usersDir, entry.Name())
				addIfExists(filepath.Join(userPath, ".opencode", "opencode.json"))
				addIfExists(filepath.Join(userPath, ".config", "opencode", "opencode.json"))
			}
		}
		// Also check systemprofile
		addIfExists(filepath.Join(`C:\Windows\System32\config\systemprofile`, ".opencode", "opencode.json"))
		addIfExists(filepath.Join(`C:\Windows\System32\config\systemprofile`, ".config", "opencode", "opencode.json"))
	} else {
		addIfExists(filepath.Join(homeDir, ".config", "opencode", "opencode.json"))
		addIfExists(filepath.Join(homeDir, ".opencode", "opencode.json"))
	}

	opencodeDiscoveryCache = result
	opencodeDiscoveryCacheHome = homeDir
	opencodeDiscoveryCachedAt = time.Now()
	return result
}

func (p *Proxy) SetupOpencodeConfig(homeDir string, port int) bool {
	paths := DiscoverOpencodeConfigs(homeDir)
	firstRun := false
	for _, configFile := range paths {
		existing := map[string]interface{}{}
		fileExisted := false
		if content, err := os.ReadFile(configFile); err == nil {
			if err := json.Unmarshal(content, &existing); err != nil {
				log.Printf("opencode config %s: invalid JSON, starting fresh: %v", configFile, err)
				existing = map[string]interface{}{}
			}
			fileExisted = true
		}
		if fileExisted {
			backupPath := filepath.Join(filepath.Dir(configFile), "openconfig.b4umans.json")
			if _, err := os.Stat(backupPath); os.IsNotExist(err) {
				content, _ := os.ReadFile(configFile)
				os.WriteFile(backupPath, content, 0644)
				firstRun = true
			}
		} else {
			continue
		}
		models := map[string]interface{}{}
		modelsDevCatalog := getModelsDevCatalog()
		for _, m := range p.GetEffectiveModels() {
			info := p.ModelInfoMap[m]
			dn := p.DisplayNameMap[m]
			var devEntry *ModelsDevEntry
			if modelsDevCatalog != nil {
				devEntry = findModelsDevEntry(modelsDevCatalog, m)
			}
			if dn == "" {
				dn = strings.TrimPrefix(m, "umans-")
			}
			entry := map[string]interface{}{
				"id":          m,
				"name":        dn,
				"reasoning":   *resolveReasoningMode(devEntry, info.Capabilities.Reasoning),
				"temperature": true,
			}
			if cw, ok := toInt(info.Capabilities.ContextWindow); ok && cw > 0 {
				outputLimit := cw
				if rmt, ok := toInt(info.Capabilities.RecommendedMaxTokens); ok && rmt > 0 {
					outputLimit = rmt
				} else if mct, ok := toInt(info.Capabilities.MaxCompletionTokens); ok && mct > 0 {
					outputLimit = mct
				}
				entry["limit"] = map[string]interface{}{
					"context": cw,
					"output":  outputLimit,
				}
			}
			if st, ok := info.Capabilities.SupportsTools.(bool); ok {
				entry["tool_call"] = st
			}
			if sv, ok := info.Capabilities.SupportsVision.(bool); ok {
				entry["attachment"] = sv
			}
			inputMods := []string{"text"}
			if sv, ok := info.Capabilities.SupportsVision.(bool); ok && sv {
				inputMods = append(inputMods, "image")
			}
			entry["modalities"] = map[string]interface{}{
				"input":  inputMods,
				"output": []string{"text"},
			}
			if variants := BuildReasoningVariants(info.Capabilities.Reasoning); variants != nil {
				entry["variants"] = variants
			}
			models[m] = entry
		}
		provider, ok := existing["provider"].(map[string]interface{})
		if !ok {
			provider = map[string]interface{}{}
		}
		provider["umans"] = map[string]interface{}{
			"npm":  "@ai-sdk/openai-compatible",
			"name": "Umans.AI-Dash",
			"options": map[string]interface{}{
				"baseURL": fmt.Sprintf("http://localhost:%d/v1", port),
				"apiKey":  firstProxyAPIKey(p.Config),
			},
			"models": models,
		}
		existing["provider"] = provider
		instructions, ok := existing["instructions"].([]interface{})
		if !ok {
			instructions = []interface{}{}
		}
		for _, g := range []string{"AGENTS.md", "skills.md"} {
			found := false
			for _, inst := range instructions {
				if inst == g {
					found = true
					break
				}
			}
			if !found {
				instructions = append(instructions, g)
			}
		}
		existing["instructions"] = instructions
		b, err := json.MarshalIndent(existing, "", "  ")
		if err != nil {
			log.Printf("opencode config %s: failed to marshal: %v", configFile, err)
			continue
		}
		os.WriteFile(configFile, b, 0644)
		log.Printf("opencode config written: %s (%d models)", configFile, len(models))
	}
	return firstRun
}

func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

func firstProxyAPIKey(cfg *Config) string {
	if cfg == nil {
		return "umans-proxy"
	}
	if cfg.APIKey != "" {
		return cfg.APIKey
	}
	if len(cfg.APIKeys) > 0 && cfg.APIKeys[0] != "" {
		return cfg.APIKeys[0]
	}
	for _, k := range cfg.Keys {
		if k.Key != "" {
			return k.Key
		}
	}
	return "umans-proxy"
}

// ─── HTTP Handlers (§17-§29) ───────────────────────────────────────────────

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

	// api_key_valid — calls ValidateApiKey() which implements the
	// 5-min cache refresh per spec. Also refreshes user info as a side effect.
	apiKeyValid := p.ValidateApiKey()

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
	var data []map[string]interface{}
	for _, m := range models {
		info := p.ModelInfoMap[m]
		entry := map[string]interface{}{
			"id":           m,
			"object":       "model",
			"created":      p.StartedAt.Unix(),
			"owned_by":     "umans",
			"root":         m,
			"permission":   []interface{}{},
			"display_name": p.DisplayNameMap[m],
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
	WriteJSON(w, http.StatusOK, map[string]interface{}{
		"models":         models,
		"disabledModels": p.DisabledModels,
		"displayNames":   p.DisplayNameMap,
	})
}

// HandleModelsInfo returns the raw model catalog (ModelInfoMap). §17.1 Route #17.
func (p *Proxy) HandleModelsInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}
	WriteJSON(w, http.StatusOK, p.ModelInfoMap)
}

// HandleChatCompletions is the entry point for /v1/chat/completions (§19).
// It performs auth, body reading, JSON parsing, model extraction, and
// dispatches through the concurrency queue to proxyChatRequest.
func (p *Proxy) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
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
		// Dispatched immediately — caller must manage ActiveRequests lifecycle
		p.queueMu.Lock()
		p.ActiveRequests++
		p.queueMu.Unlock()

		defer func() {
			p.OnRequestComplete()
		}()

		p.proxyChatRequest(w, r, payload, model,
			OpenAIErrorWriter(), OpenAIPassthroughErrorWriter())
	}
	// If TryEnqueueOrDispatch returned false, the request was either enqueued
	// (will be dispatched later by ProcessQueue → dispatchQueuedItem, which
	// handles ActiveRequests increment/decrement) or rejected with a 503
	// (WriteQueueFullError already called inside TryEnqueueOrDispatch).
}

// HandleMessages is the Anthropic Messages pipeline (§20).
func (p *Proxy) HandleMessages(w http.ResponseWriter, r *http.Request) {
	// §35.1: recover from panics
	defer func() {
		if rv := recover(); rv != nil {
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
	if !dispatched {
		return
	}

	// Direct dispatch: increment active requests, process, decrement on completion
	p.queueMu.Lock()
	p.ActiveRequests++
	p.queueMu.Unlock()
	defer p.OnRequestComplete()

	p.proxyAnthropicRequest(item)
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
	debouncedSetupOpencodeConfig(p)
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
		debouncedSetupOpencodeConfig(p)
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
		debouncedSetupOpencodeConfig(p)
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
		debouncedSetupOpencodeConfig(p)
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
	if p.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.httpServer.Shutdown(ctx) // graceful drain; ignore error (we're exiting anyway)
	}
	p.flushErrorLog() // §35.2: flush logs before exit
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
// 1. Poll ActiveRequests (proxy-level) until it reaches 0 or a 5-second
//    timeout elapses — drains in-flight proxied requests before closing
//    the HTTP listener.
// 2. Close the HTTP server with a 5-second drain timeout.
// 3. Flush and close the error log file.
// 4. Call exitFn(0) (or os.Exit(0) if exitFn is nil).
func (p *Proxy) Shutdown() {
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
	w.Header().Set("Content-Type", "image/jpeg")
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

	// Fetch from wallhaven.cc API
	wallhavenURL := "https://wallhaven.cc/api/v1/search?categories=100&purity=100&topRange=1M&sorting=toplist&order=desc&page=3"
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
	if imageData == nil || !isJPEG(imageData) {
		fallback()
		return
	}

	// Save to cache.
	saveCacheFile(cacheFile, imageData)

	// Serve the fresh image.
	serveWallpaperImage(w, imageData, nil)
}

// PerformVisionHandoff replaces images with text descriptions from the handoff
// model (SPEC §11.5). It accepts optional pre-collected image parts via variadic
// to avoid re-scanning the payload when parts have already been collected.
func (p *Proxy) PerformVisionHandoff(payload map[string]interface{}, resolvedModel string, preCollectedParts ...[]ImagePart) int {
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

	descriptions := make([]string, len(parts))
	var wg sync.WaitGroup
	for i := range parts {
		wg.Add(1)
		go func(idx int, dataURI string) {
			defer wg.Done()
			if p.Upstream == nil {
				descriptions[idx] = p.HandoffResponse
				return
			}
			descriptions[idx] = p.analyzeImageViaHandoff(dataURI, handoffModel)
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

// getDashboardHtml reads dashboard.html with mtime-based caching.
// It looks in the binary's directory first (spec-compliant), then falls
// back to CWD (test-compatible). Returns nil on read error.
func (p *Proxy) getDashboardHtml() []byte {
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "dashboard.html"))
	}
	candidates = append(candidates, "dashboard.html") // CWD fallback
	candidates = append(candidates, filepath.Join("..", "dashboard.html")) // parent of CWD (test-compatible)

	for _, path := range candidates {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}

		p.dashMu.Lock()
		if p.dashHtml != nil && p.dashMtime.Equal(info.ModTime()) {
			cached := p.dashHtml
			p.dashMu.Unlock()
			return cached
		}

		data, err := os.ReadFile(path)
		if err != nil {
			p.dashHtml = nil
			p.dashMu.Unlock()
			continue // try next candidate
		}
		p.dashHtml = data
		p.dashMtime = info.ModTime()
		p.dashMu.Unlock()
		return data
	}
	return nil
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

	tw := &responseWriterTracker{ResponseWriter: w}

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
		tw.mu.Lock()
		alreadyWritten := tw.written
		if !alreadyWritten {
			tw.hijacked = true
		}
		tw.mu.Unlock()

		if !alreadyWritten {
			WriteOpenAIError(tw, http.StatusGatewayTimeout,
				"request timeout", "server_error", "timeout")
		} else if tw.Header().Get("Content-Type") == "text/event-stream" {
			errEv := SSEEvent{
				Event: "error",
				Data:  `{"error":"request timeout"}`,
			}
			tw.Write([]byte(errEv.Format()))
			tw.Flush()
		}

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
		html := p.getDashboardHtml()
		if html == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		// Determine wallpaper style and build injection CSS
		var inject string
		switch p.Config.WallpaperSource {
		case "bing", "wallhaven":
			wpPath := ".cache/wallpaper.jpg"
			if p.Config.WallpaperSource == "wallhaven" {
				wpPath = ".cache/wallpaper-haven.jpg"
			}
			if data, err := os.ReadFile(wpPath); err == nil {
				b64 := base64.StdEncoding.EncodeToString(data)
				inject = fmt.Sprintf(`<style>body{background-image:url('data:image/jpeg;base64,%s')}</style>`, b64)
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
