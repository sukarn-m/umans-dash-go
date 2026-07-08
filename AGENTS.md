# UMANS-Dash-Go — Developer Guide

## Project Structure

```
UMANS-DASH-GO/
├── go.mod # Module: umans-dash-go, go 1.22, zero external deps
├── main.go # Entry point (startup sequence, signal handling)
├── dashboard.html # Dashboard UI (ported from JS proxy, minus excluded features)
├── dashboard.js # Dashboard JavaScript (extracted, template-rendered)
├── embed.go # go:embed wrapper for dashboard assets
├── proxy/
│ ├── types.go # Type definitions (Config, KeyPool, ImageHandoffCache, Proxy, etc.)
│ └── proxy.go # Full proxy implementation
├── .cache/ # Cached wallpaper images (auto-created) — `.jpg` extension but may contain PNG/WebP data
│ ├── wallpaper.jpg # Cached Bing wallpaper (daily TTL)
│ └── wallpaper-haven.jpg # Cached Wallhaven wallpaper (hourly TTL)
├── .logs/ # HTTP error logs (auto-created, per-session rotating files)
└── AGENTS.md # This file
```

## What This Project Is

A Go rewrite of the [UMANS-Dash](../umans-dash/proxy.js) (~3,326 lines of Node.js). The Go version targets zero external dependencies (stdlib only). Excludes Sleev and FreeGen features from the original.

## Current State

- **`proxy/proxy.go` + `proxy/types.go`** contain the full implementation of all proxy functions.
- **`dashboard.html`** is the dashboard UI, ported from the JS proxy minus excluded features (see "Excluded from Original" below).

## Key Types (proxy/types.go)

| Type | Purpose |
|---|---|
| `Config` | Runtime configuration (API keys, cache settings, concurrency override, **`ApiKeyMode`** / JSON `API_KEY_MODE`: `"smart"` (default), `"managed"`, or `"passthrough"`, **`ConcurrencyLimitMode`** / JSON `CONCURRENCY_LIMIT_MODE`: `"soft"` (default), `"hard"`, or `"manual"` — controls which cap gates concurrency, **`ManualConcurrencyLimit`** / JSON `MANUAL_CONCURRENCY_LIMIT`: `int` — user-chosen gate when mode is `"manual"`, **`SlotFreeDelay`** / JSON `SLOT_FREE_DELAY`: `int` seconds — delay before freeing a concurrency slot after request completion (0 = immediate, default), **`RetryAttempts`** / JSON `RETRY_ATTEMPTS`: `*int` (omitempty) — max retry attempts (0–10, default 2, nil = unset → default), **`BackoffStrategy`** / JSON `BACKOFF_STRATEGY`: `string` — preset name (`"aggressive"` (default) or `"conservative"`), deprecated `BurstMode` / JSON `BURST_MODE` kept for migration + rollback safety, etc.) |
| `KeyConfig` | A single API key entry (name, key, session) |
| `KeyPool` | Round-robin multi-key pool with cooldown/unhealthy marking |
| `ImageHandoffCache` | LRU cache for vision handoff image descriptions (SHA-256 keyed, 50 entries, 24h TTL) |
| `Proxy` | Central state holder — all runtime state lives here. Includes `lastClientKeyMu` (`RWMutex`) / `lastClientKey` for last-known-good client API key tracking; `concurrencyLimitMu` (`RWMutex`) / `concurrencyLimitMode` / `manualLimit` for thread-safe concurrency limit mode state; `retryConfigMu` (`RWMutex`) / `retryAttempts` / `backoffStrategy` for thread-safe retry config state |
| `ModelInfo` / `Capabilities` | Model metadata from upstream catalog |
| `UsageData` / `UsageInfo` / `WindowInfo` / `PlanInfo` | Usage tracking types |
| `ConcurrencyData` | Concurrency limits and current state |
| `ImagePart` | An image found in a request payload (for vision handoff) |

## Key Functions (proxy/proxy.go)

