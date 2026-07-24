package eval

import (
	"context"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/llm/llmfake"
)

// verdictProvider scripts one verdict JSON string per judge sample.
func verdictProvider(verdicts ...string) *llmfake.ScriptedProvider {
	turns := make([]llmfake.Turn, len(verdicts))
	for i, v := range verdicts {
		turns[i] = llmfake.Turn{Content: v}
	}
	return &llmfake.ScriptedProvider{Turns: turns}
}

func vpass() string    { return `{"verdict":"pass","score":1,"reasoning":"good"}` }
func vfail() string    { return `{"verdict":"fail","score":0,"reasoning":"bad"}` }
func vunknown() string { return `{"verdict":"unknown","reasoning":"unclear"}` }

func TestJudgeUnanimousPass(t *testing.T) {
	j := &Judge{Provider: verdictProvider(vpass(), vpass(), vpass()), Rubric: "must be good", Samples: 3}
	g := j.Grade(context.Background(), trWith("the answer"))
	if !g.Pass || g.Score != 1 {
		t.Fatalf("expected pass score 1, got %+v", g)
	}
}

func TestJudgeMajorityVote(t *testing.T) {
	j := &Judge{Provider: verdictProvider(vpass(), vpass(), vfail()), Rubric: "r", Samples: 3}
	g := j.Grade(context.Background(), trWith("x"))
	if !g.Pass {
		t.Fatalf("2 pass vs 1 fail should pass: %+v", g)
	}
}

func TestJudgeTieResolvesToFail(t *testing.T) {
	j := &Judge{Provider: verdictProvider(vpass(), vfail()), Rubric: "r", Samples: 2}
	g := j.Grade(context.Background(), trWith("x"))
	if g.Pass {
		t.Fatalf("tie should resolve to fail: %+v", g)
	}
}

func TestJudgeAllUnknown(t *testing.T) {
	j := &Judge{Provider: verdictProvider(vunknown(), vunknown()), Rubric: "r", Samples: 2}
	g := j.Grade(context.Background(), trWith("x"))
	if !g.Unknown || g.Pass {
		t.Fatalf("all-unknown should yield Unknown: %+v", g)
	}
}

func TestJudgeMalformedSampleDropped(t *testing.T) {
	// 2 valid passes + 1 malformed → malformed dropped, still passes.
	j := &Judge{Provider: verdictProvider(vpass(), "not json", vpass()), Rubric: "r", Samples: 3}
	g := j.Grade(context.Background(), trWith("x"))
	if !g.Pass {
		t.Fatalf("malformed sample should be dropped, not fail: %+v", g)
	}
}

func TestJudgeNoValidSamplesErr(t *testing.T) {
	j := &Judge{Provider: verdictProvider("garbage", "junk"), Rubric: "r", Samples: 2}
	g := j.Grade(context.Background(), trWith("x"))
	if g.Err == nil {
		t.Fatalf("all-malformed should surface Err, got %+v", g)
	}
}

func TestJudgeNilProvider(t *testing.T) {
	j := &Judge{Rubric: "r"}
	g := j.Grade(context.Background(), trWith("x"))
	if g.Err == nil {
		t.Fatalf("nil provider should error")
	}
}

func TestJudgeEvidenceDisciplineInPrompt(t *testing.T) {
	j := &Judge{Provider: verdictProvider(vpass()), Rubric: "r", Samples: 1, IncludeTrajectory: true}
	msgs := j.buildMessages(&Transcript{FinalAnswer: "a", ToolCalls: []ToolCallRecord{{Name: "t", Result: "r"}}})
	if !strings.Contains(msgs[0].Content, "trusted evidence") {
		t.Fatalf("evidence-discipline instruction missing from system prompt")
	}
	if !strings.Contains(msgs[1].Content, "Tool calls") {
		t.Fatalf("trajectory section missing from user prompt")
	}
}

func TestMiddleTruncate(t *testing.T) {
	s := strings.Repeat("a", 100) + strings.Repeat("b", 100)
	out := middleTruncate(s, 60)
	if len(out) > 60+len(judgeTruncationMarker) {
		t.Fatalf("truncated length too long: %d", len(out))
	}
	if !strings.HasPrefix(out, "a") || !strings.HasSuffix(out, "b") {
		t.Fatalf("head/tail not preserved: %q", out)
	}
	if s2 := middleTruncate("short", 60); s2 != "short" {
		t.Fatalf("short string should be unchanged: %q", s2)
	}
}
