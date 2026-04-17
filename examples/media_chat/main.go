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

type uploadedFile struct {
	Path     string // absolute path on disk
	MIMEType string // e.g. "image/jpeg"
	Name     string // original filename
}

// allowedMIME lists accepted upload types and how to handle them.
var allowedMIME = map[string]string{
	"image/jpeg":      "image",
	"image/png":       "image",
	"image/gif":       "image",
	"image/webp":      "image",
	"image/svg+xml":   "image",
	"video/mp4":       "video",
	"video/webm":      "video",
	"video/quicktime": "video",
	"video/ogg":       "video",
	"text/plain":      "text",
	"text/markdown":   "text",
	"text/csv":        "text",
	"application/json": "text",
}

// ── Analyzer interface ────────────────────────────────────────────────────

// analyzer is the narrow interface satisfied by llm.OpenAIVisionAnalyzer and
// llm.GeminiMediaAnalyzer.
type analyzer interface {
	Analyze(ctx context.Context, media, prompt string) (string, error)
}

// ── DescribeFileTool ──────────────────────────────────────────────────────

// DescribeFileTool lets the agent query the file the user uploaded for the
// current session. It encodes images/videos as base64 data URIs and passes
// them to the vision/media model; text files are returned verbatim.
type DescribeFileTool struct {
	mu       sync.RWMutex
	sessions map[string]uploadedFile // sessionKey → file
	vision   analyzer                // images (OpenAI or Gemini); may be nil
	media    analyzer                // images + videos (Gemini); may be nil
}

func newDescribeFileTool(v, m analyzer) *DescribeFileTool {
	return &DescribeFileTool{
		sessions: make(map[string]uploadedFile),
		vision:   v,
		media:    m,
	}
}

func (t *DescribeFileTool) setFile(sessionKey string, f uploadedFile) {
	t.mu.Lock()
	t.sessions[sessionKey] = f
	t.mu.Unlock()
}

func (t *DescribeFileTool) getFile(sessionKey string) (uploadedFile, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	f, ok := t.sessions[sessionKey]
	return f, ok
}

func (t *DescribeFileTool) Name() string { return "describe_file" }
func (t *DescribeFileTool) Description() string {
	return "Analyze the file the user uploaded in this session. Pass the user's exact question or analysis instruction as the prompt. For images this calls a vision model; for text files it reads the content. Always call this before answering questions about the uploaded file."
}
func (t *DescribeFileTool) ParametersSchema() tools.ToolSchema {
	return tools.ToolSchema{
		Type: "object",
		Properties: map[string]any{
			"prompt": map[string]any{
				"type":        "string",
				"description": "Specific question or instruction for the uploaded file, e.g. 'What does this chart show?' or 'Summarize the key points.'",
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
		return "", fmt.Errorf("tools: no file uploaded for this session — ask the user to upload a file first")
	}

	category := allowedMIME[f.MIMEType]

	switch category {
	case "image", "video":
		return t.describeMedia(ctx, f, args.Prompt)
	case "text":
		return t.describeText(f, args.Prompt)
	default:
		return "", fmt.Errorf("tools: unsupported file type %q", f.MIMEType)
	}
}

// describeMedia handles both image and video files by base64-encoding them as
// data URIs. Video files require a media-capable analyzer (Gemini); images
// fall back to the OpenAI vision analyzer if no media analyzer is configured.
func (t *DescribeFileTool) describeMedia(ctx context.Context, f uploadedFile, prompt string) (string, error) {
	category := allowedMIME[f.MIMEType]

	// Pick the right analyzer.
	var a analyzer
	if t.media != nil {
		a = t.media // Gemini: handles images and videos
	} else if category == "image" && t.vision != nil {
		a = t.vision // OpenAI: images only
	}
	if a == nil {
		if category == "video" {
			return "", fmt.Errorf("tools: video analysis requires GEMINI_API_KEY — set it to enable video support")
		}
		return "", fmt.Errorf("tools: vision analysis not configured — set OPENAI_API_KEY or GEMINI_API_KEY")
	}

	raw, err := os.ReadFile(f.Path)
	if err != nil {
		return "", fmt.Errorf("tools: read file: %w", err)
	}
	dataURI := fmt.Sprintf("data:%s;base64,%s", f.MIMEType, base64.StdEncoding.EncodeToString(raw))
	return a.Analyze(ctx, dataURI, prompt)
}

func (t *DescribeFileTool) describeText(f uploadedFile, prompt string) (string, error) {
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		return "", fmt.Errorf("tools: read file: %w", err)
	}
	const maxRunes = 12_000
	content := string(raw)
	runes := []rune(content)
	truncated := false
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
		truncated = true
	}
	result := fmt.Sprintf("File: %s\nType: %s\n\nContent:\n%s", f.Name, f.MIMEType, string(runes))
	if truncated {
		result += "\n\n[File truncated at 12,000 characters]"
	}
	result += fmt.Sprintf("\n\n---\nUser instruction: %s", prompt)
	return result, nil
}

