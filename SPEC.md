# UMANS-Dash Go Rewrite — Technical Specification

> **Source**: `proxy.js` (3,326 lines) in https://github.com/sukarn-m/umans-dash.  
> **Target**: Go binary, zero external dependencies (stdlib only).  
> **Scope**: All features except Sleev gateway and FreeGen wallpaper generation.

---

## 1. Overview

A local HTTP reverse proxy that sits between OpenAI/Anthropic-compatible clients (e.g. opencode) and the UMANS AI upstream API (`https://api.code.umans.ai/v1`). The proxy provides multi-key pool management, API key mode selection (smart/managed/passthrough), concurrency limiting with a bounded queue, response caching, retry logic with key rotation, vision handoff for models that cannot see images, tool schema normalization, model catalog management, usage tracking, opencode config auto-discovery, UHD wallpaper support, and a dashboard.

### Design Principles

- **Zero external dependencies**: Go stdlib only (`net/http`, `encoding/json`, `crypto/md5`, `os`, `os/exec`, `regexp`, `sync`, `time`, `path/filepath`, `sort`, `container/list`, `net/url`, `strings`, `strconv`, `fmt`, `log`, `bufio`, `context`, `sync/atomic`, `path`).
- **Single binary**: No runtime install, no node_modules, no npm.
- **127.0.0.1 only**: The proxy listens on localhost only.
- **Graceful shutdown**: SIGINT/SIGTERM close the HTTP server and exit cleanly.

---

## 2. Configuration

### 2.1 Config File

