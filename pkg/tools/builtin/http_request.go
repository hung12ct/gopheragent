package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// HTTPRequestTool performs arbitrary HTTP calls on behalf of the agent.
// Unlike ReadURLTool, it returns the raw response body (not HTML→text),
// making it suitable for JSON APIs and webhook calls.
//
// Guardrails layered on every request:
//   - an optional host allowlist rejects any URL outside it (SSRF defence)
//   - method whitelist: GET / HEAD / POST / PUT / PATCH / DELETE
//   - configurable client timeout, body size cap, max redirects
//
// Non-GET/HEAD methods trigger RequiresConfirmation() so the HITL layer
// can gate mutating calls.
type HTTPRequestTool struct {
	client       *http.Client
	allowedHosts map[string]bool
	maxBytes     int64
	defaultGET   bool
}

// NewHTTPRequestTool returns a tool configured with sensible defaults:
// 15s request timeout, 1 MiB response cap, no host allowlist, default
// redirect behaviour.
func NewHTTPRequestTool() *HTTPRequestTool {
	return &HTTPRequestTool{
		client:   &http.Client{Timeout: 15 * time.Second},
		maxBytes: 1 << 20,
	}
}

// WithTimeout overrides the HTTP client timeout.
func (t *HTTPRequestTool) WithTimeout(d time.Duration) *HTTPRequestTool {
	t.client = &http.Client{Timeout: d}
	return t
}

// WithMaxBytes caps response body size (truncated past the limit).
// n <= 0 resets to the 1 MiB default.
func (t *HTTPRequestTool) WithMaxBytes(n int64) *HTTPRequestTool {
	if n <= 0 {
		t.maxBytes = 1 << 20
	} else {
		t.maxBytes = n
	}
	return t
}

// WithAllowedHosts restricts the tool to only call the given hostnames
// (exact match, case-insensitive). Leaving this unset allows any host —
// strongly recommend setting it for any agent exposed to untrusted input
// to block SSRF to internal services (169.254.169.254, localhost, ...).
func (t *HTTPRequestTool) WithAllowedHosts(hosts ...string) *HTTPRequestTool {
	m := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		m[strings.ToLower(h)] = true
	}
	t.allowedHosts = m
	return t
}

func (t *HTTPRequestTool) Name() string { return "http_request" }

func (t *HTTPRequestTool) Description() string {
	return "Make an HTTP request to an external URL. Returns {status, headers, body, truncated}. Use for calling JSON APIs or webhooks. For reading human-readable web pages, prefer read_url."
}

func (t *HTTPRequestTool) ParametersSchema() tools.ToolSchema {
	return tools.ToolSchema{
		Type: "object",
		Properties: map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "Absolute HTTP(S) URL.",
			},
			"method": map[string]any{
				"type":        "string",
				"description": "HTTP method (GET, HEAD, POST, PUT, PATCH, DELETE). Defaults to GET.",
				"enum":        []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"},
			},
			"headers": map[string]any{
				"type":        "object",
				"description": "Optional request headers as a flat string→string map.",
			},
			"body": map[string]any{
				"type":        "string",
				"description": "Optional request body (send JSON as a string). Ignored for GET/HEAD.",
			},
		},
		Required: []string{"url"},
	}
}

// RequiresConfirmation returns true — HTTP calls can have side effects
// (POST/DELETE/etc.) and can exfiltrate data. HITL operators decide.
// GET-only usage can override by wrapping the tool in middleware.
func (t *HTTPRequestTool) RequiresConfirmation() bool { return true }

func (t *HTTPRequestTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		URL     string            `json:"url"`
		Method  string            `json:"method"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("tools: invalid arguments: %w", err)
	}

	method := strings.ToUpper(strings.TrimSpace(args.Method))
	if method == "" {
		method = http.MethodGet
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost,
		http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return "", fmt.Errorf("tools: method %q is not permitted", method)
	}

	u, err := url.Parse(strings.TrimSpace(args.URL))
	if err != nil {
		return "", fmt.Errorf("tools: invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("tools: only http(s) URLs are permitted, got %q", u.Scheme)
	}
	if t.allowedHosts != nil && !t.allowedHosts[strings.ToLower(u.Hostname())] {
		return "", fmt.Errorf("tools: host %q is not in the allowlist", u.Hostname())
	}

	var body io.Reader
	if args.Body != "" && method != http.MethodGet && method != http.MethodHead {
		body = bytes.NewBufferString(args.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return "", fmt.Errorf("tools: build request: %w", err)
	}
	for k, v := range args.Headers {
		req.Header.Set(k, v)
	}
	// Default User-Agent keeps the tool identifiable in server logs.
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "gopheragent-http-tool/1.0")
	}

	start := time.Now()
	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("tools: request failed: %w", err)
	}
	defer resp.Body.Close()

	cap := t.maxBytes
	if cap <= 0 {
		cap = 1 << 20
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cap+1))
	if err != nil {
		return "", fmt.Errorf("tools: read response: %w", err)
	}
	truncated := false
	if int64(len(raw)) > cap {
		raw = raw[:cap]
		truncated = true
	}

	// Flatten headers to the first value per key — good enough for LLM
	// consumption and keeps the payload small.
	flatHeaders := make(map[string]string, len(resp.Header))
	for k, v := range resp.Header {
		if len(v) > 0 {
			flatHeaders[k] = v[0]
		}
	}

	envelope := map[string]any{
		"status":       resp.StatusCode,
		"headers":      flatHeaders,
		"body":         string(raw),
		"truncated":    truncated,
		"elapsed_ms":   time.Since(start).Milliseconds(),
		"final_url":    resp.Request.URL.String(),
		"content_type": resp.Header.Get("Content-Type"),
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("tools: marshal: %w", err)
	}
	return string(out), nil
}
