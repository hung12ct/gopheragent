package eval

import (
	"time"

	"github.com/hung12ct/gopheragent/pkg/agent"
)

// TurnReport is the graded result of one turn within a trial.
type TurnReport struct {
	TurnIndex   int              `json:"turn_index"`
	Input       string           `json:"input"`
	FinalAnswer string           `json:"final_answer"`
	ToolCalls   []ToolCallRecord `json:"tool_calls,omitempty"`
	Grades      []Grade          `json:"grades"`
	Pass        bool             `json:"pass"`
}

// TrialReport is the result of one attempt at a task (all turns).
type TrialReport struct {
	TaskID       string           `json:"task_id"`
	Trial        int              `json:"trial"`
	Pass         bool             `json:"pass"`
	Turns        []TurnReport     `json:"turns"`
	Usage        agent.TokenUsage `json:"usage"`
	CostUSD      float64          `json:"cost_usd,omitempty"`
	Latency      time.Duration    `json:"latency"`
	TerminatedBy string           `json:"terminated_by"`
	Error        string           `json:"error,omitempty"` // harness/factory error for this trial
}

// TaskReport aggregates a task's trials. PassAtK (≥1 trial passed) and
// PassAllK (every trial passed) are always computed; Pass reflects the task's
// PassPolicy.
type TaskReport struct {
	TaskID   string        `json:"task_id"`
	Trials   []TrialReport `json:"trials"`
	Pass     bool          `json:"pass"`
	PassAtK  bool          `json:"pass_at_k"`
	PassAllK bool          `json:"pass_all_k"`
}

// Summary rolls up a suite run.
type Summary struct {
	Tasks          int              `json:"tasks"`
	Passed         int              `json:"passed"`
	Failed         int              `json:"failed"`
	PassRate       float64          `json:"pass_rate"`
	TotalUsage     agent.TokenUsage `json:"total_usage"`
	TotalCostUSD   float64          `json:"total_cost_usd,omitempty"`
	Duration       time.Duration    `json:"duration"`
	BelowThreshold bool             `json:"below_threshold"`
}

// SuiteReport is the full result of RunSuite.
type SuiteReport struct {
	Suite     string       `json:"suite"`
	StartedAt time.Time    `json:"started_at"`
	Tasks     []TaskReport `json:"tasks"`
	Summary   Summary      `json:"summary"`
}
