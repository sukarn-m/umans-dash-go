# UMANS-Dash-Go — Developer Guide

## Project Structure

```
UMANS-DASH-GO/
├── go.mod                # Module: umans-dash-go, go 1.22, zero external deps
├── SPEC.md               # Technical spec for the Go rewrite
├── dashboard.html        # Dashboard UI (ported from JS proxy, minus excluded features)
├── types.go              # Type definitions (Config, KeyPool, ImageHandoffCache, Proxy, etc.)
├── proxy.go              # Full proxy implementation
├── cmd/umans-dash-go/
│   └── main.go           # Entry point (startup sequence, signal handling)
├── .cache/               # Cached wallpaper images (auto-created)
│   ├── wallpaper.jpg           # Cached Bing wallpaper (daily TTL)
│   └── wallpaper-haven.jpg      # Cached Wallhaven wallpaper (hourly TTL)
├── .logs/                # HTTP error logs (auto-created, per-session rotating files)
└── AGENTS.md             # This file
```

## What This Project Is

A Go rewrite of the [UMANS-Dash](../umans-dash/proxy.js) (~3,326 lines of Node.js). The Go version targets zero external dependencies (stdlib only). Excludes Sleev and FreeGen features from the original.

## Current State

- **`proxy.go` + `types.go`** contain the full implementation of all proxy functions.
- **`dashboard.html`** is the dashboard UI, ported from the JS proxy minus excluded features (FreeGen, Sleev, Shell Guard).

## Key Types (types.go)

| Type | Purpose |
|---|---|
| `Config` | Runtime configuration (API keys, cache settings, concurrency override, etc.) |
| `KeyConfig` | A single API key entry (name, key, session) |
| `KeyPool` | Round-robin multi-key pool with cooldown/unhealthy marking |
| `ImageHandoffCache` | LRU cache for vision handoff image descriptions (SHA-256 keyed, 50 entries, 24h TTL) |
| `Proxy` | Central state holder — all runtime state lives here |
| `ModelInfo` / `Capabilities` | Model metadata from upstream catalog |
| `UsageData` / `UsageInfo` / `WindowInfo` / `PlanInfo` | Usage tracking types |
| `ConcurrencyData` | Concurrency limits and current state |
| `ImagePart` | An image found in a request payload (for vision handoff) |
| `ModelsDevEntry` | A resolved models.dev catalog entry |

## Key Functions (proxy.go)

### Config (§2)
- `ParseDuration(str string) Duration` — Parse `"15m"`, `"6h"`, `"30s"`; bare numbers (pure digits) interpreted as **milliseconds**
- `MaskToken(key string) string` — first 10 + `"..."` + last 4
- `ParseListenPort(addr string) int` — Extract port from `"host:port"`
- `LoadConfig() (Config, error)` — validates `RequestTimeout > 0` (fatal if not)
- `saveConfig(cfg Config) error` — **atomic write** via `config.json.tmp` + `os.Rename`

### KeyPool (§3)
- `NewKeyPool(keys []KeyConfig) *KeyPool` — each entry defaults `CooldownMs = 30000`, `Healthy = true`
- `(*KeyPool) Acquire(preferredIndex int) (*KeySlot, bool)`
- `(*KeyPool) MarkUnhealthy(index, status int)` — cooldown: 503→60s, 502→30s, else 10s
- `(*KeyPool) MarkHealthy(index int)`
- `(*KeyPool) HealthyCount() int` / `Total() int` / `State() []KeyState`

### ImageHandoffCache (§11)
- `NewImageHandoffCache(maxSize int, ttl time.Duration) *ImageHandoffCache`
- `(*ImageHandoffCache) Get(key string) (string, bool)` — updates LRU on hit
- `(*ImageHandoffCache) Set(key, desc string)` — evicts oldest at capacity
- `(*ImageHandoffCache) Stats() HandoffCacheStats` / `Resize(maxSize, ttl)`
- `sha256Hash(s string) string` — SHA-256 hex digest for cache keys

### Model Catalog (§8)
- `(*Proxy) ApplyCatalogData(data map[string]interface{})` — writes under `catalogMu.Lock()`
- `(*Proxy) GetModelInfo(id string) ModelInfo` — reads under `catalogMu.RLock()`
- `(*Proxy) GetModelDisplayName(id string) string` — reads under `catalogMu.RLock()`
- `(*Proxy) GetOrderedModelIds() []string` — sorted by display name; reads under `catalogMu.RLock()`
- `(*Proxy) GetEffectiveModels() []string` — filters disabled models; reads under `catalogMu.RLock()`

### Vision Handoff (§11)
- `(*Proxy) NeedsVisionHandoff(resolvedModel string) bool`
- `(*Proxy) ResolveModelId(requestedModel string) string`
- `CollectImageParts(payload map[string]interface{}) []ImagePart`
- `(*Proxy) PerformVisionHandoff(payload, resolvedModel) int`

