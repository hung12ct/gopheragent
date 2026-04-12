package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

var dmlMatcher = regexp.MustCompile(`(?i)(update\s|delete\s|drop\s|insert\s|create\s|alter\s|truncate\s|merge\s|grant\s|revoke\s)`)

// SQLQueryEvent carries metadata about a SQL query executed by the sub-agent.
// Error is non-empty when the query failed (DB error or DML rejection).
type SQLQueryEvent struct {
	SessionKey string
	Query      string
	Error      string // empty on success
}

// SQLAgentTool spins up an isolated Sub-Agent dedicated solely to converting Natural Language to SQL and querying the database safely.
type SQLAgentTool struct {
	db             *sql.DB
	schemaContext  string
	sessionManager agent.SessionManager
	provider       agent.LLMProvider
	onSQL          func(context.Context, SQLQueryEvent)
}

// NewSQLAgentTool initializes a tool capable of querying databases.
func NewSQLAgentTool(db *sql.DB, schemaContext string, sm agent.SessionManager, provider agent.LLMProvider) *SQLAgentTool {
	return &SQLAgentTool{
		db:             db,
		schemaContext:  schemaContext,
		sessionManager: sm,
		provider:       provider,
	}
}

// OnSQL registers a callback invoked every time the sub-agent executes a SQL query.
// Use it for logging, auditing, or streaming SQL to the parent application.
//
//	tool.OnSQL(func(ctx context.Context, ev builtin.SQLQueryEvent) {
//	    log.Printf("[sql-agent] session=%s sql=%s", ev.SessionKey, ev.Query)
//	})
func (t *SQLAgentTool) OnSQL(fn func(context.Context, SQLQueryEvent)) *SQLAgentTool {
	t.onSQL = fn
	return t
}

func (t *SQLAgentTool) Name() string {
	return "call_sql_agent"
}

func (t *SQLAgentTool) Description() string {
	return "Translate natural language business questions into SQL and query the Database directly. It automatically determines tables, runs queries, and returns structured data."
}

func (t *SQLAgentTool) ParametersSchema() tools.ToolSchema {
	return tools.ToolSchema{
		Type: "object",
		Properties: map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "The natural language query or task for the SQL database Sub-Agent to perform.",
			},
		},
		Required: []string{"query"},
	}
}

func (t *SQLAgentTool) RequiresConfirmation() bool {
	// Enforce Human-In-The-Loop on SQL agent execution if desired by the dev framework
	return true
}

func (t *SQLAgentTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	subSessionKey := fmt.Sprintf("sub_agent_sql_%d", time.Now().UnixNano())

	// 1. Create a sub-registry with exactly one tool: execute_sql
	sqlTools := tools.NewRegistry()
	sqlTools.Register(&executeSQLTool{db: t.db, onSQL: t.onSQL, sessionKey: subSessionKey})

	// 2. Instantiate the sub-agent
	subAgent := agent.NewAgentLoop(t.sessionManager, sqlTools, t.provider)

	// 3. Build system prompt: concise preamble + injected schema context with its own mandatory rules.
	systemInstruction := fmt.Sprintf(`You are a SQL database expert. Translate the user's natural-language question into a valid, read-only SQL query, execute it with execute_sql, and return the raw JSON result untouched.

**Schema (use ONLY these tables and columns — never invent names):**

%s

**SQL Rules (follow unconditionally):**

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
11. For fuzzy text matching: LOWER(col) LIKE '%%value%%'.
12. Read-only: NEVER write UPDATE, DELETE, DROP, INSERT, CREATE, ALTER, TRUNCATE — the tool will reject them.

**Reasoning strategy (use for complex queries):**

1. Restate the question. Write Pseudo SQL with <placeholder> for unknowns.
2. For each placeholder, write its sub-question and Pseudo SQL. Recurse if needed.
3. Replace placeholders bottom-up to assemble the final SQL.
4. Simplify: remove redundant subqueries, convert to JOINs where possible.
5. Execute with execute_sql.

**Self-correction:** If execute_sql returns an error, analyze the error message carefully. Fix only the SQL syntax or column references — do not change table names or literal values. Retry with the corrected query.`, t.schemaContext)
	t.sessionManager.SetHistory(ctx, subSessionKey, []history.Message{
		{Role: "system", Content: systemInstruction},
	})

	// 4. Run Sub-Agent loop (blocking, collecting all stdout basically)
	streamChan := make(chan agent.StreamEvent, 100)
	go subAgent.RunIterationStream(ctx, subSessionKey, args.Query, streamChan)

	var finalResult string
	for event := range streamChan {
		if event.Type == "content" {
			finalResult += event.Content
		}
	}

	if finalResult == "" {
		return "Process finished but no verbal response was given. Check logs.", nil
	}
	return finalResult, nil
}

// ---------------------------------------------------------
// executeSQLTool is an internal tool available only to the SQLAgent sub-agent.
// It includes strict anti-DDL/DML checks to prevent injection.
// ---------------------------------------------------------
type executeSQLTool struct {
	db         *sql.DB
	sessionKey string
	onSQL      func(context.Context, SQLQueryEvent)
}

func (t *executeSQLTool) Name() string { return "execute_sql" }

func (t *executeSQLTool) Description() string {
	return "Executes a read-only SQL query against the database and returns the result in JSON format. Expect validation errors if the SQL is invalid or uses DML."
}

func (t *executeSQLTool) ParametersSchema() tools.ToolSchema {
	return tools.ToolSchema{
		Type: "object",
		Properties: map[string]interface{}{
			"sql_query": map[string]interface{}{
				"type":        "string",
				"description": "The exact SQL query string to run.",
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
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	sqlStr := args.SQLQuery

	notify := func(errMsg string) {
		if t.onSQL != nil {
			t.onSQL(ctx, SQLQueryEvent{SessionKey: t.sessionKey, Query: sqlStr, Error: errMsg})
		}
	}

	// 1. Anti-DML/DDL Safety Validation via Regex
	if dmlMatcher.MatchString(sqlStr) {
		msg := "Invalid SQL: Contains disallowed DML/DDL operations. Database access is strictly read-only."
		notify(msg)
		return msg, nil
	}

	// 2. Query execution
	rows, err := t.db.QueryContext(ctx, sqlStr)
	if err != nil {
		notify(err.Error())
		return fmt.Sprintf("Query error: %v\nPlease analyze the error and try correcting the SQL statement.", err), nil
	}
	notify("") // success
	defer rows.Close()

	// 3. Dynamic columns parsing
	cols, err := rows.Columns()
	if err != nil {
		return fmt.Sprintf("Query error (failed to retrieve columns): %v", err), nil
	}

	var results []map[string]interface{}
	for rows.Next() {
		columns := make([]interface{}, len(cols))
		columnPointers := make([]interface{}, len(cols))
		for i := range columns {
			columnPointers[i] = &columns[i]
		}
		if err := rows.Scan(columnPointers...); err != nil {
			return fmt.Sprintf("Result extraction error: %v", err), nil
		}

		rowData := make(map[string]interface{})
		for i, colName := range cols {
			val := columns[i]
			if b, ok := val.([]byte); ok {
				rowData[colName] = string(b)
			} else {
				rowData[colName] = val
			}
		}
		results = append(results, rowData)
	}
	if err := rows.Err(); err != nil {
		return fmt.Sprintf("Row iteration error: %v", err), nil
	}

	if len(results) == 0 {
		return "Valid SQL. Query executed successfully (no results).", nil
	}

	resJSON, err := json.Marshal(results)
	if err != nil {
		return fmt.Sprintf("Result serialization error: %v", err), nil
	}

	return string(resJSON), nil
}
