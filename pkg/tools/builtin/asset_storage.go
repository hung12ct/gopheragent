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
// bytes under saveDir, returns urlBase/<filename>. Suitable for
// long-lived VMs with persistent disk and an HTTP server fronting
// saveDir; broken on container platforms with ephemeral disk — use
// the GCS / S3 / Azure Blob adapter shipped in your integration there.
//
// Construct via NewLocalDiskStorage; the unexported fields prevent the
// "did my post-construction assignment take effect?" tax that public
// mutable fields invite.
type LocalDiskStorage struct {
	saveDir string
	urlBase string
}

// NewLocalDiskStorage constructs a LocalDiskStorage that writes files
// under saveDir and serves them under urlBase. Both arguments are
// required; passing "" returns an error so misconfiguration surfaces at
// startup rather than on the first generation. urlBase trailing slashes
// are tolerated.
func NewLocalDiskStorage(saveDir, urlBase string) (*LocalDiskStorage, error) {
	if saveDir == "" {
		return nil, fmt.Errorf("builtin: NewLocalDiskStorage: saveDir is required")
	}
	if urlBase == "" {
		return nil, fmt.Errorf("builtin: NewLocalDiskStorage: urlBase is required")
	}
	return &LocalDiskStorage{saveDir: saveDir, urlBase: strings.TrimRight(urlBase, "/")}, nil
}

// Save implements AssetStorage by writing data to <saveDir>/<filename>
// and returning <urlBase>/<filename>.
func (s *LocalDiskStorage) Save(_ context.Context, filename string, data []byte, _ string) (string, error) {
	if err := os.MkdirAll(s.saveDir, 0o755); err != nil {
		return "", fmt.Errorf("builtin: LocalDiskStorage: mkdir: %w", err)
	}
	dest := filepath.Join(s.saveDir, filename)
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return "", fmt.Errorf("builtin: LocalDiskStorage: write: %w", err)
	}
	return s.urlBase + "/" + filename, nil
}
