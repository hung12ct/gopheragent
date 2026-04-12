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

// SQLQueryEvent carries metadata about a SQL query the sub-agent is about to execute.
type SQLQueryEvent struct {
	SessionKey string
	Query      string
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

	// 3. Build a structured system prompt using Divide-and-Conquer + Query Plan techniques.
	systemInstruction := fmt.Sprintf(`You are an elite SQL Database Agent. Your task is to translate natural language queries into read-only SQL using a "Divide-and-Conquer" approach, then execute them with the execute_sql tool.

**Mandatory SQL Rules (follow unconditionally):**
1. SELECT only the columns needed — no SELECT *.
2. Complete all JOINs before applying MAX() or MIN().
3. Use INNER JOIN over nested subqueries when possible.
4. Filter NULLs with WHERE <col> IS NOT NULL or via JOIN.
5. Use LIMIT to cap results at 100 rows maximum.
6. Use DISTINCT only when unique values are explicitly needed.
7. When unsure which column contains a value, use: LOWER(col) LIKE '%%value%%' OR LOWER(col2) LIKE '%%value%%'.
8. In GROUP BY queries, every SELECT column must be in GROUP BY or an aggregate (COUNT, SUM, MAX, MIN, AVG).
9. Use ORDER BY ... NULLS LAST when ordering potentially nullable columns.
10. Use ONLY column names from the schema below — never hallucinate column names.
11. Read-Only: NEVER write UPDATE, DELETE, DROP, INSERT, CREATE, ALTER, TRUNCATE. The tool will reject them.

**Schema & Context:**
%s

**Reasoning Strategy — Divide and Conquer:**
Before calling execute_sql, reason through the query in this format:

1. **Main Question**: Restate the question. Write Pseudo SQL with <placeholder> for unknowns.
2. **Sub-questions**: For each placeholder, write its Analysis + Pseudo SQL.
   - Recurse deeper if a sub-question has its own sub-questions.
3. **Assemble**: Replace placeholders bottom-up to form the full SQL.
4. **Simplify**: Optimize (remove redundant subqueries, convert to JOINs).
5. **Execute**: Call execute_sql with the final query. Return the raw JSON result untouched.`, t.schemaContext)
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

	if t.onSQL != nil {
		t.onSQL(ctx, SQLQueryEvent{SessionKey: t.sessionKey, Query: sqlStr})
	}

	// 1. Anti-DML/DDL Safety Validation via Regex
	if dmlMatcher.MatchString(sqlStr) {
		return "Invalid SQL: Contains disallowed DML/DDL operations. Database access is strictly read-only.", nil
	}

	// 2. Query execution
	rows, err := t.db.QueryContext(ctx, sqlStr)
	if err != nil {
		// Crucial Error Feedback Loop! We return the raw error message to the LLM.
		return fmt.Sprintf("Query error: %v\nPlease analyze the error and try correcting the SQL statement.", err), nil
	}
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
