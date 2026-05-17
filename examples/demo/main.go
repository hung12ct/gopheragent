package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	_ "github.com/go-sql-driver/mysql"
	"github.com/joho/godotenv"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/builder"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/llm"
	"github.com/hung12ct/gopheragent/pkg/tools/builtin"
)

var myAgentApp *agent.AgentLoop

// agentInfo exposes the loaded YAML config to the frontend so the UI can
// render the agent's name, model, and tool roster without hardcoding.
type AgentInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Model       string   `json:"model"`
	Tools       []string `json:"tools"`
}

var agentInfo AgentInfo

// memoryStore is shared across all sessions and keyed internally by sessionKey.
var memoryStore = builtin.NewInMemoryStore()

// taskStore powers the create_task / update_task / list_tasks tools and is
// shared across sessions (each call carries a sessionKey for isolation).
// Mutations stream task_list events to the frontend so the planner panel
// re-renders with strikethrough on completed entries.
var taskStore = builtin.NewInMemoryTaskStore()

// planModeDefault is read once from PLAN_MODE env var at startup.
// When true, every request runs in plan mode regardless of the UI toggle.
var planModeDefault bool

func loadEnvFiles() {
	for _, p := range []string{".env", "../../.env"} {
		if _, err := os.Stat(p); err == nil {
			if err := godotenv.Overload(p); err != nil {
				log.Printf("Warning: failed loading %s: %v", p, err)
			}
		}
	}
}

func buildProvider() agent.LLMProvider {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("LLM_PROVIDER")))
	log.Printf("LLM_PROVIDER=%q", provider)
	switch provider {
	case "openai":
		p, err := llm.NewOpenAIProvider("", strings.TrimSpace(os.Getenv("OPENAI_MODEL")))
		if err == nil {
			log.Printf("Using OpenAI provider")
			return p
		}
		log.Printf("Warning: OpenAI provider init failed: %v", err)
	case "anthropic", "claude":
		p, err := llm.NewAnthropicProvider("", strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL")))
		if err == nil {
			log.Printf("Using Anthropic provider")
			return p
		}
		log.Printf("Warning: Anthropic provider init failed: %v", err)
	case "gemini":
		p, err := llm.NewGeminiProvider("", strings.TrimSpace(os.Getenv("GEMINI_MODEL")))
		if err == nil {
			log.Printf("Using Gemini provider")
			return p
		}
		log.Printf("Warning: Gemini provider init failed: %v", err)
	}
	log.Printf("Using Mock provider (set LLM_PROVIDER=openai|anthropic|gemini to use real model)")
	return agent.NewMockProvider()
}

func SecurityHook(ctx context.Context, sessionKey string, userInput string) error {
	normalized := strings.ToLower(userInput)
	if strings.Contains(normalized, "hack") || strings.Contains(normalized, "ignore all previous instructions") {
		return fmt.Errorf("prompt injection or policy violation detected")
	}
	return nil
}

// buildSessionManager picks the best session backend.
func buildSessionManager(systemPrompt string) agent.SessionManager {
	if dsn := strings.TrimSpace(os.Getenv("MYSQL_DSN")); dsn != "" {
		db, err := sql.Open("mysql", dsn)
		if err == nil {
			if err := db.Ping(); err == nil {
				sm, err := history.NewMySQLSessionManager(db, systemPrompt)
				if err == nil {
					log.Printf("Session backend: MySQL")
					return sm
				}
			}
			_ = db.Close()
		}
		log.Printf("Warning: MySQL unreachable, falling back to file/memory")
	}

	if dir := strings.TrimSpace(os.Getenv("SESSION_DIR")); dir != "" {
		sm, err := history.NewFileSessionManager(dir, systemPrompt)
		if err == nil {
			log.Printf("Session backend: File (%s)", dir)
			return sm
		}
		log.Printf("Warning: cannot create file session dir %q: %v", dir, err)
	}

	log.Printf("Session backend: InMemory (ephemeral)")
	return history.NewInMemSessionManager(systemPrompt)
}

// pendingApprovals holds HITL confirmations that are waiting on the user.
// Maps approvalID → channel that receives true (approved) or false (denied).
var (
	pendingMu       sync.Mutex
	pendingApprovals = make(map[string]chan bool)
)

