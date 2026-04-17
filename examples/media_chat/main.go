// media_chat demonstrates native multimodal input: uploaded images are
// injected into the conversation history as MediaParts and become directly
// visible to the LLM — no tool call, no separate vision API, no base64
// round-trip each turn. "User uploads photo, assistant references it 3
// turns later" works for free.
//
// Video and text files still use a describe_file tool fallback because
// video input isn't uniformly supported across providers (Anthropic
// declines it; Gemini requires Files-API uploads).
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/llm"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// ── Constants ──────────────────────────────────────────────────────────────

const (
	listenAddr    = ":8889"
	maxUploadSize = 20 << 20 // 20 MB
)

// uploadDir is where uploaded files are stored for the lifetime of the server.
var uploadDir = filepath.Join(os.TempDir(), "gopheragent-media")

// ── File session tracking ──────────────────────────────────────────────────

// uploadedFile tracks a non-image file (video, text) that still uses the
// describe_file tool path. Image uploads bypass this — they go straight
// into conversation history as MediaParts.
type uploadedFile struct {
	Path     string
	MIMEType string
	Name     string
}

// allowedMIME lists accepted upload types and how to handle them.
// The "image" category routes through native MediaParts; everything
// else routes through describe_file.
var allowedMIME = map[string]string{
	"image/jpeg":       "image",
	"image/png":        "image",
	"image/gif":        "image",
	"image/webp":       "image",
	"image/svg+xml":    "image",
	"video/mp4":        "video",
	"video/webm":       "video",
	"video/quicktime":  "video",
	"video/ogg":        "video",
	"text/plain":       "text",
	"text/markdown":    "text",
	"text/csv":         "text",
	"application/json": "text",
}

// ── Analyzer interface ────────────────────────────────────────────────────

// analyzer is the narrow interface satisfied by llm.GeminiMediaAnalyzer —
// kept only for the video path. OpenAI vision is no longer needed because
// image analysis happens natively in the main LLM call.
type analyzer interface {
	Analyze(ctx context.Context, media, prompt string) (string, error)
}

// ── DescribeFileTool (video + text only) ──────────────────────────────────

// DescribeFileTool analyzes the video or text file the user uploaded for
// the current session. Image uploads never reach this tool — they're
// attached natively to the conversation so the main LLM sees them
// directly.
type DescribeFileTool struct {
	mu       sync.RWMutex
	sessions map[string]uploadedFile
	media    analyzer // Gemini media analyzer; may be nil
}

func newDescribeFileTool(m analyzer) *DescribeFileTool {
	return &DescribeFileTool{
		sessions: make(map[string]uploadedFile),
		media:    m,
	}
}

func (t *DescribeFileTool) setFile(sessionKey string, f uploadedFile) {
	t.mu.Lock()
	t.sessions[sessionKey] = f
	t.mu.Unlock()
}

func (t *DescribeFileTool) clearFile(sessionKey string) {
	t.mu.Lock()
	delete(t.sessions, sessionKey)
	t.mu.Unlock()
}

func (t *DescribeFileTool) Name() string { return "describe_file" }
func (t *DescribeFileTool) Description() string {
	return "Analyze a video or text file the user uploaded in this session. Pass the user's exact question or analysis instruction as the prompt. Not used for images — images are visible directly in the conversation."
}
func (t *DescribeFileTool) ParametersSchema() tools.ToolSchema {
	return tools.ToolSchema{
		Type: "object",
		Properties: map[string]any{
			"prompt": map[string]any{
				"type":        "string",
				"description": "Specific question or instruction for the uploaded file, e.g. 'Summarize the key events in this video.'",
			},
		},
		Required: []string{"prompt"},
	}
}
func (t *DescribeFileTool) RequiresConfirmation() bool { return false }