### Config
- `ParseDuration(str string) Duration` — Parse `"15m"`, `"6h"`, `"30s"`; bare numbers (pure digits) interpreted as **milliseconds**
- `MaskToken(key string) string` — first 10 + `"..."` + last 4
- `ParseListenPort(addr string) int` — Extract port from `"host:port"`
- `LoadConfig() (Config, error)` — validates `RequestTimeout > 0` (fatal if not)
- `saveConfig(cfg Config) error` — **atomic write** via `config.json.tmp` + `os.Rename`

### KeyPool
- `NewKeyPool(keys []KeyConfig) *KeyPool` — each entry defaults `CooldownMs = 30000`, `Healthy = true`
- `(*KeyPool) Acquire(preferredIndex int) (*KeySlot, bool)`
- `(*KeyPool) MarkUnhealthy(index, status int)` — cooldown: 503→60s, 502→30s, else 10s
- `(*KeyPool) MarkHealthy(index int)`
- `(*KeyPool) HealthyCount() int` / `Total() int` / `State() []KeyState`

### ImageHandoffCache
- `NewImageHandoffCache(maxSize int, ttl time.Duration) *ImageHandoffCache`
- `(*ImageHandoffCache) Get(key string) (string, bool)` — updates LRU on hit
- `(*ImageHandoffCache) Set(key, desc string)` — evicts oldest at capacity
- `(*ImageHandoffCache) Stats() HandoffCacheStats` / `Resize(maxSize, ttl)`
- `sha256Hash(s string) string` — SHA-256 hex digest for cache keys

### Model Catalog
- `(*Proxy) ApplyCatalogData(data map[string]interface{})` — writes under `catalogMu.Lock()`
- `(*Proxy) GetModelInfo(id string) ModelInfo` — reads under `catalogMu.RLock()`
- `(*Proxy) GetModelDisplayName(id string) string` — reads under `catalogMu.RLock()`
- `(*Proxy) GetOrderedModelIds() []string` — sorted by display name; reads under `catalogMu.RLock()`
- `(*Proxy) GetEffectiveModels() []string` — filters disabled models; reads under `catalogMu.RLock()`

### Vision Handoff
- `(*Proxy) NeedsVisionHandoff(resolvedModel string) bool`
- `(*Proxy) ResolveModelId(requestedModel string) string`
- `CollectImageParts(payload map[string]interface{}) []ImagePart`
- `(*Proxy) PerformVisionHandoff(payload, resolvedModel) int`

### Tool Schema Normalization
- `NormalizeToolSchemas(tools []interface{})`
- `TryResolveRef(node, defs) map[string]interface{}`
- `SimplifyNullableCombinator(schema, key string)`
- `NormalizeTypeField(schema)` / `NormalizeEnumField(schema)`

### Payload Normalization
- `StripReasoningContent(payload)` — removes `reasoning_content` from assistant messages
- `NormalizeThinkingPayload(payload)` — camelCase `budgetTokens` → snake_case `budget_tokens`
- `LimitImagesInMessages(payload, maxImages int)` — replaces excess images with placeholder text
- `FingerprintPayload(payload) string` — MD5 hash of first user message, 12 chars
- `MsgText(m) string` / `ExtractUserPrompt(payload) string`

### Reasoning Helpers
- `ParseLevels(raw interface{}) []string`
- `InferReasoningModeFromCapabilities(reasoningCaps) *bool`
- `BuildReasoningVariants(reasoningCaps) map[string]interface{}`

### Error Logging
- `RedactHeaders(headers map[string][]string) map[string][]string`
- `RedactBodyJson(body string) string`

### API Key Mode & Client Key Tracking
- `applyDefaultApiKeyMode(cfg *Config)` — defaults `ApiKeyMode` to `"smart"` when unset (called in `LoadConfig` paths)
- `extractClientAPIKey(req *http.Request) string` — reads `X-Api-Key` header, else `Authorization: Bearer` token
- `(*Proxy) setLastClientKey(key string)` — thread-safe write under `lastClientKeyMu.Lock`
- `(*Proxy) getLastClientKey() string` — thread-safe read under `lastClientKeyMu.RLock`
- `(*Proxy) upstreamAPIKeyForDashboard() string` — selects the key for usage/history/concurrency calls per mode: passthrough→last client key; smart→last client key with pool fallback; managed→pool key

