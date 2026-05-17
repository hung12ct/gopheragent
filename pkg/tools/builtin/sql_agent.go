package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// SQLQueryEvent carries metadata about a SQL query executed by the sub-agent.
// Error is non-empty when the query failed (DB error, DML rejection, or
// validation failure before execution).
//
// Columns / Rows / Truncated are populated on successful execution so adopters
// can flow result sets out of the sub-agent (e.g. into a side-by-side data
// grid) without re-running the query against their own DB handle. On failure
// these fields are zero-valued; partial rows from a mid-iteration error are
// still surfaced, mirroring the SQLResult returned to the model.
type SQLQueryEvent struct {
	SessionKey string
	Query      string
	Error      string           // empty on success
	Columns    []string         // populated on success; nil on early validation failure
	Rows       []map[string]any // populated on success; nil on early validation failure
	Truncated  bool             // true when the MaxRows safety cap clipped output
}

// SQLResult is the structured envelope returned from execute_sql back to the
// sub-agent. Keeping the result structured (rather than a bare JSON array)
// makes the model's retry behaviour much more reliable — it can see the row
// count, truncation flag, and execution time and decide whether the result
// is complete or needs refinement.
type SQLResult struct {
	SQL         string           `json:"sql"`
	Columns     []string         `json:"columns,omitempty"`
	Rows        []map[string]any `json:"rows,omitempty"`
	RowCount    int              `json:"row_count"`
	ExecutionMs int64            `json:"execution_ms"`
	Truncated   bool             `json:"truncated,omitempty"`
	Error       string           `json:"error,omitempty"`
}

// CallSQLAgentTool converts natural-language questions to SQL by delegating to an
// isolated sub-agent. The sub-agent sees only the execute_sql tool and a
// schema-grounded system prompt; the database handle is never exposed to the
// parent agent.
//
// Hardening applied to every executed statement:
//   - multi-statement input is rejected
//   - comments are stripped before classification (so "/* SELECT */ DROP"
//     cannot masquerade as read-only)
//   - only SELECT / WITH / EXPLAIN / SHOW / DESCRIBE are allowed
//   - an implicit LIMIT is appended when MaxRows > 0 and the query lacks one
//   - each query runs under an independent QueryTimeout context
//
// Builder methods (OnSQL, WithSchema, WithExamples, WithBusinessRules,
// WithMaxRows, WithQueryTimeout) are chainable and safe to call in any
// order; they mutate the receiver and return it.
type CallSQLAgentTool struct {
	db                   *sql.DB
	schemaRaw            string
	schema               *Schema
	examples             []SQLExample
	businessRules        []string
	maxRows              int
	queryTimeout         time.Duration
	selfConsistency      int
	sessionManager       agent.SessionManager
	provider             agent.LLMProvider
	onSQL                func(context.Context, SQLQueryEvent)
	name                 string
	display              *tools.ToolDisplay
	requiresConfirmation         bool
	allowMutations               bool
	execSQLRequiresConfirmation  bool
}

// NewCallSQLAgentTool initializes a tool capable of querying databases. The
// schemaContext string is used verbatim when no structured Schema is
// registered via WithSchema — pass an empty string and call WithSchema to
// use the structured path exclusively.
//
// Defaults: Name() = "call_sql_agent", RequiresConfirmation() = true. Use
// WithName / WithDisplay / WithRequiresConfirmation to override when
// registering multiple instances or running unsupervised.
func NewCallSQLAgentTool(db *sql.DB, schemaContext string, sm agent.SessionManager, provider agent.LLMProvider) *CallSQLAgentTool {
	return &CallSQLAgentTool{
		db:                   db,
		schemaRaw:            schemaContext,
		sessionManager:       sm,
		provider:             provider,
		requiresConfirmation: true,
	}
}

// OnSQL registers a callback invoked every time the sub-agent executes a SQL
// query. Use it for logging, auditing, or streaming SQL to the parent
// application. Called on both success and failure — ev.Error is non-empty on
// failure.
func (t *CallSQLAgentTool) OnSQL(fn func(context.Context, SQLQueryEvent)) *CallSQLAgentTool {
	t.onSQL = fn
	return t
}

