package eval

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
)

// ToolCallRecord is one tool invocation observed on the event stream,
// enriched after the run with its result from session history when the
// Target implements HistoryReader.
type ToolCallRecord struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ArgsJSON string `json:"args,omitempty"`
	Reused   bool   `json:"reused,omitempty"` // wave executor reused a speculative result
	Source   string `json:"source,omitempty"` // "" = top-level; "subagent:<name>" = forwarded
	// Result is the tool's output, joined from history by correlation ID.
	// Empty when the Target does not implement HistoryReader or the loop did
	// not correlate the row (pre-correlation sessions).
	Result string `json:"result,omitempty"`
}

// HITL record kinds — the observable outcomes of a human-in-the-loop gate.
// The approved path emits no event and so produces no record.
const (
	HITLRequired = "required"  // gate reached with no ConfirmHITL wired (out-of-band)
	HITLDenied   = "denied"    // ConfirmHITL returned false
	HITLTimedOut = "timed_out" // ConfirmHITLTimeout expired before a response
)

// HITLRecord is one human-in-the-loop approval gate observed during a turn.
type HITLRecord struct {
	Tool   string `json:"tool"`
	Args   string `json:"args,omitempty"`
	Kind   string `json:"kind"`             // required | denied | timed_out
	Source string `json:"source,omitempty"` // "" = top-level; "subagent:<name>" = forwarded
}

// Transcript is the record of one turn's run. It is JSON-serializable so it
// can be persisted (see record.go) and re-graded without re-running the
// agent. Bounded by construction: the tool-call and event counts are capped
// by the target's MaxIters configuration; Events is retained only when
// CaptureOptions.KeepEvents is set.
type Transcript struct {
	TaskID     string          `json:"task_id"`
	Trial      int             `json:"trial"`
	TurnIndex  int             `json:"turn_index"`
	SessionKey string          `json:"session_key"`
	Input      history.Message `json:"input"`
	// FinalAnswer is the accumulated top-level (Source=="") assistant text.
	// A top-level ReflectedEvent discards the buffer and replaces it, matching
	// AgentLoop.RunIteration's canonical-answer semantics.
	FinalAnswer  string              `json:"final_answer"`
	ToolCalls    []ToolCallRecord    `json:"tool_calls,omitempty"`
	HITL         []HITLRecord        `json:"hitl,omitempty"` // approval gates that fired
	Events       []agent.StreamEvent `json:"-"`              // retained only when KeepEvents
	Usage        agent.TokenUsage    `json:"usage"`
	CostUSD      float64             `json:"cost_usd,omitempty"`
	Model        string              `json:"model,omitempty"`
	Latency      time.Duration       `json:"latency"`
	Err          error               `json:"-"`
	ErrMessage   string              `json:"error,omitempty"` // Err.Error() for serialization
	TerminatedBy string              `json:"terminated_by"`   // done|error|max_iters|limit_exhausted|ctx
}

// CaptureOptions tunes what Capture records.
type CaptureOptions struct {
	// KeepEvents retains the raw StreamEvent slice on Transcript.Events. Off
	// by default — most graders read the derived fields, not the raw trace.
	KeepEvents bool
	// IncludeSubagents records tool calls forwarded from sub-agents
	// (Source != ""). Off by default: forwarded events are observational, and
	// the framework itself treats them as non-authoritative for the answer.
	IncludeSubagents bool
}

// Capture ranges the target's event stream for one turn and builds a
// Transcript. It never returns an error: agent failures surface as
// Transcript.Err / TerminatedBy so a whole suite keeps running. When target
// implements HistoryReader, tool results are joined afterward.
func Capture(ctx context.Context, target Target, sessionKey string, msg history.Message, opts CaptureOptions) *Transcript {
	tr := &Transcript{SessionKey: sessionKey, Input: msg, TerminatedBy: "done"}
	var answer strings.Builder
	start := time.Now()
	for ev := range target.Run(ctx, sessionKey, msg) {
		if opts.KeepEvents {
			tr.Events = append(tr.Events, ev)
		}
		captureEvent(tr, &answer, ev, opts.IncludeSubagents)
	}
	tr.Latency = time.Since(start)
	tr.FinalAnswer = answer.String()
	// Only attribute termination to ctx when nothing more specific fired — a
	// run that legitimately hit max_iters/limit_exhausted keeps that cause
	// even if the per-trial deadline also expired.
	if ctx.Err() != nil && tr.Err == nil && tr.TerminatedBy == "done" {
		tr.Err = ctx.Err()
		tr.TerminatedBy = "ctx"
	}
	if tr.Err != nil {
		tr.ErrMessage = tr.Err.Error()
	}
	if hr, ok := target.(HistoryReader); ok {
		attachToolResults(ctx, hr, sessionKey, tr)
	}
	return tr
}

