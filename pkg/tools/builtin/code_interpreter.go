package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// CodeInterpreterTool runs a short snippet of code in a supported language
// via a subprocess. It is intended for quick calculations, data shaping,
// and ad-hoc scripts — NOT for long-running programs or anything that
// needs network / disk access beyond what the local binary already exposes.
//
// Security posture:
//   - Only whitelisted languages (python / python3 / node) are accepted.
//   - The operator picks the concrete binary via WithInterpreter — callers
//     cannot inject an arbitrary path.
//   - Every run is bounded by a context timeout (default 30s).
//   - Output size is capped (default 64 KiB per stream) to prevent a
//     runaway process from blowing up the LLM context.
//   - RequiresConfirmation() == true — the HITL layer decides whether to
//     actually execute. Turn it off only in trusted, sandboxed environments.
type CodeInterpreterTool struct {
	// interpreters maps language name → absolute/relative binary path.
	// Only languages present here are executable.
	interpreters map[string]string
	timeout      time.Duration
	maxBytes     int
	workingDir   string
}

// NewCodeInterpreterTool returns a tool configured with the default set of
// language bindings resolved via PATH (python3, node), a 30-second timeout,
// and a 64 KiB output cap per stream. Tune further with WithInterpreter /
// WithTimeout / WithMaxBytes / WithWorkingDir.
func NewCodeInterpreterTool() *CodeInterpreterTool {
	return &CodeInterpreterTool{
		interpreters: map[string]string{
			"python":  "python3",
			"python3": "python3",
			"node":    "node",
		},
		timeout:  30 * time.Second,
		maxBytes: 64 * 1024,
	}
}

// WithInterpreter maps (or overrides) a language name to an explicit
// interpreter binary. Pass an absolute path in hardened environments so the
// resolution is not PATH-dependent.
func (t *CodeInterpreterTool) WithInterpreter(language, binary string) *CodeInterpreterTool {
	if t.interpreters == nil {
		t.interpreters = make(map[string]string)
	}
	t.interpreters[strings.ToLower(language)] = binary
	return t
}

// WithTimeout sets the max wall-clock duration for a single run.
func (t *CodeInterpreterTool) WithTimeout(d time.Duration) *CodeInterpreterTool {
	if d > 0 {
		t.timeout = d
	}
	return t
}

// WithMaxBytes caps stdout / stderr per stream. Output past the cap is
// truncated and a flag is returned to the caller.
func (t *CodeInterpreterTool) WithMaxBytes(n int) *CodeInterpreterTool {
	if n > 0 {
		t.maxBytes = n
	}
	return t
}

// WithWorkingDir sets the CWD for the subprocess. Defaults to the parent
// process's CWD. Point this at a scratch directory when running untrusted
// code.
func (t *CodeInterpreterTool) WithWorkingDir(dir string) *CodeInterpreterTool {
	t.workingDir = dir
	return t
}

const codeInterpreterName = "code_interpreter"
const codeInterpreterDescription = "Execute a short code snippet in a supported language (python, node) and return {stdout, stderr, exit_code, timed_out, truncated}. Use for calculations, data transforms, and small utility scripts."

// Descriptor returns metadata for code_interpreter. RequiresConfirmation is
// true because arbitrary code execution is always side-effecting and should
// never run without a human / policy gate.
func (t *CodeInterpreterTool) Descriptor() tools.ToolDescriptor {
	langs := make([]string, 0, len(t.interpreters))
	for k := range t.interpreters {
		langs = append(langs, k)
	}
	return tools.ToolDescriptor{
		Name:        codeInterpreterName,
		Description: codeInterpreterDescription,
		Parameters: tools.ToolSchema{
			Type: "object",
			Properties: map[string]any{
				"language": map[string]any{
					"type":        "string",
					"description": "Language to execute. Must be one of the configured interpreters.",
					"enum":        langs,
				},
				"code": map[string]any{
					"type":        "string",
					"description": "Source code to execute. Will be passed to the interpreter on stdin.",
				},
			},
			Required: []string{"language", "code"},
		},
		RequiresConfirmation: true,
		Display:              tools.DefaultDisplay(codeInterpreterName, codeInterpreterDescription),
	}
}

func (t *CodeInterpreterTool) Execute(ctx context.Context, argsJSON string) (tools.Result, error) {
	var args struct {
		Language string `json:"language"`
		Code     string `json:"code"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return tools.Result{}, fmt.Errorf("tools: invalid arguments: %w", err)
	}
	lang := strings.ToLower(strings.TrimSpace(args.Language))
	if lang == "" {
		return tools.Result{}, fmt.Errorf("tools: language is required")
	}
	if strings.TrimSpace(args.Code) == "" {
		return tools.Result{}, fmt.Errorf("tools: code is required")
	}
	binary, ok := t.interpreters[lang]
	if !ok {
		return tools.Result{}, fmt.Errorf("tools: language %q is not configured", lang)
	}

	timeout := t.timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, binary)
	cmd.Stdin = strings.NewReader(args.Code)
	if t.workingDir != "" {
		cmd.Dir = t.workingDir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	timedOut := runCtx.Err() == context.DeadlineExceeded
	exitCode := 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else if !timedOut {
			// Couldn't launch (binary missing, permission denied, ...).
			return tools.Result{}, fmt.Errorf("tools: exec %s: %w", binary, runErr)
		} else {
			exitCode = -1
		}
	}

	outBytes, outTrunc := capBytes(stdout.Bytes(), t.maxBytes)
	errBytes, errTrunc := capBytes(stderr.Bytes(), t.maxBytes)

	envelope := map[string]any{
		"language":   lang,
		"stdout":     string(outBytes),
		"stderr":     string(errBytes),
		"exit_code":  exitCode,
		"timed_out":  timedOut,
		"truncated":  outTrunc || errTrunc,
		"elapsed_ms": elapsed.Milliseconds(),
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return tools.Result{}, fmt.Errorf("tools: marshal: %w", err)
	}
	return tools.Text(string(out)), nil
}

// capBytes returns b (possibly truncated to max) and whether truncation
// happened. max <= 0 disables the cap.
func capBytes(b []byte, max int) ([]byte, bool) {
	if max <= 0 || len(b) <= max {
		return b, false
	}
	return b[:max], true
}