### Concurrency Limit Mode & Slot Free Delay
- `(*Proxy) getConcurrencyLimitMode() string` — thread-safe read under `concurrencyLimitMu.RLock`
- `(*Proxy) setConcurrencyLimitMode(mode string)` — thread-safe write under `concurrencyLimitMu.Lock`
- `(*Proxy) getManualLimit() int` — thread-safe read under `concurrencyLimitMu.RLock`
- `(*Proxy) setManualLimit(limit int)` — thread-safe write under `concurrencyLimitMu.Lock`; clamps to ≥ 1
- `(*Proxy) getSlotFreeDelay() int` — reads `Config.SlotFreeDelay` under `configMu.RLock`
- `(*Proxy) gateLimit() int` — returns the effective concurrency gate based on mode: `soft` → soft cap (`Limit`), `hard` → hard cap (`HardCap`) if available else `Limit`, `manual` → `ManualConcurrencyLimit` clamped to `[1, HardCap]` (falls back to soft cap if `ManualConcurrencyLimit` is 0/uninitialized); returns `-1` if no gate applies
- `Config.ConcurrencyLimitMode` (JSON `CONCURRENCY_LIMIT_MODE`) and `Config.ManualConcurrencyLimit` (JSON `MANUAL_CONCURRENCY_LIMIT`) persisted via `saveConfig()`; restored in `NewProxy()` on startup. Old `BURST_MODE` bool migrated: `true` → `"hard"`, `false`/missing → `"soft"`. `BurstMode` kept in sync (`true` when mode is `"hard"`) for rollback safety.
- `Config.SlotFreeDelay` (JSON `SLOT_FREE_DELAY`) — delay in seconds before `OnRequestComplete()` decrements `ActiveRequests`. Default 0 (immediate).
- `HandleConfigPost` calls `ProcessQueue()` on any mode change (not just `hard`/`manual`) since switching to `soft` from a more restrictive mode can also raise the gate. When switching to `manual` with `manualLimit == 0`, initializes it to the current soft cap (or hard cap if no soft cap).

### Retry Config & Backoff
- `(*Proxy) getRetryAttempts() int` — thread-safe read under `retryConfigMu.RLock`; uses `< 0` guard (not `<= 0`) so explicit 0 (no retries) passes through. Clamps to `MaxRetryAttemptsCap` (10). Returns `DefaultRetryAttempts` (2) when negative.
- `(*Proxy) getBackoffStrategy() string` — thread-safe read under `retryConfigMu.RLock`; returns `"aggressive"` if empty.
- `(*Proxy) setRetryConfig(attempts int, strategy string)` — thread-safe write under `retryConfigMu.Lock`. Called from `NewProxy()` and `HandleConfigPost`.
- `(*Proxy) getRetryDelay(attempt int) time.Duration` — looks up `BackoffPresets[strategy][attempt-1]`, clamping to last entry if index exceeds preset length.
- `(*Proxy) retryLoop(fn)` — method (was free function `RetryLoop`). Loops up to `getRetryAttempts()` times. When 0, runs callback once with `isLast=true`.
- `BackoffPresets` — package-level `var` (map[string][]int): `"aggressive"` → {1,3,5,10,15,20,25,30,45,60}, `"conservative"` → {5,15,30,45,60,120,180,240,300}.
- `Config.RetryAttempts` (JSON `RETRY_ATTEMPTS`, `*int` omitempty) — nil = unset → default 2; 0 = no retries; range 0–10. `Config.BackoffStrategy` (JSON `BACKOFF_STRATEGY`, string) — `"aggressive"` (default) or `"conservative"`.
- Both persisted via `saveConfig()`; restored in `NewProxy()`. Old config.json files without these fields migrate gracefully (nil → default 2, empty → "aggressive").
- `HandleConfigPost` accepts `retryAttempts` (float64, clamped [0,10]), `backoffStrategy` (string, validated against `BackoffPresets`), `requestTimeout` (float64 seconds, clamped [30,1800]).
- `HandleConfigGet` exposes `retryAttempts`, `backoffStrategy`, `requestTimeout` (seconds).
- Retry applies to both OpenAI (`proxyChatRequest`) and Anthropic (`proxyAnthropicRequest`) paths.
- `(*UpstreamClient) SetTimeout(timeout time.Duration)` — live-updates `httpClient.Timeout` and transport's `ResponseHeaderTimeout` without restart.
- `NewUpstreamClient` uses the configured timeout for `ResponseHeaderTimeout` (was hardcoded 300s).

