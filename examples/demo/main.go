package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	_ "github.com/go-sql-driver/mysql"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

var myAgentApp *agent.AgentLoop

func SecurityHook(ctx context.Context, sessionKey string, userInput string) error {
	normalized := strings.ToLower(userInput)
	if strings.Contains(normalized, "hack") || strings.Contains(normalized, "ignore all previous instructions") {
		return fmt.Errorf("prompt injection or policy violation detected")
	}
	return nil
}

func initApp() {
	// db, err := sql.Open("mysql", "user:password@tcp(127.0.0.1:3306)/dbname")
	var db *sql.DB // stub

	var sessionManager agent.SessionManager
	mysqlManager, err := history.NewMySQLSessionManager(db)
	if err != nil {
		log.Printf("Warning: Failed to init DB sessions, falling back to in-memory: %v", err)
		sessionManager = history.NewInMemSessionManager("You are an internal AI Agent assisting employees with data queries.")
	} else {
		sessionManager = mysqlManager
	}

	registry := tools.NewRegistry()
	registry.Register(NewFindTopCreativeTool(db))
	registry.Register(NewDeleteSystemTool())

	myAgentApp = agent.NewAgentLoop(sessionManager, registry, agent.NewMockProvider(), SecurityHook)
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
		userInput = "Find top creatives on Facebook"
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
	initApp()
	http.Handle("/", http.FileServer(http.Dir("./frontend")))
	http.HandleFunc("/api/chat", ChatHandler)
	fmt.Println("Server started at http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
