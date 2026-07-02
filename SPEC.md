# UMANS-Dash Go Rewrite — Technical Specification

> **Source**: `proxy.js` (3,326 lines) in https://github.com/sukarn-m/umans-dash.  
> **Target**: Go binary, zero external dependencies (stdlib only).  
> **Scope**: All features except Sleev gateway and FreeGen wallpaper generation.

---

## 1. Overview

A local HTTP reverse proxy that sits between OpenAI/Anthropic-compatible clients (e.g. opencode) and the UMANS AI upstream API (`https://api.code.umans.ai/v1`). The proxy provides multi-key pool management, concurrency limiting with a bounded queue, response caching, retry logic with key rotation, vision handoff for models that cannot see images, tool schema normalization, model catalog management, usage tracking, opencode config auto-discovery, and a dashboard.

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
}
```

### 2.2 Defaults

| Key | Default | Description |
|-----|---------|-------------|
| `LISTEN_ADDR` | `127.0.0.1:8084` | Listen address |
| `UPSTREAM_BASE_URL` | `https://api.code.umans.ai/v1` | Upstream API base |
| `REQUEST_TIMEOUT` | `15m` | Upstream request timeout (parsed via `parseDuration`) |
| `OVERRIDE_CONCURRENCY` | `0` | Override concurrency limit (0 = use API limit) |
| `MAX_IMAGES` | `9` | Max images per request |
| `wallpaperSource` | `bing` | Wallpaper source: `none`, `bing`, or `wallhaven` |
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
| `VISION_HANDOFF_ENABLED` | `VISION_HANDOFF_ENABLED` | `"false"` disables |
| `VISION_HANDOFF_CACHE_ENABLED` | `VISION_HANDOFF_CACHE_ENABLED` | `"false"` disables |
| `VISION_HANDOFF_CACHE_TTL` | `VISION_HANDOFF_CACHE_TTL` | |

### 2.3.1 Config Validation

`LoadConfig` validates critical fields after parsing and env-var overrides:

- `RequestTimeout` must be positive (`> 0`). A zero or negative value is a fatal error (`log.Fatal`).

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

The gate limit is `hard_cap ?? limit` (hard_cap preferred). If `hard_cap` is null and `limit` is null, no gating occurs (all requests execute immediately).

If `activeRequests >= gateLimit`:
- If queue is full (`len(requestQueue) >= MAX_QUEUE_SIZE`): increment `throttledCount`, return 503 with `queue_full` error.
- Otherwise: enqueue the request.

### 5.4 `processQueue()`

Called after each request completes (in `.finally` equivalent). Dequeues items while `activeRequests < gate` and dispatches:
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
- **Throttle reset**: When the usage window changes (detected via `window.started_at`), resets `ThrottledCount` to 0 under `queueMu.Lock()` and updates `throttledWindowStart`. The lock block is split so that HTTP fetching and JSON parsing happen **outside** the lock, avoiding nested/long-held locks.

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

## 9. Models.dev Integration

### 9.1 `fetchModelsDevCatalog()`

- `GET https://models.dev/api.json` (no auth).
- 15-second timeout.
- Returns parsed JSON.

### 9.2 `getModelsDevCatalog()`

- 5-minute cache (`modelsDevCache`).
- On failure, logs and returns `nil` (non-fatal).

### 9.3 Model ID Resolution

`deriveModelsDevId(umansId)`: Strips `umans-` prefix.

`umansIdCandidates(umansId)`: Returns `[umansId, baseId]` where `baseId` is the stripped version (deduplicated).

### 9.4 `findModelsDevEntry(catalog, umansId)`

Search order (first match wins):
1. `catalog["umans-ai"].models[candidate]`
2. Canonical providers: `openai`, `anthropic`, `google`, `mistral`, `meta`, `xai`, `deepseek`, `moonshotai`, `zhipuai`, `alibaba`, `nvidia`, `cohere`, `minimax`, `stepfun`, `xiaomi`
3. Fallback: scan every provider for matching candidate ID
4. Last resort: match by nested `model.id` field

Returns `{providerId, modelId, model}` or `nil`.

### 9.5 `buildModelsDevIndex(catalog)`

Builds a flat index: `modelId → {providerId, modelId, model}`. Priority order: `umans-ai` first, then canonical providers, then all others. First provider to claim a model ID wins.

### 9.6 Reasoning Resolution

**`inferReasoningModeFromCapabilities(reasoningCaps)`**:
- If `supported == true` → `true`
- If `levels` array is non-empty → `true`
- Otherwise → `nil`

**`resolveReasoningMode(devEntry, reasoningCaps)`**:
- If `devEntry` has non-empty `reasoning_options` array → `true`
- Else use `inferReasoningModeFromCapabilities`
- Default: `true`