func (t *DescribeFileTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("tools: invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return "", fmt.Errorf("tools: prompt is required")
	}

	sessionKey, _ := ctx.Value(agent.SessionKeyCtx("sessionKey")).(string)
	t.mu.RLock()
	f, ok := t.sessions[sessionKey]
	t.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("tools: no video or text file uploaded for this session")
	}

	switch allowedMIME[f.MIMEType] {
	case "video":
		if t.media == nil {
			return "", fmt.Errorf("tools: video analysis requires GEMINI_API_KEY")
		}
		raw, err := os.ReadFile(f.Path)
		if err != nil {
			return "", fmt.Errorf("tools: read file: %w", err)
		}
		dataURI := fmt.Sprintf("data:%s;base64,%s", f.MIMEType, base64.StdEncoding.EncodeToString(raw))
		return t.media.Analyze(ctx, dataURI, args.Prompt)
	case "text":
		raw, err := os.ReadFile(f.Path)
		if err != nil {
			return "", fmt.Errorf("tools: read file: %w", err)
		}
		const maxRunes = 12_000
		runes := []rune(string(raw))
		truncated := false
		if len(runes) > maxRunes {
			runes = runes[:maxRunes]
			truncated = true
		}
		out := fmt.Sprintf("File: %s\nType: %s\n\nContent:\n%s", f.Name, f.MIMEType, string(runes))
		if truncated {
			out += "\n\n[File truncated at 12,000 characters]"
		}
		out += fmt.Sprintf("\n\n---\nUser instruction: %s", args.Prompt)
		return out, nil
	default:
		return "", fmt.Errorf("tools: unsupported file type %q for describe_file", f.MIMEType)
	}
}

// ── Agent setup ───────────────────────────────────────────────────────────

const systemPrompt = `You are an expert file-analysis assistant.

**Images:** Uploaded images are visible to you directly in this conversation — describe, analyze, compare, and reference them as if they were part of the user's message. You do NOT need a tool for images.

**Videos and text files:** Call ` + "`describe_file(prompt)`" + ` to analyze them. Pass the user's exact question as the prompt.

**Memory:** When a user uploads an image and then asks follow-up questions (possibly many turns later), the image is still in this conversation history — you can still see and reason about it.

Be specific, detailed, and insightful — don't just rephrase tool output or image captions. Point out noteworthy details, patterns, or implications the user might miss.`

var (
	agentLoop      *agent.AgentLoop
	sessionManager *history.InMemSessionManager
	descFileTool   *DescribeFileTool
)

// buildMediaAnalyzer returns the Gemini media analyzer for video analysis.
// Nil is a valid return value — the tool surfaces a clear error when
// GEMINI_API_KEY is missing and the user uploads a video.
func buildMediaAnalyzer() analyzer {
	a, err := llm.NewGeminiMediaAnalyzer("", "")
	if err != nil {
		log.Printf("Gemini media analyzer unavailable (video uploads will fail): %v", err)
		return nil
	}
	log.Printf("Media analyzer: Gemini (video)")
	return a
}

func buildLLMProvider() agent.LLMProvider {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("LLM_PROVIDER")))
	switch provider {
	case "anthropic", "claude":
		p, err := llm.NewAnthropicProvider("", strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL")))
		if err == nil {
			log.Printf("LLM: Anthropic")
			return p
		}
	case "gemini":
		p, err := llm.NewGeminiProvider("", strings.TrimSpace(os.Getenv("GEMINI_MODEL")))
		if err == nil {
			log.Printf("LLM: Gemini")
			return p
		}
	default:
		p, err := llm.NewOpenAIProvider("", strings.TrimSpace(os.Getenv("OPENAI_MODEL")))
		if err == nil {
			log.Printf("LLM: OpenAI")
			return p
		}
	}
	log.Printf("LLM: Mock (set LLM_PROVIDER + API key to use a real model)")
	return agent.NewMockProvider()
}

func initAgent() {
	descFileTool = newDescribeFileTool(buildMediaAnalyzer())

	reg := tools.NewRegistry()
	reg.Register(descFileTool)

	sessionManager = history.NewInMemSessionManager(systemPrompt)
	agentLoop = agent.NewAgentLoop(sessionManager, reg, buildLLMProvider())
	agentLoop.EmitThoughts = true
}

// ── HTTP handlers ─────────────────────────────────────────────────────────

