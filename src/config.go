package proxy

import (
	"encoding/json"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ─── Constants & Config (§2) ──────────────────────────────────────────────────

// ─── Constants ─────────────────────────────────────────────────────────────

const (
	MaxQueueSize           = 256
	QueueFullErrorCode     = "queue_full"
	MaxBodySize            = 5 * 1024 * 1024
	ConvMapMax             = 10000
	ConvMapEvictTarget     = 8000 // 80% of ConvMapMax — eviction target when map is at capacity
	UmansAPIBase           = "https://api.code.umans.ai/v1"
	APIKeyEnvVar           = "UMANS_API_KEY"
	ModelCatalogCacheTTL   = 5 * time.Minute
	UsageCacheTTL          = 5 * time.Minute
	StatusCacheTTL         = 1 * time.Minute  // status is relatively static
	StatusHistoryCacheTTL = 5 * time.Minute  // matches usage history
	UpstreamModelsCacheTTL = 5 * time.Minute  // §8.7
	UserInfoCacheTTL       = 5 * time.Minute  // §8.8
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
		ListenAddr:                "127.0.0.1:8084",
		UpstreamBaseURL:           "https://api.code.umans.ai/v1",
		RequestTimeout:            ParseDuration("15m"),
		OverrideConcurrency:       0,
		MaxImages:                 9,
		VisionHandoffEnabled:      false,
		VisionHandoffModel:        "umans-coder",
		VisionHandoffPrompt:       "",
		VisionHandoffCacheEnabled: false,
		VisionHandoffCacheTtl:     ParseDuration("24h"),
		VisionHandoffConcurrency:  4,
		WallpaperSource:           "bing",
		ConcurrencyLimitMode:      "soft",
		SlotFreeDelay:             2,
		RetryAttempts:             &defRetry,
		BackoffStrategy:           "aggressive",
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
