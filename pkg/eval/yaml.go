package eval

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"gopkg.in/yaml.v3"
)

// SuiteConfig is the YAML schema for an eval suite. See examples/agent_eval
// for a worked file.
type SuiteConfig struct {
	Suite struct {
		Name              string       `yaml:"name"`
		Trials            int          `yaml:"trials"`
		Concurrency       int          `yaml:"concurrency"`
		Timeout           string       `yaml:"timeout"`
		PassRateThreshold float64      `yaml:"pass_rate_threshold"`
		Tasks             []taskConfig `yaml:"tasks"`
	} `yaml:"suite"`
}

type taskConfig struct {
	ID      string        `yaml:"id"`
	Trials  int           `yaml:"trials"`
	Timeout string        `yaml:"timeout"`
	Pass    passConfig    `yaml:"pass"`
	Input   string        `yaml:"input"`  // single-turn shorthand
	Expect  *expectConfig `yaml:"expect"` // single-turn shorthand
	Turns   []turnConfig  `yaml:"turns"`
}

type passConfig struct {
	AllTrials bool    `yaml:"all_trials"`
	MinScore  float64 `yaml:"min_score"`
}

type turnConfig struct {
	Input  string        `yaml:"input"`
	Expect *expectConfig `yaml:"expect"`
}

type expectConfig struct {
	Tools   *toolsConfig  `yaml:"tools"`
	Answer  *answerConfig `yaml:"answer"`
	NoError bool          `yaml:"no_error"`
	HITL    *hitlConfig   `yaml:"hitl"`
	Judge   *judgeConfig  `yaml:"judge"`
}

type hitlConfig struct {
	Triggered bool     `yaml:"triggered"` // assert a gate fired
	Tools     []string `yaml:"tools"`     // restrict to these tools
	None      bool     `yaml:"none"`      // assert no gate fired
}

type toolsConfig struct {
	Mode             string       `yaml:"mode"`
	IncludeSubagents bool         `yaml:"include_subagents"`
	IgnoreReused     bool         `yaml:"ignore_reused"`
	Calls            []callConfig `yaml:"calls"`
}

type callConfig struct {
	Name       string           `yaml:"name"`
	ArgsExact  string           `yaml:"args_exact"`
	ArgsSubset map[string]any   `yaml:"args_subset"`
	ArgsRegex  *argsRegexConfig `yaml:"args_regex"`
}

type argsRegexConfig struct {
	Key     string `yaml:"key"`
	Pattern string `yaml:"pattern"`
}

type answerConfig struct {
	Contains     []string `yaml:"contains"`
	ContainsFold []string `yaml:"contains_fold"`
	Regex        string   `yaml:"regex"`
	Exact        string   `yaml:"exact"`
}

type judgeConfig struct {
	Rubric            string   `yaml:"rubric"`
	Rubrics           []string `yaml:"rubrics"`
	Samples           int      `yaml:"samples"`
	IncludeTrajectory bool     `yaml:"include_trajectory"`
}

// SuiteValidationError aggregates every problem found in a suite file so they
// can be fixed in one pass (mirrors builder.YAMLValidationError).
type SuiteValidationError struct {
	Path   string
	Issues []string
}

func (e *SuiteValidationError) Error() string {
	return fmt.Sprintf("eval: suite validation failed for %q:\n  - %s", e.Path, strings.Join(e.Issues, "\n  - "))
}

var trajModes = map[string]MatchMode{
	"strict": MatchStrict, "in_order": MatchInOrder, "unordered": MatchUnordered,
	"subset": MatchSubset, "superset": MatchSuperset,
}

// LoadSuite reads and compiles an eval suite YAML file. The judge provider is
// injected here (never named in YAML, so files carry no keys or endpoints);
// pass nil when no task uses a judge block.
func LoadSuite(path string, judge agent.LLMProvider) (Suite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Suite{}, fmt.Errorf("eval: load suite: %w", err)
	}
	return ParseSuiteBytes(path, data, judge)
}

// ParseSuiteBytes compiles suite YAML from bytes. Validation aggregates all
// issues into a single SuiteValidationError.
func ParseSuiteBytes(path string, data []byte, judge agent.LLMProvider) (Suite, error) {
	var cfg SuiteConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Suite{}, fmt.Errorf("eval: parse suite %q: %w", path, err)
	}
	if issues := validateSuite(&cfg, judge); len(issues) > 0 {
		return Suite{}, &SuiteValidationError{Path: path, Issues: issues}
	}
	return compileSuite(&cfg, judge), nil
}

