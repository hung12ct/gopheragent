package builtin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPRequestTool_GETReturnsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool()
	out, err := tool.Execute(context.Background(), `{"url":"`+srv.URL+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var env struct {
		Status    int    `json:"status"`
		Body      string `json:"body"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("bad envelope: %v", err)
	}
	if env.Status != 200 {
		t.Fatalf("status: %d", env.Status)
	}
	if env.Body != `{"ok":true}` {
		t.Fatalf("body: %q", env.Body)
	}
	if env.Truncated {
		t.Fatal("unexpected truncation")
	}
}

func TestHTTPRequestTool_PostForwardsBodyAndHeaders(t *testing.T) {
	var gotBody string
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotHeader = r.Header.Get("X-Test")
		w.WriteHeader(201)
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool()
	args := `{"url":"` + srv.URL + `","method":"POST","body":"hi","headers":{"X-Test":"v"}}`
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var env struct {
		Status int `json:"status"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if env.Status != 201 {
		t.Fatalf("status: %d", env.Status)
	}
	if gotBody != "hi" {
		t.Fatalf("body: %q", gotBody)
	}
	if gotHeader != "v" {
		t.Fatalf("header: %q", gotHeader)
	}
}

func TestHTTPRequestTool_RejectsDisallowedMethod(t *testing.T) {
	tool := NewHTTPRequestTool()
	_, err := tool.Execute(context.Background(), `{"url":"https://example.com","method":"TRACE"}`)
	if err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("expected method not permitted, got %v", err)
	}
}

func TestHTTPRequestTool_RejectsNonHTTPScheme(t *testing.T) {
	tool := NewHTTPRequestTool()
	_, err := tool.Execute(context.Background(), `{"url":"file:///etc/passwd"}`)
	if err == nil || !strings.Contains(err.Error(), "http(s)") {
		t.Fatalf("expected scheme rejection, got %v", err)
	}
}

func TestHTTPRequestTool_HostAllowlistBlocks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool().WithAllowedHosts("only.example.com")
	_, err := tool.Execute(context.Background(), `{"url":"`+srv.URL+`"}`)
	if err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("expected allowlist block, got %v", err)
	}
}

func TestHTTPRequestTool_TruncatesLargeBody(t *testing.T) {
	big := strings.Repeat("a", 2000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool().WithMaxBytes(100)
	out, err := tool.Execute(context.Background(), `{"url":"`+srv.URL+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var env struct {
		Body      string `json:"body"`
		Truncated bool   `json:"truncated"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if !env.Truncated {
		t.Fatal("expected truncation flag")
	}
	if len(env.Body) != 100 {
		t.Fatalf("body length: %d", len(env.Body))
	}
}

func TestHTTPRequestTool_RequiresConfirmation(t *testing.T) {
	tool := NewHTTPRequestTool()
	if !tool.RequiresConfirmation() {
		t.Fatal("http_request should require confirmation")
	}
}