### 9.7 Reasoning Level Budgets

```go
var ReasoningLevelBudgets = map[string]int{
    "low":    8000,
    "medium": 16000,
    "high":   16000,
    "max":    32000,
}
```

### 9.8 `buildReasoningVariants(reasoningCaps)`

- Returns `nil` if `supported != true` or `can_disable == false`.
- For each level in `levels` (excluding `"none"`), if a budget exists, creates a variant: `{thinking: {type: "enabled", budget_tokens: budget}}`.
- Returns the variants map or `nil` if no valid levels.

### 9.9 `parseLevels(raw)`

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

`authorized(req)`:
- If `config.apiKeys` is empty → allow all (open access).
- Check `X-Api-Key` header against `config.apiKeys`.
- Check `Authorization: Bearer {key}` header against `config.apiKeys`.
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
   - `bing` or `wallhaven`: If a cached wallpaper file exists (`.cache/wallpaper.jpg` or `.cache/wallpaper-haven.jpg`), embed as base64 in a `<style>` tag with `background-image` to prevent white flash. If no cached file, fall back to dark background.
4. Serve as `text/html`.

---

## 19. OpenAI Chat Completions Pipeline (`proxyChatRequest`)

Full request pipeline for `/v1/chat/completions`:

1. **Config lock**: Acquire `configMu.RLock()` for the duration of config reads.
2. **Key acquire**: Fingerprint → session lookup → preferred key → round-robin fallback. 503 if no healthy keys.
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
    a. On retry > 1: try to acquire a fresh key if pool > 1.
    b. Call `upstream.chatCompletions(payload)`.
    c. On network error: mark unhealthy, retry (or 502 if last attempt).
    d. On HTTP 2xx:
       - Mark key healthy.
       - If SSE content type:
          - Pipe SSE stream directly to client with backpressure handling.
        - If JSON:
          - Parse response body.
          - If streaming was requested (but got JSON): wrap as SSE chunk.
         - Write response, cache non-streaming responses.
    e. On HTTP 500/503: mark unhealthy, log error, retry (or pass through error if last).
    f. On other HTTP errors: pass through error immediately.

---

## 20. Anthropic Messages Pipeline (`proxyAnthropicRequest`)

Pass-through for `/v1/messages` and `/messages`:

1. **Config lock**: Acquire `configMu.RLock()` for the duration of config reads.
2. **Nil guards**: All access to `KeyPool` and `Upstream` is guarded — if either is nil, a 503/500 error is returned immediately rather than panicking.
3. **Key acquire**: Same session affinity as OpenAI path.
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
    - Mark unhealthy only on 503 (not 500, which is usually a payload issue).
    - Bump throttled on 429 or 503.
12. **On success**:
    - Mark key healthy.
    - Write upstream status + headers (`Content-Type`, `Cache-Control: no-cache`, `Connection: keep-alive`).
    - Pipe body to response with backpressure + client disconnect detection.
13. **On network error**:
    - `writeAnthropicPassthroughError` with 502.
    - Mark unhealthy (502).

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

### 21.2 `pipeBodyToResponse(body, res)`

Pipes upstream response body to HTTP response with:
- **Client disconnect detection**: On `res.close`, destroy/cancel the upstream body.
- **Backpressure**: When `res.write()` returns false, pause the upstream and resume on drain.
- **EOF handling**: Uses `errors.Is(err, io.EOF)` for clean stream-end detection.
- **Cleanup**: Remove all listeners on completion.
- Returns a promise/goroutine that resolves when piping is complete.

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
   - For each model, look up `modelInfoMap` and `modelsDevIndex`.
   - Entry: `{id, name, reasoning, variants?, limit?, temperature: true, tool_call?, attachment?, modalities}`.
   - `reasoning`: from `resolveReasoningMode`.
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
}
```

### 25.2 `POST /api/config`

Accepts partial config updates. Updates any provided fields:
- `apiKey`, `apiKeys`, `listenAddr`, `enabledModels`, `modelDisplayNames`, `wallpaperSource` (`none`, `bing`, or `wallhaven`), `overrideConcurrency`, `maxImages`, `disabledModels`, `visionHandoffEnabled`, `visionHandoffModel`, `visionHandoffPrompt`, `visionHandoffCacheEnabled`, `keys` (rebuilds key pool).

After update: `debouncedSaveConfig()` + `debouncedSetupOpencodeConfig()`.

**Response includes `restartRequired`**: If the `listenAddr` field is changed in the POST, the response sets `restartRequired: true` to signal the dashboard that a proxy restart is needed for the new port to take effect.

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
  "queued": 1
}
```