// validateSuite collects every problem in the parsed config.
func validateSuite(cfg *SuiteConfig, judge agent.LLMProvider) []string {
	var issues []string
	s := &cfg.Suite
	if s.Name == "" {
		issues = append(issues, "suite.name is required")
	}
	if s.PassRateThreshold < 0 || s.PassRateThreshold > 1 {
		issues = append(issues, fmt.Sprintf("suite.pass_rate_threshold must be in [0,1], got %v", s.PassRateThreshold))
	}
	checkDuration("suite.timeout", s.Timeout, &issues)
	if len(s.Tasks) == 0 {
		issues = append(issues, "suite.tasks is empty")
	}
	seen := make(map[string]bool)
	for i := range s.Tasks {
		validateTask(fmt.Sprintf("suite.tasks[%d]", i), &s.Tasks[i], judge, seen, &issues)
	}
	return issues
}

// validateTask checks one task and its turns.
func validateTask(prefix string, t *taskConfig, judge agent.LLMProvider, seen map[string]bool, issues *[]string) {
	if t.ID == "" {
		*issues = append(*issues, prefix+".id is required")
	} else if seen[t.ID] {
		*issues = append(*issues, fmt.Sprintf("%s.id %q is duplicated", prefix, t.ID))
	}
	seen[t.ID] = true
	checkDuration(prefix+".timeout", t.Timeout, issues)
	hasShorthand := t.Input != "" || t.Expect != nil
	if hasShorthand && len(t.Turns) > 0 {
		*issues = append(*issues, prefix+": set either input/expect (single turn) or turns, not both")
	}
	if !hasShorthand && len(t.Turns) == 0 {
		*issues = append(*issues, prefix+": task has no turns (add input/expect or turns)")
	}
	if t.Expect != nil {
		validateExpect(prefix+".expect", t.Expect, judge, issues)
	}
	for i := range t.Turns {
		if t.Turns[i].Expect != nil {
			validateExpect(fmt.Sprintf("%s.turns[%d].expect", prefix, i), t.Turns[i].Expect, judge, issues)
		}
	}
}

// validateExpect checks trajectory mode, regexes, and judge provider presence.
func validateExpect(prefix string, e *expectConfig, judge agent.LLMProvider, issues *[]string) {
	if e.Tools != nil {
		if _, ok := trajModes[e.Tools.Mode]; !ok {
			*issues = append(*issues, fmt.Sprintf("%s.tools.mode %q is invalid (strict|in_order|unordered|subset|superset)", prefix, e.Tools.Mode))
		}
		for i, c := range e.Tools.Calls {
			if c.Name == "" {
				*issues = append(*issues, fmt.Sprintf("%s.tools.calls[%d].name is required", prefix, i))
			}
			if c.ArgsRegex != nil {
				checkRegex(fmt.Sprintf("%s.tools.calls[%d].args_regex", prefix, i), c.ArgsRegex.Pattern, issues)
			}
		}
	}
	if e.Answer != nil && e.Answer.Regex != "" {
		checkRegex(prefix+".answer.regex", e.Answer.Regex, issues)
	}
	if e.HITL != nil {
		if !e.HITL.Triggered && !e.HITL.None {
			*issues = append(*issues, prefix+".hitl needs triggered: true or none: true")
		}
		if e.HITL.None && e.HITL.Triggered {
			*issues = append(*issues, prefix+".hitl: set only one of triggered / none")
		}
	}
	if e.Judge != nil {
		if judge == nil {
			*issues = append(*issues, prefix+".judge requires a provider — pass one to LoadSuite")
		}
		if e.Judge.Rubric == "" && len(e.Judge.Rubrics) == 0 {
			*issues = append(*issues, prefix+".judge needs rubric or rubrics")
		}
	}
}

func checkDuration(field, val string, issues *[]string) {
	if val == "" {
		return
	}
	if _, err := time.ParseDuration(val); err != nil {
		*issues = append(*issues, fmt.Sprintf("%s %q is not a duration (e.g. 30s, 2m)", field, val))
	}
}

