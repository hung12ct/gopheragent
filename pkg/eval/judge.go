package eval

import (
	"context"
	"fmt"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
)

const (
	defaultJudgeSamples       = 3
	defaultMaxTranscriptBytes = 16 << 10 // 16 KiB
	judgeTruncationMarker     = "\n…[trajectory truncated]…\n"
)

// Judge grades a transcript with an LLM against a natural-language rubric.
// It issues Samples independent calls and takes a majority vote, dropping
// unparseable samples — model judges are noisy, so a single call is unreliable.
// A "unknown" verdict is an escape hatch: it is excluded from the trial's
// pass/fail decision rather than counted as a failure.
type Judge struct {
	// Provider is any agent.LLMProvider — a real vendor for CI or a fake in
	// tests. Required.
	Provider agent.LLMProvider
	// Rubric describes what a passing answer looks like. Required unless
	// Rubrics is set.
	Rubric string
	// Rubrics, when non-empty, lists individual criteria; the judge scores the
	// fraction satisfied and passes only when all are met. Combined with Rubric
	// if both are set.
	Rubrics []string
	// Samples is the number of independent judge calls (default 3). Ties and
	// all-unknown outcomes resolve conservatively (see Grade).
	Samples int
	// IncludeTrajectory appends the tool-call sequence (names, args, truncated
	// results) to the prompt and switches on evidence discipline: only tool
	// results count as trusted evidence, never the agent's own claims.
	IncludeTrajectory bool
	// MaxTranscriptBytes bounds the trajectory section of the prompt
	// (default 16 KiB, middle-truncated).
	MaxTranscriptBytes int
	// GraderName labels the grade in reports (default "judge").
	GraderName string
}

// judgeVerdict is the schema-constrained response from one judge call.
type judgeVerdict struct {
	Verdict   string  `json:"verdict"` // pass | fail | unknown
	Score     float64 `json:"score"`
	Reasoning string  `json:"reasoning"`
}

// Name implements Grader.
func (j *Judge) Name() string {
	if j.GraderName != "" {
		return j.GraderName
	}
	return "judge"
}

// Grade implements Grader: sample the judge, majority-vote, and build a Grade.
func (j *Judge) Grade(ctx context.Context, tr *Transcript) Grade {
	name := j.Name()
	if j.Provider == nil {
		return Grade{Grader: name, Err: fmt.Errorf("eval: judge %s: provider is nil", name)}
	}
	msgs := j.buildMessages(tr)
	samples := j.Samples
	if samples < 1 {
		samples = defaultJudgeSamples
	}
	var passes, fails, unknowns, scoreN int
	var scoreSum float64
	var lastErr error
	var lastReason string
	for i := 0; i < samples; i++ {
		v, err := j.callOnce(ctx, msgs)
		if err != nil {
			lastErr = err
			continue
		}
		lastReason = v.Reasoning
		switch strings.ToLower(strings.TrimSpace(v.Verdict)) {
		case "pass":
			passes++
			scoreSum += v.Score
			scoreN++
		case "fail":
			fails++
			scoreSum += v.Score
			scoreN++
		default:
			unknowns++
		}
	}
	return tallyVerdict(name, passes, fails, unknowns, scoreN, scoreSum, lastReason, lastErr)
}

// tallyVerdict converts sample counts into a Grade. Ties resolve to fail; an
// all-unknown outcome yields Unknown; zero valid samples yields Err.
func tallyVerdict(name string, passes, fails, unknowns, scoreN int, scoreSum float64, reason string, lastErr error) Grade {
	if passes+fails+unknowns == 0 {
		return Grade{Grader: name, Err: fmt.Errorf("eval: judge %s: no valid samples: %w", name, lastErr)}
	}
	if passes+fails == 0 {
		return Grade{Grader: name, Unknown: true, Reason: "judge returned unknown"}
	}
	score := 0.0
	if scoreN > 0 {
		score = scoreSum / float64(scoreN)
	}
	pass := passes > fails
	tally := fmt.Sprintf("[pass=%d fail=%d unknown=%d] %s", passes, fails, unknowns, reason)
	return Grade{Grader: name, Pass: pass, Score: score, Reason: strings.TrimSpace(tally)}
}

