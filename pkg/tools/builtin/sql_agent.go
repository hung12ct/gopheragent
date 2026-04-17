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
type SQLQueryEvent struct {
	SessionKey string
	Query      string
	Error      string // empty on success
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

// SQLAgentTool converts natural-language questions to SQL by delegating to an
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
type SQLAgentTool struct {
	db              *sql.DB
	schemaRaw       string
	schema          *Schema
	examples        []SQLExample
	businessRules   []string
	maxRows         int
	queryTimeout    time.Duration
	selfConsistency int
	sessionManager  agent.SessionManager
	provider        agent.LLMProvider
	onSQL           func(context.Context, SQLQueryEvent)
}

// NewSQLAgentTool initializes a tool capable of querying databases. The
// schemaContext string is used verbatim when no structured Schema is
// registered via WithSchema — pass an empty string and call WithSchema to
// use the structured path exclusively.
func NewSQLAgentTool(db *sql.DB, schemaContext string, sm agent.SessionManager, provider agent.LLMProvider) *SQLAgentTool {
	return &SQLAgentTool{
		db:             db,
		schemaRaw:      schemaContext,
		sessionManager: sm,
		provider:       provider,
	}
}

// OnSQL registers a callback invoked every time the sub-agent executes a SQL
// query. Use it for logging, auditing, or streaming SQL to the parent
// application. Called on both success and failure — ev.Error is non-empty on
// failure.
func (t *SQLAgentTool) OnSQL(fn func(context.Context, SQLQueryEvent)) *SQLAgentTool {
	t.onSQL = fn
	return t
}

// WithSchema registers a structured schema. When set, its markdown rendering
// replaces the raw schemaContext passed to NewSQLAgentTool in the system
// prompt. Structured schemas produce tighter, more consistent grounding and
// are required for downstream features like schema linking.
func (t *SQLAgentTool) WithSchema(s Schema) *SQLAgentTool {
	t.schema = &s
	return t
}

// WithExamples registers few-shot Question→SQL demonstrations that are
// injected into the sub-agent's system prompt. Even 2–3 examples anchored
// to the schema materially reduce hallucination on domain-specific queries.
func (t *SQLAgentTool) WithExamples(examples ...SQLExample) *SQLAgentTool {
	t.examples = append(t.examples, examples...)
	return t
}

// WithBusinessRules registers free-form domain rules (glossary, naming
// conventions, metric definitions) that are injected into the system prompt.
// Use short, imperative sentences — "revenue" means NET revenue, exclude
// refunds; "active" users have login_at in the last 30 days.
func (t *SQLAgentTool) WithBusinessRules(rules ...string) *SQLAgentTool {
	t.businessRules = append(t.businessRules, rules...)
	return t
}

// WithMaxRows appends "LIMIT n" to any SELECT or WITH statement that does
// not already contain a LIMIT clause. n <= 0 disables the behaviour
// (default). Set this to keep accidental "SELECT * FROM large_table" calls
// from returning millions of rows.
func (t *SQLAgentTool) WithMaxRows(n int) *SQLAgentTool {
	t.maxRows = n
	return t
}

// WithQueryTimeout caps the wall-clock time of each underlying QueryContext
// call. d <= 0 disables the timeout (default). Separate from the agent's
// overall request context, which may be much longer.
func (t *SQLAgentTool) WithQueryTimeout(d time.Duration) *SQLAgentTool {
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
func (t *SQLAgentTool) WithSelfConsistency(n int) *SQLAgentTool {
	t.selfConsistency = n
	return t
}

// Name implements tools.Tool.
func (t *SQLAgentTool) Name() string {
	return "call_sql_agent"
}

// Description implements tools.Tool.
func (t *SQLAgentTool) Description() string {
	return "Translate natural language business questions into SQL and query the Database directly. It automatically determines tables, runs queries, and returns structured data."
}

// ParametersSchema implements tools.Tool.
func (t *SQLAgentTool) ParametersSchema() tools.ToolSchema {
	return tools.ToolSchema{
		Type: "object",
		Properties: map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "The natural language query or task for the SQL database Sub-Agent to perform.",
			},
		},
		Required: []string{"query"},
	}
}

// RequiresConfirmation returns true so that every SQL-agent invocation goes
// through the HITL hook when one is wired up. Override via tool middleware
// if you want unsupervised execution in trusted environments.
func (t *SQLAgentTool) RequiresConfirmation() bool {
	return true
}