### Tool Schema Normalization (§12)
- `NormalizeToolSchemas(tools []interface{})`
- `TryResolveRef(node, defs) map[string]interface{}`
- `SimplifyNullableCombinator(schema, key string)`
- `NormalizeTypeField(schema)` / `NormalizeEnumField(schema)`

### Payload Normalization (§13)
- `StripReasoningContent(payload)` — removes `reasoning_content` from assistant messages
- `NormalizeThinkingPayload(payload)` — camelCase `budgetTokens` → snake_case `budget_tokens`
- `LimitImagesInMessages(payload, maxImages int)` — replaces excess images with placeholder text
- `FingerprintPayload(payload) string` — MD5 hash of first user message, 12 chars
- `MsgText(m) string` / `ExtractUserPrompt(payload) string`

### Models.dev (§9)
- `DeriveModelsDevId(umansId string) string` — strips "umans-" prefix
- `UmansIdCandidates(umansId string) []string`
- `ParseLevels(raw interface{}) []string`
- `InferReasoningModeFromCapabilities(reasoningCaps) *bool`
- `BuildReasoningVariants(reasoningCaps) map[string]interface{}`
- `BuildModelsDevIndex(catalog) map[string]ModelsDevEntry`

### Error Logging (§16)
- `RedactHeaders(headers map[string][]string) map[string][]string`
- `RedactBodyJson(body string) string`

### HTTP Helpers (§17)
- `(*Proxy) Authorized(req *http.Request) bool`
- `ReadBody(req *http.Request) (string, error)` — 5 MB cap; uses `errors.Is(err, io.EOF)` for clean stream end
- `WriteJSON(w, status, payload)`
- `WriteOpenAIError(w, status, message, errType, code)`
- `WriteAnthropicError(w, status, message, errType)`
- `WritePassthroughError(w, status, body)` / `WriteAnthropicPassthroughError(w, status, body)`

### HTTP Handlers (§17–§29)
- `(*Proxy) HandleHealthz(w, r)` / `HandleModels(w, r)` / `HandleConfigGet(w, r)`
- `(*Proxy) HandleKeysGet(w, r)` / `HandleKeysPost(w, r)` — `HandleKeysGet` acquires `configMu.RLock()`
- `(*Proxy) HandleUsage(w, r)` / `HandleConcurrency(w, r)`
- `(*Proxy) HandleUsageHistory(w, r)` / `HandleUser(w, r)` — `HandleUser` returns `user_id` from `LastConcurrency`
- `(*Proxy) HandleRequest(w, r)` — main router

### Opencode Config (§24)
- `DiscoverOpencodeConfigs(homeDir string) []string` — only returns existing files
- `(*Proxy) SetupOpencodeConfig(homeDir string, port int) bool` — no-op if no config exists

## Building

```bash
go build ./...                     # Compile check
go vet ./...                       # Static analysis
go run ./cmd/umans-dash-go/        # Start the proxy
```

## Concurrency & Mutex Model

The `Proxy` struct holds several dedicated mutexes. Code touching these fields must acquire the correct lock:

| Mutex | Protects | Notes |
|-------|----------|-------|
| `configMu` (RWMutex) | All `Config` fields | `RLock()` in read-only handlers (`proxyChatRequest`, `proxyAnthropicRequest`, `ResolveModelId`, `NeedsVisionHandoff`, `HandleKeysGet`). `Lock()` in `HandleConfigPost` / mutations. |
| `queueMu` (RWMutex) | `ActiveRequests`, `requestQueue`, `ThrottledCount` | All access via accessor helpers (`getActiveRequests`, `getQueueLen`, `getThrottledCount`, `bumpThrottled`). Never read/write these fields directly. |
| `catalogMu` (RWMutex) | `ModelInfoMap`, `DisplayNameMap` | `Lock()` in `ApplyCatalogData`; `RLock()` in all catalog readers. |
| `convMu` (Mutex) | `conversationMap`, `globalSessionCounter` | Used by both OpenAI and Anthropic paths for session tracking. |
| `rw.mu` (in `responseWriterTracker`) | Hijack/flush state | `Flush()` acquires `rw.mu`, checks `hijacked`, releases the lock, then calls `Flush()` — preventing flush-after-hijack panics. |

**Graceful shutdown** (`Shutdown()`): polls `ActiveRequests` under `queueMu.RLock` every 100ms (5s timeout) before calling `httpServer.Shutdown()`, ensuring in-flight requests drain before the listener closes.

## Dependencies

Zero. Go standard library only.

## Excluded from Original

- Sleev context-compression gateway
- FreeGen AI wallpaper generation (including `/api/bg-freegen` endpoint)
- i18n translation system (`/api/i18n` route, `I18N_STRINGS`, `HandleI18n` — dead code, fully removed)
- `Enqueue()` and `HandleRestartMock()` — dead code removed

## Included from Original

- Bing wallpaper proxy (`/api/bg`) — daily cache, peapix.com API
- Wallhaven wallpaper proxy (`/api/bg-wallhaven`) — hourly cache, wallhaven.cc API
- Dashboard HTML (full port minus excluded features)