// WithSchema registers a structured schema. When set, its markdown rendering
// replaces the raw schemaContext passed to NewCallSQLAgentTool in the system
// prompt. Structured schemas produce tighter, more consistent grounding and
// are required for downstream features like schema linking.
func (t *CallSQLAgentTool) WithSchema(s Schema) *CallSQLAgentTool {
	t.schema = &s
	return t
}

// WithExamples registers few-shot Question→SQL demonstrations that are
// injected into the sub-agent's system prompt. Even 2–3 examples anchored
// to the schema materially reduce hallucination on domain-specific queries.
func (t *CallSQLAgentTool) WithExamples(examples ...SQLExample) *CallSQLAgentTool {
	t.examples = append(t.examples, examples...)
	return t
}

// WithBusinessRules registers free-form domain rules (glossary, naming
// conventions, metric definitions) that are injected into the system prompt.
// Use short, imperative sentences — "revenue" means NET revenue, exclude
// refunds; "active" users have login_at in the last 30 days.
func (t *CallSQLAgentTool) WithBusinessRules(rules ...string) *CallSQLAgentTool {
	t.businessRules = append(t.businessRules, rules...)
	return t
}

// WithMaxRows appends "LIMIT n" to any SELECT or WITH statement that does
// not already contain a LIMIT clause. n <= 0 disables the behaviour
// (default). Set this to keep accidental "SELECT * FROM large_table" calls
// from returning millions of rows.
func (t *CallSQLAgentTool) WithMaxRows(n int) *CallSQLAgentTool {
	t.maxRows = n
	return t
}

// WithQueryTimeout caps the wall-clock time of each underlying QueryContext
// call. d <= 0 disables the timeout (default). Separate from the agent's
// overall request context, which may be much longer.
func (t *CallSQLAgentTool) WithQueryTimeout(d time.Duration) *CallSQLAgentTool {
	t.queryTimeout = d
	return t
}

// WithSelfConsistency enables self-consistency decoding: the tool spawns n
// independent sub-agent runs in parallel, clusters their final SQL results
// by execution output, and returns the natural-language answer from the
// winning (largest) cluster.
//
// Costs scale linearly with n — each run is a full sub-agent invocation —
// so this is opt-in and intended for high-stakes analytical queries.
// n <= 1 disables the behaviour (default); odd values (3, 5) break ties
// naturally.
//
// All candidates share the same schema, examples, and business rules —
// variation comes from the LLM's own sampling temperature. For stronger
// diversity, point each run at a slightly-different provider (cheap base
// + reasoning-tuned) via llm.RouterProvider.
func (t *CallSQLAgentTool) WithSelfConsistency(n int) *CallSQLAgentTool {
	t.selfConsistency = n
	return t
}

// WithName overrides the tool name reported to the LLM. Use this to register
// multiple SQL-agent instances (e.g. one per tenant or datalake) in the same
// registry without wrapping in a shim type. Empty string restores the default
// "call_sql_agent".
func (t *CallSQLAgentTool) WithName(name string) *CallSQLAgentTool {
	t.name = name
	return t
}

// WithDisplay overrides the tool display metadata (label, category) shown by
// integrators in UI surfaces. Pass the zero value to fall back to the
// auto-derived default.
func (t *CallSQLAgentTool) WithDisplay(d tools.ToolDisplay) *CallSQLAgentTool {
	t.display = &d
	return t
}

// WithRequiresConfirmation overrides the HITL gate. The default is true so
// that every SQL invocation flows through the confirmation hook in
// human-supervised setups. Set to false for autonomous agents where the
// caller has already vetted the SQL surface (typically read-only DBs paired
// with WithMaxRows / WithQueryTimeout).
func (t *CallSQLAgentTool) WithRequiresConfirmation(b bool) *CallSQLAgentTool {
	t.requiresConfirmation = b
	return t
}

