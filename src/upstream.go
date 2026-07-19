package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ─── Upstream Client (§7) ───────────────────────────────────────────────────

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
		DisableKeepAlives:     false,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   128,
		IdleConnTimeout:       60 * time.Second,
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
// Also updates the transport's ResponseHeaderTimeout and TLSClientConfig to match.
// The goal is to avoid per-request http.Client/http.Transport allocation;
// the existing transport is mutated in place.
func (u *UpstreamClient) SetTimeout(timeout time.Duration) {
	u.timeout = timeout
	u.httpClient.Timeout = timeout
	if tr, ok := u.httpClient.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = timeout
		if tr.TLSClientConfig != nil {
			// No TLS config changes needed for timeout; just ensure it persists.
		}
	}
}

// CloseIdleConnections closes idle connections on the underlying transport.
// Called after SetTimeout to force new connections with updated timeouts.
func (u *UpstreamClient) CloseIdleConnections() {
	if tr, ok := u.httpClient.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
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
		// Do NOT defer cancel() — the caller must read resp.Body after this
		// function returns, and a deferred cancel would fire immediately on
		// return, canceling the context before the body is read. The
		// cancelOnClose wrapper below calls cancel() when the body is closed.
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.baseURL+path, bytes.NewReader(body))
	if err != nil {
		if cancel != nil {
			cancel()
		}
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
		if cancel != nil {
			cancel()
		}
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
