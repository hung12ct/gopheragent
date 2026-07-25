package agenteval

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hung12ct/gopheragent/pkg/builder"
	"github.com/hung12ct/gopheragent/pkg/eval"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/llm/openai"
)

// This suite answers the one question the skills feature rests on and that
// no mechanical test can: does a real model actually reach for the right
// skill from its description alone?
//
// Both directions matter. A model that never activates makes the feature
// useless; a model that activates for everything is worse than useless,
// because it pays the body's tokens on every turn and defeats the point of
// progressive disclosure. So the suite asserts activation AND restraint.
//
// Live by design — it needs a real provider to be meaningful. Skipped
// without OPENAI_API_KEY so CI stays green:
//
//	OPENAI_API_KEY=... go test ./examples/agent_eval/ -run TestSkills -v
const skillsAgentYAML = "../yaml_agents/skilled_assistant.yaml"

func newSkillsTarget(ctx context.Context, _ string, _ int) (eval.Target, error) {
	provider, err := openai.New(os.Getenv("OPENAI_API_KEY"), skillsEvalModel(),
		openai.WithTemperature(0))
	if err != nil {
		return nil, err
	}
	path, err := filepath.Abs(skillsAgentYAML)
	if err != nil {
		return nil, err
	}
	loop, _, _, err := builder.BuildFromYAMLContext(
		ctx, path, builder.NewGlobalCatalog(), provider, nil, nil)
	if err != nil {
		return nil, err
	}
	loop.MaxIters = 4 // activate, then answer — no room to wander
	return eval.WrapLoop(loop), nil
}

func skillsEvalModel() string {
	if m := os.Getenv("SKILLS_EVAL_MODEL"); m != "" {
		return m
	}
	return "gpt-4o-mini"
}

// activates asserts the named skill was loaded. MatchSubset because the model
// may legitimately call other tools around it; what matters is that this one
// happened.
func activates(skill string) eval.Grader {
	return eval.Trajectory(eval.MatchSubset, []eval.ExpectedCall{
		{Name: "read_skill", Args: eval.ArgsSubset(map[string]any{"name": skill})},
	})
}

// activatesNothing asserts no tool ran at all. MatchSuperset means every
// actual call must match some expected entry, and the expected set is empty.
func activatesNothing() eval.Grader {
	return eval.Trajectory(eval.MatchSuperset, nil)
}

func TestSkillsActivation(t *testing.T) {
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("set OPENAI_API_KEY to run the live skills-activation suite")
	}

	suite := eval.Suite{
		Name: "skills-activation",
		// Trials > 1 turns this into a measurement rather than an anecdote:
		// pass@k says it can activate, pass^k says it does so reliably.
		Trials:            3,
		Timeout:           90 * time.Second,
		Concurrency:       4,
		PassRateThreshold: 0.9,
		Tasks: []eval.Task{
			// --- should activate, and which one ---
			{
				ID: "outage-activates-incident-response",
				Turns: []eval.Turn{{
					Input:   history.Message{Role: "user", Content: "the checkout service is throwing 500s across all regions, what do I do?"},
					Graders: []eval.Grader{activates("incident-response"), eval.NoError()},
				}},
			},
			{
				ID: "rollback-activates-incident-response",
				Turns: []eval.Turn{{
					Input:   history.Message{Role: "user", Content: "we shipped a bad deploy 20 minutes ago. how do I roll it back safely?"},
					Graders: []eval.Grader{activates("incident-response"), eval.NoError()},
				}},
			},
			{
				ID: "slow-query-activates-postgres",
				Turns: []eval.Turn{{
					Input:   history.Message{Role: "user", Content: "this report query takes 40 seconds in postgres. should I add an index?"},
					Graders: []eval.Grader{activates("postgres-slow-query"), eval.NoError()},
				}},
			},
			{
				ID: "breaking-change-activates-api-versioning",
				Turns: []eval.Turn{{
					Input:   history.Message{Role: "user", Content: "I need to remove a field from our public API response. how should I roll that out?"},
					Graders: []eval.Grader{activates("api-versioning"), eval.NoError()},
				}},
			},
			{
				ID: "changelog-activates-release-notes",
				Turns: []eval.Turn{{
					Input:   history.Message{Role: "user", Content: "can you turn this changelog into release notes for v2.1?"},
					Graders: []eval.Grader{activates("release-notes"), eval.NoError()},
				}},
			},

			// --- restraint: activating here would waste tokens every turn ---
			{
				ID: "trivia-activates-nothing",
				Turns: []eval.Turn{{
					Input:   history.Message{Role: "user", Content: "what does the acronym TTL stand for?"},
					Graders: []eval.Grader{activatesNothing(), eval.NoError()},
				}},
			},
			{
				ID: "greeting-activates-nothing",
				Turns: []eval.Turn{{
					Input:   history.Message{Role: "user", Content: "hey, what can you help me with?"},
					Graders: []eval.Grader{activatesNothing(), eval.NoError()},
				}},
			},

			// --- near miss: mentions a deploy, but is not an incident ---
			{
				ID: "deploy-question-does-not-activate-incident-response",
				Turns: []eval.Turn{{
					Input: history.Message{Role: "user", Content: "what time of day is best to deploy, generally speaking?"},
					Graders: []eval.Grader{
						eval.Trajectory(eval.MatchSuperset, nil),
						eval.NoError(),
					},
				}},
			},
		},
	}

	rep := eval.RunT(t, &eval.Runner{NewTarget: newSkillsTarget}, suite)

	// pass@k alone would hide flakiness: one lucky trial in three passes the
	// task. pass^k is the number that says the description reliably carries
	// the decision, so report both and name the tasks that differ.
	var reliable int
	var flaky []string
	for _, tr := range rep.Tasks {
		switch {
		case tr.PassAllK:
			reliable++
		case tr.PassAtK:
			flaky = append(flaky, tr.TaskID)
		}
	}
	t.Logf("pass@k  %d/%d  (activated at least once)", rep.Summary.Passed, rep.Summary.Tasks)
	t.Logf("pass^k  %d/%d  (activated on every trial)", reliable, rep.Summary.Tasks)
	if len(flaky) > 0 {
		t.Logf("inconsistent — description is ambiguous for: %v", flaky)
	}
	t.Logf("tokens: %d prompt / %d completion",
		rep.Summary.TotalUsage.PromptTokens, rep.Summary.TotalUsage.CompletionTokens)

	if rep.Summary.BelowThreshold {
		t.Errorf("activation below threshold — the skill descriptions are not carrying the decision")
	}
}
