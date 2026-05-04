package builtin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGenerateImageTool_DownloadDelegatesToStorage(t *testing.T) {
	// Stand up a tiny HTTP server that pretends to be the OpenAI CDN
	// hosting a generated image. The tool downloads from here, then
	// hands the bytes to Storage.
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("PNGBYTES"))
	}))
	defer cdn.Close()

	store := &fakeAssetStorage{}
	tool := &GenerateImageTool{
		httpClient: &http.Client{Timeout: 5 * time.Second},
		Storage:    store,
	}

	url, err := tool.download(context.Background(), cdn.URL+"/x.png")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if !strings.HasPrefix(url, "https://cdn.example.test/img-") {
		t.Fatalf("expected storage-assigned URL, got %q", url)
	}

	if len(store.calls) != 1 {
		t.Fatalf("expected 1 Save call, got %d", len(store.calls))
	}
	c := store.calls[0]
	if c.ContentType != "image/png" {
		t.Errorf("ContentType: got %q, want image/png", c.ContentType)
	}
	if c.ByteCount != len("PNGBYTES") {
		t.Errorf("ByteCount: got %d, want %d", c.ByteCount, len("PNGBYTES"))
	}
	if !strings.HasPrefix(c.Filename, "img-") || !strings.HasSuffix(c.Filename, ".png") {
		t.Errorf("Filename: got %q, want img-<ts>.png", c.Filename)
	}
}

func TestGenerateImageTool_DownloadErrorsWithoutStorage(t *testing.T) {
	tool := &GenerateImageTool{
		httpClient: &http.Client{Timeout: time.Second},
	}
	if _, err := tool.download(context.Background(), "https://example.test/x.png"); err == nil {
		t.Fatal("expected error when Storage is nil")
	}
}

func TestGenerateImageTool_DownloadFailsClosedOnUpstreamError(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer cdn.Close()

	store := &fakeAssetStorage{}
	tool := &GenerateImageTool{
		httpClient: &http.Client{Timeout: time.Second},
		Storage:    store,
	}

	if _, err := tool.download(context.Background(), cdn.URL+"/x.png"); err == nil {
		t.Fatal("expected error when CDN returns 500")
	}
	if len(store.calls) != 0 {
		t.Fatalf("Storage must not be called on upstream error, got %d calls", len(store.calls))
	}
}
