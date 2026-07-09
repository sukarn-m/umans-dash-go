package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ─── Error Logging (§16) ───────────────────────────────────────────────────

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
			Body:    RedactBodyJson(truncateBody(reqBody, 4096)),
		},
		Upstream: ErrorLogUpstream{
			URL:        upstreamURL,
			Method:     upstreamMethod,
			Headers:    RedactHeaders(upstreamHeaders),
			Status:     upstreamStatus,
			StatusText: upstreamStatusText,
			Body:       RedactBodyJson(truncateBody(upstreamBody, 4096)),
		},
	}
	return p.LogHttpError(record)
}