// WithExecuteSQLConfirmation toggles HITL on the inner execute_sql tool
// — the leaf that actually hits the database — rather than (only) on the
// outer call_sql_agent boundary. When true, every SQL statement the
// sub-agent generates is surfaced through the parent loop's ConfirmHITL
// with the rendered SQL (the `sql_query` field of executeSQLArgs) as the
// argsJSON the host UI shows for approval. Default is false, preserving
// the original "approve the natural-language request" gate.
//
// **Workbench UX pattern (Phin):** `WithExecuteSQLConfirmation(true).
// WithRequiresConfirmation(false)` — skip the outer "may I call the
// sub-agent?" prompt and instead approve the actual SQL the model
// produced. Pair with WithAllowMutations(true) when the user must see
// UPDATE/DELETE statements before they run.
//
// **Chatbot pattern (default):** `WithRequiresConfirmation(true).
// WithExecuteSQLConfirmation(false)` — adopters care about intent, not
// statements; one approval per request.
//
// **Both true:** double-confirmation. Intent gated, then every SQL
// re-confirmed.
//
// The parent's ConfirmHITL is propagated to the sub-agent automatically
// via the tool ctx (no extra wiring required). A nil parent ConfirmHITL
// will fall back to the EventTypeActionRequired path inside the sub-agent
// same as on the parent loop.
func (t *CallSQLAgentTool) WithExecuteSQLConfirmation(b bool) *CallSQLAgentTool {
	t.execSQLRequiresConfirmation = b
	return t
}

// WithAllowMutations toggles whether the sub-agent may emit DML — INSERT,
// UPDATE, DELETE, MERGE — in addition to the read-only verbs. Default is
// false, preserving the original read-only contract.
//
// DDL (DROP, CREATE, ALTER, TRUNCATE, GRANT, REVOKE) remains rejected
// regardless of this flag. The existing hardening (multi-statement reject,
// comment strip, per-query timeout, structured SQLResult envelope) stays
// in force for the mutation path; mutations execute via ExecContext and
// surface the affected-row count in SQLResult.RowCount.
//
// **Pair with the HITL gate.** Mutations have side effects you usually
// want the human in the loop for: leave RequiresConfirmation at the true
// default and wire AgentLoop.ConfirmHITL so every UPDATE / DELETE is
// surfaced for explicit user approval. Auto-approve should be off for
// mutation-enabled agents.
func (t *CallSQLAgentTool) WithAllowMutations(allow bool) *CallSQLAgentTool {
	t.allowMutations = allow
	return t
}

// Name implements tools.Tool.
func (t *CallSQLAgentTool) Name() string {
	if t.name != "" {
		return t.name
	}
	return "call_sql_agent"
}

// Description implements tools.Tool.
func (t *CallSQLAgentTool) Description() string {
	return "Translate natural language business questions into SQL and query the Database directly. It automatically determines tables, runs queries, and returns structured data."
}

type sqlAgentArgs struct {
	Query string `json:"query" description:"The natural language query or task for the SQL database Sub-Agent to perform."`
}

// ParametersSchema implements tools.Tool.
func (t *CallSQLAgentTool) ParametersSchema() tools.ToolSchema {
	return tools.SchemaFor[sqlAgentArgs]()
}

// RequiresConfirmation reports whether each invocation must pass the HITL
// gate. Defaults to true; override via WithRequiresConfirmation(false) for
// autonomous agents.
func (t *CallSQLAgentTool) RequiresConfirmation() bool {
	return t.requiresConfirmation
}

// Execute runs the sub-agent loop. The sub-agent sees the schema, examples,
// and business rules but never the database handle directly — it must call
// execute_sql to actually touch the DB.
//
// When WithSelfConsistency(n) with n > 1 is configured, n independent sub-
// agents run in parallel and their execution results are clustered by hash;
// the answer from the winning cluster is returned.
func (t *CallSQLAgentTool) Display() tools.ToolDisplay {
	if t.display != nil {
		return *t.display
	}
	return tools.DefaultDisplay(t.Name(), t.Description())
}
func (t *CallSQLAgentTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args sqlAgentArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("tools: invalid arguments: %w", err)
	}

	if t.selfConsistency > 1 {
		return t.runCandidates(ctx, args.Query, t.selfConsistency)
	}
	cand := t.runOnce(ctx, args.Query, 0)
	if cand.finalResp == "" {
		return "Process finished but no verbal response was given. Check logs.", nil
	}
	return cand.finalResp, nil
}