// UploadHandler accepts multipart/form-data. Image uploads are written
// directly into session history as a user message carrying a MediaPart
// plus a short synthetic assistant ack (keeping the user/assistant
// alternation that Anthropic requires). Video and text uploads register
// with describe_file as before.
func UploadHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	sessionKey := r.FormValue("session_id")
	if sessionKey == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}

	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		http.Error(w, "file too large (max 20 MB)", http.StatusRequestEntityTooLarge)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file field missing", http.StatusBadRequest)
		return
	}
	defer file.Close()

	mimeType := header.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = mime.TypeByExtension(filepath.Ext(header.Filename))
	}
	if idx := strings.Index(mimeType, ";"); idx >= 0 {
		mimeType = strings.TrimSpace(mimeType[:idx])
	}
	category, ok := allowedMIME[mimeType]
	if !ok {
		http.Error(w, fmt.Sprintf("unsupported file type %q", mimeType), http.StatusBadRequest)
		return
	}

	ext := filepath.Ext(header.Filename)
	if ext == "" {
		ext = extensionForMIME(mimeType)
	}
	filename := fmt.Sprintf("%d%s", time.Now().UnixNano(), ext)
	destPath := filepath.Join(uploadDir, filename)

	dest, err := os.Create(destPath)
	if err != nil {
		http.Error(w, "could not save file", http.StatusInternalServerError)
		return
	}
	defer dest.Close()
	if _, err := io.Copy(dest, file); err != nil {
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}

	switch category {
	case "image":
		if err := injectImageIntoHistory(r.Context(), sessionKey, destPath, mimeType, header.Filename); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Drop any stale non-image file association — the user has moved on.
		descFileTool.clearFile(sessionKey)
		log.Printf("Upload: session=%s image=%s (injected into history)", sessionKey, header.Filename)
	default:
		descFileTool.setFile(sessionKey, uploadedFile{
			Path:     destPath,
			MIMEType: mimeType,
			Name:     header.Filename,
		})
		log.Printf("Upload: session=%s file=%s type=%s (describe_file)", sessionKey, header.Filename, mimeType)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":        true,
		"name":      header.Filename,
		"mime_type": mimeType,
		"category":  category,
		"native":    category == "image",
	})
}

// injectImageIntoHistory appends (user-with-image, assistant-ack) pair to
// the session. The ack message preserves provider alternation rules and
// gives the frontend a visible signal that the file was received. Every
// subsequent chat turn sees the image in history without re-encoding.
func injectImageIntoHistory(ctx context.Context, sessionKey, path, mimeType, name string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read upload: %w", err)
	}
	msgs := sessionManager.GetHistory(ctx, sessionKey)
	msgs = append(msgs,
		history.Message{
			Role:    "user",
			Content: fmt.Sprintf("[Uploaded image: %s]", name),
			Parts:   []history.MediaPart{history.NewImagePartBytes(mimeType, raw)},
		},
		history.Message{
			Role:    "assistant",
			Content: fmt.Sprintf("Got it — I can see %q. What would you like to know about it?", name),
		},
	)
	sessionManager.SetHistory(ctx, sessionKey, msgs)
	return sessionManager.Save(ctx, sessionKey)
}

// ChatHandler streams the agent's response over SSE. Plain text in, SSE
// events out — all the multimodal magic is already in session history by
// the time we get here.
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
		userMsg = "What can you tell me about this file?"
	}

	streamChan := make(chan agent.StreamEvent, 32)
	go agentLoop.RunIterationStream(r.Context(), sessionKey, userMsg, streamChan)

	for {
		select {
		case event, ok := <-streamChan:
			if !ok {
				return
			}
			b, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
			if event.Type == "done" {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func extensionForMIME(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/svg+xml":
		return ".svg"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	case "video/ogg":
		return ".ogv"
	case "text/plain":
		return ".txt"
	case "text/markdown":
		return ".md"
	case "text/csv":
		return ".csv"
	case "application/json":
		return ".json"
	}
	return ".bin"
}

// ── main ──────────────────────────────────────────────────────────────────

func loadEnvFiles() {
	for _, p := range []string{".env", "../../.env"} {
		if _, err := os.Stat(p); err == nil {
			if err := godotenv.Overload(p); err != nil {
				log.Printf("Warning: failed loading %s: %v", p, err)
			}
		}
	}
}

func main() {
	loadEnvFiles()

	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		log.Fatalf("cannot create upload dir: %v", err)
	}

	initAgent()

	http.Handle("/", http.FileServer(http.Dir("./frontend")))
	http.HandleFunc("/api/upload", UploadHandler)
	http.HandleFunc("/api/chat", ChatHandler)

	fmt.Printf("Media Chat started at http://localhost%s\n", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, nil))
}