// buildConfirmPlan returns a ConfirmPlanFunc that emits an action_required
// event with the proposed plan over SSE and blocks until the user approves
// or denies via POST /api/approve. On approval, the plan's bullet list is
// converted into pending entries in taskStore so the Task Plan panel
// renders the checklist immediately — without this, plan mode would leave
// the panel empty because every tool except exit_plan_mode is gated.
func buildConfirmPlan() agent.ConfirmPlanFunc {
	return func(ctx context.Context, plan string) bool {
		approvalID := fmt.Sprintf("%d", uniqueID())
		ch := make(chan bool, 1)

		pendingMu.Lock()
		pendingApprovals[approvalID] = ch
		pendingMu.Unlock()

		defer func() {
			pendingMu.Lock()
			delete(pendingApprovals, approvalID)
			pendingMu.Unlock()
		}()

		sessionKey, _ := agent.SessionKeyFromContext(ctx)

		if sseStream := getStream(sessionKey); sseStream != nil {
			sseStream <- agent.Event(agent.ActionRequiredEvent{
				Tool: "exit_plan_mode",
				Args: fmt.Sprintf(`{"approval_id":%q,"plan":%q}`, approvalID, plan),
			})
		}

		select {
		case approved := <-ch:
			log.Printf("[PLAN] approval_id=%s approved=%v", approvalID, approved)
			if approved {
				populateTasksFromPlan(ctx, sessionKey, plan)
			}
			return approved
		case <-ctx.Done():
			return false
		}
	}
}

// extractPlanBullets pulls top-level markdown bullets from plan text. A
// bullet is a line whose first non-whitespace characters are "- ", "* ",
// or "N. " (numbered). Sub-bullets (lines indented further than the first
// bullet seen) are folded into the parent's notes so the Task Plan panel
// stays readable. Returns titles + notes pairs in document order.
func extractPlanBullets(plan string) []struct{ Title, Notes string } {
	type entry struct{ Title, Notes string }
	var out []entry
	lines := strings.Split(plan, "\n")

	parentIndent := -1
	for _, raw := range lines {
		trimmed := strings.TrimLeft(raw, " \t")
		if trimmed == "" {
			continue
		}
		indent := len(raw) - len(trimmed)
		title, ok := stripBulletPrefix(trimmed)
		if !ok {
			continue
		}
		if parentIndent < 0 || indent <= parentIndent {
			parentIndent = indent
			out = append(out, entry{Title: title})
			continue
		}
		// Indented bullet — append as a note line on the most recent parent.
		if len(out) > 0 {
			if out[len(out)-1].Notes == "" {
				out[len(out)-1].Notes = "• " + title
			} else {
				out[len(out)-1].Notes += "\n• " + title
			}
		}
	}
	// Convert the local entry slice to the anonymous-struct return shape.
	res := make([]struct{ Title, Notes string }, len(out))
	for i, e := range out {
		res[i] = struct{ Title, Notes string }{Title: e.Title, Notes: e.Notes}
	}
	return res
}

// stripBulletPrefix returns the line content past a "- ", "* ", or "N. "
// prefix, and whether such a prefix was found.
func stripBulletPrefix(line string) (string, bool) {
	for _, p := range []string{"- ", "* ", "• "} {
		if strings.HasPrefix(line, p) {
			return strings.TrimSpace(line[len(p):]), true
		}
	}
	// Numbered list: skip the leading digits, expect ". " next.
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(line) && line[i] == '.' && line[i+1] == ' ' {
		return strings.TrimSpace(line[i+2:]), true
	}
	return "", false
}

// populateTasksFromPlan parses plan bullets into pending task entries and
// pushes a single task_list SSE event so the Task Plan panel renders the
// checklist before the model resumes execution.
func populateTasksFromPlan(ctx context.Context, sessionKey, plan string) {
	bullets := extractPlanBullets(plan)
	if len(bullets) == 0 {
		return
	}
	for _, b := range bullets {
		if _, err := taskStore.Create(ctx, sessionKey, b.Title, b.Notes); err != nil {
			log.Printf("[PLAN] task create failed: %v", err)
		}
	}
	tasks, err := taskStore.List(ctx, sessionKey)
	if err != nil {
		return
	}
	items := make([]agent.TaskListItem, 0, len(tasks))
	for _, t := range tasks {
		items = append(items, agent.TaskListItem{
			ID:     t.ID,
			Title:  t.Title,
			Status: string(t.Status),
			Notes:  t.Notes,
		})
	}
	if sseStream := getStream(sessionKey); sseStream != nil {
		sseStream <- agent.Event(agent.TaskListEvent{Tasks: items})
	}
}