// runOnce executes a single sub-agent run and returns its candidate record.
// The `idx` argument disambiguates parallel runs so their session keys do
// not collide when spawned from runCandidates.
func (t *CallSQLAgentTool) runOnce(ctx context.Context, query string, idx int) sqlCandidate {
	subSessionKey := fmt.Sprintf("sub_agent_sql_%d_%d", time.Now().UnixNano(), idx)
	defer func() {
		if err := t.sessionManager.Delete(context.Background(), subSessionKey); err != nil && t.onSQL != nil {
			t.onSQL(ctx, SQLQueryEvent{
				SessionKey: subSessionKey,
				Error:      fmt.Sprintf("session cleanup failed: %v", err),
			})
		}
	}()

	exec := &executeSQLTool{
		db:                   t.db,
		onSQL:                t.onSQL,
		sessionKey:           subSessionKey,
		maxRows:              t.maxRows,
		queryTimeout:         t.queryTimeout,
		allowMutations:       t.allowMutations,
		requiresConfirmation: t.execSQLRequiresConfirmation,
	}
	sqlTools := tools.NewRegistry()
	sqlTools.Register(exec)

	subAgent := agent.NewAgentLoop(t.sessionManager, sqlTools, t.provider)
	subAgent.DynamicContext = agent.DynamicContextFuncFromContext(ctx)
	// Propagate the parent loop's HITL gate so RequiresConfirmation on
	// execute_sql surfaces through the same approval path as the outer
	// call_sql_agent boundary. Nil-safe — when the parent has no
	// ConfirmHITL, the sub-agent falls back to EventTypeActionRequired.
	subAgent.ConfirmHITL = agent.ConfirmHITLFromContext(ctx)
	subAgent.ConfirmHITLTimeout = agent.ConfirmHITLTimeoutFromContext(ctx)

	if err := t.sessionManager.SaveHistory(ctx, subSessionKey, []history.Message{
		{Role: "system", Content: t.buildSystemPrompt()},
	}); err != nil && t.onSQL != nil {
		t.onSQL(ctx, SQLQueryEvent{
			SessionKey: subSessionKey,
			Error:      fmt.Sprintf("seed history failed: %v", err),
		})
	}

	streamChan := make(chan agent.StreamEvent, 100)
	go subAgent.RunIterationStream(ctx, subSessionKey, query, streamChan)

	var finalResult strings.Builder
	var hitlEvent agent.StreamEvent
	for event := range streamChan {
		switch event.Type {
		case agent.EventTypeContent:
			finalResult.WriteString(event.Content)
		case agent.EventTypeHITLDenied, agent.EventTypeHITLTimedOut:
			// Latch the most recent HITL signal so the candidate report
			// surfaces it explicitly to the outer agent instead of letting
			// the inner sub-agent's paraphrase ("the query was not
			// executed") absorb the cause.
			if event.Source == "" {
				hitlEvent = event
			}
		}
	}

	return sqlCandidate{
		index:      idx,
		finalResp:  hitlBlockedReport(hitlEvent, finalResult.String()),
		lastResult: exec.lastCapture(),
	}
}

// hitlBlockedReport prepends an unambiguous "HITL_BLOCKED" directive to the
// sub-agent's final answer when the inner HITL gate fired, so the outer
// agent sees the cause directly rather than the worker's paraphrase. Pass
// through unchanged when no HITL event was observed.
func hitlBlockedReport(hitlEvent agent.StreamEvent, innerSummary string) string {
	if hitlEvent.Type == "" {
		return innerSummary
	}
	switch hitlEvent.Type {
	case agent.EventTypeHITLTimedOut:
		p, _ := hitlEvent.Payload().(agent.HITLTimedOutEvent)
		return fmt.Sprintf("HITL_BLOCKED: timeout — the human approval prompt for tool %q expired after %s before the operator responded. The user did NOT refuse. Tell the user the approval window closed and ask them to retry when they are ready to confirm; do not paraphrase this as a denial or seek a workaround. Inner agent summary (may be misleading): %s", p.Tool, p.Timeout, innerSummary)
	default:
		p, _ := hitlEvent.Payload().(agent.HITLDeniedEvent)
		return fmt.Sprintf("HITL_BLOCKED: denied — the human operator denied approval for tool %q. Tell the user directly that they have not granted the permission this action requires, and ask whether they have an alternative approach; do not silently rephrase or work around the gate. Inner agent summary (may be misleading): %s", p.Tool, innerSummary)
	}
}

// runCandidates fans out n parallel sub-agent runs, clusters their execution
// results, and returns the answer from the winning cluster.
func (t *CallSQLAgentTool) runCandidates(ctx context.Context, query string, n int) (string, error) {
	cands := make([]sqlCandidate, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			cands[idx] = t.runOnce(ctx, query, idx)
		}(i)
	}
	wg.Wait()

	winner := pickByMajority(cands)
	if winner == nil || winner.finalResp == "" {
		return "Process finished but no verbal response was given. Check logs.", nil
	}
	return winner.finalResp, nil
}