func checkRegex(field, pattern string, issues *[]string) {
	if _, err := regexp.Compile(pattern); err != nil {
		*issues = append(*issues, fmt.Sprintf("%s /%s/ does not compile: %v", field, pattern, err))
	}
}

// compileSuite builds a Suite from a validated config (cannot fail).
func compileSuite(cfg *SuiteConfig, judge agent.LLMProvider) Suite {
	s := &cfg.Suite
	out := Suite{
		Name: s.Name, Trials: s.Trials, Concurrency: s.Concurrency,
		Timeout: mustDuration(s.Timeout), PassRateThreshold: s.PassRateThreshold,
	}
	for i := range s.Tasks {
		out.Tasks = append(out.Tasks, compileTask(&s.Tasks[i], judge))
	}
	return out
}

func compileTask(t *taskConfig, judge agent.LLMProvider) Task {
	task := Task{
		ID: t.ID, Trials: t.Trials, Timeout: mustDuration(t.Timeout),
		Policy: PassPolicy{MinScore: t.Pass.MinScore, AllTrials: t.Pass.AllTrials},
	}
	if len(t.Turns) > 0 {
		for i := range t.Turns {
			task.Turns = append(task.Turns, compileTurn(t.Turns[i].Input, t.Turns[i].Expect, judge))
		}
	} else {
		task.Turns = append(task.Turns, compileTurn(t.Input, t.Expect, judge))
	}
	return task
}

func compileTurn(input string, e *expectConfig, judge agent.LLMProvider) Turn {
	return Turn{
		Input:   history.Message{Role: "user", Content: input},
		Graders: compileGraders(e, judge),
	}
}

// compileGraders turns an expect block into graders.
func compileGraders(e *expectConfig, judge agent.LLMProvider) []Grader {
	if e == nil {
		return nil
	}
	var gs []Grader
	if e.Tools != nil {
		gs = append(gs, compileTrajectory(e.Tools))
	}
	if e.Answer != nil {
		gs = append(gs, compileAnswer(e.Answer)...)
	}
	if e.NoError {
		gs = append(gs, NoError())
	}
	if e.HITL != nil {
		if e.HITL.None {
			gs = append(gs, NoHITL())
		} else {
			gs = append(gs, HITLTriggered(e.HITL.Tools...))
		}
	}
	if e.Judge != nil {
		gs = append(gs, &Judge{
			Provider: judge, Rubric: e.Judge.Rubric, Rubrics: e.Judge.Rubrics,
			Samples: e.Judge.Samples, IncludeTrajectory: e.Judge.IncludeTrajectory,
		})
	}
	return gs
}

func compileTrajectory(tc *toolsConfig) Grader {
	calls := make([]ExpectedCall, len(tc.Calls))
	for i, c := range tc.Calls {
		calls[i] = ExpectedCall{Name: c.Name, Args: compileArgMatcher(c)}
	}
	return Trajectory(trajModes[tc.Mode], calls, TrajectoryOptions{
		IncludeSubagents: tc.IncludeSubagents, IgnoreReused: tc.IgnoreReused,
	})
}

// compileArgMatcher picks a matcher by precedence: exact > subset > regex.
func compileArgMatcher(c callConfig) ArgMatcher {
	switch {
	case c.ArgsExact != "":
		return ArgsExact(c.ArgsExact)
	case c.ArgsSubset != nil:
		return ArgsSubset(c.ArgsSubset)
	case c.ArgsRegex != nil:
		return ArgsRegex(c.ArgsRegex.Key, c.ArgsRegex.Pattern)
	default:
		return AnyArgs()
	}
}

func compileAnswer(a *answerConfig) []Grader {
	var gs []Grader
	if len(a.Contains) > 0 {
		gs = append(gs, Contains(a.Contains...))
	}
	if len(a.ContainsFold) > 0 {
		gs = append(gs, ContainsFold(a.ContainsFold...))
	}
	if a.Regex != "" {
		gs = append(gs, Regexp(a.Regex))
	}
	if a.Exact != "" {
		gs = append(gs, Exact(a.Exact))
	}
	return gs
}

// mustDuration parses a validated duration string; "" → 0.
func mustDuration(val string) time.Duration {
	if val == "" {
		return 0
	}
	d, _ := time.ParseDuration(val)
	return d
}