- `?fresh=1` bypasses cache.
- `active` is the proxy's `activeRequests` count.
- `queued` is the `requestQueue` length.

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
   - `GET https://peapix.com/bing/feed` with `User-Agent: Mozilla/5.0...` header, 15s timeout.
   - Parse JSON response, extract first item's `fullUrl` (or `imageUrl` or `url`).
   - Download the image (30s timeout, browser User-Agent).
   - Save to `.cache/wallpaper.jpg`.
   - Serve as `image/jpeg`.
3. On fetch error: serve cached file if available, else 500.
4. Set `Expires` header to end of today (UTC).

Cache TTL: 24 hours (daily).

### 30.2 `GET /api/bg-wallhaven` — Wallhaven Wallpaper

Fetches a random top-listed wallpaper from wallhaven.cc.

1. Check cache: `.cache/wallpaper-haven.jpg`. If file exists and is < 1 hour old, serve cached JPEG.
2. If cache miss/expired:
   - `GET https://wallhaven.cc/api/v1/search?categories=100&purity=100&topRange=1M&sorting=toplist&order=desc&page=3` with `User-Agent: umans-proxy/1.0`, 15s timeout.
   - Parse JSON, pick random entry from `data` array.
   - Download `path` URL (30s timeout, browser User-Agent).
   - Save to `.cache/wallpaper-haven.jpg`.
   - Serve as `image/jpeg`.
3. On fetch error: serve cached file if available, else 500.

Cache TTL: 1 hour.

---

## 31. Startup Sequence

1. `loadConfig()` — Load `.config/config.json` + env var overrides.
2. Initialize `ImageHandoffCache` (50 entries, 24h TTL or configured).
3. Initialize `KeyPool` from `config.keys` (or single default key).
4. Initialize `UpstreamClient`.
5. `validateApiKey()` — Verify via `/v1/models/info`, populate `modelInfoMap` + `modelDisplayNameMap`.
6. `fetchConcurrency()` — Fetch concurrent sessions & limit.
7. `getModelsDevCatalog()` — Preload reasoning metadata (non-fatal on failure).
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
- Inject a `<style>` tag with a dark background (`#0d1117`) before `</head>`. If `wallpaperSource` is `bing` or `wallhaven` and a cached wallpaper exists, embed it as base64 `background-image` instead.
- Content-Type: `text/html`.

### Ported features:
- 5-hour Window card (Requests, Throttled, Cached %, Error %, Start Time, Tokens In/Out, plan badge)
- Current Concurrency card (Active, Queued, Limit, Burst, dual-fill progress bar with soft-cap/burst zones, upstream overlay, percentage, detail grid)
- Usage History card (bar chart with Y-axis labels, dashed grid lines, X-axis labels, click-to-filter table, expandable per-model breakdown, sortable headers, metric toggle Tokens/Requests, status legend)
- User ID in header bar (click-to-reveal masking)
- API Key section (key pool display with status badges, collapsible)
- Models section (enable/disable toggle per model via `DISABLED_MODELS`, collapsible)
- Quick Settings (Automatic Refresh interval, Wallpaper selector (None/Bing/Wallhaven), Vision Handoff toggle with info tooltip, Handoff Cache toggle (shown only when Vision Handoff is enabled) with cache stats line)
- Quick Actions (Check Health, Test Connection, Manual Refresh, Restart Proxy)
- Environment (Runtime, Port, Started At)
- Key Management Modal (add/edit/delete API keys with inline editing, account info with User ID)
- Glass UI (procedural SVG filter-based glassmorphism via `initLiquidGlass()`)
- Auto-refresh (status every 15s, usage via configurable interval, concurrency every 15s, usage history every 5 min)
- Dashboard always fetches usage and concurrency with `?fresh=1`
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
    MODELS_DEV_CATALOG_URL  = "https://models.dev/api.json"
    MAX_RETRIES             = 10
    RETRY_DELAY_MS          = 3000  // 3 seconds
    MAX_QUEUE_SIZE          = 256
    MAX_BODY_SIZE           = 5 * 1024 * 1024  // 5 MB
    CONVERSATION_MAP_MAX    = 10000
    MODEL_CATALOG_CACHE_TTL = 5 * time.Minute
    MODELS_DEV_CACHE_TTL   = 5 * time.Minute
    USAGE_CACHE_TTL        = 5 * time.Minute
    OPENCODE_CONFIG_CACHE_TTL = 5 * time.Minute
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

Canonical models.dev providers (in priority order):
`umans-ai`, `openai`, `anthropic`, `google`, `mistral`, `meta`, `xai`, `deepseek`, `moonshotai`, `zhipuai`, `alibaba`, `nvidia`, `cohere`, `minimax`, `stepfun`, `xiaomi`.
