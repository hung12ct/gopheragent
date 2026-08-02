package builtin

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func hasBinary(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func TestCodeInterpreterTool_RunsPython(t *testing.T) {
	if !hasBinary("python3") {
		t.Skip("python3 not available")
	}
	tool := NewCodeInterpreterTool()
	out, err := tool.Execute(context.Background(), `{"language":"python","code":"print(2+2)"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var env struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
		TimedOut bool   `json:"timed_out"`
	}
	if err := json.Unmarshal([]byte(out.Text), &env); err != nil {
		t.Fatalf("bad envelope: %v", err)
	}
	if strings.TrimSpace(env.Stdout) != "4" {
		t.Fatalf("stdout: %q", env.Stdout)
	}
	if env.ExitCode != 0 {
		t.Fatalf("exit code: %d", env.ExitCode)
	}
	if env.TimedOut {
		t.Fatal("unexpected timeout")
	}
}

func TestCodeInterpreterTool_CapturesStderrAndExitCode(t *testing.T) {
	if !hasBinary("python3") {
		t.Skip("python3 not available")
	}
	tool := NewCodeInterpreterTool()
	code := `import sys; sys.stderr.write("boom"); sys.exit(3)`
	out, err := tool.Execute(context.Background(), `{"language":"python","code":`+jsonQuote(code)+`}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var env struct {
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}
	_ = json.Unmarshal([]byte(out.Text), &env)
	if !strings.Contains(env.Stderr, "boom") {
		t.Fatalf("stderr: %q", env.Stderr)
	}
	if env.ExitCode != 3 {
		t.Fatalf("exit code: %d", env.ExitCode)
	}
}

func TestCodeInterpreterTool_TimesOut(t *testing.T) {
	if !hasBinary("python3") {
		t.Skip("python3 not available")
	}
	tool := NewCodeInterpreterTool().WithTimeout(200 * time.Millisecond)
	code := `import time; time.sleep(5)`
	out, err := tool.Execute(context.Background(), `{"language":"python","code":`+jsonQuote(code)+`}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var env struct {
		TimedOut bool `json:"timed_out"`
	}
	_ = json.Unmarshal([]byte(out.Text), &env)
	if !env.TimedOut {
		t.Fatalf("expected timeout flag, envelope: %s", out.Text)
	}
}

func TestCodeInterpreterTool_RejectsUnknownLanguage(t *testing.T) {
	tool := NewCodeInterpreterTool()
	_, err := tool.Execute(context.Background(), `{"language":"ruby","code":"puts 1"}`)
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("expected unknown language error, got %v", err)
	}
}

func TestCodeInterpreterTool_RequiresConfirmation(t *testing.T) {
	if !NewCodeInterpreterTool().Descriptor().RequiresConfirmation {
		t.Fatal("code_interpreter must require confirmation")
	}
}

func TestCodeInterpreterTool_RequiresCode(t *testing.T) {
	tool := NewCodeInterpreterTool()
	_, err := tool.Execute(context.Background(), `{"language":"python","code":""}`)
	if err == nil {
		t.Fatal("expected error for empty code")
	}
}

// jsonQuote wraps s as a JSON string literal.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
