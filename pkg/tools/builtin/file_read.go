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

// FileReadConfig tunes the read-only file_read tool registered via
// RegisterFileRead. Root is required; zero MaxBytes resolves to the
// 1 MiB default.
type FileReadConfig struct {
	// Root is the absolute directory paths must be contained within.
	// Relative paths in requests are resolved against Root.
	Root string
	// MaxBytes caps how many bytes a single Execute reads. 0 applies
	// the default of 1 MiB.
	MaxBytes int64
}

const fileReadName = "file_read"
const fileReadDescription = "Read the contents of a file from the local workspace. Paths are resolved relative to a fixed root directory; attempts to traverse outside root are rejected."

type fileReadArgs struct {
	Path   string `json:"path"             description:"File path to read, relative to the configured root directory."`
	Offset int64  `json:"offset,omitempty" description:"Optional byte offset to start reading from. Defaults to 0."`
	Length int64  `json:"length,omitempty" description:"Optional number of bytes to read. Capped by MaxBytes. Defaults to MaxBytes."`
}

// RegisterFileRead registers a read-only file tool sandboxed to
// cfg.Root. Every read is constrained to that directory — requests
// whose cleaned absolute path does not live under Root are rejected
// before the OS is even touched.
//
// Cacheable=true opts identical (path, offset, length) tuples into
// the agent-loop tool-result cache for the process lifetime; files
// can change on disk between turns, but the cache is in-memory and
// short-lived so staleness is bounded.
//
// To pair this with a companion writer, gate that tool behind
// RequiresConfirmation=true rather than extending this registration.
func RegisterFileRead(reg tools.Registerer, cfg FileReadConfig) {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 1 << 20
	}
	tools.RegisterFunc(reg, fileReadName, fileReadDescription,
		func(ctx context.Context, args fileReadArgs) (tools.Result, error) {
			return executeFileRead(ctx, cfg, args)
		},
		tools.FuncToolOpts{Cacheable: true})
}

// executeFileRead is the typed-arg body extracted so the
// RegisterFileRead closure captures only cfg and the sandbox /
// streaming logic stays independently testable.
func executeFileRead(_ context.Context, cfg FileReadConfig, args fileReadArgs) (tools.Result, error) {
	if strings.TrimSpace(args.Path) == "" {
		return tools.Result{}, fmt.Errorf("tools: path is required")
	}
	if cfg.Root == "" {
		return tools.Result{}, fmt.Errorf("tools: file_read has no Root configured")
	}

	absRoot, err := filepath.Abs(cfg.Root)
	if err != nil {
		return tools.Result{}, fmt.Errorf("tools: invalid root: %w", err)
	}

	// Resolve the request path relative to root, then confirm it stays there.
	req := args.Path
	if !filepath.IsAbs(req) {
		req = filepath.Join(absRoot, req)
	}
	cleaned, err := filepath.Abs(filepath.Clean(req))
	if err != nil {
		return tools.Result{}, fmt.Errorf("tools: invalid path: %w", err)
	}
	if !pathWithin(cleaned, absRoot) {
		return tools.Result{}, fmt.Errorf("tools: path %q is outside the configured root", args.Path)
	}

	info, err := os.Stat(cleaned)
	if err != nil {
		return tools.Result{}, fmt.Errorf("tools: stat %q: %w", args.Path, err)
	}
	if info.IsDir() {
		return tools.Result{}, fmt.Errorf("tools: %q is a directory, not a file", args.Path)
	}

	f, err := os.Open(cleaned)
	if err != nil {
		return tools.Result{}, fmt.Errorf("tools: open %q: %w", args.Path, err)
	}
	defer f.Close()

	if args.Offset > 0 {
		if _, err := f.Seek(args.Offset, io.SeekStart); err != nil {
			return tools.Result{}, fmt.Errorf("tools: seek: %w", err)
		}
	}

	cap := cfg.MaxBytes
	if args.Length > 0 && args.Length < cap {
		cap = args.Length
	}

	buf, err := io.ReadAll(io.LimitReader(f, cap))
	if err != nil {
		return tools.Result{}, fmt.Errorf("tools: read: %w", err)
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
		return tools.Result{}, fmt.Errorf("tools: marshal: %w", err)
	}
	return tools.Text(string(out)), nil
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
