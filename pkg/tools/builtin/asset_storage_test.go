package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeAssetStorage records every Save call so tests can assert on the
// (filename, contentType, byteCount) tuple. Returns a deterministic URL
// derived from the filename.
type fakeAssetStorage struct {
	mu    sync.Mutex
	calls []fakeAssetSaveCall
	err   error
}

type fakeAssetSaveCall struct {
	Filename    string
	ContentType string
	ByteCount   int
}

func (f *fakeAssetStorage) Save(_ context.Context, filename string, data []byte, contentType string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeAssetSaveCall{
		Filename:    filename,
		ContentType: contentType,
		ByteCount:   len(data),
	})
	return "https://cdn.example.test/" + filename, nil
}

func TestLocalDiskStorage_WritesFileAndReturnsURL(t *testing.T) {
	dir := t.TempDir()
	store := &LocalDiskStorage{SaveDir: dir, URLBase: "/media/"}

	url, err := store.Save(context.Background(), "kitten.png", []byte("PNGBYTES"), "image/png")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if url != "/media/kitten.png" {
		t.Fatalf("URL: got %q, want %q", url, "/media/kitten.png")
	}

	got, err := os.ReadFile(filepath.Join(dir, "kitten.png"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "PNGBYTES" {
		t.Fatalf("disk contents: got %q, want PNGBYTES", got)
	}
}

func TestLocalDiskStorage_TrimsTrailingSlashOnURLBase(t *testing.T) {
	dir := t.TempDir()
	cases := []string{"/media", "/media/", "/media//"}
	for _, base := range cases {
		store := &LocalDiskStorage{SaveDir: dir, URLBase: base}
		url, err := store.Save(context.Background(), "x.png", []byte("x"), "image/png")
		if err != nil {
			t.Fatalf("base=%q Save: %v", base, err)
		}
		if !strings.HasSuffix(url, "/x.png") || strings.Contains(url, "//x.png") {
			t.Fatalf("base=%q produced malformed url %q", base, url)
		}
	}
}

func TestLocalDiskStorage_ErrorsWhenSaveDirEmpty(t *testing.T) {
	store := &LocalDiskStorage{URLBase: "/media"}
	if _, err := store.Save(context.Background(), "x.png", []byte("x"), "image/png"); err == nil {
		t.Fatal("expected error when SaveDir is empty")
	}
}

func TestLocalDiskStorage_CreatesDirectoryIfMissing(t *testing.T) {
	parent := t.TempDir()
	nested := filepath.Join(parent, "does", "not", "exist", "yet")
	store := &LocalDiskStorage{SaveDir: nested, URLBase: "/media"}

	if _, err := store.Save(context.Background(), "x.png", []byte("x"), "image/png"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(nested, "x.png")); err != nil {
		t.Fatalf("nested dir not created: %v", err)
	}
}

// Compile-time assertion that LocalDiskStorage implements AssetStorage.
var _ AssetStorage = (*LocalDiskStorage)(nil)

// Compile-time assertion that fakeAssetStorage implements AssetStorage —
// guarantees the test stub matches the real interface.
var _ AssetStorage = (*fakeAssetStorage)(nil)
