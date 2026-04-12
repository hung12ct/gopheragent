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

	_ "github.com/go-sql-driver/mysql"
	"github.com/joho/godotenv"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/builder"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/llm"
	"github.com/hung12ct/gopheragent/pkg/tools/builtin"
)

var myAgentApp *agent.AgentLoop

func loadEnvFiles() {
	// Load local .env (if exists), then repo-root .env.
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

// buildSessionManager picks the best session backend:
//   - MYSQL_DSN set + reachable → MySQLSessionManager (survives restarts)
//   - SESSION_DIR set           → FileSessionManager (survives restarts, no DB)
//   - fallback                  → InMemSessionManager (fast, ephemeral)
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

func initApp() {
	catalog := builder.NewGlobalCatalog()
	webSearch, err := builtin.NewWebSearchTool("")
	if err != nil {
		log.Printf("Warning: web_search disabled: %v", err)
	} else {
		catalog.Register(webSearch)
	}
	catalog.Register(builtin.NewReadURLTool())
	catalog.Register(builtin.NewShowMediaTool())

	yamlPath := strings.TrimSpace(os.Getenv("AGENT_YAML_PATH"))
	if yamlPath == "" {
		yamlPath = "../yaml_agents/web_research_chat.yaml"
	}

	// Parse YAML first to get system prompt for session manager
	cfg, err := builder.ParseYAMLConfig(yamlPath)
	if err != nil {
		log.Fatalf("Failed to parse YAML (%s): %v", yamlPath, err)
	}

	sm := buildSessionManager(cfg.Agent.SystemPrompt)

	// Wire background behavioral summarizer if OpenAI key available
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

	myAgentApp = loop
	log.Printf("Demo YAML loaded: %s", yamlPath)
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
		userInput = "What is the latest AI news today?"
	}
	sessionKey := r.URL.Query().Get("session_id")
	if sessionKey == "" {
		sessionKey = r.RemoteAddr
	}

	streamChan := make(chan agent.StreamEvent)
	go myAgentApp.RunIterationStream(r.Context(), sessionKey, userInput, streamChan)

	for {
		select {
		case event, ok := <-streamChan:
			if !ok {
				return
			}
			eventBytes, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", string(eventBytes))
			flusher.Flush()
			if event.Type == "done" {
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
	fmt.Println("Server started at http://localhost:8888")
	log.Fatal(http.ListenAndServe(":8888", nil))
}
