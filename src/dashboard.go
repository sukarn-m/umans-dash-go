package proxy

import (
	"bytes"
	"encoding/json"
	"text/template"
)

// ─── Dashboard Rendering ────────────────────────────────────────────────────

type dashboardTemplateData struct {
	Version               string
	BackoffPresets        string // JSON-encoded
	DefaultRetryAttempts  int
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
