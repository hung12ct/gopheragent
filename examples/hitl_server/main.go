// Package main is a reference HITL (Human-In-The-Loop) server built on
// GopherAgent. It bridges the synchronous ConfirmHITL callback with an async
// HTTP approval workflow so a web client can approve / deny risky tool calls
// in real time.
//
// The flow:
//
//  1. Client POSTs a prompt to /chat. The server starts an AgentLoop and
//     streams StreamEvents back as SSE.
//  2. When the agent wants to call a tool that declares
//     RequiresConfirmation() == true, the loop invokes the ConfirmHITL hook.
//     The server enqueues a pending approval (keyed by a generated ID),
//     emits an "approval_required" SSE event containing that ID, and blocks
//     the tool call until a decision arrives.
//  3. The client inspects the pending call and POSTs a decision to
//     /approve with {"id": "...", "approved": true|false}. The server
//     delivers the boolean to the waiting hook and the agent resumes.
//
// This is a single-process reference — the pending map lives in memory, so
// restarting the server cancels in-flight approvals. For production, persist
// pending approvals to a store that survives restarts and fan them out to
// every replica (Redis pub/sub, a DB row with a TTL, etc.).
//
// Run:
//
//	OPENAI_API_KEY=sk-... go run ./examples/hitl_server
//
// Demo with curl (two terminals):
//
//	# terminal 1 — start the chat
//	curl -N -X POST http://localhost:8080/chat \
//	  -H "Content-Type: application/json" \
//	  -d '{"session_id":"demo","message":"Delete /tmp/foo using the shell tool."}'
//
//	# watch for: event: approval_required
//	#            data: {"id":"appr-...","tool":"shell","args":{...}}
//
//	# terminal 2 — approve the call
//	curl -X POST http://localhost:8080/approve \
//	  -H "Content-Type: application/json" \
//	  -d '{"id":"appr-...","approved":true}'
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/llm"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// ── Approval broker ─────────────────────────────────────────────────────────

// pendingApproval is a tool call waiting for a human decision.
type pendingApproval struct {
	Tool     string
	Args     string
	decision chan bool
}

// approvalBroker is the bridge between the sync ConfirmFunc signature and the
// async /approve HTTP endpoint. Callers of Await block until Decide is called
// for the same ID (or the context is cancelled).
type approvalBroker struct {
	mu      sync.Mutex
	pending map[string]*pendingApproval
}

func newApprovalBroker() *approvalBroker {
	return &approvalBroker{pending: make(map[string]*pendingApproval)}
}

// Await registers a new pending approval and blocks until Decide delivers a
// verdict or ctx is cancelled (in which case the call is auto-denied).
// Returns the generated ID alongside the decision channel so the caller can
// surface the ID to the human before blocking.
func (b *approvalBroker) Await(ctx context.Context, tool, args string, onEnqueue func(id string)) bool {
	id := fmt.Sprintf("appr-%d", time.Now().UnixNano())
	p := &pendingApproval{Tool: tool, Args: args, decision: make(chan bool, 1)}

	b.mu.Lock()
	b.pending[id] = p
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
	}()

	if onEnqueue != nil {
		onEnqueue(id)
	}

	select {
	case verdict := <-p.decision:
		return verdict
	case <-ctx.Done():
		return false
	}
}

// Decide delivers a verdict for a pending approval. Returns an error if the
// ID is unknown (typically because it timed out or the agent was cancelled).
func (b *approvalBroker) Decide(id string, approved bool) error {
	b.mu.Lock()
	p, ok := b.pending[id]
	b.mu.Unlock()
	if !ok {
		return errors.New("unknown or expired approval id")
	}
	select {
	case p.decision <- approved:
		return nil
	default:
		return errors.New("approval already decided")
	}
}

// ── Demo tool ───────────────────────────────────────────────────────────────

// shellTool is a fake risky tool that always requires confirmation. It does
// not actually run a shell — the point is to exercise the HITL flow.
type shellTool struct{}

func (shellTool) Name() string        { return "shell" }
func (shellTool) Description() string { return "Execute a shell command (simulation)." }
func (shellTool) ParametersSchema() tools.ToolSchema {
	return tools.ToolSchema{
		Type: "object",
		Properties: map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "Shell command to run.",
			},
		},
		Required: []string{"command"},
	}
}
func (shellTool) RequiresConfirmation() bool { return true }
func (shellTool) Execute(_ context.Context, argsJSON string) (string, error) {
	return fmt.Sprintf("(simulated) executed: %s", argsJSON), nil
}

// ── Server wiring ───────────────────────────────────────────────────────────

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
		"You are a helpful assistant. Use the shell tool whenever the user asks you to run a command.",
	)
	registry := tools.NewRegistry()
	registry.Register(shellTool{})

	broker := newApprovalBroker()

	// The SSE chat handler needs a per-request way to push "approval_required"
	// events into the event stream. We solve this by keeping a per-session
	// event forwarder registered at request time — see chatHandler below.
	loop := agent.NewAgentLoop(sessions, registry, provider)
	loop.EmitThoughts = true

	// Per-request state: the current event channel for a given sessionKey.
	// ConfirmHITL uses this to push "approval_required" events back to the
	// client that initiated the chat.
	var channelMu sync.Mutex
	channels := map[string]chan<- agent.StreamEvent{}

	loop.ConfirmHITL = func(ctx context.Context, toolName, argsJSON string) bool {
		return broker.Await(ctx, toolName, argsJSON, func(id string) {
			payload, _ := json.Marshal(map[string]string{
				"id":   id,
				"tool": toolName,
				"args": argsJSON,
			})
			// Look up the current SSE channel for this session.
			// The sessionKey isn't available in ConfirmHITL's signature, so
			// this reference impl broadcasts to every active channel. For a
			// multi-tenant server, either (a) extend the ConfirmFunc to carry
			// sessionKey, or (b) key pending approvals by sessionKey upfront.
			channelMu.Lock()
			defer channelMu.Unlock()
			for _, ch := range channels {
				select {
				case ch <- agent.StreamEvent{Type: "approval_required", Content: string(payload)}:
				default:
				}
			}
		})
	}

	http.HandleFunc("/chat", chatHandler(loop, &channelMu, channels))
	http.HandleFunc("/approve", approveHandler(broker))
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	addr := ":8080"
	log.Printf("gopheragent HITL server on %s (POST /chat, POST /approve)", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func chatHandler(loop *agent.AgentLoop, mu *sync.Mutex, channels map[string]chan<- agent.StreamEvent) http.HandlerFunc {
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
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		events := make(chan agent.StreamEvent, 64)

		mu.Lock()
		channels[req.SessionID] = events
		mu.Unlock()
		defer func() {
			mu.Lock()
			delete(channels, req.SessionID)
			mu.Unlock()
		}()

		go loop.RunIterationStream(r.Context(), req.SessionID, req.Message, events)

		for ev := range events {
			payload, err := json.Marshal(ev)
			if err != nil {
				log.Printf("sse: marshal: %v", err)
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func approveHandler(broker *approvalBroker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID       string `json:"id"`
			Approved bool   `json:"approved"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.ID == "" {
			http.Error(w, "id is required", http.StatusBadRequest)
			return
		}
		if err := broker.Decide(req.ID, req.Approved); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}
