// Package proxy provides a local HTTP reverse proxy for the UMANS AI API.
//
// This file contains type definitions shared across the proxy implementation.
// During development, these types serve as the contract for the test suite.
package proxy

import (
	"bufio"
	"container/list"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// KeyConfig represents a single API key in the key pool.
type KeyConfig struct {
	Name    string `json:"name"`
	Key     string `json:"key"`
	Session string `json:"session"`
}

// KeyState is the public representation of a key's current health status.
type KeyState struct {
	Name              string `json:"name"`
	Status            string `json:"status"`
	Healthy           bool   `json:"healthy"`
	RemainingCooldown int64  `json:"remainingCooldown"`
	Token             string `json:"token"`
}

// Config holds all runtime configuration for the proxy.
type Config struct {
	ListenAddr           string            `json:"LISTEN_ADDR"`
	UpstreamBaseURL      string            `json:"UPSTREAM_BASE_URL"`
	RequestTimeout       Duration          `json:"REQUEST_TIMEOUT"`
	APIKey               string            `json:"API_KEY"`
	APIKeys              []string          `json:"API_KEYS"`
	Keys                 []KeyConfig       `json:"KEYS"`
	EnabledModels        []string          `json:"ENABLED_MODELS"`
	ModelDisplayNames    map[string]string `json:"MODEL_DISPLAY_NAMES"`
	OverrideConcurrency     int               `json:"OVERRIDE_CONCURRENCY"`
	MaxImages               int               `json:"MAX_IMAGES"`
	DisabledModels          []string          `json:"DISABLED_MODELS"`
	VisionHandoffEnabled    bool              `json:"VISION_HANDOFF_ENABLED"`
	VisionHandoffModel      string            `json:"VISION_HANDOFF_MODEL"`
	VisionHandoffPrompt     string            `json:"VISION_HANDOFF_PROMPT"`
	VisionHandoffCacheEnabled bool           `json:"VISION_HANDOFF_CACHE_ENABLED"`
	VisionHandoffCacheTtl   Duration          `json:"VISION_HANDOFF_CACHE_TTL"`
	WallpaperSource         string            `json:"wallpaperSource"`
	ApiKeyMode              string            `json:"API_KEY_MODE"` // "smart" (default), "managed", or "passthrough"
	// ConcurrencyLimitMode controls which cap gates concurrency:
	//   "soft"   → gate at Limit (soft cap)        [default, same as old BurstMode=false]
	//   "hard"   → gate at HardCap (hard cap)       [same as old BurstMode=true]
	//   "manual" → gate at ManualConcurrencyLimit   [user-specified via slider]
	ConcurrencyLimitMode  string `json:"CONCURRENCY_LIMIT_MODE"`
	// ManualConcurrencyLimit is the user-chosen gate value when mode == "manual".
	// Default 0 = uninitialized; gateLimit() falls back to soft cap.
	ManualConcurrencyLimit int `json:"MANUAL_CONCURRENCY_LIMIT"`
	// BurstMode is DEPRECATED — kept for config migration + rollback safety.
	// Synced in HandleConfigPost: true when mode=="hard", false otherwise.
	BurstMode bool `json:"BURST_MODE"`
	// SlotFreeDelay is the delay (in seconds) before freeing a concurrency slot
	// after a request completes. 0 = immediate (default). Useful to avoid
	// hitting upstream rate limits when requests complete in rapid succession.
	SlotFreeDelay int `json:"SLOT_FREE_DELAY"`
}

// Duration is a wrapper around time.Duration that can be JSON-serialized.
type Duration struct {
	time.Duration
}

// DurationMs returns the duration in milliseconds.
func (d Duration) DurationMs() int64 { return d.Duration.Milliseconds() }

func (d Duration) MarshalJSON() ([]byte, error) {
	if d.Duration == 0 {
		return json.Marshal("0s")
	}
	hours := int64(d.Duration / time.Hour)
	if hours > 0 && d.Duration%time.Hour == 0 {
		return json.Marshal(fmt.Sprintf("%dh", hours))
	}
	minutes := int64(d.Duration / time.Minute)
	if minutes > 0 && d.Duration%time.Minute == 0 {
		return json.Marshal(fmt.Sprintf("%dm", minutes))
	}
	seconds := int64(d.Duration / time.Second)
	if seconds > 0 && d.Duration%time.Second == 0 {
		return json.Marshal(fmt.Sprintf("%ds", seconds))
	}
	return json.Marshal(d.Duration.String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" || s == "0s" || s == "0" {
		*d = Duration{}
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err == nil {
		*d = Duration{parsed}
		return nil
	}
	*d = ParseDuration(s)
	return nil
}

// HandoffCacheStats holds statistics about the image handoff cache.
type HandoffCacheStats struct {
	Size      int   `json:"size"`
	MaxSize   int   `json:"maxSize"`
	TtlMs     int64 `json:"ttlMs"`
	Hits      int64 `json:"hits"`
	Misses    int64 `json:"misses"`
	Evictions int64 `json:"evictions"`
}

// ModelInfo holds metadata about a single model from the upstream catalog.
type ModelInfo struct {
	DisplayName  string       `json:"display_name"`
	Capabilities Capabilities `json:"capabilities"`
}

// Capabilities describes what a model supports.
type Capabilities struct {
	ContextWindow          interface{} `json:"context_window"`
	RecommendedMaxTokens   interface{} `json:"recommended_max_tokens"`
	MaxCompletionTokens    interface{} `json:"max_completion_tokens"`
	SupportsTools          interface{} `json:"supports_tools"`
	SupportsVision         interface{} `json:"supports_vision"`
	Reasoning              interface{} `json:"reasoning"`
}

// Pricing holds per-million-token pricing for a model.
type Pricing struct {
	Input  float64
	Output float64
}

// UsageInfo holds the current usage window data.
type UsageInfo struct {
	RequestsInWindow   int           `json:"requests_in_window"`
	TokensIn           int           `json:"tokens_in"`
	TokensOut          int           `json:"tokens_out"`
	TokensCached       int           `json:"tokens_cached"`
	ConcurrentSessions int           `json:"concurrent_sessions"`
	Priority           *PriorityInfo `json:"priority"`
}

// PriorityInfo holds the upstream priority status.
type PriorityInfo struct {
	Low        bool    `json:"low"`
	BoxedUntil *string `json:"boxed_until"`
	Reason     *string `json:"reason"`
}

// WindowInfo holds the usage window metadata.
type WindowInfo struct {
	StartedAt string `json:"started_at"`
}

// PlanInfo holds the user's plan information.
type PlanInfo struct {
	DisplayName string `json:"display_name"`
}

// UsageData is the full usage response from upstream.
// The upstream /usage endpoint returns additional fields beyond usage/window/plan
// that are needed by fetchConcurrency() (§6.2).
type UsageData struct {
	Usage              UsageInfo     `json:"usage"`
	Window             *WindowInfo  `json:"window"`
	Plan               PlanInfo      `json:"plan"`
	ConcurrentSessions int           `json:"concurrent_sessions"` // §6.2
	Limits             *UsageLimits  `json:"limits"`              // §6.2
	UserID             string        `json:"user_id"`             // §6.2/A1
}

// UsageLimits holds concurrency limits from the upstream /usage response.
type UsageLimits struct {
	Concurrency *ConcurrencyLimits `json:"concurrency"`
}

// ConcurrencyLimits holds the concurrency limit and hard_cap from upstream.
type ConcurrencyLimits struct {
	Limit   *int `json:"limit"`
	HardCap *int `json:"hard_cap"`
}

// ConcurrencyData holds concurrency limits and current state.
type ConcurrencyData struct {
	Concurrent int
	Limit      *int
	HardCap    *int
	UserID     string
	Overridden bool
}

// SSEEvent represents a single Server-Sent Events event.
type SSEEvent struct {
	Event string
	ID    string
	Retry string
	Data  string
}

// Format serializes an SSEEvent to SSE wire format.
func (e SSEEvent) Format() string {
	var sb strings.Builder
	if e.Event != "" {
		sb.WriteString("event: ")
		sb.WriteString(e.Event)
		sb.WriteByte('\n')
	}
	if e.ID != "" {
		sb.WriteString("id: ")
		sb.WriteString(e.ID)
		sb.WriteByte('\n')
	}
	if e.Retry != "" {
		sb.WriteString("retry: ")
		sb.WriteString(e.Retry)
		sb.WriteByte('\n')
	}
	if e.Data != "" {
		for _, line := range strings.Split(e.Data, "\n") {
			sb.WriteString("data: ")
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
	}
	sb.WriteByte('\n')
	return sb.String()
}

// ParseSSEEvent parses raw SSE text into an SSEEvent.
func ParseSSEEvent(raw string) (SSEEvent, error) {
	var ev SSEEvent
	scanner := bufio.NewScanner(strings.NewReader(raw))
	var dataLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			// Event delimiter — skip (we parse a single event)
			continue
		}
		if strings.HasPrefix(line, ":") {
			// Comment line — skip
			continue
		}
		field, value, found := strings.Cut(line, ": ")
		if !found {
			// No ": " separator; field is the whole line, value is empty
			field = line
			value = ""
		}
		switch field {
		case "event":
			ev.Event = value
		case "id":
			ev.ID = value
		case "retry":
			ev.Retry = value
		case "data":
			dataLines = append(dataLines, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return ev, fmt.Errorf("ParseSSEEvent: scanning raw text: %w", err)
	}
	if len(dataLines) > 0 {
		ev.Data = strings.Join(dataLines, "\n")
	}
	return ev, nil
}

// ImagePart represents an image found in a request payload.
type ImagePart struct {
	Container interface{}
	Index     int
	DataURI   string
}

// ErrorWriter writes an error response in OpenAI or Anthropic format.
type ErrorWriter func(w http.ResponseWriter, status int, message, errType, code string)

// PassthroughErrorWriter writes a passthrough error response.
type PassthroughErrorWriter func(w http.ResponseWriter, status int, body string)

// QueueItem represents a queued request waiting for concurrency capacity.
type QueueItem struct {
	Response              http.ResponseWriter
	Payload               map[string]interface{}
	Model                 string
	WriteError            ErrorWriter
	WritePassthroughError PassthroughErrorWriter
	Format                string
	Req                   *http.Request
	Fingerprint           string // conversation fingerprint for session affinity
	PreferredKeyIndex     int    // -1 = no preference, ≥0 for specific key
	IsStream              bool   // whether this is a streaming request
}

// HttpErrorRecord captures the context for an HTTP error log entry per SPEC §15.5.
// Used by LogHttpError to write structured error logs.
// All header and body fields should be pre-redacted by the caller (using
// RedactHeaders and RedactBodyJson) before constructing the record.
type HttpErrorRecord struct {
	Timestamp time.Time `json:"timestamp"`
	ErrorType string    `json:"errorType"` // "upstream_http_error"
	Stage     string    `json:"stage"`     // "retryable_attempt" or "final_attempt"
	Attempt   int       `json:"attempt"`
	SessNum   int64     `json:"sessNum"`
	SlotName  string    `json:"slotName"`

	// Request details (from the incoming client request)
	RequestMethod  string              `json:"requestMethod"`
	RequestURL     string              `json:"requestUrl"`
	RequestHeaders map[string][]string `json:"requestHeaders"` // already redacted
	RequestBody    string              `json:"requestBody"`     // already redacted

	// Upstream details (from the proxied request to the upstream API)
	UpstreamURL        string              `json:"upstreamUrl"`
	UpstreamMethod     string              `json:"upstreamMethod"`
	UpstreamHeaders    map[string][]string `json:"upstreamHeaders"`    // already redacted
	UpstreamStatus     int                 `json:"upstreamStatus"`
	UpstreamStatusText string              `json:"upstreamStatusText"`
	UpstreamBody       string              `json:"upstreamBody"` // already redacted
}

// ConversationSession tracks a conversation's key affinity and request count.
type ConversationSession struct {
	TokenIndex   int
	RequestCount int
	SessNum      int64
}

// KeyPool is a round-robin multi-key pool with cooldown/unhealthy marking.
type KeyPool struct {
	entries []*keyEntry
	index   int
	mu      sync.Mutex
	config  *Config
}

type keyEntry struct {
	Key        string
	Name       string
	Healthy    bool
	LastError  time.Time
	CooldownMs int64
}

// KeySlot is the result of a successful key acquisition.
type KeySlot struct {
	Key   string
	Name  string
	Index int
}

// catalogCache holds cached model catalog data with a fetch timestamp.
type catalogCache struct {
	data      map[string]interface{}
	fetchedAt time.Time
	fetchErr  error
}

// usageHistoryCacheEntry holds cached usage history data with a fetch timestamp and cache key.
type usageHistoryCacheEntry struct {
	data      interface{}
	fetchedAt time.Time
	key       string
}

// ImageHandoffCache is an LRU cache for vision handoff image descriptions.
// Keyed by SHA-256 hash of the image data URI. 50 entries max, 24h TTL.
type ImageHandoffCache struct {
	maxSize   int
	ttl       time.Duration
	mu        sync.Mutex
	lru       *list.List
	lookup    map[string]*list.Element
	hits      int64
	misses    int64
	evictions int64
}

type handoffCacheEntry struct {
	hash string
	desc string
	time time.Time
}

// upstreamModelsCacheEntry holds cached upstream /models response with a fetch timestamp.
type upstreamModelsCacheEntry struct {
	data      []interface{}
	fetchedAt time.Time
	fetchErr  error
}

// userInfoCacheEntry holds cached user info with a fetch timestamp.
type userInfoCacheEntry struct {
	data      map[string]interface{}
	fetchedAt time.Time
}

// Proxy is the central state holder for the proxy.
// During implementation, all state (config, key pool, cache, model catalog, etc.)
// will live as fields on this struct.
type Proxy struct {
	Config          *Config
	Upstream        *UpstreamClient
	KeyPool         *KeyPool
	ImageHandoffCache *ImageHandoffCache
	StartedAt       time.Time
	Version         string
	ModelInfoMap    map[string]ModelInfo
	DisplayNameMap  map[string]string
	DisabledModels  []string
	UpstreamPricing   map[string]Pricing
	HandoffResponse  string

	// Runtime state for usage/concurrency (set by tests, populated by real impl)
	UsageData         *UsageData
	ThrottledCount    int
	ThrottledWindow   string
	ActiveRequests    int
	QueueLen          int
	// Concurrency queue (§5)
	queueMu      sync.RWMutex
	requestQueue []QueueItem

	// Catalog maps (§8) — protects ModelInfoMap and DisplayNameMap
	catalogMu sync.RWMutex
	LastConcurrency   ConcurrencyData
	// §6.1: usage cache (5-min TTL)
	usageCache            *UsageData
	usageCacheFetchedAt   time.Time
	// §6.1: usage history cache (5-min TTL)
	usageHistoryCache     *usageHistoryCacheEntry
	// §6.3: effective concurrency cache (invalidated by fetchConcurrency)
	effectiveConcurrencyCache *ConcurrencyData

	// Cache fields for §8 Model Catalog
	mu                  sync.RWMutex

	// Config read/write lock (§25)
	configMu            sync.RWMutex
	CatalogCache        *catalogCache
	CatalogFetching     bool
	CatalogFetchDone    chan struct{}
	UpstreamModelsCache *upstreamModelsCacheEntry
	UserInfoCache       *userInfoCacheEntry

	// Dashboard HTML cache (§18.1)
	dashMu    sync.Mutex
	dashHtml  []byte
	dashMtime time.Time

	// Conversation tracking (§14)
	convMu               sync.Mutex
	conversationMap      map[string]*ConversationSession
	convLRU              *list.List
	convLRUIndex         map[string]*list.Element
	globalSessionCounter int64

	// Error logging (§16)
	errorLogFile     *os.File
	errorLogMu       sync.Mutex
	errorLogInitDone bool

	// §29 Restart API: HTTP server to close on restart, and an injectable
	// exit function so tests don't kill the test runner. When both are nil
	// (the shared in-process test instance), HandleRestart returns the
	// JSON response but skips arming the shutdown timer.
	httpServer *http.Server
	exitFn     func(int)

	// Wallpaper cache mutex (§30) — protects .cache/wallpaper*.jpg file operations
	wallpaperMu sync.Mutex

	// Pass-through mode: last-known-good client API key used for usage/history
	// fetching when ApiKeyMode == "passthrough".
	lastClientKeyMu sync.RWMutex
	lastClientKey   string

	// Concurrency limit mode + manual limit, protected by concurrencyLimitMu.
	// gateLimit() takes one RLock and reads both fields atomically.
	concurrencyLimitMu   sync.RWMutex
	concurrencyLimitMode string
	manualLimit          int
}

// responseWriterTracker wraps an http.ResponseWriter to track whether
// WriteHeader has been called. Used by the recovery/timeout middleware
// (§35.1, §35.3) to know if it's safe to write a 500/504 response or if
// the response is already in flight (e.g., SSE streaming).
type responseWriterTracker struct {
	http.ResponseWriter
	mu       sync.Mutex
	written  bool // true once WriteHeader or Write is called
	hijacked bool // true after middleware takes over the response
}

func (rw *responseWriterTracker) WriteHeader(code int) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.hijacked {
		return
	}
	rw.written = true
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriterTracker) Write(b []byte) (int, error) {
	rw.mu.Lock()
	if rw.hijacked {
		rw.mu.Unlock()
		return 0, fmt.Errorf("response writer hijacked by middleware")
	}
	rw.written = true
	rw.mu.Unlock()
	return rw.ResponseWriter.Write(b)
}

// Flush delegates to the underlying writer if it implements http.Flusher.
func (rw *responseWriterTracker) Flush() {
	rw.mu.Lock()
	if rw.hijacked {
		rw.mu.Unlock()
		return
	}
	flusher, ok := rw.ResponseWriter.(http.Flusher)
	rw.mu.Unlock()
	if ok {
		flusher.Flush()
	}
}

// UpstreamClient is the HTTP client for communicating with the upstream UMANS API.
// It maintains a keep-alive connection pool and is safe for concurrent use.
type UpstreamClient struct {
	baseURL    string
	timeout    time.Duration
	apiKey     string
	httpClient *http.Client // keep-alive, custom transport
}