// buildHITL returns a ConfirmFunc that emits an action_required event over
// the session's active SSE stream and blocks until the user approves or
// denies via POST /api/approve.
func buildHITL() agent.ConfirmFunc {
	return func(ctx context.Context, toolName, argsJSON string) bool {
		approvalID := fmt.Sprintf("%d", uniqueID())
		ch := make(chan bool, 1)

		pendingMu.Lock()
		pendingApprovals[approvalID] = ch
		pendingMu.Unlock()

		defer func() {
			pendingMu.Lock()
			delete(pendingApprovals, approvalID)
			pendingMu.Unlock()
		}()

		// Extract session key injected by AgentLoop so we can route to the
		// right SSE connection.
		sessionKey, _ := agent.SessionKeyFromContext(ctx)

		if sseStream := getStream(sessionKey); sseStream != nil {
			sseStream <- agent.Event(agent.ActionRequiredEvent{
				Tool: toolName,
				Args: fmt.Sprintf(`{"approval_id":%q,"args":%s}`, approvalID, argsJSON),
			})
		}

		select {
		case approved := <-ch:
			log.Printf("[HITL] tool=%s approval_id=%s approved=%v", toolName, approvalID, approved)
			return approved
		case <-ctx.Done():
			return false
		}
	}
}

var idCounter uint64
var idMu sync.Mutex

func uniqueID() uint64 {
	idMu.Lock()
	defer idMu.Unlock()
	idCounter++
	return idCounter
}

// activeStreams maps sessionKey → the current SSE write channel (or nil).
var (
	streamsMu     sync.RWMutex
	activeStreams  = make(map[string]chan<- agent.StreamEvent)
)

func registerStream(sessionKey string, ch chan<- agent.StreamEvent) {
	streamsMu.Lock()
	activeStreams[sessionKey] = ch
	streamsMu.Unlock()
}

func unregisterStream(sessionKey string) {
	streamsMu.Lock()
	delete(activeStreams, sessionKey)
	streamsMu.Unlock()
}

func getStream(sessionKey string) chan<- agent.StreamEvent {
	streamsMu.RLock()
	defer streamsMu.RUnlock()
	return activeStreams[sessionKey]
}

func initApp() {
	catalog := builder.NewGlobalCatalog()

	// Web tools
	webSearch, err := builtin.NewWebSearchTool("")
	if err != nil {
		log.Printf("Warning: web_search disabled: %v", err)
	} else {
		catalog.Register(webSearch)
	}
	catalog.Register(builtin.NewReadURLTool())
	catalog.Register(builtin.NewShowMediaTool())

	// Memory tools — one shared store, per-session namespacing done internally
	catalog.Register(builtin.NewMemorySetTool(memoryStore))
	catalog.Register(builtin.NewMemoryGetTool(memoryStore))
	catalog.Register(builtin.NewMemoryDeleteTool(memoryStore))
	catalog.Register(builtin.NewMemoryListTool(memoryStore))

	// Task tools — drive the planner panel in the frontend. Each create/
	// update emits a task_list StreamEvent that the UI renders with
	// strikethrough on completed entries.
	catalog.Register(builtin.NewCreateTaskTool(taskStore))
	catalog.Register(builtin.NewUpdateTaskTool(taskStore))
	catalog.Register(builtin.NewListTasksTool(taskStore))

	// Code interpreter
	catalog.Register(builtin.NewCodeInterpreterTool())

	yamlPath := strings.TrimSpace(os.Getenv("AGENT_YAML_PATH"))
	if yamlPath == "" {
		yamlPath = "../yaml_agents/research_assistant.yaml"
	}

	cfg, err := builder.ParseYAMLConfig(yamlPath)
	if err != nil {
		log.Fatalf("Failed to parse YAML (%s): %v", yamlPath, err)
	}

	agentInfo = AgentInfo{
		Name:        cfg.Agent.Name,
		Description: cfg.Agent.Description,
		Model:       cfg.Agent.Model,
		Tools:       append([]string(nil), cfg.Agent.ToolsRequired...),
	}

	sm := buildSessionManager(cfg.Agent.SystemPrompt)

	if sp, err := llm.NewSummaryProvider("", ""); err == nil {
		switch v := sm.(type) {
		case *history.InMemSessionManager:
			v.SummaryProvider = sp
		case *history.MySQLSessionManager:
			v.SummaryProvider = sp
		case *history.FileSessionManager:
			v.SummaryProvider = sp
		}
		log.Printf("Background summarizer enabled (gpt-4o-mini)")
	}

	loop, _, _, err := builder.BuildFromYAMLWithSession(yamlPath, catalog, buildProvider(), SecurityHook, sm)
	if err != nil {
		log.Fatalf("Failed to build demo agent from YAML (%s): %v", yamlPath, err)
	}

	// Wire HITL: emits action_required over SSE and waits for /api/approve.
	loop.ConfirmHITL = buildHITL()
	loop.ConfirmPlan = buildConfirmPlan()
	planModeDefault = os.Getenv("PLAN_MODE") == "true"

	myAgentApp = loop
	log.Printf("Demo loaded: %s", yamlPath)
}

