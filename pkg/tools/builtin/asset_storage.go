package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AssetStorage abstracts how generated media (images, videos) is persisted
// and made addressable. Implementations turn raw bytes into a public URL
// the LLM can reference inline. Adopters running on container platforms
// with ephemeral disk (Cloud Run, Lambda, ECS Fargate) plug in a cloud
// storage adapter (GCS, S3, Azure Blob) so generated files survive
// between requests.
//
// Filename is the basename the caller would have used on local disk
// (e.g. "img-1717531234567890000.png"). Implementations may use it
// verbatim, namespace it under a tenant prefix, or rewrite it entirely —
// the returned publicURL is the only contract that matters.
//
// ContentType is the MIME type derived from the source response or the
// extension; backends that need it for proper Content-Type headers (S3,
// GCS) should honor it. Pass-through stores (e.g. local disk) may ignore it.
type AssetStorage interface {
	Save(ctx context.Context, filename string, data []byte, contentType string) (publicURL string, err error)
}

// LocalDiskStorage is the default AssetStorage implementation: writes
// bytes under SaveDir, returns URLBase/<filename>. Suitable for
// long-lived VMs with persistent disk and an HTTP server fronting
// SaveDir; broken on container platforms with ephemeral disk — use
// the GCS / S3 / Azure Blob adapter shipped in your integration there.
type LocalDiskStorage struct {
	// SaveDir is the local directory to write files into. Created with
	// 0755 if it does not exist. Required.
	SaveDir string
	// URLBase is prepended to the filename to form the returned
	// publicURL. Trailing slashes are tolerated. Required.
	URLBase string
}

// Save implements AssetStorage by writing data to <SaveDir>/<filename>
// and returning <URLBase>/<filename>.
func (s *LocalDiskStorage) Save(_ context.Context, filename string, data []byte, _ string) (string, error) {
	if s.SaveDir == "" {
		return "", fmt.Errorf("builtin: LocalDiskStorage: SaveDir not configured")
	}
	if err := os.MkdirAll(s.SaveDir, 0o755); err != nil {
		return "", fmt.Errorf("builtin: LocalDiskStorage: mkdir: %w", err)
	}
	dest := filepath.Join(s.SaveDir, filename)
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return "", fmt.Errorf("builtin: LocalDiskStorage: write: %w", err)
	}
	base := strings.TrimRight(s.URLBase, "/")
	return base + "/" + filename, nil
}
