package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return p
}

func TestFileReadTool_ReadsFileWithinRoot(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "hello.txt", "hello world")
	tool := NewFileReadTool(dir)

	out, err := tool.Execute(context.Background(), `{"path":"hello.txt"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var env struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
		SizeBytes int64  `json:"size_bytes"`
	}
	if err := json.Unmarshal([]byte(out.Text), &env); err != nil {
		t.Fatalf("bad envelope: %v", err)
	}
	if env.Content != "hello world" {
		t.Fatalf("content mismatch: %q", env.Content)
	}
	if env.Truncated {
		t.Fatal("unexpected truncation")
	}
	if env.SizeBytes != int64(len("hello world")) {
		t.Fatalf("size mismatch: %d", env.SizeBytes)
	}
}

func TestFileReadTool_RejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	// create a sibling directory with a secret file OUTSIDE the root
	parent := filepath.Dir(dir)
	secretPath := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("nope"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(secretPath) })

	tool := NewFileReadTool(dir)
	_, err := tool.Execute(context.Background(), `{"path":"../secret.txt"}`)
	if err == nil {
		t.Fatal("expected error for path traversal, got nil")
	}
	if !strings.Contains(err.Error(), "outside") {
		t.Fatalf("expected 'outside' in error, got %v", err)
	}
}

func TestFileReadTool_RejectsAbsolutePathOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	tool := NewFileReadTool(dir)
	req := fmt.Sprintf(`{"path":%q}`, "/etc/passwd")
	_, err := tool.Execute(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for absolute out-of-root path")
	}
}

func TestFileReadTool_OffsetAndLength(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "a.txt", "0123456789")
	tool := NewFileReadTool(dir)

	out, err := tool.Execute(context.Background(), `{"path":"a.txt","offset":3,"length":4}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var env struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
	}
	_ = json.Unmarshal([]byte(out.Text), &env)
	if env.Content != "3456" {
		t.Fatalf("content mismatch: %q", env.Content)
	}
	if !env.Truncated {
		t.Fatalf("should have flagged truncation: %+v", env)
	}
}

func TestFileReadTool_ErrorsOnDirectory(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tool := NewFileReadTool(dir)
	_, err := tool.Execute(context.Background(), `{"path":"sub"}`)
	if err == nil {
		t.Fatal("expected error for directory target")
	}
}

func TestFileReadTool_ErrorsOnMissingPath(t *testing.T) {
	dir := t.TempDir()
	tool := NewFileReadTool(dir)
	_, err := tool.Execute(context.Background(), `{"path":""}`)
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestPathWithin_HandlesEquality(t *testing.T) {
	if !pathWithin("/tmp/a", "/tmp/a") {
		t.Fatal("equal paths should be within")
	}
	if pathWithin("/tmp/abc", "/tmp/a") {
		t.Fatal("prefix-but-not-separator must not be within")
	}
	if !pathWithin("/tmp/a/b", "/tmp/a") {
		t.Fatal("subdir must be within")
	}
}