### Panic Safety & SSE Streaming
- **`headersCommitted` flag** — both OpenAI and Anthropic retry loops track whether response headers have been written to the client. Once committed, the retry callback returns an error instead of attempting to write again. Prevents "superfluous response.WriteHeader" panics when a mid-stream pipe break triggers a retry.
- **`safeFlush(w)`** — central helper that wraps `flusher.Flush()` with `defer func() { recover() }()`. All flush calls in the codebase go through this helper (or through `responseWriterTracker.Flush` which has its own hijack guard). No bare `flusher.Flush()` calls remain.
- **`writeSSEErrorEvent`** — panic-safe: wrapped in `defer func() { recover() }()` so writing an error event to a dead/hijacked connection is silently swallowed.
- **`writeSSEHeaders`** — delegates to `responseWriterTracker.commitSSE()` when the writer is a tracker, making header commitment atomic under the tracker lock and guarded against `written`/`hijacked`/`aborted`.
- **`responseWriterTracker`** — wraps the real `ResponseWriter` with `written`/`hijacked`/`aborted` tracking and implements `http.Hijacker`. `WriteHeader`/`Write`/`Flush` return early if the response is hijacked, written, or aborted. Used by the timeout middleware (`ServeHTTP`) to safely take over the response when a request times out.
- **Retry callback panic recovery** — both OpenAI and Anthropic retry callbacks use named return values (`retry bool, err error`) with a `defer` recovery that logs the panic with `attempt`, `isLast`, and `headersCommitted` state, then returns the error to the retry loop.

### Request Lifecycle & Queueing
- **`ServeHTTP`** — wraps every request in a `responseWriterTracker`, enforces `REQUEST_TIMEOUT` for the pre-streaming phase, and supports request abort on timeout. Streaming responses continue after timeout.
- **`dispatchItem(item *QueueItem)`** — single dispatch path used for both direct dispatch and queued dispatch. Increments `ActiveRequests`, runs `defer OnRequestComplete()` exactly once, and guards against aborted/committed responses.
- **`TryEnqueueOrDispatch(item)`** — owns `ActiveRequests` increment for direct dispatch and rejects new requests during shutdown.
- **`ProcessQueue()`** — dispatches queued items while capacity exists, drops items whose client context was cancelled or response was aborted, and early-returns when shutting down.
- **`OnRequestComplete()`** — decrements `ActiveRequests` (after optional `SlotFreeDelay`) and triggers `ProcessQueue()`.
- **`Shutdown()`** — sets shutdown flag, rejects queued requests with 503, drains active requests, then exits.

### Context Propagation
- **`UpstreamClient.ChatCompletions(ctx, ...)`**, **`Messages(ctx, ...)`**, **`doPost(ctx, ...)`** — accept a caller-supplied context so client disconnects/timeouts cancel in-flight upstream work.
- **Streaming requests** use the original client context once headers are committed, so long SSE streams are not truncated by the request timeout.

### Vision Handoff
- **`PerformVisionHandoff`** — parallel image analysis with a configurable concurrency cap (`VisionHandoffConcurrency`, default 4) to prevent goroutine explosion.
- **`analyzeImageViaHandoff(ctx, ...)`** — accepts a context for cancellation propagation.

### Catalog & Concurrency Caching
- **Catalog singleflight** — custom singleflight group prevents thundering-herd `/models/info` fetches; stale cache is returned with `nil` error when upstream fails.
- **Shared `catalogClient`** — reused `http.Client` for catalog/models/user-info fetches with keep-alive.
- **`gateLimit()` cache** — 1-second TTL cache to reduce lock churn under load.
- **Effective-concurrency cache** — keyed on `Limit`/`HardCap`/`OverrideConcurrency` to avoid recomputation per queue operation.

### API Key Thread Safety
- **No shared mutable `apiKey` field on `UpstreamClient`** — the API key is passed as an explicit parameter to `ChatCompletions`, `Messages`, `doPost`, `GetUsage`, and `GetUserInfo`. This eliminates the data race that occurred when multiple goroutines called `SetAPIKey` concurrently on the shared `p.Upstream` instance.
- `SetAPIKey(key string)` still exists for backward compatibility but is not used in the request hot path.