Path: `.config/config.json` (relative to the binary's working directory).

```json
{
  "LISTEN_ADDR": "127.0.0.1:8084",
  "UPSTREAM_BASE_URL": "https://api.code.umans.ai/v1",
  "REQUEST_TIMEOUT": "15m",
  "API_KEY": "sk-...",
  "API_KEYS": ["proxy-key-1", "proxy-key-2"],
  "KEYS": [
    { "name": "Default", "key": "sk-...", "session": "" },
    { "name": "Backup", "key": "sk-...", "session": "" }
  ],
  "ENABLED_MODELS": [],
  "MODEL_DISPLAY_NAMES": {},
  "OVERRIDE_CONCURRENCY": 0,
  "MAX_IMAGES": 9,
  "DISABLED_MODELS": [],
  "VISION_HANDOFF_ENABLED": false,
  "VISION_HANDOFF_MODEL": "umans-coder",
  "VISION_HANDOFF_PROMPT": "",
  "VISION_HANDOFF_CACHE_ENABLED": false,
  "VISION_HANDOFF_CACHE_TTL": "24h",
  "API_KEY_MODE": "smart",
  "CONCURRENCY_LIMIT_MODE": "soft",
  "MANUAL_CONCURRENCY_LIMIT": 0,
  "SLOT_FREE_DELAY": 0,
  "BURST_MODE": false
}
```

The `API_KEY_MODE` field controls how the proxy handles API keys (see §27). Valid values: `"smart"` (default), `"managed"`, `"passthrough"`.

The `CONCURRENCY_LIMIT_MODE` field (string) controls the concurrency gate (§5.3): `"soft"` (default) gates at the soft cap (`limit`), `"hard"` gates at the hard cap (`hard_cap`), `"manual"` gates at `MANUAL_CONCURRENCY_LIMIT` (clamped to `[1, hard_cap]`). When switching to `"manual"` with `MANUAL_CONCURRENCY_LIMIT == 0`, it is auto-initialized to the current soft cap.

The `MANUAL_CONCURRENCY_LIMIT` field (int) is the user-chosen gate value when mode is `"manual"`. Default 0 (uninitialized). Clamped to `[1, hard_cap]` at read time in `gateLimit()`.

The `SLOT_FREE_DELAY` field (int, seconds) introduces a delay before `OnRequestComplete()` decrements `ActiveRequests` and processes the queue. Default 0 (immediate). Useful to avoid upstream rate limits when requests complete in rapid succession.

The `BURST_MODE` field (bool) is deprecated. Kept for config migration + rollback safety. Migrated to `CONCURRENCY_LIMIT_MODE` on startup in `NewProxy()`: `true` → `"hard"`, `false`/missing → `"soft"`. Synced in `HandleConfigPost`: set to `true` when mode is `"hard"`, `false` otherwise.

### 2.2 Defaults

| Key | Default | Description |
|-----|---------|-------------|
| `LISTEN_ADDR` | `127.0.0.1:8084` | Listen address |
| `UPSTREAM_BASE_URL` | `https://api.code.umans.ai/v1` | Upstream API base |
| `REQUEST_TIMEOUT` | `15m` | Upstream request timeout (parsed via `parseDuration`) |
| `OVERRIDE_CONCURRENCY` | `0` | Override concurrency limit (0 = use API limit) |
| `MAX_IMAGES` | `9` | Max images per request |
| `wallpaperSource` | `bing` | Wallpaper source: `none`, `bing`, or `wallhaven` |
| `API_KEY_MODE` | `smart` | API key handling mode: `smart`, `managed`, or `passthrough` (§27) |
| `CONCURRENCY_LIMIT_MODE` | `soft` | Concurrency gating mode: `soft`, `hard`, or `manual` (§5.3) |
| `MANUAL_CONCURRENCY_LIMIT` | `0` | Custom gate when mode is `manual` (0 = auto-init to soft cap) |
| `SLOT_FREE_DELAY` | `0` | Delay (seconds) before freeing a concurrency slot (0 = immediate) |
| `BURST_MODE` | `false` | Deprecated. Migrated to `CONCURRENCY_LIMIT_MODE`. Kept for rollback safety. |
| `VISION_HANDOFF_ENABLED` | `false` | Enable vision handoff |
| `VISION_HANDOFF_MODEL` | `umans-coder` | Model to use for image analysis |
| `VISION_HANDOFF_PROMPT` | `""` (uses built-in default) | System prompt for image analysis |
| `VISION_HANDOFF_CACHE_ENABLED` | `false` | Enable image handoff description cache |
| `VISION_HANDOFF_CACHE_TTL` | `24h` | TTL for handoff cache entries |

### 2.3 Env Var Overrides

Env vars override config file values:

| Env Var | Config Key | Notes |
|---------|-----------|-------|
| `LISTEN_ADDR` | `LISTEN_ADDR` | |
| `UPSTREAM_BASE_URL` | `UPSTREAM_BASE_URL` | |
| `REQUEST_TIMEOUT` | `REQUEST_TIMEOUT` | |
| `UMANS_API_KEY` | `API_KEY` | |
| `API_KEYS` | `API_KEYS` | Comma-separated |
| `OVERRIDE_CONCURRENCY` | `OVERRIDE_CONCURRENCY` | Parsed as int |
| `MAX_IMAGES` | `MAX_IMAGES` | Parsed as int |
| `API_KEY_MODE` | `API_KEY_MODE` | `smart`, `managed`, or `passthrough` |
| `VISION_HANDOFF_ENABLED` | `VISION_HANDOFF_ENABLED` | `"false"` disables |
| `VISION_HANDOFF_CACHE_ENABLED` | `VISION_HANDOFF_CACHE_ENABLED` | `"false"` disables |
| `VISION_HANDOFF_CACHE_TTL` | `VISION_HANDOFF_CACHE_TTL` | |

### 2.3.1 Config Validation

`LoadConfig` validates critical fields after parsing and env-var overrides:

- `RequestTimeout` must be positive (`> 0`). A zero or negative value is a fatal error (`log.Fatal`).
- `applyDefaultApiKeyMode(&cfg)` is called last — if `ApiKeyMode` is empty, it defaults to `"smart"` (§27).

### 2.4 `parseDuration(str)` → `time.Duration`

Parse strings like `"15m"`, `"6h"`, `"30s"` into milliseconds.

- Regex: `^(\d+)(h|m|s)$`
- `h` → hours, `m` → minutes, `s` → seconds
- **Bare numbers** (pure digits with no unit suffix) are interpreted as **milliseconds**.
- Invalid/empty input returns the zero-value `Duration{}`.

### 2.5 `maskToken(key)` → `string`

Mask an API key for display: first 10 chars + `...` + last 4 chars. Empty string returns empty string.

### 2.6 `parseListenPort(addr)` → `int`

Extract the port from a `host:port` string. Falls back to `8084` if parse fails.

### 2.7 Config Persistence

- `saveConfig(cfg)`: Write config to `.config/config.json` as pretty-printed JSON using an **atomic write** — data is first written to `.config/config.json.tmp`, then `os.Rename` swaps it into place. On rename failure the temp file is removed (best-effort).
- `debouncedSaveConfig(cfg)`: Debounced 500ms write. Only one write occurs even if called multiple times within the window. Uses a timer that resets on each call.

---

## 3. Key Pool

Round-robin multi-key pool with cooldown/unhealthy marking.

### 3.1 Structure

```go
type KeyEntry struct {
    Key        string
    Name       string
    Healthy    bool
    LastError  time.Time
    CooldownMs int64
}

type KeyPool struct {
    entries []*KeyEntry
    index   int  // round-robin counter
    mu      sync.Mutex
}
```

**NewKeyPool default**: Each key entry is created with `CooldownMs = 30000` (30s default cooldown window) and `Healthy = true`.

### 3.2 `Acquire(preferredIndex int) (*KeySlot, bool)`

- If `preferredIndex` is valid (>= 0) and that key is healthy or past cooldown, return it. Set `Healthy = true`.
- Round-robin through all keys starting at `index`. Return the first healthy or cooldown-expired key. Set `Healthy = true`.
- Set the global config's active API key to the acquired key.
- Return `nil, false` if no healthy keys exist.
- **Thread-safe** via mutex.

### 3.3 `MarkUnhealthy(index int, status int)`

- Set `Healthy = false`, `LastError = now`.
- Cooldown depends on status:
  - `>= 503`: 60 seconds
  - `>= 502`: 30 seconds
  - Otherwise: 10 seconds

### 3.4 `MarkHealthy(index int)`

- Set `Healthy = true`, `LastError = zero`.

### 3.5 `HealthyCount() int`

Count of keys that are healthy OR past their cooldown period.

### 3.6 `Total() int`

Total number of keys in the pool.

### 3.7 `State() []KeyState`

Returns array of state objects for each key:

```go
type KeyState struct {
    Name              string `json:"name"`
    Status            string `json:"status"`  // "active", "cooldown", "none"
    Healthy           bool   `json:"healthy"`
    RemainingCooldown int64  `json:"remainingCooldown"` // ms
    Token             string `json:"token"`   // masked
}
```

Status logic:
- `"none"` if key is empty
- `"active"` if healthy or cooldown expired
- `"cooldown"` if unhealthy and within cooldown window

---

## 4. Image Handoff Cache

LRU cache for vision handoff image descriptions, keyed by SHA-256 hash of the image data URI. 50 entries max, 24h TTL. When enabled, `analyzeImageViaHandoff` checks the cache before making the upstream call and stores results on success.

### 4.1 Structure

```go
type ImageHandoffCache struct {
    maxSize   int
    ttl       time.Duration
    mu        sync.Mutex
    lru       *list.List  // front = most recent
    lookup    map[string]*list.Element
    hits      int64
    misses    int64
    evictions int64
}
```

### 4.2 Operations

- `Get(key) (string, bool)`: Returns cached description if exists and not expired. On access, moves to front (LRU update). On miss/expiry, increments `misses` and returns `false`. On hit, increments `hits`.
- `Set(key, desc)`: If key exists, move to front. If at capacity, evict oldest (increment `evictions`). Insert at front.
- `Stats() HandoffCacheStats`: Return `{size, maxSize, ttlMs, hits, misses, evictions}`.
- `Resize(maxSize, ttl)`: Update max size and TTL.

### 4.3 Cache Key

SHA-256 hash of the image data URI (`sha256Hash(dataURI)`).

### 4.4 Cache Behavior

- Only used when `VISION_HANDOFF_CACHE_ENABLED` is `true`.
- Checked in `analyzeImageViaHandoff` before the upstream call.
- On successful upstream response, the description is stored in the cache.
- Cache stats are exposed in `/healthz` under `visionHandoff.cache` and in the dashboard's Quick Settings.

## 5. Concurrency Queue

Bounded FIFO queue with active request counter.

### 5.1 Globals

```go
var (
    activeRequests int
    requestQueue   []QueueItem
    MAX_QUEUE_SIZE = 256
)
```

**Concurrency safety**: All access to `activeRequests`, `requestQueue` (queue length), and `throttledCount` is serialized through the `Proxy`'s `queueMu` `sync.RWMutex`. Helper accessors `getActiveRequests()`, `getQueueLen()`, `getThrottledCount()`, and `bumpThrottled()` all acquire `queueMu` (read or write as appropriate) so callers never touch these fields directly.

### 5.2 Queue Item

```go
type QueueItem struct {
    Response             http.ResponseWriter
    Payload              map[string]interface{}
    Model                string
    WriteError           ErrorWriter      // OpenAI or Anthropic error format
    WritePassthroughError PassthroughErrorWriter
    Format               string           // "openai" or "anthropic"
    Req                  *http.Request
}
```

### 5.3 Concurrency Gating

The gate limit depends on the **concurrency limit mode** (`getConcurrencyLimitMode()`, backed by `Config.ConcurrencyLimitMode`):

- **Soft Cap** (`"soft"`, default): gate at the soft cap (`limit`). If `limit` is null, fall back to `hard_cap`. If both null, no gating (-1).
- **Hard Cap** (`"hard"`): gate at the hard cap (`hard_cap`). If `hard_cap` is null, fall back to `limit`. If both null, no gating (-1).
- **Manual** (`"manual"`): gate at `manualLimit` (from `Config.ManualConcurrencyLimit`). If `manualLimit` is 0 (uninitialized), fall back to soft cap behavior. Clamp `manualLimit` to `[1, hard_cap]` if `hard_cap` is available. If `hard_cap` is nil, use `manualLimit` as-is (clamped to ≥ 1).

`gateLimit()` acquires `concurrencyLimitMu.RLock()` once, snapshots both `concurrencyLimitMode` and `manualLimit`, releases the lock, then switches on mode. This avoids holding the lock during `GetEffectiveConcurrency()` (which takes its own locks internally).

`getConcurrencyLimitMode()` and `setConcurrencyLimitMode(mode)` are thread-safe accessors protected by `concurrencyLimitMu` (`sync.RWMutex`). Similarly, `getManualLimit()` and `setManualLimit(limit)` (clamps to ≥ 1) use the same mutex.

`NewProxy()` migrates the old `BurstMode` field: if `ConcurrencyLimitMode` is empty, `BurstMode == true` → `"hard"`, `false`/missing → `"soft"`. Calls `setConcurrencyLimitMode(mode)` and `setManualLimit(cfg.ManualConcurrencyLimit)` (if > 0) on startup.

`HandleConfigPost` accepts `concurrencyLimitMode` (validated `"soft"`/`"hard"`/`"manual"`), `manualConcurrencyLimit` (int, clamped ≥ 1), and `slotFreeDelay` (int seconds, clamped ≥ 0). When switching to `"manual"` with `manualLimit == 0`, it auto-initializes to the current soft cap (or hard cap if no soft cap). It syncs `Config.BurstMode = (mode == "hard")` for rollback safety. It always calls `ProcessQueue()` on any mode change (not just `hard`/`manual`) since switching to `soft` from a more restrictive mode can also raise the gate.

If `activeRequests >= gateLimit`:
- If queue is full (`len(requestQueue) >= MAX_QUEUE_SIZE`): increment `throttledCount`, return 503 with `queue_full` error.
- Otherwise: enqueue the request.

### 5.4 `processQueue()`

Called after each request completes (via `OnRequestComplete()`). If `Config.SlotFreeDelay > 0`, `OnRequestComplete()` sleeps that many seconds before decrementing `ActiveRequests` — this delays slot freeing to avoid upstream rate limits when requests complete in rapid succession. After decrementing, it calls `ProcessQueue()` which dequeues items while `activeRequests < gate` and dispatches:
- `format == "anthropic"` → `proxyAnthropicRequest`
- else → `proxyChatRequest`

Each dispatched request increments `activeRequests`, and decrements it on completion (then calls `processQueue` again).

---

## 6. Usage Tracking & Concurrency

### 6.1 `fetchUsage(fresh bool)`

- Fetches `GET {upstreamBaseURL}/usage` with `Authorization: Bearer ***` header.
- 10-second timeout.
- 5-minute cache TTL (`usageCache`).
- `fresh = true` bypasses cache.
- On non-OK response, returns cached data (if any) or nil.
- **Throttle reset**: When the usage window changes (detected via `window.started_at`), resets `ThrottledCount` to 0 under `queueMu.Lock()` and updates `throttledWindowStart`. The lock block is split so that HTTP fetching and JSON parsing happen **outside** the lock, avoiding nested/long-held locks. The `Window` field is nullable (`*WindowInfo`); when `nil`, there is no current window and the reset is skipped.

### 6.2 `fetchConcurrency(fresh bool)`

Extracts from usage data:
- `concurrent = usage.concurrent_sessions ?? 0`
- `limit = limits.concurrency.limit`
- `hard_cap = limits.concurrency.hard_cap`
- `user_id`

Stores in `lastConcurrency`. Invalidates `_effectiveConcurrencyCache`.

### 6.3 `getEffectiveConcurrency()`

Returns `{concurrent, limit, hard_cap, overridden, user_id}`:

- If `overrideConcurrency > 0`:
  - `hard_cap = min(override, apiHardCap)` if `apiHardCap != null`
  - `hard_cap = override` if `apiHardCap == null`
  - `overridden = true`
- Otherwise: use API values directly, `overridden = false`
- Result is cached (`_effectiveConcurrencyCache`).

### 6.4 `bumpThrottled()`

Increments `throttledCount`. Called when queue is full and a 503 is returned.

---

## 7. Upstream Client

### 7.1 Structure

```go
type UpstreamClient struct {
    baseURL    string
    timeout    time.Duration
    apiKey     string
    httpClient *http.Client  // keep-alive, custom transport
}
```

HTTP client configuration:
- Keep-alive enabled
- Max idle connections: 128
- Idle connection timeout: 60 seconds
- Transport timeout: 300 seconds

### 7.2 Methods

**`GetUserInfo() (json.RawMessage, error)`**
- `GET {baseURL}/models/info`
- Headers: `Authorization: Bearer {apiKey}`, `Connection: keep-alive`
- 10-second timeout
- Returns parsed JSON

**`ChatCompletions(body []byte, isStream bool) (*http.Response, error)`**
- `POST {baseURL}/chat/completions`
- Headers: `Authorization: Bearer {apiKey}`, `Content-Type: application/json`, `Accept: text/event-stream` (if stream) or `application/json`, `Connection: keep-alive`
- Timeout: `config.requestTimeout`
- Returns raw `*http.Response`

**`Messages(body []byte, isStream bool) (*http.Response, error)`**
- `POST {baseURL}/messages`
- Same headers as ChatCompletions
- Returns raw `*http.Response`

---

## 8. Model Catalog

### 8.1 `fetchModelCatalog()`

- `GET {baseURL}/models/info` with optional `Authorization` header.
- 15-second timeout.
- Returns parsed JSON (map of model ID → model info object).

### 8.2 `getCatalogData()`

- Cached for 5 minutes (`MODEL_CATALOG_CACHE_TTL`).
- Dedup concurrent fetches: if a fetch is in progress, wait for it instead of starting another.
- On failure, falls back to stale cache if available.
- Populates `ModelInfoMap` (model ID → full info) and `DisplayNameMap` (model ID → display name with `Umans ` prefix stripped).

### 8.3 `applyCatalogData(data)`

For each entry in the data object:
- Store in `ModelInfoMap[id] = info` (write under `catalogMu.Lock()`).
- If `info.display_name` exists, strip leading `Umans ` (case-insensitive) and store in `DisplayNameMap[id]`.
- Invalidate the ordered model IDs cache.

**Concurrency safety**: `ModelInfoMap` and `DisplayNameMap` are protected by a dedicated `catalogMu` `sync.RWMutex`. All writes in `ApplyCatalogData` acquire `catalogMu.Lock()`; all reads in `GetModelInfo`, `GetModelDisplayName`, `GetOrderedModelIds`, `GetEffectiveModels`, and `NeedsVisionHandoff` acquire `catalogMu.RLock()`.

### 8.4 `getOrderedModelIds()`

Returns model IDs sorted by display name (case-insensitive), then by ID as tiebreaker.

### 8.5 `getEffectiveModels()`

- Uses `getOrderedModelIds()` as the authoritative list.
- If catalog is empty, falls back to `config.enabledModels`.
- Filters out `config.disabledModels`.

### 8.6 `getAllCatalogModels()`

Same as `getOrderedModelIds()` (without the disabled filter). Falls back to `config.enabledModels`.

### 8.7 `fetchUpstreamModels()`

- `GET {baseURL}/models` for pricing data.
- 5-minute cache (`upstreamModelsCache`).
- 10-second timeout.
- Returns the `data` array from the response (array of model objects with `id` and `pricing` fields).

### 8.8 `validateApiKey()`

- Calls `upstream.GetUserInfo()`.
- On success: stores in `userInfoCache` (5-min TTL), calls `applyCatalogData(data)`, returns `true`.
- On failure: logs error, returns `false`.

---

## 9. Reasoning Helpers

### 9.1 Reasoning Resolution

**`inferReasoningModeFromCapabilities(reasoningCaps)`**:
- If `supported == true` → `true`
- If `levels` array is non-empty → `true`
- Otherwise → `nil`

Callers default to `true` when this returns `nil`.

### 9.2 Reasoning Level Budgets

```go
var ReasoningLevelBudgets = map[string]int{
    "low":    8000,
    "medium": 16000,
    "high":   16000,
    "max":    32000,
}
```

### 9.3 `buildReasoningVariants(reasoningCaps)`

- Returns `nil` if `supported != true` or `can_disable == false`.
- For each level in `levels` (excluding `"none"`), if a budget exists, creates a variant: `{thinking: {type: "enabled", budget_tokens: budget}}`.
- Returns the variants map or `nil` if no valid levels.

### 9.4 `parseLevels(raw)`

- If array: filter to non-empty strings.
- If string: split on whitespace, filter empty.
- Otherwise: empty slice.

---

## 11. Vision Handoff

Models whose `capabilities.supports_vision === "via-handoff"` cannot process images natively. The proxy intercepts images in requests to such models, sends each image to a vision-capable handoff model, and replaces the image part with a text description before forwarding.

### 11.1 `needsVisionHandoff(resolvedModel) bool`

- Returns `false` if `config.visionHandoffEnabled` is false.
- Looks up `modelInfoMap[resolvedModel]`.
- Returns `true` if `capabilities.supports_vision == "via-handoff"`.

### 11.2 `resolveModelId(requestedModel) string`

- If model starts with `umans-` → return as-is.
- Try prefixing with `umans-`. If the prefixed version is in the effective models list → return prefixed.
- Try direct match in effective models → return it.
- Otherwise → return original.

### 11.3 `collectImageParts(payload) []ImagePart`

Walks `payload.system` (if array) and `payload.messages` (each message's `content` if array), collecting image parts:

- **OpenAI format**: `part.type == "image_url"` with `part.image_url.url` → collect `{container, index, dataUri: url}`.
- **Anthropic format**: `part.type == "image"` with `part.source`:
  - `source.type == "base64"` with `media_type` and `data` → collect with `data:{media_type};base64,{data}` URI.
  - `source.type == "url"` with `url` → collect with URL.
- Recurses into nested `part.content` arrays (e.g. tool_result blocks).

### 11.4 `analyzeImageViaHandoff(dataUri, slot, ...) string`

Makes a non-streaming `chatCompletions` call to the handoff model with:
- System prompt: `config.visionHandoffPrompt` or `DEFAULT_VISION_HANDOFF_PROMPT`.
- User message: text prompt + image_url with the data URI.

On success, extracts the content from the response (string or array of text parts, concatenated).

On failure (HTTP error, network error, empty content): returns an error message string like `[Image analysis failed: ...]`.

### 11.5 `performVisionHandoff(payload, resolvedModel, ...) int`

1. If `!needsVisionHandoff(resolvedModel)` → return 0.
2. Collect image parts (or use pre-collected parts).
3. If no images → return 0.
4. Analyze all images in parallel (`Promise.all` equivalent / goroutines with `sync.WaitGroup`).
5. Replace each image part in-place with:
   ```json
   {
     "type": "text",
     "text": "[Image content — analyzed by vision module, shown as text because the active model cannot see images:]\n{description}"
   }
   ```
   For multiple images, the label is `[Image {i+1} content — ...]`.
6. Return count of images handed off.

### 11.6 SSE Keepalive During Handoff

For streaming requests that will trigger vision handoff:
1. Flush SSE headers (`Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`) and a keepalive comment (`: keepalive — analyzing image via vision handoff\n\n`) before the handoff runs.
2. This prevents client timeouts and duplicate sessions from retries.
3. If headers were already flushed, subsequent error responses are emitted as SSE `data:` events instead of JSON.

### 11.7 Default Vision Handoff Prompt

```
You are an image captioning module. Your output is fed verbatim into another model as the sole visual content of the image — it cannot see the image itself, only your text.

Produce a factual, third-person description of the image contents. Do NOT use first person ("I see..."). Do NOT address the reader. Do NOT speculate about what the user wants.

Cover:
- Type of image (screenshot, photograph, diagram, UI, log, etc.) and overall layout
- All visible elements (objects, UI widgets, people, regions) and their spatial arrangement
- Exact transcription of any visible text, code, or labels (use quotes)
- Salient technical details (file paths, error messages, colors, dimensions, filenames)

Write as a single coherent description, not a bulleted list. Be thorough but concise.
```

---

## 12. Tool Schema Normalization

Normalizes JSON Schema in tool definitions to handle `$ref`, `$defs`, `definitions`, nullable patterns.

### 12.1 `normalizeToolSchemas(tools)`

For each tool in the tools array:
- Get `tool.function.parameters`.
- Extract definitions (from `definitions` and `$defs` keys).
- Run `normalizeSchemaMap` with max depth 12.

Only runs if at least one tool has parameters with `$defs`, `$definitions`, or `$ref`.

### 12.2 `normalizeSchemaMap(node, defs, maxDepth)`

1. If `maxDepth <= 0` → return deep clone of node.
2. Merge any local definitions from this node into `defs`.
3. Try to resolve `$ref` (see below). If resolved → recurse on the resolved schema.
4. Otherwise, iterate all keys:
   - Skip `definitions`, `$defs`, `nullable`.
   - Recurse on each value.
5. Apply simplifications:
   - `simplifyNullableCombinator` for `anyOf` and `oneOf`.
   - `normalizeTypeField`.
   - `normalizeEnumField`.
   - Remove `const: null`.

### 12.3 `tryResolveRef(node, defs)`

- Only resolves if `node.$ref` is a string AND the node has no other keys (exactly one key: `$ref`).
- Supports `#/definitions/{name}` and `#/$defs/{name}`.
- Returns a deep clone of the definition, or `nil` if not found.

### 12.4 `simplifyNullableCombinator(schema, key)`

For `anyOf`/`oneOf` arrays:
- Filter out null schemas (`type: "null"`, `const: null`, `enum: [null]`).
- If empty → delete the key.
- If single remaining item → inline it (delete key, merge item's keys into schema).
- Otherwise → keep filtered array.

### 12.5 `normalizeTypeField(schema)`

If `type` is an array:
- Filter out `"null"` and empty strings.
- If empty → delete `type`.
- Otherwise → set `type` to the first non-null value.

### 12.6 `normalizeEnumField(schema)`

If `enum` is an array:
- Remove `null` entries.
- Deduplicate by `typeof:JSON.stringify` key.
- If empty → delete `enum`.
- Otherwise → keep filtered array.

---

## 13. Payload Normalization

### 13.1 `stripReasoningContent(payload)`

For each message with `role == "assistant"`:
- Delete `reasoning_content` field.
- Delete `reasoningContent` field.

### 13.2 `normalizeThinkingPayload(payload)`

If `payload.thinking` is an object and has `budgetTokens` but not `budget_tokens`:
- Copy `budgetTokens` → `budget_tokens`.
- Delete `budgetTokens`.

This reverses camelCasing done by `@ai-sdk/openai-compatible`.

### 13.3 `limitImagesInMessages(payload, maxImages)`

If `maxImages <= 0` → no-op.

1. Walk `payload.system` (if array) and all message `content` arrays, collecting all image parts (`image_url` or `image` type) with their position (message index, `-1` for system).
2. If count <= maxImages → no-op.
3. Sort collected parts by time (message index, ascending).
4. Replace the oldest `(count - maxImages)` image parts with `{type: "text", text: "(Image previously shared)"}`.

### 13.4 `fingerprintPayload(payload) string`

- Find the first user message.
- Extract its text content via `msgText`.
- Return MD5 hash of the text, truncated to 12 hex chars.
- Returns empty string if no messages or no user message.

### 13.5 `msgText(m) string`

- If `m.content` is a string → return it.
- If `m.content` is an array → return the `text` field of the first part with `type == "text"`, or empty string.

### 13.6 `extractUserPrompt(payload) string`

- Find the **last** user message in `payload.messages`.
- Return its text content, with leading `[/...]` prefix stripped (regex: `^\[[^\]]+\]\s*`).

### 13.7 Reasoning Caps Auto-Think

After model resolution, if `modelInfo.capabilities.reasoning.supported == true` and `can_disable == false`:
- Set `payload.thinking = {type: "adaptive"}`.

---

## 14. Conversation Tracking

### 14.1 Purpose

Track sessions by conversation fingerprint to provide key affinity (same conversation uses same API key) and session numbering for logs.

### 14.2 Structure

```go
type ConversationSession struct {
    TokenIndex   int
    RequestCount int
    SessNum      int64
}
```

- `conversationMap`: `map[string]*ConversationSession`, max 10,000 entries.
- `CONVERSATION_MAP_MAX = 10000`
- `globalSessionCounter`: int64, incrementing. Incremented under the `convMu` mutex (same lock that protects `conversationMap`), so both the OpenAI and Anthropic paths use the same synchronized counter.

### 14.3 `touchConversation(fingerprint)`

- If fingerprint exists in map: delete and re-insert (moves to end = most recent in Go map iteration, though Go maps are unordered; use a separate LRU structure or ordered list for correct eviction).
- Returns the session or nil.

### 14.4 `trackConversationSession(fingerprint, session, alreadyTouched)`

- If map is at capacity (`CONVERSATION_MAP_MAX`) and fingerprint is new:
  - Evict entries until size is 80% of max (`8000`).
- If `alreadyTouched` → just set the fingerprint.
- Otherwise → delete and re-insert (move to end).

### 14.5 Session Flow

For each request:
1. Compute fingerprint.
2. `touchConversation(fingerprint)` → get cached session.
3. Acquire key: if cached session exists, try preferred index first. Otherwise round-robin.
4. If new fingerprint: create new session with `sessNum = ++globalSessionCounter`, `requestCount = 1`.
5. If existing: increment `requestCount`, update `tokenIndex`.
6. Log first prompt if `requestCount == 1` — both the OpenAI (`proxyChatRequest`) and Anthropic (`proxyAnthropicRequest`) paths log the first prompt of a new session (80-char truncated) via `fmt.Fprintf(os.Stderr, "[session %d] first prompt: %s\n", ...)`.

---

## 15. Retry Logic

### 15.1 `retryLoop(fn)`

Retries up to `MAX_RETRIES` (10) times with escalating delays.

- Delay formula: `RETRY_DELAY_MS + (3000 * (attempt - 1))`
  - Attempt 1→2: 3s
  - Attempt 2→3: 6s
  - Attempt 3→4: 9s
  - ...
- The callback `fn` receives `{attempt, isLast}` and returns `{retry: bool, ...}`.

### 15.2 Retry Conditions

Retries occur on:
- **HTTP 500** — regardless of response body
- **HTTP 503** — regardless of response body
- **Network/fetch failures** (treated as 502) — errors thrown before a response is received
- **Empty SSE stream** (OpenAI path only) — upstream returns HTTP 200 + `text/event-stream` but sends 0 data bytes before closing. The proxy peeks at the first chunk before committing SSE headers to the client; if empty, it treats this as a retryable 502, marks the key unhealthy, logs the error, and retries. If all retries fail, returns 502 with `"upstream returned empty stream"`. This prevents the "empty stream with no finish_reason" client error.

### 15.3 Key Rotation on Retry

- On each retry, the current key is marked unhealthy.
- On retry attempts after the first (attempt > 1), if the pool has > 1 key, acquire a fresh key from the pool.

### 15.4 Non-Retryable Errors

HTTP errors other than 500/503 (e.g. 400, 401, 404, 429) are returned immediately.

### 15.5 Throttle Bumping

On upstream 429 or 503 responses (both retryable final attempts and non-retryable errors), `bumpThrottled()` is called to increment the proxy-side throttle counter. This applies to both the OpenAI and Anthropic paths.

### 15.6 Error Logging

On retryable errors (500/503), log to `.logs/errors-{timestamp}.log` with:
- Timestamp
- Error type (`upstream_http_error`)
- Stage (`retryable_attempt` or `final_attempt`)
- Attempt number
- Session info (`sessNum`, `slotName`)
- Request details (method, URL, redacted headers, redacted body)
- Upstream details (URL, method, redacted headers, status, status text, redacted body)

---

## 16. Error Logging

### 16.1 Error Log File

- Directory: `.logs/` (auto-created)
- Filename: `errors-{ISO-timestamp}.log` (colons and dots replaced with dashes)
- One file per session (initialized once, not per-entry)

### 16.2 `redactHeaders(headers)`

Redact sensitive headers by replacing value with `[REDACTED]`:
- Exact match (case-insensitive): `authorization`, `x-api-key`, `cookie`, `set-cookie`, `api-key`
- Substring match (case-insensitive): `auth`, `token`, `key`, `password`, `secret`

### 16.3 `redactBodyJson(body)`

Parse body as JSON, walk the tree, redact:
- Keys matching (case-insensitive): `api_key`, `apikey`, `token`, `password`, `secret`, `authorization` → `[REDACTED]`
- `messages` array → recurse into elements.
- `content` strings longer than 2000 chars → truncated to 2000 + `...[truncated]`.
- Non-JSON bodies returned as-is (or `[unserializable]` on error).

### 16.4 `logHttpError(record)`

Append `--- HTTP ERROR ---\n{json}\n\n` to the error log file.

---

## 17. HTTP Routes

### 17.1 Route Table

| Route | Method | Auth | Description |
|-------|--------|------|-------------|
| `/` or `/dashboard` | GET | No | Serve `dashboard.html` |
| `/api/config` | GET | Yes | Read config (API key masked) |
| `/api/config` | POST | Yes | Update config fields |
| `/api/validate` | GET | Yes | Validate API key |
| `/api/models` | GET | Yes | List models + disabled models + display names |
| `/api/keys` | GET | Yes | List keys (masked) |
| `/api/keys` | POST | Yes | Add/update/delete keys |
| `/api/umans/usage` | GET | Yes | UMANS usage data (5-min cache, `?fresh=1` bypasses) |
| `/api/umans/concurrency` | GET | Yes | Concurrency data (`?fresh=1` bypasses) |
| `/api/umans/usage-history` | GET | Yes | UMANS usage history (proxied from upstream `/usage/history` with 5-min cache; `?fresh=1` bypasses cache). Supports `from`, `to`, `granularity`, `scope` query params. |
|| `/api/umans/user` | GET | Yes | Returns `{loggedIn: true, email: "", user_id: "..."}` — `user_id` sourced from `LastConcurrency.UserID` |
| `/api/restart` | POST | Yes | Triggers graceful exit with code 42 |
| `/healthz` | GET | No | Health check |
| `/v1/models` | GET | Yes | OpenAI-format model list with pricing |
| `/v1/models/info` | GET | Yes | Raw model catalog (`modelInfoMap`) |
| `/v1/chat/completions` | POST | Yes | OpenAI chat (concurrency-queued) |
| `/v1/messages` or `/messages` | POST | Yes | Anthropic Messages API (concurrency-queued) |
| `/api/bg` | GET | Yes | Bing wallpaper proxy (cached daily) |
| `/api/bg-wallhaven` | GET | Yes | Wallhaven wallpaper proxy (cached hourly) |
| `/api/bg-freegen` | GET/POST | Yes | Removed (return 404) |

### 17.2 Authentication

`Authorized(req)` — behavior depends on `ApiKeyMode` (§27):

- **`passthrough`**: Accept any request that carries a client API key (`extractClientAPIKey(req) != ""`). The client's key will be passed through to upstream.
- **`smart`**: Always accept (`return true`). Either the client's key will be used, or the proxy's own key.
- **`managed`**: Validate against `config.APIKeys`:
  - If `config.APIKeys` is empty → allow all (open access).
  - Check `X-Api-Key` header against `config.APIKeys`.
  - Check `Authorization: Bearer *** header against `config.APIKeys`.
  - If neither matches → 401.

### 17.3 Request Body Reading

`readBody(req)`:
- Collect chunks, enforce `MAX_BODY_SIZE` (5 MB).
- On overflow: pause request, reject with 400.
- On abort: reject with error.
- Returns body as string.

### 17.4 Response Writers

**`writeJSON(res, status, payload)`**:
- If headers already sent (SSE keepalive case): emit payload as SSE `data:` event and end response.
- Otherwise: write `Content-Type: application/json`, send JSON.
- On encode failure: send 500 with error JSON.

**`writeOpenAIError(res, status, message, type, code)`**:
- Payload: `{error: {message, type, code?}}`
- Uses `writeJSON`.

**`writeAnthropicError(res, status, message, type)`**:
- Payload: `{type: "error", error: {type, message}}`
- Type defaults from status: 400→`invalid_request_error`, 401→`authentication_error`, 403→`permission_error`, 404→`not_found_error`, 429→`rate_limit_error`, 500→`api_error`, 503→`overloaded_error`.

**`writePassthroughError(res, status, body)`** (OpenAI path):
- Parse upstream error body as JSON.
- Extract `error.message` or `message`.
- Extract `error.type` or default `upstream_error`.
- Extract `error.code`.
- Write as OpenAI error.

**`writeAnthropicPassthroughError(res, status, body)`**:
- Parse upstream error body as JSON.
- Extract `error.message` or `message`.
- Extract `error.type` or default `api_error`.
- Write as Anthropic error.

---

## 18. Dashboard Serving

### 18.1 `getDashboardHtml(path)`

- Read `dashboard.html` from the binary's directory.
- Cache by file mtime: if mtime unchanged, return cached content.
- On read error → return nil.

### 18.2 Dashboard Route Handler

1. Get dashboard HTML (from cache or file).
2. If not found → 404.
3. Determine wallpaper style:
   - `none`: Inject `<style>body{background:#0d1117}</style>` before `</head>`.
   - `bing` or `wallhaven`: If a cached wallpaper file exists (`.cache/wallpaper.jpg` or `.cache/wallpaper-haven.jpg`), embed as base64 in a `<style>` tag with full background properties:
     ```html
     <style>body{background-image:url('data:{contentType};base64,{data}')!important;
       background-size:cover!important;background-position:center!important;
       background-repeat:no-repeat!important;background-attachment:fixed!important;
       background-color:#070912}</style>
     ```
     The `contentType` is determined by `imageContentType()` (§30.3). If no cached file, fall back to dark background (`#0d1117`).
4. Inject before `</head>`; if no `</head>` found, append to the end.
5. Serve as `text/html`.

---

## 19. OpenAI Chat Completions Pipeline (`proxyChatRequest`)

Full request pipeline for `/v1/chat/completions`:

1. **Config snapshot**: Acquire `configMu.RLock()` briefly to snapshot the needed fields (`ApiKeyMode`, `MaxImages`, `UpstreamBaseURL`), then release the lock immediately. This avoids holding the RLock for the entire upstream request lifecycle, which would block `HandleConfigPost` (which needs `configMu.Lock()`) for the full duration of LLM completions. The snapshotted values (`cfgMode`, `cfgMaxImages`, `cfgUpstreamURL`) are used throughout the rest of the pipeline.
2. **Key acquire**: Mode-aware key selection (§27):
   - Read `clientKey = extractClientAPIKey(r)`.
   - If mode is `passthrough` or `smart` **and** `clientKey != ""`: set `usingClientKey = true`, call `p.Upstream.SetAPIKey(clientKey)`. Pool acquire is skipped entirely.
   - If mode is `passthrough` with no client key: reject with 401 `authentication_error`.
   - Otherwise (managed, or smart/managed with no client key): acquire from pool via `KeyPool.Acquire(preferredIndex)`. 503 if no healthy keys.
3. **Session tracking**: New or existing session. Log first prompt.
4. **`stripReasoningContent`**: Remove reasoning fields from assistant messages.
5. **Model resolution**: `resolveModelId(requestedModel)`.
6. **Tool normalization**: If tools have `$ref`/`$defs`/`$definitions`, run `normalizeToolSchemas`.
7. **Image limit**: If not a handoff model, run `limitImagesInMessages`.
8. **Reasoning caps**: If `supported && !can_disable`, set `thinking = {type: "adaptive"}`.
9. **`normalizeThinkingPayload`**: Fix camelCase `budgetTokens` → `budget_tokens`.
10. **Vision handoff preparation**: If handoff needed and streaming, flush SSE headers + keepalive comment.
11. **`performVisionHandoff`**: Replace images with text descriptions.
12. **Retry loop**:
    a. On retry > 1: try to acquire a fresh key **only if** `!usingClientKey` and pool > 1.
    b. Call `upstream.chatCompletions(payload)`.
    c. On network error: mark unhealthy (only if `!usingClientKey`), retry (or 502 if last attempt).
    d. On HTTP 2xx:
       - Mark key healthy (only if `!usingClientKey`).
       - If mode is `passthrough` or `smart` and `clientKey != ""`: call `setLastClientKey(clientKey)` to record last-known-good.
       - If SSE content type:
          - Peek at first chunk to detect empty streams. If 0 bytes + EOF, treat as retryable 502 (log, mark unhealthy, retry or return 502 if last attempt).
          - If data exists, commit SSE headers, write peeked bytes, then pipe the rest of the body to the client.
        - If JSON:
          - Parse response body.
          - If streaming was requested (but got JSON): wrap as SSE chunk.
         - Write response, cache non-streaming responses.
    e. On HTTP 500/503: mark unhealthy (only if `!usingClientKey`), log error, retry (or pass through error if last).
    f. On other HTTP errors: pass through error immediately.

---

## 20. Anthropic Messages Pipeline (`proxyAnthropicRequest`)

Pass-through for `/v1/messages` and `/messages`:

1. **Config snapshot**: Acquire `configMu.RLock()` briefly to snapshot the needed fields (`ApiKeyMode`, `MaxImages`), then release the lock immediately. Same rationale as §19 — avoids blocking config writes during the upstream call. The snapshotted values (`cfgMode`, `cfgMaxImages`) are used throughout the pipeline.
2. **Nil guards**: All access to `KeyPool` and `Upstream` is guarded — if either is nil, a 503/500 error is returned immediately rather than panicking.
3. **Key acquire**: Mode-aware key selection (§27), same logic as OpenAI path (§19 step 2):
   - If mode is `passthrough` or `smart` **and** `clientKey != ""`: set `usingClientKey = true`, call `p.Upstream.SetAPIKey(clientKey)`.
   - If mode is `passthrough` with no client key: reject with 401 `authentication_error`.
   - Otherwise: acquire from pool. 503 if no healthy keys.
4. **Session tracking**: Same as OpenAI path. Log first prompt of a new session (80-char truncated).
5. **`normalizeThinkingPayload`**: Fix camelCase.
6. **Model resolution**: `resolveModelId(requestedModel)`, set `payload.model`.
7. **Image limit**: Apply `limitImagesInMessages(payload, config.maxImages)` to cap the number of images before forwarding.
8. **Vision handoff**: Run `performVisionHandoff` (same as OpenAI path).
9. **Upstream call**: `upstream.messages(payload)` — direct pass-through, no retry loop.
10. **Response body lifecycle**: `resp.Body` is closed explicitly in the error path (no `defer`), avoiding a double-close when both an error branch and a deferred close run.
11. **On HTTP >= 400**:
    - Log error.
    - `writeAnthropicPassthroughError`.
    - Mark unhealthy only on 503 (not 500, which is usually a payload issue) — **only if** `!usingClientKey`.
    - Bump throttled on 429 or 503.
12. **On success**:
    - Mark key healthy (only if `!usingClientKey`).
    - If mode is `passthrough` or `smart` and `clientKey != ""`: call `setLastClientKey(clientKey)`.
    - Write upstream status + headers (`Content-Type`, `Cache-Control: no-cache`, `Connection: keep-alive`).
    - Pipe body to response with backpressure + client disconnect detection.
13. **On network error**:
    - `writeAnthropicPassthroughError` with 502.
    - Mark unhealthy (502) — only if `!usingClientKey`.

**No cache, no retry for Anthropic path.**

---

## 21. Stream/Body Utilities

### 21.1 `readBody(req) string` / `readBodyText(body) string`

Reads the full body from an `*http.Request` or raw body string. Enforces `MAX_BODY_SIZE` (5 MB).

- Collects chunks, concatenates.
- On overflow: pause request, reject with 400.
- On abort: reject with error.
- Returns body as string.
- **EOF handling**: All body-reading paths (`ReadBody`, `readBodyText`, `pipeBodyToResponse`) use `errors.Is(err, io.EOF)` to detect clean stream end rather than direct `==` comparison.

### 21.2 `pipeBodyToResponse(body, res)` → `(int, error)`

Pipes upstream response body to HTTP response with:
- **Client disconnect detection**: Checks `r.Context().Done()` before each read; returns error if client disconnected.
- **Flushing**: Flushes after each write via `http.Flusher`.
- **Byte counting**: Returns total bytes written so caller can detect empty streams.
- **EOF handling**: Uses `errors.Is(err, io.EOF)` for clean stream-end detection.
- **Cleanup**: Defers `body.Close()`.
- Returns `(totalWritten, error)`.

---

## 22. Health Check (`/healthz`)

Returns:

```json
{
  "ok": true,
  "started_at": "ISO timestamp",
  "uptime_sec": 12345,
  "api_key_valid": true,
  "provider": "umans",
  "token_state": [...],
  "valid_tokens": 2,
  "total_tokens": 3,
  "models_count": 15,
  "runtime": "go",
  "runtime_version": "go1.24",
  "port": 8084,
  "visionHandoff": {
    "enabled": false,
    "cacheEnabled": false,
    "cache": {
      "size": 0,
      "maxSize": 50,
      "ttlMs": 86400000,
      "hits": 0,
      "misses": 0,
      "evictions": 0
    }
  }
}
```

- Uses cached user info (refreshes if > 5 min old).
- `runtime` is `"go"`.
- `runtime_version` is the Go version (e.g. `go1.24`).

---

## 23. Models Endpoint (`/v1/models`)

OpenAI-format model list with pricing.

For each effective model:
- `id`, `object: "model"`, `created` (server start timestamp), `owned_by: "umans"`, `root`, `permission: []`.
- `display_name`: from `modelDisplayNameMap` or stripped `umans-` prefix.
- If `capabilities.context_window` is a positive number:
  - `context_length = context_window`
  - `max_output_tokens`: `recommended_max_tokens` → `max_completion_tokens` → `context_window` (first available).
- If upstream pricing exists for the model:
  - `pricing.prompt = input / 1_000_000` (per-token, if input is number)
  - `pricing.completion = output / 1_000_000` (per-token, if output is number)
  - Strip undefined pricing keys.
- **Important**: `max_output_tokens` is a top-level field, not nested inside a `limit` object. This avoids key collisions with Hermes' pricing alias extractor.

---

## 24. Opencode Config Discovery & Setup

### 24.1 `discoverOpencodeConfigs()`

- 5-minute cache.
- On Linux/macOS: Check `~/.config/opencode/opencode.json` and `~/.opencode/opencode.json`. Return existing paths.
- On Windows: Scan `C:\Users\*` for `.opencode/opencode.json` and `.config/opencode/opencode.json`. Also check systemprofile.
- Deduplicate results.
- Only return paths that exist on disk.

### 24.2 `debouncedSetupOpencodeConfig()`

Debounced 500ms. Prevents concurrent runs via a `pending` flag.

### 24.3 `setupOpencodeConfig()`

For each discovered `opencode.json`:

1. Build models map from `getEffectiveModels()`:
   - For each model, look up `modelInfoMap`.
   - Entry: `{id, name, reasoning, variants?, limit?, temperature: true, tool_call?, attachment?, modalities}`.
   - `reasoning`: from `inferReasoningModeFromCapabilities` (default `true` when it returns `nil`).
   - `variants`: from `buildReasoningVariants`.
   - `limit`: `{context, output}` if context_window is available.
   - `tool_call`: `caps.supports_tools` if boolean.
   - `attachment`: `caps.supports_vision` if boolean.
   - `modalities.input`: `["text"]` + `["image"]` if `supports_vision`.
   - `modalities.output`: `["text"]`.

2. Provider entry:
   ```json
   {
     "npm": "@ai-sdk/openai-compatible",
     "name": "Umans.AI-Dash",
     "options": {
       "baseURL": "http://localhost:{port}/v1",
       "apiKey": "{first proxy API key or 'umans-proxy'}"
     },
     "models": {...}
   }
   ```
   (No Sleev variant — Sleev is removed.)

3. Read existing config file. Parse as JSON. If the file is not valid JSON, start with `{}`.
   - If the config file does not exist, skip this path entirely — the user does not use opencode, so no config should be created.
4. Create backup `openconfig.b4umans.json` on first edit (if file existed).
5. Set `existing.provider["umans"] = providerEntry`.
6. Ensure `existing.instructions` includes `"AGENTS.md"` and `"skills.md"`.
7. Write back as pretty-printed JSON.
8. Log config path and model count.

### 24.4 First-Run Browser Open

On first run (new config file or first backup), open the dashboard URL in the browser:
- Linux: `xdg-open`
- macOS: `open`
- Windows: `start`

---

## 25. Config API Endpoints

### 25.1 `GET /api/config`

Returns config with API key masked:

```json
{
  "listenAddr": "127.0.0.1:8084",
  "upstreamBaseURL": "https://api.code.umans.ai/v1",
  "apiKey": "sk-1234567...wxyz",
  "enabledModels": [],
  "modelDisplayNames": {},
  "overrideConcurrency": 0,
  "maxImages": 9,
  "disabledModels": [],
  "visionHandoffEnabled": false,
  "visionHandoffModel": "umans-coder",
  "visionHandoffPrompt": "",
  "visionHandoffCacheEnabled": false,
  "wallpaperSource": "bing",
  "apikeyMode": "smart",
  "concurrencyLimitMode": "soft",
  "manualConcurrencyLimit": 0,
  "slotFreeDelay": 0
}
```

### 25.2 `POST /api/config`

Accepts partial config updates. Updates any provided fields:
- `apiKey`, `apiKeys`, `listenAddr`, `enabledModels`, `modelDisplayNames`, `wallpaperSource` (`none`, `bing`, or `wallhaven`), `overrideConcurrency`, `maxImages`, `disabledModels`, `visionHandoffEnabled`, `visionHandoffModel`, `visionHandoffPrompt`, `visionHandoffCacheEnabled`, `apikeyMode` (`smart`, `managed`, or `passthrough`), `concurrencyLimitMode` (`soft`, `hard`, or `manual` — validates, syncs `BurstMode` for rollback, auto-inits `manualLimit` to soft cap when switching to manual with limit 0, always triggers `ProcessQueue()`), `manualConcurrencyLimit` (int, clamped ≥ 1, triggers `ProcessQueue()`), `slotFreeDelay` (int seconds, clamped ≥ 0), `keys` (rebuilds key pool).

After update: `debouncedSaveConfig()` + `debouncedSetupOpencodeConfig()`.

**Response includes `restartRequired`**: If the `listenAddr` field is changed in the POST, the response sets `restartRequired: true` to signal the dashboard that a proxy restart is needed for the new port to take effect. The response also echoes back the full config (same fields as `GET /api/config`).

---

## 26. Key Management API

### 26.1 `GET /api/keys`

Returns:
```json
{
  "keys": [...],
  "safe": [
    { "name": "Default", "token_masked": "sk-1234...wxyz", "has_token": true, "has_session": false }
  ]
}
```

### 26.2 `POST /api/keys`

Actions:
- `add`: Push new key `{name, key, session: ""}`. Set as `config.apiKey` if none set. Rebuild key pool.
- `update`: Update key at index. If index 0 and key present, set as `config.apiKey`. Rebuild pool.
- `delete`: Remove key at index. If empty, push a placeholder `{name: "Key 1", key: "", session: ""}`. If index 0, update `config.apiKey`. Rebuild pool.

All actions: `debouncedSaveConfig()` + `debouncedSetupOpencodeConfig()`.

---

## 27. API Key Mode

Controls how the proxy handles API keys for authentication, request proxying, and dashboard (usage/history) calls. Configured via `API_KEY_MODE` (§2.1), defaults to `"smart"` via `applyDefaultApiKeyMode()` (§2.3.1).

### 27.1 Modes

| Mode | Auth | Proxy Key Source | Dashboard/Usage Key Source |
|------|------|-----------------|---------------------------|
| `smart` (default) | Always accept | Client key if provided, else pool | Last-known-good client key, else pool |
| `managed` | Validate against `APIKeys` | Pool (round-robin) | Pool (first available) |
| `passthrough` | Require client key | Client key only | Last-known-good client key |

### 27.2 `extractClientAPIKey(req) string`

Reads the API key from the incoming client request:
1. Check `X-Api-Key` header (trimmed). If present → return it.
2. Check `Authorization: Bearer *** header (trimmed). If present → return the bearer token.
3. Return empty string if neither found.

### 27.3 `applyDefaultApiKeyMode(cfg *Config)`

Called at the end of `LoadConfig`. If `cfg.ApiKeyMode` is empty, sets it to `"smart"`.

### 27.4 `upstreamAPIKeyForDashboard() string`

Returns the API key used for upstream dashboard calls (usage, concurrency, history fetching). Selection per mode:

- **`passthrough`**: Return `getLastClientKey()`.
- **`smart`**: Return `getLastClientKey()` if non-empty; otherwise fall through to pool.
- **`managed`** (or smart with no client key): Acquire from `KeyPool.Acquire(-1)`, mark it healthy, return its key. Fall back to `Config.APIKey`, then `Config.APIKeys[0]`.

### 27.5 Last-Known-Good Client Key Tracking

The proxy maintains a single last-known-good client key (`lastClientKey` field on `Proxy`, protected by `lastClientKeyMu sync.RWMutex`):

- **`setLastClientKey(key)`**: Records a client key. Called on successful upstream responses in passthrough/smart mode when `clientKey != ""`. No-op if key is empty.
- **`getLastClientKey() string`**: Returns the last-known-good client key.

### 27.6 Request Proxying Behavior (`usingClientKey` flag)

Both `proxyChatRequest` (§19) and `proxyAnthropicRequest` (§20) use a `usingClientKey` boolean to skip pool operations:

- If mode is `passthrough` or `smart` **and** `clientKey != ""`: `usingClientKey = true`. The client's key is set directly on the upstream client via `SetAPIKey()`. Pool acquire, mark healthy/unhealthy, and key rotation are all skipped.
- If mode is `passthrough` with no client key: reject with 401.
- Otherwise (managed, or smart/managed with no client key): acquire from pool as normal.

### 27.7 Dashboard Exposure

Both `HandleConfigGet` and `HandleConfigPost` expose and accept the field as `apikeyMode` (camelCase JSON). `HandleConfigPost` validates that the value is one of `smart`, `managed`, or `passthrough`.

---

## 28. Usage API Endpoints

### 28.1 `GET /api/umans/usage`

Returns:
```json
{
  "usage": { "requests_in_window": 246, "tokens_in": 24000000, "tokens_out": 11732073, "tokens_cached": 9360000 },
  "window": { "started_at": "..." },
  "plan": { "display_name": "..." },
  "throttled": 0
}
```

- `window` may be `null` when no usage window is active; the dashboard renders a `--` placeholder for Start Time in that case.

- `?fresh=1` bypasses cache.
- `throttled` is the proxy-side count of 503 queue-full rejections.

### 28.2 `GET /api/umans/concurrency`

Returns:
```json
{
  "concurrent": 3,
  "limit": 8,
  "hard_cap": 16,
  "user_id": "...",
  "overridden": false,
  "active": 2,
  "queued": 1,
  "concurrency_limit_mode": "soft",
  "manual_limit": 0
}
```

- `?fresh=1` bypasses cache.
- `active` is the proxy's `activeRequests` count.
- `queued` is the `requestQueue` length.
- `concurrency_limit_mode` is the current concurrency limit mode (`soft`, `hard`, or `manual`).
- `manual_limit` is the current manual concurrency limit (used only when mode is `manual`).

---

## 29. Restart API

### `POST /api/restart`

1. Respond `{success: true, message: "Restarting..."}`.
2. After 500ms: close HTTP server, exit with code 42.

The external process manager (e.g. systemd) should restart on exit code 42.

---

## 30. Wallpaper Proxy Endpoints

### 30.1 `GET /api/bg` — Bing Wallpaper

Fetches the daily Bing wallpaper via the peapix.com API.

1. Check cache: `.cache/wallpaper.jpg`. If file exists and was modified today, serve cached JPEG.
2. If cache miss/expired:
   - `GET https://peapix.com/bing/feed` with browser `User-Agent`, 15s timeout.
   - Parse JSON response, extract first item's `fullUrl` (or `imageUrl` or `url`).
   - **UHD upgrade**: Call `upgradePeapixResolution(imageURL)` to regex-replace the `_NNNN` suffix with `_3840` (3840×2160 UHD). Pattern: `_\d+\.(jpg|jpeg|png)$`.
   - Download the image (30s timeout, browser User-Agent).
   - Validate: `isJPEG(imageData)` checks for `0xFF 0xD8` magic bytes.
   - Save to `.cache/wallpaper.jpg`.
   - Serve as `image/jpeg`.
3. On fetch error: serve cached file if available, else 404.
4. Set `Expires` header to end of today (UTC).

Cache TTL: 24 hours (daily).

### 30.2 `GET /api/bg-wallhaven` — Wallhaven Wallpaper

Fetches a random top-listed wallpaper from wallhaven.cc.

1. Check cache: `.cache/wallpaper-haven.jpg`. If file exists and is < 1 hour old, serve cached JPEG.
2. If cache miss/expired:
   - `GET https://wallhaven.cc/api/v1/search?categories=100&purity=100&topRange=1M&sorting=toplist&order=desc&page=3&atleast=2560x1440` with `User-Agent: umans-proxy/1.0`, 15s timeout. The `atleast=2560x1440` parameter filters for high-resolution wallpapers.
   - Parse JSON, pick random entry from `data` array.
   - Download `path` URL (30s timeout, browser User-Agent).
   - Validate: `isValidImage(imageData)` checks magic bytes for JPEG, PNG, or WebP (§30.3).
   - Save to `.cache/wallpaper-haven.jpg`.
   - Serve with correct content type via `serveWallpaperImageTyped()` + `imageContentType()`.
3. On fetch error: serve cached file if available, else 404.

Cache TTL: 1 hour.

### 30.3 Image Format Utilities

**`upgradePeapixResolution(imageURL) string`**: Regex-replaces `_NNNN` suffix (e.g. `_1920`) with `_3840` in peapix URLs for UHD output. Falls back to original URL if pattern doesn't match.

**`isValidImage(data) bool`**: Validates freshly downloaded wallpapers by checking magic bytes:
- JPEG: `0xFF 0xD8`
- PNG: `0x89 0x50 0x4E 0x47` (`‰PNG`)
- WebP: `0x52 0x49 0x46 0x46 ... 0x57 0x45 0x42 0x50` (`RIFF....WEBP`)

**`imageContentType(data) string`**: Returns MIME type based on magic bytes:
- PNG → `image/png`
- WebP → `image/webp`
- Default (including JPEG) → `image/jpeg`

**`serveWallpaperImageTyped(w, data, expires, contentType)`**: Writes image data to response with caller-specified content type and optional `Expires` header.

**`isJPEG(data) bool`**: Checks JPEG magic bytes (`0xFF 0xD8`) only. Used for Bing wallpaper validation.

---

## 31. Startup Sequence

1. `loadConfig()` — Load `.config/config.json` + env var overrides.
2. Initialize `ImageHandoffCache` (50 entries, 24h TTL or configured).
3. Restore concurrency limit mode from config: `setConcurrencyLimitMode(mode)` (migrates old `BurstMode` → `ConcurrencyLimitMode`). Restore `manualLimit` if `ManualConcurrencyLimit > 0`.
4. Initialize `KeyPool` from `config.keys` (or single default key).
5. Initialize `UpstreamClient`.
6. `validateApiKey()` — Verify via `/v1/models/info`, populate `modelInfoMap` + `modelDisplayNameMap`.
7. `fetchConcurrency()` — Fetch concurrent sessions & limit.
8. `http.createServer(handleRequest).listen(port, "127.0.0.1")` — Start HTTP server with port-retry on `EADDRINUSE` (3 retries on same port, then increment port).
9. `setupOpencodeConfig()` — Discover + write models to opencode configs, deferred 100ms after server starts listening.
10. Graceful shutdown hooks (SIGINT, SIGTERM) — close server and exit.

### 30.1 Port Retry

On `EADDRINUSE`:
- Retry up to 3 times on the same port (2-second delay between retries).
- After 3 failures, increment port by 1 and try again.
- Reset retry count on port change.

---

## 32. Dashboard HTML

The dashboard is a standalone `dashboard.html` file, ported from the original JavaScript proxy with excluded features removed. The Go binary serves it at `/` and `/dashboard`.

### Dashboard serving specifics:
- Read and cache by mtime.
- Inject a `<style>` tag with a dark background (`#0d1117`) before `</head>`. If `wallpaperSource` is `bing` or `wallhaven` and a cached wallpaper exists, embed it as base64 `background-image` with full background properties (`cover`, `no-repeat`, `center`, `fixed`, `background-color: #070912`) using the correct content type from `imageContentType()`.
- Content-Type: `text/html`.

### Ported features:
- 5-hour Window card (Requests, Throttled, Cached %, Error %, Start Time, Tokens In/Out, plan badge)
- Current Concurrency card (Active, Queued, Limit, Burst, dual-fill progress bar with soft-cap/burst zones, upstream overlay, percentage, detail grid)
- **Concurrency Limit selector** (backend-gated via `CONCURRENCY_LIMIT_MODE` config field): Three-button group (Soft Cap / Hard Cap / Manual) replacing the old Burst Mode On/Off toggle. Soft Cap gates at `limit`, Hard Cap gates at `hard_cap`, Manual reveals a slider (1 to `hard_cap`) for a custom limit. `setConcurrencyLimitMode(mode)` POSTs `{concurrencyLimitMode: mode}` to `/api/config`, reverts UI on failure, calls `fetchConcurrency()` on success. `onManualLimitChange(value)` POSTs `{manualConcurrencyLimit: value}` on slider release. `loadConfig()` reads `concurrencyLimitMode` and `manualConcurrencyLimit` from the backend. `fetchConcurrency()` reads `concurrency_limit_mode` and `manual_limit` from the concurrency API response to determine `barMax` for the progress bar and dynamically updates the slider max to the current hard cap. The redundant "Queued" card in the detail grid below the bar was removed (the count is already shown in the stat card above).
- **Slot Free Delay input** — number input in Quick Settings (positive integer, default 0). POSTs `{slotFreeDelay: value}` to `/api/config` on change.
- Usage History card (bar chart with Y-axis labels, dashed grid lines, X-axis labels, click-to-filter table, expandable per-model breakdown, sortable headers, metric toggle Tokens/Requests, status legend)
- User ID in header bar (click-to-reveal masking)
- API Key section (key pool display with status badges, collapsible)
- **API Key Mode toggle** (Smart/Managed/Pass-Through button group in Quick Settings). In `passthrough` mode, the API Key (token pool) card is hidden.
- Models section (enable/disable toggle per model via `DISABLED_MODELS`, collapsible)
- Quick Settings (Automatic Refresh interval, Wallpaper selector (None/Bing/Wallhaven), Concurrency Limit selector (Soft Cap/Hard Cap/Manual with slider), Slot Free Delay input, API Key Mode selector, Vision Handoff toggle with info tooltip, Handoff Cache toggle (shown only when Vision Handoff is enabled) with cache stats line)
- Quick Actions (Check Health, Test Connection, Manual Refresh, Restart Proxy)
- Environment (Runtime, Port, Started At)
- Key Management Modal (add/edit/delete API keys with inline editing, account info with User ID)
- Glass UI (procedural SVG filter-based glassmorphism via `initLiquidGlass()`)
- **Unified refresh cycle**: `setRefreshInterval(seconds, skipImmediate)` controls all dashboard content (status, usage, concurrency, history) on a single interval. The selected interval is persisted in localStorage (`refreshInterval`, default 30s). Options: 30s, 1m, 2m, 5m. When `skipImmediate` is false (or omitted), changing the interval immediately triggers all fetches (`updateStatus`, `fetchUsage`, `fetchConcurrency`, `fetchHistory`). When `skipImmediate` is true, only the timer is set — used during init since `loadConfig` and `setHistoryRange` already fetch.
- Dashboard always fetches usage and concurrency with `?fresh=1`
- **Dashboard load performance**: `loadConfig()` fetches `/api/config` and `/api/models` in parallel; `/healthz` is fetched fire-and-forget in the background (not awaited) because it hits upstream and can be slow. The wallpaper loader is hidden before `fetchUsage()` (also fire-and-forget) so the dashboard renders as soon as config + models + wallpaper are ready. `fetchConcurrency()` is called on `DOMContentLoaded` (was previously missing, which delayed the concurrency display by up to 30s until the first refresh cycle).
- **Wallpaper None fix**: `clearWallpaper()` uses `document.body.style.setProperty('background-image', 'none', 'important')` to override the server-injected `!important` CSS (the inline `<style>` tag injected by the Go binary for `wallpaperSource: "none"` or cached wallpapers).
- **Chevron icon fix**: `toggleSection()` swaps `bi-chevron-down` ↔ `bi-chevron-right` Bootstrap Icons classes directly on the toggle icon element, rather than using CSS `transform: rotate()` rotation. This ensures the icon visually matches the expanded/collapsed state.
- **Animated section collapse/expand**: `toggleSection()` drives a JS height transition (`scrollHeight` → `0` on collapse, `0` → `scrollHeight` on expand) over 300ms. CSS `.collapse-section.collapsed` sets `height:0;opacity:0;padding:0`; `display:none` is applied inline by JS after the `transitionend` event fires, so the collapse animation is visible (instant `display:none` in CSS would skip it). Pre-collapsed sections get `display:none` on `DOMContentLoaded`.
- **Wallpaper init**: `setWallpaper(source, skipConfigSave)` accepts a second parameter to skip the redundant config POST during initial load (when the config was just fetched and the wallpaper source is already persisted).
- Toast notifications
- Bootstrap 5 + Bootstrap Icons (via CDN)

### Removed from original dashboard:
- FreeGen prompt input + Generate button — FreeGen excluded
- Context Compression (Sleev) toggle — Sleev excluded
- FreeGen wallpaper option in wallpaper selector — FreeGen excluded

---

## 33. Removed Features

The following features from the original proxy are **not** included in the Go rewrite:

| Feature | Reason |
|---------|--------|
| Sleev context-compression gateway | Explicitly excluded per requirements |
| FreeGen AI wallpaper generation | Explicitly excluded per requirements |
| General response cache | Removed from source proxy; only ImageHandoffCache remains |
| `/api/sleev` endpoint | Sleev removed |
| `/api/bg-freegen` endpoint | FreeGen removed |
| `/api/cache` endpoint | Response cache removed |
| `/api/i18n` endpoint & `I18N_STRINGS` catalog | Removed — dead code; unused by dashboard |

---

## 34. Error Handling

### 34.1 `uncaughtException` / `unhandledRejection` Equivalent

Go doesn't have direct equivalents. Use `recover()` in HTTP handlers to catch panics and return 500 errors instead of crashing the process.

### 34.2 Graceful Shutdown

On SIGINT/SIGTERM, the `Shutdown()` method performs a two-phase drain:
1. Log the signal.
2. **Proxy-level drain**: Poll `ActiveRequests` (under `queueMu.RLock`) every 100ms until it reaches 0, with a 5-second overall timeout. This allows in-flight proxied requests to complete before closing the HTTP listener.
3. **HTTP server shutdown**: Call `httpServer.Shutdown()` with a 5-second context timeout to force-close any remaining hanging connections.
4. Flush and close the error log file.
5. Exit with code 0 (via `exitFn` or `os.Exit(0)`).

On `/api/restart`:
1. Close HTTP server.
2. Exit with code 42.

---

## 35. Constant Reference

```go
const (
    UMANS_API_BASE          = "https://api.code.umans.ai/v1"
    API_KEY_ENV_VAR         = "UMANS_API_KEY"
    MAX_RETRIES             = 10
    RETRY_DELAY_MS          = 3000  // 3 seconds
    MAX_QUEUE_SIZE          = 256
    MAX_BODY_SIZE           = 5 * 1024 * 1024  // 5 MB
    CONVERSATION_MAP_MAX    = 10000
    CONV_MAP_EVICT_TARGET   = 8000  // 80% of CONVERSATION_MAP_MAX
    MODEL_CATALOG_CACHE_TTL = 5 * time.Minute
    USAGE_CACHE_TTL        = 5 * time.Minute
    OPENCODE_CONFIG_CACHE_TTL = 5 * time.Minute
    UPSTREAM_MODELS_CACHE_TTL = 5 * time.Minute
    USER_INFO_CACHE_TTL     = 5 * time.Minute
    USER_INFO_CACHE_TIMEOUT  = 10 * time.Second
)
```

```go
var ReasoningLevelBudgets = map[string]int{
    "low":    8000,
    "medium": 16000,
    "high":   16000,
    "max":    32000,
}
```