// callOnce issues one schema-constrained judge call.
func (j *Judge) callOnce(ctx context.Context, msgs []history.Message) (judgeVerdict, error) {
	req := agent.GenerateJSONRequest{
		Messages: msgs,
		Output: agent.StructuredOutput{
			Name:        "verdict",
			Description: "evaluation verdict for the agent's answer",
			Schema:      judgeSchema(),
			Strict:      true,
		},
	}
	var v judgeVerdict
	if _, err := agent.GenerateJSONInto(ctx, j.Provider, req, &v); err != nil {
		return judgeVerdict{}, err
	}
	return v, nil
}

// judgeSchema builds the verdict JSON schema fresh each call so no caller can
// mutate a shared map.
func judgeSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"verdict":   map[string]any{"type": "string", "enum": []any{"pass", "fail", "unknown"}},
			"score":     map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			"reasoning": map[string]any{"type": "string"},
		},
		// score is required so a PassPolicy.MinScore gate never sees a
		// silently-zero score from a judge that returned verdict:"pass".
		"required": []any{"verdict", "score", "reasoning"},
	}
}

// buildMessages assembles the system + user prompt for the judge.
func (j *Judge) buildMessages(tr *Transcript) []history.Message {
	var sys strings.Builder
	sys.WriteString("You are a strict evaluator. Grade the OUTCOME of an AI agent's answer against the rubric. ")
	sys.WriteString("Respond only through the structured schema. Quote evidence in your reasoning. ")
	sys.WriteString("Use verdict \"unknown\" when the rubric does not determine an answer — never guess. ")
	sys.WriteString("Set score to the fraction of the rubric satisfied (0..1); verdict \"pass\" only when fully satisfied.")
	if j.IncludeTrajectory {
		sys.WriteString(" Treat ONLY tool results as trusted evidence; the agent's own claims and reasoning are not evidence.")
	}
	return []history.Message{
		{Role: "system", Content: sys.String()},
		{Role: "user", Content: j.userPrompt(tr)},
	}
}

// userPrompt renders the rubric, task input, answer, and optional trajectory.
func (j *Judge) userPrompt(tr *Transcript) string {
	var b strings.Builder
	b.WriteString("# Rubric\n")
	if j.Rubric != "" {
		b.WriteString(j.Rubric)
		b.WriteByte('\n')
	}
	for i, r := range j.Rubrics {
		fmt.Fprintf(&b, "%d. %s\n", i+1, r)
	}
	fmt.Fprintf(&b, "\n# Task input\n%s\n\n# Agent answer\n%s\n", tr.Input.Content, tr.FinalAnswer)
	if j.IncludeTrajectory {
		b.WriteString("\n# Tool calls (evidence)\n")
		b.WriteString(j.formatTrajectory(tr.ToolCalls))
	}
	return b.String()
}

// formatTrajectory renders tool calls, middle-truncated to MaxTranscriptBytes.
func (j *Judge) formatTrajectory(calls []ToolCallRecord) string {
	var b strings.Builder
	for _, c := range calls {
		fmt.Fprintf(&b, "- %s(%s) => %s\n", c.Name, c.ArgsJSON, c.Result)
	}
	limit := j.MaxTranscriptBytes
	if limit <= 0 {
		limit = defaultMaxTranscriptBytes
	}
	return middleTruncate(b.String(), limit)
}

// middleTruncate keeps the head and tail of s when it exceeds limit bytes,
// dropping the middle so both the first and last tool calls stay visible.
func middleTruncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	keep := (limit - len(judgeTruncationMarker)) / 2
	if keep <= 0 {
		return s[:limit]
	}
	return s[:keep] + judgeTruncationMarker + s[len(s)-keep:]
}
