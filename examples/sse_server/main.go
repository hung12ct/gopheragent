// Package main is a minimal SSE (Server-Sent Events) chat server built on
// GopherAgent. It exposes POST /chat which streams every StreamEvent the
// AgentLoop produces back to the client as Server-Sent Events.
//
// Run:
//
//	OPENAI_API_KEY=sk-... go run ./examples/sse_server
//
// Stream from curl:
//
//	curl -N -X POST http://localhost:8080/chat \
//	  -H "Content-Type: application/json" \
//	  -d '{"session_id":"demo","message":"Hello, who are you?"}'
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/llm"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

func main() {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		log.Fatal("OPENAI_API_KEY is required")
	}

	provider, err := llm.NewOpenAIProvider(apiKey, "gpt-4o-mini")
	if err != nil {
		log.Fatalf("create provider: %v", err)
	}

	sessions := history.NewInMemSessionManager(
		"You are a helpful, concise assistant. Answer in one short paragraph unless asked otherwise.",
	)
	registry := tools.NewRegistry()

	loop := agent.NewAgentLoop(sessions, registry, provider)
	loop.EmitThoughts = true
	loop.Retry = agent.DefaultRetryConfig()

	http.HandleFunc("/chat", chatHandler(loop))
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	addr := ":8080"
	log.Printf("gopheragent SSE server listening on %s (POST /chat)", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// chatHandler returns an http.Handler that streams AgentLoop events as SSE.
//
// The JSON request body is {"session_id": "...", "message": "..."}. Each
// StreamEvent is emitted as a separate SSE frame with its Type as the event
// name, so browsers and EventSource clients can route them easily:
//
//	event: content
//	data: {"type":"content","content":"Hello"}
//
//	event: done
//	data: {"type":"done","content":""}
func chatHandler(loop *agent.AgentLoop) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			SessionID string `json:"session_id"`
			Message   string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.SessionID == "" || req.Message == "" {
			http.Error(w, "session_id and message are required", http.StatusBadRequest)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported by this ResponseWriter", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx et al.)

		events := make(chan agent.StreamEvent, 64)
		go loop.RunIterationStream(r.Context(), req.SessionID, req.Message, events)

		for ev := range events {
			payload, err := json.Marshal(ev)
			if err != nil {
				// Shouldn't happen for a plain StreamEvent, but guard anyway.
				log.Printf("sse: marshal event: %v", err)
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload); err != nil {
				// Client disconnected mid-write; let the loop's context handle the rest.
				return
			}
			flusher.Flush()
		}
	}
}