// buildSystemPrompt assembles the sub-agent's system instruction from the
// configured schema, examples, and business rules.
func (t *CallSQLAgentTool) buildSystemPrompt() string {
	schemaBlock := t.schemaRaw
	if t.schema != nil {
		schemaBlock = t.schema.String()
	}

	var b strings.Builder
	if t.allowMutations {
		b.WriteString(`You are a SQL database expert. Translate the user's natural-language request into a valid SQL statement (read or DML), execute it with execute_sql, and return the raw JSON result untouched. Propose mutations only when the user explicitly asks to change data; default to reads otherwise.

**Schema (use ONLY these tables and columns — never invent names):**

`)
	} else {
		b.WriteString(`You are a SQL database expert. Translate the user's natural-language question into a valid, read-only SQL query, execute it with execute_sql, and return the raw JSON result untouched.

**Schema (use ONLY these tables and columns — never invent names):**

`)
	}
	b.WriteString(schemaBlock)
	b.WriteString("\n")

	if len(t.businessRules) > 0 {
		b.WriteString("\n**Business rules & glossary (follow unconditionally):**\n\n")
		for _, r := range t.businessRules {
			fmt.Fprintf(&b, "- %s\n", r)
		}
	}

	if len(t.examples) > 0 {
		b.WriteString("\n**Examples (mimic their style and column selection):**\n\n")
		for i, ex := range t.examples {
			fmt.Fprintf(&b, "Example %d — Q: %s\n```sql\n%s\n```\n", i+1, ex.Question, strings.TrimSpace(ex.SQL))
			if ex.Notes != "" {
				fmt.Fprintf(&b, "Reasoning: %s\n", ex.Notes)
			}
			b.WriteString("\n")
		}
	}

	b.WriteString(`**SQL Rules (follow unconditionally):**

1. SELECT only the columns needed — never SELECT *.
2. Refer to tables by their exact name. Use "table_name" quoting when names are case-sensitive or contain special characters.
3. Join as few tables as possible. Ensure all join columns share the same data type. Prefer INNER JOIN over nested subqueries.
4. Complete all JOINs before applying aggregate functions (MAX, MIN, SUM, etc.).
5. Every non-aggregated column in SELECT must appear in GROUP BY.
6. Use SQL AS to alias columns and subqueries for clarity.
7. Enclose subqueries and UNION queries in parentheses.
8. Use WHERE, HAVING, and other filters to minimize returned rows. Add LIMIT when the result set could be large and no aggregation is applied.
9. Filter NULLs with WHERE <col> IS NOT NULL or via JOIN. Use ORDER BY ... NULLS LAST for nullable columns.
10. Use DISTINCT only when unique values are explicitly needed.
11. For fuzzy text matching: LOWER(col) LIKE '%value%'.
`)
	if t.allowMutations {
		b.WriteString("12. Mutations (INSERT, UPDATE, DELETE, MERGE) are permitted. Only emit them when the user explicitly asks to change data, and target a narrow, scoped row set (always include a WHERE clause for UPDATE/DELETE). DDL (DROP, CREATE, ALTER, TRUNCATE, GRANT, REVOKE) and other statements remain rejected.\n")
	} else {
		b.WriteString("12. Read-only: NEVER write UPDATE, DELETE, DROP, INSERT, CREATE, ALTER, TRUNCATE, MERGE, GRANT, or REVOKE — the tool will reject them.\n")
	}
	b.WriteString(`13. Submit one statement at a time. Multiple statements separated by ';' are rejected.

**Reasoning strategy (Chain-of-Thought, use for complex queries):**

1. Restate the question. Write Pseudo SQL with <placeholder> for unknowns.
2. For each placeholder, write its sub-question and Pseudo SQL. Recurse if needed.
3. Replace placeholders bottom-up to assemble the final SQL.
4. Simplify: remove redundant subqueries, convert to JOINs where possible.
5. Execute with execute_sql.

**Self-correction:** If execute_sql returns a structured result with non-empty "error", analyze the error carefully. Fix only the SQL syntax or column references — do not change table names or literal values. Retry with the corrected query. If a result is empty but the error field is empty, inspect your WHERE/JOIN/GROUP BY before retrying.`)

	return b.String()
}