### HTTP Helpers
- `(*Proxy) Authorized(req *http.Request) bool` — behavior now depends on `ApiKeyMode`: `passthrough` requires a client key present; `smart` always accepts (client or own key used downstream); `managed` validates the client's key against configured `APIKeys` (or accepts if `APIKeys` is empty)
- `ReadBody(req *http.Request) (string, error)` — 5 MB cap; uses `errors.Is(err, io.EOF)` for clean stream end
- `pipeBodyToResponse(body, w, r) (int, error)` — copies upstream response body to client with flushing; returns bytes written so caller can detect empty streams
- **Empty SSE stream detection** — before committing SSE headers to the client, the proxy peeks at the first chunk from the upstream body. If 0 bytes + EOF (empty stream), it treats this as a retryable 502 error (logs, retries with key rotation). Prevents the "empty stream with no finish_reason" client error.
- `WriteJSON(w, status, payload)`
- `WriteOpenAIError(w, status, message, errType, code)`
- `WriteAnthropicError(w, status, message, errType)`
- `WritePassthroughError(w, status, body)` / `WriteAnthropicPassthroughError(w, status, body)`

### Wallpaper Helpers
- `upgradePeapixResolution(imageURL string) string` — regex `_NNNN.(jpg|jpeg|png)$` → `_3840` for UHD peapix images
- `downloadImage(imageURL, userAgent string, timeout time.Duration) []byte` — HTTP fetch with UA + timeout
- `isValidImage(data []byte) bool` — validates JPEG/PNG/WebP magic bytes
- `isJPEG(data []byte) bool` — JPEG-only magic check (`0xFF 0xD8`)
- `imageContentType(data []byte) string` — returns `"image/png"`, `"image/webp"`, or `"image/jpeg"` (fallback) based on magic bytes
- `serveWallpaperImage(w, data, expires)` — serves image as `image/jpeg` (legacy; delegates to `serveWallpaperImageTyped`)
- `serveWallpaperImageTyped(w, data, expires, contentType)` — typed wallpaper serving with caller-specified Content-Type
- `saveCacheFile(cacheFile string, data []byte) bool` — writes cache file, mkdir if needed

### HTTP Handlers
- `(*Proxy) HandleHealthz(w, r)` / `HandleModels(w, r)` / `HandleConfigGet(w, r)` — `HandleConfigGet` exposes `apikeyMode`, `concurrencyLimitMode`, `manualConcurrencyLimit`, and `slotFreeDelay` fields
- `(*Proxy) HandleConfigPost(w, r)` — accepts `apikeyMode` (`managed`/`passthrough`/`smart`), `concurrencyLimitMode` (`soft`/`hard`/`manual`), `manualConcurrencyLimit` (int, clamped ≥ 1), `slotFreeDelay` (int seconds, clamped ≥ 0); syncs `BurstMode` for rollback safety; auto-initializes `manualLimit` to soft cap when switching to manual with limit 0; triggers `ProcessQueue()` on any mode change; triggers debounced save
- `(*Proxy) HandleKeysGet(w, r)` / `HandleKeysPost(w, r)` — `HandleKeysGet` acquires `configMu.RLock()`
- `(*Proxy) HandleUsage(w, r)` / `HandleConcurrency(w, r)` — `HandleConcurrency` response includes `concurrency_limit_mode` and `manual_limit` fields; `FetchUsage`/`FetchUsageHistory` call `upstreamAPIKeyForDashboard()` before upstream calls
- `(*Proxy) HandleUsageHistory(w, r)` / `HandleUser(w, r)` — `HandleUser` returns `user_id` from `LastConcurrency`
- `(*Proxy) HandleRequest(w, r)` — main router
- `(*Proxy) HandleBingWallpaper(w, r)` — daily cache; calls `upgradePeapixResolution()` for UHD variant
- `(*Proxy) HandleWallhavenWallpaper(w, r)` — hourly cache; `atleast=2560x1440` filter; uses `isValidImage` + `serveWallpaperImageTyped` with `imageContentType`