// captureEvent folds one stream event into the transcript. Only top-level
// events (Source=="") count toward the answer, usage, cost, and termination;
// forwarded sub-agent events are observational.
func captureEvent(tr *Transcript, answer *strings.Builder, ev agent.StreamEvent, includeSubagents bool) {
	top := ev.Source == ""
	switch p := ev.Payload.(type) {
	case agent.ToolCallEvent:
		if top || includeSubagents {
			tr.ToolCalls = append(tr.ToolCalls, ToolCallRecord{
				ID: p.ID, Name: p.Name, ArgsJSON: p.ArgsJSON, Reused: p.Reused, Source: ev.Source,
			})
		}
	case agent.ContentEvent:
		if top {
			answer.WriteString(p.Text)
		}
	case agent.ReflectedEvent:
		if top && p.Text != "" {
			answer.Reset()
			answer.WriteString(p.Text)
		}
	case agent.UsageEvent:
		if top {
			tr.Usage.PromptTokens += p.Usage.PromptTokens
			tr.Usage.CompletionTokens += p.Usage.CompletionTokens
			tr.Usage.TotalTokens += p.Usage.TotalTokens
		}
	case agent.RunCostEvent:
		if top {
			tr.CostUSD += p.USD
			tr.Model = p.Model
		}
	case agent.ErrorEvent:
		if top {
			if tr.Err == nil {
				if p.Err != nil {
					tr.Err = p.Err
				} else if p.Message != "" {
					tr.Err = fmt.Errorf("agent: %s", p.Message)
				}
			}
			if tr.TerminatedBy == "done" {
				tr.TerminatedBy = "error"
			}
		}
	case agent.MaxItersReachedEvent:
		if top {
			tr.TerminatedBy = "max_iters"
		}
	case agent.LimitExhaustedEvent:
		if top {
			tr.TerminatedBy = "limit_exhausted"
		}
	case agent.ActionRequiredEvent:
		tr.addHITL(top || includeSubagents, p.Tool, p.Args, HITLRequired, ev.Source)
	case agent.HITLDeniedEvent:
		tr.addHITL(top || includeSubagents, p.Tool, p.Args, HITLDenied, ev.Source)
	case agent.HITLTimedOutEvent:
		tr.addHITL(top || includeSubagents, p.Tool, p.Args, HITLTimedOut, ev.Source)
	}
}

// addHITL appends a HITL record when record is true.
func (tr *Transcript) addHITL(record bool, tool, args, kind, source string) {
	if record {
		tr.HITL = append(tr.HITL, HITLRecord{Tool: tool, Args: args, Kind: kind, Source: source})
	}
}

// attachToolResults joins Role=="tool" history rows onto the transcript's
// tool calls by correlation ID (ToolCallEvent.ID == Message.CorrelationID).
// Matching by correlation ID naturally scopes to this turn's calls even on a
// shared multi-turn session, since each turn's stream emits distinct IDs.
func attachToolResults(ctx context.Context, hr HistoryReader, sessionKey string, tr *Transcript) {
	msgs, err := hr.History(ctx, sessionKey)
	if err != nil || len(msgs) == 0 {
		return
	}
	results := make(map[string]string, len(tr.ToolCalls))
	for i := range msgs {
		if msgs[i].Role == "tool" && msgs[i].CorrelationID != "" {
			results[msgs[i].CorrelationID] = msgs[i].Content
		}
	}
	for i := range tr.ToolCalls {
		if r, ok := results[tr.ToolCalls[i].ID]; ok {
			tr.ToolCalls[i].Result = r
		}
	}
}
