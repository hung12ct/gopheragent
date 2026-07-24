// Package eval is an evaluation harness for GopherAgent agents. It runs an
// agent against a suite of tasks, checks the tool-call trajectory and the
// final answer (code graders and an optional LLM-as-judge), and produces
// pass/fail reports (JSON, JUnit XML, Markdown) that plug into CI.
//
// # Terminology
//
//   - Task    — one test case; a conversation of one or more Turns.
//   - Turn    — one user input plus the graders applied to that turn's answer.
//   - Trial   — one attempt at a Task. Agents are non-deterministic, so a
//     Task may run N trials and report pass@k / pass^k.
//   - Transcript — the record of one turn's run: final answer, tool calls,
//     token usage, cost, latency, termination reason.
//   - Grader  — scores one Transcript. Built-ins (Contains, Regexp, Exact,
//     Trajectory) are deterministic; Judge calls an LLM.
//   - Suite   — a named collection of Tasks with shared defaults.
//
// # Grading philosophy
//
// Grade outcomes first: prefer checking the final answer and the tool
// results over mandating a rigid step sequence. Trajectory graders offer
// relaxed modes (in_order, subset) precisely so a creative-but-correct agent
// path is not penalized. The Judge grader carries an "unknown" escape hatch
// so a model judge never has to guess.
//
// # Zero-cost when unused
//
// Nothing here runs unless a Task references it. Transcript capture allocates
// only what the event stream produces; the raw event slice is retained only
// when CaptureOptions.KeepEvents is set. The judge issues LLM calls only for
// tasks that declare a Judge grader.
//
// # Two ways to run
//
// Inside `go test` via RunT — fully deterministic when the Target is backed
// by llmfake.ScriptedProvider, so it needs no API keys and runs in the
// library's own CI. Or through the cmd/gopherevals CLI against a real
// provider, writing reports and exiting non-zero below a pass-rate threshold.
package eval