### Dashboard (dashboard.html)
- **Concurrency Limit selector** — three-button group (Soft Cap / Hard Cap / Manual) replacing the old Burst Mode On/Off toggle. Soft Cap gates at the soft cap (Limit), Hard Cap gates at the hard cap (HardCap), Manual reveals a slider (1 to hard cap) for a custom concurrency limit. Frontend POSTs `concurrencyLimitMode` and `manualConcurrencyLimit` to `/api/config`; `loadConfig()` initializes the state from backend; `fetchConcurrency()` reads `concurrency_limit_mode` and `manual_limit` from the concurrency API response and dynamically updates the slider max/position
- **Slot Free Delay input** — number input in Quick Settings for a positive integer (seconds, default 0). POSTs `slotFreeDelay` to `/api/config` on change
- **Retry Attempts slider** — range slider (0–10, default 2) in Quick Settings. POSTs `retryAttempts` to `/api/config` on change. Shows live value.
- **Backoff Strategy dropdown** — select in Quick Settings (hidden when retries=0). Options: Aggressive (1s→60s) or Conservative (5s→300s). POSTs `backoffStrategy` to `/api/config` on change. Shows delay sequence preview below.
- **Request Timeout slider** — range slider (30s–30m, step 30s, default 15m) in Quick Settings. POSTs `requestTimeout` (seconds) to `/api/config` on change. Applied live via `Upstream.SetTimeout()` without restart.
- **Priority card** — replaces the old Throttled card in the usage stat grid. Always visible. Shows "Low (reason)" in yellow when priority is low, "Normal" in white otherwise.
- **Collapsed-by-default sections** — Quick Actions, Models, and Environment sections start collapsed (have `collapsed` class in HTML). Other sections (Quick Settings, Usage History, etc.) start expanded.
- **Environment section** — shows Version (from `proxyData.version`), Runtime (Go version), Port, and Started At. The collapsed header preview text shows `v{version} · {runtime} · :{port}`.
- **Header title** — "UMANS Dash Go" (was "UMANS Dash").
- `setWallpaper(src, skipConfigSave)` — `skipConfigSave` parameter avoids redundant POST during init
- `loadConfig()` — decoupled from `/healthz` (fetched in background, not awaited); hides loader before `fetchUsage()` (fire-and-forget)
- `toggleSection()` — animates section collapse/expand via JS-driven height transition (`scrollHeight` → `0` and back); swaps `bi-chevron-down` ↔ `bi-chevron-right` icon classes. CSS `.collapsed` sets `height:0;opacity:0;padding:0`; `display:none` is applied inline by JS only after `transitionend` fires, so the collapse animation is visible
- `clearWallpaper()` — uses `setProperty(..., 'important')` to override server-injected `!important` CSS
- **Unified refresh cycle** — all content (status, usage, concurrency, history) on one timer, persisted to localStorage
- `fetchConcurrency()` — called on init (was missing previously). Reads `concurrency_limit_mode` and `manual_limit` from the API response to determine the effective gate (`barMax`) for the progress bar. Updates the manual slider max to the current hard cap. The redundant "Queued" card in the detail grid below the bar was removed (the count is already shown in the stat card above the bar)

## Building

```bash
go build ./... # Compile check
go vet ./... # Static analysis
go run . # Start the proxy
```

## Concurrency & Mutex Model

The `Proxy` struct holds several dedicated mutexes. Code touching these fields must acquire the correct lock:

