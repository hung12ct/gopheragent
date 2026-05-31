// Creative Studio — a multi-modal AI creative director that generates images
// and short videos from natural-language descriptions.
//
// Required env vars:
//
//	OPENAI_API_KEY  — image generation (DALL-E 3)
//	GEMINI_API_KEY  — video generation (Veo) + optional LLM
//	LLM_PROVIDER    — "openai" (default), "anthropic", or "gemini"
//
// Optional env vars:
//
//	VEO_MODEL       — video model ID. Defaults to "veo-2.0-generate-001"
//	                  (silent video). Set to a Veo 3 model ID to get native
//	                  audio; availability and pricing differ per tier.
//	MEDIA_DIR       — where generated images/videos are saved (default "generated").
//
// Run:
//
//	cd examples/creative_studio && go run .
//	open http://localhost:8890
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/joho/godotenv"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/llm"
	"github.com/hung12ct/gopheragent/pkg/tools"
	"github.com/hung12ct/gopheragent/pkg/tools/builtin"
)

const (
	listenAddr = ":8890"
)

// mediaDir is where generated images and videos are saved.
// Defaults to ./generated/ next to the running binary so files are easy to find.
// Override with MEDIA_DIR env var if you want a different location.
var mediaDir = func() string {
	if d := os.Getenv("MEDIA_DIR"); d != "" {
		return d
	}
	return "generated"
}()

// ── System prompt ─────────────────────────────────────────────────────────

const systemPrompt = `You are ARIA — an AI Creative Director with a distinct artistic voice and bold visual sensibility.

**Your toolkit:**
- ` + "`generate_image(prompt, size, style, quality)`" + ` — create stunning images with DALL-E 3
- ` + "`generate_video(prompt, aspect_ratio, duration_seconds)`" + ` — generate cinematic video clips with Veo (model configurable; audio is only available on Veo 3+, so default output is silent)

**Your creative philosophy:**
- Always craft *cinematic*, *evocative* prompts — not descriptions, but experiences.
- Go beyond the literal. A request for "a sunset" becomes "Golden hour. A lone lighthouse on a rocky Atlantic coast. Long shadows stretch across tide pools. Wide angle, film grain, Kodachrome palette."
- For images: specify camera angle, lighting, color palette, mood, style reference (photography, oil painting, concept art, anime, etc.), depth of field.
- For videos: describe camera *movement* (slow dolly-in, orbit, handheld, aerial descent), the subject's action, and the environmental detail.

**Decision rules:**
- Static concepts, portraits, or "show me X" → generate_image
- Action, motion, atmosphere, or "animate / video / clip" → generate_video
- Multiple items → generate each separately and compare them
- Ambiguous → pick the medium that best serves the concept and explain your choice

**Workflow:**
1. Rewrite the user's idea into a rich creative prompt.
2. Call the appropriate tool.
3. After the media appears, give a brief creative note — what choices you made and why.
4. Offer a follow-up variation (different style, angle, or medium).

**Tone:** Passionate, expert, opinionated — like a world-class creative director who genuinely loves the craft.`

// ── Agent wiring ──────────────────────────────────────────────────────────

var agentLoop *agent.AgentLoop

func buildLLMProvider() agent.LLMProvider {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("LLM_PROVIDER")))
	switch provider {
	case "anthropic", "claude":
		if p, err := llm.NewAnthropicProvider("", strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL"))); err == nil {
			log.Printf("LLM: Anthropic")
			return p
		}
	case "gemini":
		if p, err := llm.NewGeminiProvider("", strings.TrimSpace(os.Getenv("GEMINI_MODEL"))); err == nil {
			log.Printf("LLM: Gemini")
			return p
		}
	default:
		if p, err := llm.NewOpenAIProvider("", strings.TrimSpace(os.Getenv("OPENAI_MODEL"))); err == nil {
			log.Printf("LLM: OpenAI")
			return p
		}
	}
	log.Printf("LLM: Mock (set LLM_PROVIDER + API key)")
	return agent.NewMockProvider()
}

func initAgent() {
	reg := tools.NewRegistry()

	// Local-disk storage for generated assets — VMs with persistent disk.
	// For Cloud Run / Lambda / ephemeral containers, swap in a GCS / S3
	// adapter that implements builtin.AssetStorage.
	mediaStorage, err := builtin.NewLocalDiskStorage(mediaDir, "/media")
	if err != nil {
		log.Fatalf("NewLocalDiskStorage: %v", err)
	}

	// Image generation — DALL-E 3
	if imgTool, err := builtin.NewGenerateImageTool("", "", mediaStorage); err == nil {
		reg.Register(imgTool)
		log.Printf("Tool: generate_image (DALL-E 3)")
	} else {
		log.Printf("generate_image unavailable: %v", err)
	}

	// Video generation — Veo (model configurable via VEO_MODEL; defaults
	// to veo-2.0-generate-001 inside NewGenerateVideoTool when empty).
	veoModel := strings.TrimSpace(os.Getenv("VEO_MODEL"))
	if vidTool, err := builtin.NewGenerateVideoTool("", veoModel, mediaStorage); err == nil {
		reg.Register(vidTool)
		resolved := veoModel
		if resolved == "" {
			resolved = "veo-2.0-generate-001 (default)"
		}
		log.Printf("Tool: generate_video (%s)", resolved)
	} else {
		log.Printf("generate_video unavailable: %v", err)
	}

	sm := history.NewInMemSessionManager(systemPrompt)
	agentLoop = agent.NewAgentLoop(sm, reg, buildLLMProvider())
	agentLoop.EmitThoughts = true
}

// ── HTTP handlers ─────────────────────────────────────────────────────────

// ChatHandler streams the creative director's response over SSE.
func ChatHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	sessionKey := r.URL.Query().Get("session_id")
	if sessionKey == "" {
		sessionKey = r.RemoteAddr
	}
	userMsg := r.URL.Query().Get("message")
	if userMsg == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}

	for event := range agentLoop.RunText(r.Context(), sessionKey, userMsg) {
		b, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
		if event.Type == "done" {
			return
		}
	}
}

// ── main ──────────────────────────────────────────────────────────────────

func main() {
	for _, p := range []string{".env", "../../.env"} {
		if _, err := os.Stat(p); err == nil {
			_ = godotenv.Overload(p)
		}
	}

	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		log.Fatalf("cannot create media dir: %v", err)
	}

	initAgent()

	// Static frontend
	http.Handle("/", http.FileServer(http.Dir("./frontend")))
	// Serve generated images and videos
	http.Handle("/media/", http.StripPrefix("/media/", http.FileServer(http.Dir(mediaDir))))
	// SSE chat endpoint
	http.HandleFunc("/api/chat", ChatHandler)

	fmt.Printf("Creative Studio started at http://localhost%s\n", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, nil))
}
