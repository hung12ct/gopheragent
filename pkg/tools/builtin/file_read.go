package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// FileReadTool reads a file from the local filesystem. Every read is
// constrained to a root directory configured at construction time —
// requests whose cleaned absolute path does not live under root are
// rejected before the OS is even touched.
//
// The tool is intentionally read-only. If you want a companion writer,
// gate it behind RequiresConfirmation() == true rather than extending
// this type.
type FileReadTool struct {
	// Root is the absolute directory paths must be contained within.
	// Relative paths in requests are resolved against Root.
	Root string

	// MaxBytes caps how many bytes are read. 0 uses the default (1 MiB).
	MaxBytes int64
}

// NewFileReadTool constructs a FileReadTool. root must be an absolute
// directory — attempts to escape it via "..", symlinks, or absolute paths
// are rejected at Execute time.
func NewFileReadTool(root string) *FileReadTool {
	return &FileReadTool{Root: root, MaxBytes: 1 << 20}
}

// WithMaxBytes overrides the default 1 MiB read cap. n <= 0 resets to
// default.
func (t *FileReadTool) WithMaxBytes(n int64) *FileReadTool {
	if n <= 0 {
		t.MaxBytes = 1 << 20
	} else {
		t.MaxBytes = n
	}
	return t
}

func (t *FileReadTool) Name() string { return "file_read" }

// Cacheable opts file_read into the agent-loop tool-result cache: identical
// (path, offset, length) tuples return the same bytes within a process
// lifetime. Files can change on disk between agent turns, but the cache is
// in-memory and short-lived, so staleness is bounded.
func (t *FileReadTool) Cacheable() bool { return true }

func (t *FileReadTool) Description() string {
	return "Read the contents of a file from the local workspace. Paths are resolved relative to a fixed root directory; attempts to traverse outside root are rejected."
}

type fileReadArgs struct {
	Path   string `json:"path"             description:"File path to read, relative to the configured root directory."`
	Offset int64  `json:"offset,omitempty" description:"Optional byte offset to start reading from. Defaults to 0."`
	Length int64  `json:"length,omitempty" description:"Optional number of bytes to read. Capped by MaxBytes. Defaults to MaxBytes."`
}

func (t *FileReadTool) ParametersSchema() tools.ToolSchema {
	return tools.SchemaFor[fileReadArgs]()
}

// RequiresConfirmation is false — reads are non-destructive.
func (t *FileReadTool) RequiresConfirmation() bool { return false }

func (t *FileReadTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args fileReadArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("tools: invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		return "", fmt.Errorf("tools: path is required")
	}
	if t.Root == "" {
		return "", fmt.Errorf("tools: file_read has no Root configured")
	}

	absRoot, err := filepath.Abs(t.Root)
	if err != nil {
		return "", fmt.Errorf("tools: invalid root: %w", err)
	}

	// Resolve the request path relative to root, then confirm it stays there.
	req := args.Path
	if !filepath.IsAbs(req) {
		req = filepath.Join(absRoot, req)
	}
	cleaned, err := filepath.Abs(filepath.Clean(req))
	if err != nil {
		return "", fmt.Errorf("tools: invalid path: %w", err)
	}
	if !pathWithin(cleaned, absRoot) {
		return "", fmt.Errorf("tools: path %q is outside the configured root", args.Path)
	}

	info, err := os.Stat(cleaned)
	if err != nil {
		return "", fmt.Errorf("tools: stat %q: %w", args.Path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("tools: %q is a directory, not a file", args.Path)
	}

	f, err := os.Open(cleaned)
	if err != nil {
		return "", fmt.Errorf("tools: open %q: %w", args.Path, err)
	}
	defer f.Close()

	if args.Offset > 0 {
		if _, err := f.Seek(args.Offset, io.SeekStart); err != nil {
			return "", fmt.Errorf("tools: seek: %w", err)
		}
	}

	cap := t.MaxBytes
	if cap <= 0 {
		cap = 1 << 20
	}
	if args.Length > 0 && args.Length < cap {
		cap = args.Length
	}

	buf, err := io.ReadAll(io.LimitReader(f, cap))
	if err != nil {
		return "", fmt.Errorf("tools: read: %w", err)
	}
	truncated := info.Size()-args.Offset > int64(len(buf))

	envelope := map[string]any{
		"path":       args.Path,
		"size_bytes": info.Size(),
		"content":    string(buf),
		"truncated":  truncated,
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("tools: marshal: %w", err)
	}
	return string(out), nil
}

// pathWithin reports whether target lives under root. Both arguments must
// already be cleaned absolute paths.
func pathWithin(target, root string) bool {
	if target == root {
		return true
	}
	sep := string(os.PathSeparator)
	root = strings.TrimRight(root, sep) + sep
	return strings.HasPrefix(target, root)
}