| Mutex | Protects | Notes |
|-------|----------|-------|
| `configMu` (RWMutex) | All `Config` fields | `RLock()` in read-only handlers (`ResolveModelId`, `NeedsVisionHandoff`, `HandleKeysGet`, `HandleConfigGet`). `Lock()` in `HandleConfigPost` / mutations. **Note:** `proxyChatRequest` and `proxyAnthropicRequest` now snapshot needed config fields (`ApiKeyMode`, `MaxImages`, `UpstreamBaseURL`) under a brief `RLock` then release before the upstream call — previously held `RLock` for the entire request lifecycle, blocking `HandleConfigPost`. |
| `queueMu` (RWMutex) | `ActiveRequests`, `requestQueue`, `ThrottledCount` | All access via accessor helpers (`getActiveRequests`, `getQueueLen`, `getThrottledCount`, `bumpThrottled`). Never read/write these fields directly. |
| `catalogMu` (RWMutex) | `ModelInfoMap`, `DisplayNameMap` | `Lock()` in `ApplyCatalogData`; `RLock()` in all catalog readers. |
| `convMu` (Mutex) | `conversationMap`, `globalSessionCounter` | Used by both OpenAI and Anthropic paths for session tracking. |
| `rw.mu` (in `responseWriterTracker`) | Hijack/flush state | `Flush()` acquires `rw.mu`, checks `hijacked`, releases the lock, then calls `Flush()` — preventing flush-after-hijack panics. |
| `lastClientKeyMu` (RWMutex) | `lastClientKey` | `Lock()` in `setLastClientKey`; `RLock()` in `getLastClientKey`. Tracks last-known-good client API key for usage/history calls when `ApiKeyMode` is `passthrough`/`smart`. |
| `concurrencyLimitMu` (RWMutex) | `concurrencyLimitMode`, `manualLimit` | `Lock()` in `setConcurrencyLimitMode`/`setManualLimit`; `RLock()` in `getConcurrencyLimitMode`/`getManualLimit`. `gateLimit()` takes one `RLock` and reads both fields atomically. Gates `gateLimit()`: `soft` → soft cap (`Limit`), `hard` → hard cap (`HardCap`), `manual` → `manualLimit` clamped to `[1, HardCap]`. Restored from `Config.ConcurrencyLimitMode` in `NewProxy()`. |
| `retryConfigMu` (RWMutex) | `retryAttempts`, `backoffStrategy` | `Lock()` in `setRetryConfig`; `RLock()` in `getRetryAttempts`/`getBackoffStrategy`. `getRetryAttempts()` uses `< 0` guard (not `<= 0`) so explicit 0 passes through. Restored from `Config.RetryAttempts` / `Config.BackoffStrategy` in `NewProxy()`. |

**Graceful shutdown** (`Shutdown()`): polls `ActiveRequests` under `queueMu.RLock` every 100ms (5s timeout) before calling `httpServer.Shutdown()`, ensuring in-flight requests drain before the listener closes.

## Dependencies

Zero. Go standard library only.

## Excluded from Original

Features present in the upstream [Node.js original](https://github.com/notBlubbll/umans-dash) that were intentionally removed:

- **Sleev context-compression gateway** — local gateway daemon, binary resolution, OAuth sign-in (`SLEEV_ENABLED`, `SLEEV_GATEWAY_*`)
- **FreeGen AI wallpaper generation** — `/api/bg-freegen` endpoint, `FREEGEN_PROMPT`, WebSocket integration with freegen.app
- **i18n translation system** — `/api/i18n` route, `I18N_STRINGS` catalog, `LOCALE` config, one-click autotranslate via `umans-flash`
- **Shell Guard** — `isGitCommand()`, `sanitizeShellToolCall()`, blocking git commands in shell tool-call responses (streaming and non-streaming)
- **Response Cache** — LRU cache for non-streaming chat responses (`ResponseCache`, `cacheKey()`, `CACHE_TTL`/`CACHE_MAX_SIZE`/`CACHE_ENABLED` config, `/api/cache` GET/DELETE endpoints)
- **UMANS app login** — `EMAIL`/`PASSWORD`/`APP_SESSION` config, `loginToApp()`, `/api/umans/login` and `/api/umans/logout` endpoints, platform login modal
- **Rate Limit Map** — `RATE_LIMIT_MAP`, `enforceRateLimit()`, per-model rate limit delays
- **Test Chat panel** — streaming/context chat panel with model selector in the dashboard
- **SS Mode** — screenshot-safe mode (blur on hover, jumbled user ID, masked email)
- **Glass UI** — procedural SVG filter-based glassmorphism (`feDisplacementMap`, `feColorMatrix`); replaced with CSS `backdrop-filter`
- **SQLite Usage Cache** — `.cache/usage.db` persistent cache for daily usage-history buckets (`node:sqlite`/`bun:sqlite`)
- `Enqueue()` and `HandleRestartMock()` — dead code removed

