package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ─── Proxy Core ──────────────────────────────────────────────────────────────

func NewProxy(cfg *Config) *Proxy {
	p := &Proxy{
		Config:          cfg,
		ModelInfoMap:    map[string]ModelInfo{},
		DisplayNameMap:  map[string]string{},
		UpstreamPricing: map[string]Pricing{},
	}
	p.StartedAt = time.Now()

	// Initialize queue worker infrastructure.
	p.queueDone = make(chan struct{})
	p.queueCond = sync.NewCond(&p.queueMu)
	p.workerWG.Add(1)
	go p.queueWorker()

	// Vision-handoff semaphore replaces the per-call local semaphore.
	if n := p.getVisionHandoffLimit(); n > 0 {
		p.visionHandoffSem = make(chan struct{}, n)
	}
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
			DisableKeepAlives:   false,
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

				if tw.Claim() {
					// Write directly to the underlying writer, bypassing
					// WriteJSON/IsCommitted (which would skip since Claim set
					// hijacked=true). This ensures the 500 reaches the client.
					errObj, _ := json.Marshal(map[string]interface{}{
						"error": map[string]interface{}{
							"message": "internal server error",
							"type":    "server_error",
						},
					})
					tw.ResponseWriter.Header().Set("Content-Type", "application/json")
					tw.ResponseWriter.WriteHeader(http.StatusInternalServerError)
					tw.ResponseWriter.Write(errObj)
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
		// streaming duration.
		//
		// Use Claim() to atomically set written+hijacked BEFORE writing to
		// the underlying writer. This closes the race window: once hijacked
		// is set, all tracker methods (WriteHeader, Write, Flush) become
		// no-ops for the handler goroutine, preventing a concurrent handler
		// Write from triggering a superfluous WriteHeader on the underlying
		// writer.
		if tw.Claim() {
			errObj, _ := json.Marshal(map[string]interface{}{
				"error": map[string]interface{}{
					"message": "request timeout",
					"type":    "server_error",
					"code":    "timeout",
				},
			})
			tw.ResponseWriter.Header().Set("Content-Type", "application/json")
			tw.ResponseWriter.WriteHeader(http.StatusGatewayTimeout)
			tw.ResponseWriter.Write(errObj)
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
		if tw, ok := w.(*responseWriterTracker); ok && tw.IsCommitted() {
			return
		}
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
		if tw, ok := w.(*responseWriterTracker); ok && tw.IsCommitted() {
			return
		}
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