// ApproveHandler handles POST /api/approve?id=<approval_id>&approved=true|false
func ApproveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	approved := r.URL.Query().Get("approved") == "true"

	pendingMu.Lock()
	ch, ok := pendingApprovals[id]
	pendingMu.Unlock()

	if !ok {
		http.Error(w, "unknown approval_id", http.StatusNotFound)
		return
	}
	ch <- approved
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// InfoHandler handles GET /api/info — returns the loaded YAML agent config
// (name, description, model, tools) so the frontend can render metadata.
func InfoHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(agentInfo)
}

// MemoryHandler handles GET /api/memory?session_id=<key>
func MemoryHandler(w http.ResponseWriter, r *http.Request) {
	sessionKey := r.URL.Query().Get("session_id")
	if sessionKey == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}
	keys, err := memoryStore.List(r.Context(), sessionKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Fetch values for each key.
	entries := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		v, _, _ := memoryStore.Get(r.Context(), sessionKey, k)
		entries = append(entries, map[string]string{"key": k, "value": v})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}

func ChatHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	userInput := r.URL.Query().Get("message")
	if userInput == "" {
		userInput = "What can you help me with?"
	}
	sessionKey := r.URL.Query().Get("session_id")
	if sessionKey == "" {
		sessionKey = r.RemoteAddr
	}
	myAgentApp.SetPlanMode(sessionKey, planModeDefault || r.URL.Query().Get("plan_mode") == "true")
	streamChan := make(chan agent.StreamEvent, 32)
	registerStream(sessionKey, streamChan)
	defer unregisterStream(sessionKey)

	// Bridge the iter-based Run API to streamChan so the HITL gate
	// (registered via registerStream above) can multiplex its
	// action_required events onto the same channel the handler reads from.
	go func() {
		defer close(streamChan)
		for ev := range myAgentApp.RunText(r.Context(), sessionKey, userInput) {
			select {
			case streamChan <- ev:
			case <-r.Context().Done():
				return
			}
		}
	}()

	sawTerminal := false
	defer func() {
		if sawTerminal {
			return
		}
		// Synthesize a terminal frame so the frontend exits its streaming
		// state when the upstream channel closes without one (e.g. consumer
		// disconnect or any future code path that bypasses the terminal
		// emit). Prefer an error frame when our request ctx is cancelled
		// so the UI can show "stopped" rather than "completed".
		ev := agent.Event(agent.DoneEvent{})
		if err := r.Context().Err(); err != nil {
			ev = agent.Event(agent.ErrorEvent{Err: err, Message: "cancelled: " + err.Error()})
		}
		b, _ := json.Marshal(ev)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}()

	for {
		select {
		case event, ok := <-streamChan:
			if !ok {
				return
			}
			eventBytes, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", string(eventBytes))
			flusher.Flush()
			// Only top-level frames (Source=="") terminate the SSE stream;
			// forwarded sub-agent done/error events are observational.
			if event.Source == "" && (event.Type == agent.EventTypeDone || event.Type == agent.EventTypeError) {
				sawTerminal = true
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func main() {
	loadEnvFiles()
	initApp()
	http.Handle("/", http.FileServer(http.Dir("./frontend")))
	http.HandleFunc("/api/chat", ChatHandler)
	http.HandleFunc("/api/approve", ApproveHandler)
	http.HandleFunc("/api/memory", MemoryHandler)
	http.HandleFunc("/api/info", InfoHandler)
	fmt.Println("Server started at http://localhost:8888")
	log.Fatal(http.ListenAndServe(":8888", nil))
}