// Execute runs the sub-agent loop. The sub-agent sees the schema, examples,
// and business rules but never the database handle directly — it must call
// execute_sql to actually touch the DB.
//
// When WithSelfConsistency(n) with n > 1 is configured, n independent sub-
// agents run in parallel and their execution results are clustered by hash;
// the answer from the winning cluster is returned.
func (t *SQLAgentTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Query string `json:"query"`
	}
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
func (t *SQLAgentTool) runOnce(ctx context.Context, query string, idx int) sqlCandidate {
	subSessionKey := fmt.Sprintf("sub_agent_sql_%d_%d", time.Now().UnixNano(), idx)
	defer func() {
		if err := t.sessionManager.DeleteSession(context.Background(), subSessionKey); err != nil && t.onSQL != nil {
			t.onSQL(ctx, SQLQueryEvent{
				SessionKey: subSessionKey,
				Error:      fmt.Sprintf("session cleanup failed: %v", err),
			})
		}
	}()

	exec := &executeSQLTool{
		db:           t.db,
		onSQL:        t.onSQL,
		sessionKey:   subSessionKey,
		maxRows:      t.maxRows,
		queryTimeout: t.queryTimeout,
	}
	sqlTools := tools.NewRegistry()
	sqlTools.Register(exec)

	subAgent := agent.NewAgentLoop(t.sessionManager, sqlTools, t.provider)

	t.sessionManager.SetHistory(ctx, subSessionKey, []history.Message{
		{Role: "system", Content: t.buildSystemPrompt()},
	})

	streamChan := make(chan agent.StreamEvent, 100)
	go subAgent.RunIterationStream(ctx, subSessionKey, query, streamChan)

	var finalResult strings.Builder
	for event := range streamChan {
		if event.Type == "content" {
			finalResult.WriteString(event.Content)
		}
	}

	return sqlCandidate{
		index:      idx,
		finalResp:  finalResult.String(),
		lastResult: exec.lastCapture(),
	}
}

// runCandidates fans out n parallel sub-agent runs, clusters their execution
// results, and returns the answer from the winning cluster.
func (t *SQLAgentTool) runCandidates(ctx context.Context, query string, n int) (string, error) {
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
func (t *SQLAgentTool) buildSystemPrompt() string {
	schemaBlock := t.schemaRaw
	if t.schema != nil {
		schemaBlock = t.schema.String()
	}

	var b strings.Builder
	b.WriteString(`You are a SQL database expert. Translate the user's natural-language question into a valid, read-only SQL query, execute it with execute_sql, and return the raw JSON result untouched.

**Schema (use ONLY these tables and columns — never invent names):**

`)
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
12. Read-only: NEVER write UPDATE, DELETE, DROP, INSERT, CREATE, ALTER, TRUNCATE, MERGE, GRANT, or REVOKE — the tool will reject them.
13. Submit one statement at a time. Multiple statements separated by ';' are rejected.

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
	db           *sql.DB
	sessionKey   string
	onSQL        func(context.Context, SQLQueryEvent)
	maxRows      int
	queryTimeout time.Duration

	// capture, when non-nil, records the last successful SQLResult observed
	// in this instance. Used by self-consistency clustering. Protected by
	// captureMu because the LLM may drive parallel tool calls within a
	// single run.
	captureMu sync.Mutex
	capture   *SQLResult
}

func (t *executeSQLTool) Name() string { return "execute_sql" }

func (t *executeSQLTool) Description() string {
	return "Execute a single read-only SQL statement against the database and return a structured JSON envelope {sql, columns, rows, row_count, execution_ms, truncated, error}. Multi-statement input and DDL/DML are rejected."
}

func (t *executeSQLTool) ParametersSchema() tools.ToolSchema {
	return tools.ToolSchema{
		Type: "object",
		Properties: map[string]any{
			"sql_query": map[string]any{
				"type":        "string",
				"description": "The exact SQL query string to run. Single statement only, read-only.",
			},
		},
		Required: []string{"sql_query"},
	}
}

func (t *executeSQLTool) RequiresConfirmation() bool { return false }

func (t *executeSQLTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		SQLQuery string `json:"sql_query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("tools: invalid arguments: %w", err)
	}
	sqlStr := strings.TrimSpace(args.SQLQuery)

	notify := func(query, errMsg string) {
		if t.onSQL != nil {
			t.onSQL(ctx, SQLQueryEvent{SessionKey: t.sessionKey, Query: query, Error: errMsg})
		}
	}
	marshal := func(res SQLResult) (string, error) {
		b, err := json.Marshal(res)
		if err != nil {
			return "", fmt.Errorf("tools: marshal result: %w", err)
		}
		return string(b), nil
	}

	// 1. Validation — fails before the DB even sees the query.
	if err := ValidateReadOnly(sqlStr); err != nil {
		notify(sqlStr, err.Error())
		return marshal(SQLResult{SQL: sqlStr, Error: err.Error()})
	}

	// 2. Apply LIMIT if configured and missing.
	effectiveSQL := EnsureLimit(sqlStr, t.maxRows)

	// 3. Per-query timeout layered on top of the request context.
	queryCtx := ctx
	if t.queryTimeout > 0 {
		var cancel context.CancelFunc
		queryCtx, cancel = context.WithTimeout(ctx, t.queryTimeout)
		defer cancel()
	}

	start := time.Now()
	rows, err := t.db.QueryContext(queryCtx, effectiveSQL)
	if err != nil {
		notify(effectiveSQL, err.Error())
		return marshal(SQLResult{
			SQL:         effectiveSQL,
			ExecutionMs: time.Since(start).Milliseconds(),
			Error:       err.Error(),
		})
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		notify(effectiveSQL, err.Error())
		return marshal(SQLResult{
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
			notify(effectiveSQL, err.Error())
			return marshal(SQLResult{
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
		notify(effectiveSQL, err.Error())
		return marshal(SQLResult{
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
	notify(effectiveSQL, "")

	t.captureMu.Lock()
	copied := finalRes
	t.capture = &copied
	t.captureMu.Unlock()

	return marshal(finalRes)
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