// ---------------------------------------------------------
// executeSQLTool is the internal tool available only to the SQLAgent
// sub-agent. It layers validation → LIMIT injection → execution so the
// model cannot accidentally (or intentionally) escape the read-only
// contract.
// ---------------------------------------------------------
type executeSQLTool struct {
	db                   *sql.DB
	sessionKey           string
	onSQL                func(context.Context, SQLQueryEvent)
	maxRows              int
	queryTimeout         time.Duration
	allowMutations       bool
	requiresConfirmation bool

	// capture, when non-nil, records the last successful SQLResult observed
	// in this instance. Used by self-consistency clustering. Protected by
	// captureMu because the LLM may drive parallel tool calls within a
	// single run.
	captureMu sync.Mutex
	capture   *SQLResult
}

func (t *executeSQLTool) Name() string { return "execute_sql" }

func (t *executeSQLTool) Description() string {
	if t.allowMutations {
		return "Execute a single SQL statement against the database and return a structured JSON envelope {sql, columns, rows, row_count, execution_ms, truncated, error}. Reads (SELECT, WITH, EXPLAIN, SHOW, DESCRIBE) and mutations (INSERT, UPDATE, DELETE, MERGE) are accepted; DDL and multi-statement input are rejected. For mutations, row_count reports the affected-row count and columns/rows are empty."
	}
	return "Execute a single read-only SQL statement against the database and return a structured JSON envelope {sql, columns, rows, row_count, execution_ms, truncated, error}. Multi-statement input and DDL/DML are rejected."
}

type executeSQLArgs struct {
	SQLQuery string `json:"sql_query" description:"The exact SQL query string to run. Single statement only."`
}

func (t *executeSQLTool) ParametersSchema() tools.ToolSchema {
	return tools.SchemaFor[executeSQLArgs]()
}

func (t *executeSQLTool) RequiresConfirmation() bool { return t.requiresConfirmation }

func (t *executeSQLTool) Display() tools.ToolDisplay { return tools.DefaultDisplay(t.Name(), t.Description()) }

// Execute dispatches to executeRead or executeMutation based on
// ClassifySQL. Unknown verbs and multi-statement input are rejected
// here before any DB I/O.
func (t *executeSQLTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args executeSQLArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("tools: invalid arguments: %w", err)
	}
	sqlStr := strings.TrimSpace(args.SQLQuery)

	emit := t.makeEmitFunc(ctx)

	kind, err := ClassifySQL(sqlStr)
	if err != nil {
		return emit(SQLResult{SQL: sqlStr, Error: err.Error()})
	}
	if kind == SQLKindMutation {
		if !t.allowMutations {
			return emit(SQLResult{
				SQL:   sqlStr,
				Error: fmt.Sprintf("tools: statement type %q is not permitted on a read-only sql agent; enable via CallSQLAgentTool.WithAllowMutations(true)", firstKeyword(StripSQLComments(sqlStr))),
			})
		}
		return t.executeMutation(ctx, sqlStr, emit)
	}
	return t.executeRead(ctx, sqlStr, emit)
}

// makeEmitFunc returns a closure that fans the SQLResult out to OnSQL
// (so adopters see columns/rows/truncated alongside the query) and
// marshals the same payload back to the model. Centralising both keeps
// every exit path in sync — no silently-empty Rows for success and no
// missing OnSQL call on error.
func (t *executeSQLTool) makeEmitFunc(ctx context.Context) func(SQLResult) (string, error) {
	return func(res SQLResult) (string, error) {
		if t.onSQL != nil {
			t.onSQL(ctx, SQLQueryEvent{
				SessionKey: t.sessionKey,
				Query:      res.SQL,
				Error:      res.Error,
				Columns:    res.Columns,
				Rows:       res.Rows,
				Truncated:  res.Truncated,
			})
		}
		b, err := json.Marshal(res)
		if err != nil {
			return "", fmt.Errorf("tools: marshal result: %w", err)
		}
		return string(b), nil
	}
}