// ── Agent setup ───────────────────────────────────────────────────────────

const systemPrompt = `You are an expert file analysis assistant. The user can upload images, videos, and text documents, then ask you questions about them.

**Your tool:**
- ` + "`describe_file(prompt)`" + ` — analyze the uploaded file with a specific question. For images and videos, this calls a multimodal vision model. For text files, this returns the content.

**Workflow:**
1. When the user asks anything about the uploaded file, call ` + "`describe_file`" + ` with their question as the prompt.
2. After receiving the analysis, give a detailed, insightful answer — don't just repeat the raw output.
3. You may call ` + "`describe_file`" + ` multiple times with different prompts to explore different aspects.
4. If no file has been uploaded yet, ask the user to upload one.

**For images:** provide rich analysis — describe objects, layout, colors, text, data trends, or anything relevant to the user's question.
**For videos:** describe what is happening, key scenes, people, objects, actions, or any relevant content the user asks about.
**For documents:** summarize, extract key points, compare data, or answer specific questions about the content.`

var (
	agentLoop    *agent.AgentLoop
	descFileTool *DescribeFileTool
)

// buildAnalyzers returns (imageAnalyzer, mediaAnalyzer).
// mediaAnalyzer (Gemini) handles images AND videos.
// imageAnalyzer (OpenAI) is a fallback for images when Gemini is unavailable.
func buildAnalyzers() (imageOnly, media analyzer) {
	if a, err := llm.NewGeminiMediaAnalyzer("", ""); err == nil {
		log.Printf("Media analyzer: Gemini (images + video)")
		media = a
	} else {
		log.Printf("Gemini media analyzer unavailable (%v)", err)
	}
	if a, err := llm.NewOpenAIVisionAnalyzer("", ""); err == nil {
		log.Printf("Image analyzer: OpenAI gpt-4o (images only)")
		imageOnly = a
	} else {
		log.Printf("OpenAI vision analyzer unavailable (%v)", err)
	}
	if media == nil && imageOnly == nil {
		log.Printf("No vision analyzer configured — set GEMINI_API_KEY (images+video) or OPENAI_API_KEY (images only)")
	}
	return
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
	imageOnly, media := buildAnalyzers()
	descFileTool = newDescribeFileTool(imageOnly, media)

	reg := tools.NewRegistry()
	reg.Register(descFileTool)

	sm := history.NewInMemSessionManager(systemPrompt)
	agentLoop = agent.NewAgentLoop(sm, reg, buildLLMProvider())
	agentLoop.EmitThoughts = true
}

// ── HTTP handlers ─────────────────────────────────────────────────────────

// UploadHandler accepts multipart/form-data with fields:
//   - file: the file to upload
//   - session_id: identifies the session
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

	// Detect MIME type.
	mimeType := header.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = mime.TypeByExtension(filepath.Ext(header.Filename))
	}
	// Strip parameters (e.g. "text/plain; charset=utf-8" → "text/plain")
	if idx := strings.Index(mimeType, ";"); idx >= 0 {
		mimeType = strings.TrimSpace(mimeType[:idx])
	}
	if _, ok := allowedMIME[mimeType]; !ok {
		http.Error(w, fmt.Sprintf("unsupported file type %q — supported: jpg, png, gif, webp, mp4, webm, mov, txt, md, csv, json", mimeType), http.StatusBadRequest)
		return
	}

	// Save to upload directory.
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

	descFileTool.setFile(sessionKey, uploadedFile{
		Path:     destPath,
		MIMEType: mimeType,
		Name:     header.Filename,
	})

	log.Printf("Upload: session=%s file=%s type=%s", sessionKey, header.Filename, mimeType)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":        true,
		"name":      header.Filename,
		"mime_type": mimeType,
		"category":  allowedMIME[mimeType],
	})
}

// ChatHandler streams the agent's response over SSE.
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

	// Prepend file context so the LLM knows a file has been uploaded.
	if f, ok := descFileTool.getFile(sessionKey); ok {
		userMsg = fmt.Sprintf(
			"[Context: The user has uploaded a file named %q (MIME type: %s). Call describe_file to analyze it when answering questions about it.]\n\n%s",
			f.Name, f.MIMEType, userMsg,
		)
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
