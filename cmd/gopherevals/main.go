// Command gopherevals runs a GopherAgent eval suite against an agent and
// writes reports for CI. It exits non-zero when the suite's pass rate falls
// below the suite's pass_rate_threshold (or the -threshold override), so a CI
// step fails the build on a regression.
//
// Usage:
//
//	gopherevals -suite suite.yaml -agent agent.yaml -junit out.xml -md report.md
//	gopherevals -suite suite.yaml -from-transcripts run.jsonl   # re-grade offline
//
// The provider is chosen from LLM_PROVIDER (openai|anthropic|gemini) and the
// matching API-key env var; it doubles as the judge model. Two-phase runs use
// -save-transcripts to record a live run and -from-transcripts to re-grade it
// without touching the agent.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/builder"
	"github.com/hung12ct/gopheragent/pkg/eval"
	"github.com/hung12ct/gopheragent/pkg/llm/anthropic"
	"github.com/hung12ct/gopheragent/pkg/llm/gemini"
	"github.com/hung12ct/gopheragent/pkg/llm/openai"
	"github.com/hung12ct/gopheragent/pkg/tools/builtin"
)

type flags struct {
	suite           string
	agent           string
	jsonOut         string
	junitOut        string
	mdOut           string
	threshold       float64
	saveTranscripts string
	fromTranscripts string
}

func main() {
	f := parseFlags()
	if f.suite == "" {
		log.Fatal("gopherevals: -suite is required")
	}
	provider := buildProvider()
	suite, err := eval.LoadSuite(f.suite, provider)
	if err != nil {
		log.Fatalf("gopherevals: %v", err)
	}
	if f.threshold > 0 {
		suite.PassRateThreshold = f.threshold
	}
	rep, err := run(context.Background(), f, suite, provider)
	if err != nil {
		log.Fatalf("gopherevals: %v", err)
	}
	if err := writeReports(f, rep); err != nil {
		log.Fatalf("gopherevals: %v", err)
	}
	fmt.Printf("suite %q: %d/%d passed (%.1f%%)\n", rep.Suite, rep.Summary.Passed, rep.Summary.Tasks, rep.Summary.PassRate*100)
	if rep.Summary.BelowThreshold {
		log.Fatalf("gopherevals: pass rate %.1f%% below threshold %.1f%%", rep.Summary.PassRate*100, suite.PassRateThreshold*100)
	}
}

func parseFlags() flags {
	var f flags
	flag.StringVar(&f.suite, "suite", "", "path to the eval suite YAML (required)")
	flag.StringVar(&f.agent, "agent", "", "path to the builder agent YAML (required for live runs)")
	flag.StringVar(&f.jsonOut, "json", "", "write JSON report to this path")
	flag.StringVar(&f.junitOut, "junit", "", "write JUnit XML report to this path")
	flag.StringVar(&f.mdOut, "md", "", "write Markdown report to this path (- for stdout)")
	flag.Float64Var(&f.threshold, "threshold", 0, "override suite pass_rate_threshold [0,1]")
	flag.StringVar(&f.saveTranscripts, "save-transcripts", "", "write captured transcripts (JSONL) to this path")
	flag.StringVar(&f.fromTranscripts, "from-transcripts", "", "re-grade these stored transcripts instead of running the agent")
	flag.Parse()
	return f
}

// run executes the suite either live (building the agent per trial) or by
// re-grading stored transcripts.
func run(ctx context.Context, f flags, suite eval.Suite, provider agent.LLMProvider) (*eval.SuiteReport, error) {
	if f.fromTranscripts != "" {
		return runFromTranscripts(ctx, f.fromTranscripts, suite)
	}
	if f.agent == "" {
		return nil, fmt.Errorf("-agent is required for a live run (or pass -from-transcripts)")
	}
	return runLive(ctx, f, suite, provider)
}

// runLive builds a fresh agent loop per trial and runs the suite.
func runLive(ctx context.Context, f flags, suite eval.Suite, provider agent.LLMProvider) (*eval.SuiteReport, error) {
	catalog := buildCatalog()
	factory := func(_ context.Context, _ string, _ int) (eval.Target, error) {
		loop, _, _, err := builder.BuildFromYAML(f.agent, catalog, provider, nil)
		if err != nil {
			return nil, fmt.Errorf("build agent %q: %w", f.agent, err)
		}
		return eval.WrapLoop(loop), nil
	}
	runner := &eval.Runner{NewTarget: factory}
	var mu sync.Mutex
	var transcripts []*eval.Transcript
	if f.saveTranscripts != "" {
		runner.OnTranscript = func(tr *eval.Transcript) {
			mu.Lock()
			transcripts = append(transcripts, tr)
			mu.Unlock()
		}
	}
	rep, err := runner.RunSuite(ctx, suite)
	if err != nil {
		return nil, err
	}
	if f.saveTranscripts != "" {
		if err := writeFile(f.saveTranscripts, func(w *os.File) error { return eval.WriteTranscripts(w, transcripts) }); err != nil {
			return nil, err
		}
	}
	return rep, nil
}

// runFromTranscripts re-grades stored transcripts (no agent, no live calls).
func runFromTranscripts(ctx context.Context, path string, suite eval.Suite) (*eval.SuiteReport, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open transcripts %q: %w", path, err)
	}
	defer file.Close()
	ts, err := eval.ReadTranscripts(file)
	if err != nil {
		return nil, err
	}
	return eval.GradeRecorded(ctx, suite, ts)
}

// writeReports emits every requested report format.
func writeReports(f flags, rep *eval.SuiteReport) error {
	if f.jsonOut != "" {
		if err := writeFile(f.jsonOut, func(w *os.File) error { return eval.WriteJSON(w, rep) }); err != nil {
			return err
		}
	}
	if f.junitOut != "" {
		if err := writeFile(f.junitOut, func(w *os.File) error { return eval.WriteJUnit(w, rep) }); err != nil {
			return err
		}
	}
	if f.mdOut == "-" {
		return eval.WriteMarkdown(os.Stdout, rep)
	}
	if f.mdOut != "" {
		return writeFile(f.mdOut, func(w *os.File) error { return eval.WriteMarkdown(w, rep) })
	}
	return nil
}

// writeFile creates path and passes the handle to fn.
func writeFile(path string, fn func(*os.File) error) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %q: %w", path, err)
	}
	if err := fn(file); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// buildCatalog registers the standard built-in tools so YAML agents that
// declare them build. Adopters with custom tools can vendor this command and
// register their own here.
func buildCatalog() *builder.GlobalCatalog {
	catalog := builder.NewGlobalCatalog()
	if err := builtin.RegisterWebSearch(catalog, ""); err != nil {
		log.Printf("gopherevals: websearch unavailable: %v", err)
	}
	builtin.RegisterReadURL(catalog)
	return catalog
}

// buildProvider selects an LLM provider from the environment. Falls back to a
// mock provider so the pipeline can be smoke-tested without keys.
func buildProvider() agent.LLMProvider {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LLM_PROVIDER"))) {
	case "openai":
		if p, err := openai.New("", strings.TrimSpace(os.Getenv("OPENAI_MODEL"))); err == nil {
			return p
		}
	case "anthropic", "claude":
		if p, err := anthropic.New("", strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL"))); err == nil {
			return p
		}
	case "gemini":
		if p, err := gemini.New("", strings.TrimSpace(os.Getenv("GEMINI_MODEL"))); err == nil {
			return p
		}
	}
	log.Print("gopherevals: no LLM_PROVIDER set — using mock provider (set LLM_PROVIDER=openai|anthropic|gemini for real runs)")
	return agent.NewMockProvider()
}