// executeRead runs the read-only path: optional LIMIT injection, then
// QueryContext + row iteration into a structured SQLResult.
func (t *executeSQLTool) executeRead(ctx context.Context, sqlStr string, emit func(SQLResult) (string, error)) (string, error) {
	// 1. Apply LIMIT if configured and missing.
	effectiveSQL := EnsureLimit(sqlStr, t.maxRows)

	// 2. Per-query timeout layered on top of the request context.
	queryCtx := ctx
	if t.queryTimeout > 0 {
		var cancel context.CancelFunc
		queryCtx, cancel = context.WithTimeout(ctx, t.queryTimeout)
		defer cancel()
	}

	start := time.Now()
	rows, err := t.db.QueryContext(queryCtx, effectiveSQL)
	if err != nil {
		return emit(SQLResult{
			SQL:         effectiveSQL,
			ExecutionMs: time.Since(start).Milliseconds(),
			Error:       err.Error(),
		})
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return emit(SQLResult{
			SQL:         effectiveSQL,
			ExecutionMs: time.Since(start).Milliseconds(),
			Error:       fmt.Sprintf("failed to read columns: %v", err),
		})
	}

	var results []map[string]any
	truncated := false
	for rows.Next() {
		columns := make([]any, len(cols))
		columnPointers := make([]any, len(cols))
		for i := range columns {
			columnPointers[i] = &columns[i]
		}
		if err := rows.Scan(columnPointers...); err != nil {
			return emit(SQLResult{
				SQL:         effectiveSQL,
				ExecutionMs: time.Since(start).Milliseconds(),
				Error:       fmt.Sprintf("scan error: %v", err),
			})
		}
		rowData := make(map[string]any, len(cols))
		for i, colName := range cols {
			val := columns[i]
			if b, ok := val.([]byte); ok {
				rowData[colName] = string(b)
			} else {
				rowData[colName] = val
			}
		}
		results = append(results, rowData)
		// Defensive hard cap — EnsureLimit already added LIMIT, but a
		// malformed query or a server that ignores LIMIT shouldn't be able
		// to OOM the agent. 2×MaxRows gives us a small safety margin.
		if t.maxRows > 0 && len(results) >= 2*t.maxRows {
			truncated = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return emit(SQLResult{
			SQL:         effectiveSQL,
			ExecutionMs: time.Since(start).Milliseconds(),
			Error:       fmt.Sprintf("row iteration error: %v", err),
			Columns:     cols,
			Rows:        results,
			RowCount:    len(results),
		})
	}

	finalRes := SQLResult{
		SQL:         effectiveSQL,
		Columns:     cols,
		Rows:        results,
		RowCount:    len(results),
		ExecutionMs: time.Since(start).Milliseconds(),
		Truncated:   truncated,
	}

	t.captureMu.Lock()
	copied := finalRes
	t.capture = &copied
	t.captureMu.Unlock()

	return emit(finalRes)
}

// executeMutation runs a DML statement via ExecContext and reports the
// affected-row count in SQLResult.RowCount. EnsureLimit is intentionally
// skipped — auto-injecting LIMIT into UPDATE/DELETE has dialect-specific
// semantics (MySQL accepts it, Postgres doesn't) and silently changes
// the meaning of the statement.
func (t *executeSQLTool) executeMutation(ctx context.Context, sqlStr string, emit func(SQLResult) (string, error)) (string, error) {
	queryCtx := ctx
	if t.queryTimeout > 0 {
		var cancel context.CancelFunc
		queryCtx, cancel = context.WithTimeout(ctx, t.queryTimeout)
		defer cancel()
	}

	start := time.Now()
	res, err := t.db.ExecContext(queryCtx, sqlStr)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		return emit(SQLResult{SQL: sqlStr, ExecutionMs: ms, Error: err.Error()})
	}
	// RowsAffected error is driver-specific (some return it when the
	// statement type has no meaningful affected count); treat as zero
	// rather than fail the call.
	affected, _ := res.RowsAffected()
	return emit(SQLResult{
		SQL:         sqlStr,
		RowCount:    int(affected),
		ExecutionMs: ms,
	})
}

// lastCapture returns a copy of the last successfully-executed SQLResult,
// or nil if no query has succeeded on this instance.
func (t *executeSQLTool) lastCapture() *SQLResult {
	t.captureMu.Lock()
	defer t.captureMu.Unlock()
	if t.capture == nil {
		return nil
	}
	out := *t.capture
	return &out
}
