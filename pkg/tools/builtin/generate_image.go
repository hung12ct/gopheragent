package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

const (
	imageDownloadTimeout = 60 * time.Second
	imageMaxBytes        = 50 * 1024 * 1024 // 50 MB cap on downloaded image bodies
	imageAltMaxLen       = 80               // alt-text rune cap (prevents 1000-char DALL-E revisions in <img alt>)
)

// GenerateImageTool generates images using DALL-E 3. The result is a markdown
// image embed that the frontend renders inline. Implements InlineRenderer so the
// image appears in the chat immediately without waiting for the LLM to format it.
//
// Persistence: set Storage to a custom AssetStorage (GCS, S3, Azure Blob)
// for cloud / container deployments. The legacy SaveDir+URLBase fields
// still work — when Storage is nil and SaveDir is set, the tool builds a
// LocalDiskStorage internally so existing callers keep working unchanged.
// Cloud-deployed adopters MUST set Storage; local-disk on Cloud Run /
// Lambda / ephemeral containers loses the file between requests.
type generateImageArgs struct {
	Prompt  string `json:"prompt"            description:"Detailed visual description. Include style (photorealistic, oil painting, anime…), lighting, mood, camera angle, color palette."`
	Size    string `json:"size,omitempty"    description:"Image dimensions. Use 1792x1024 for landscapes/wide scenes, 1024x1792 for portraits/tall compositions, 1024x1024 for balanced square shots." enum:"1024x1024,1792x1024,1024x1792"`
	Style   string `json:"style,omitempty"   description:"vivid: hyper-real, dramatic, saturated. natural: softer, more realistic." enum:"vivid,natural"`
	Quality string `json:"quality,omitempty" description:"hd enables finer detail and sharper texture — recommended for showcase images." enum:"standard,hd"`
}

type GenerateImageTool struct {
	client     *openai.Client
	httpClient *http.Client
	model      string
	// Storage persists the downloaded image bytes and returns the public
	// URL the markdown embed points at. Required — Execute returns an
	// error if Storage is nil. Use builtin.LocalDiskStorage for VMs with
	// persistent disk; plug in a GCS / S3 / Azure Blob adapter for
	// container platforms with ephemeral disk.
	Storage AssetStorage
}

// NewGenerateImageTool builds an image generation tool.
// apiKey defaults to OPENAI_API_KEY; model defaults to dall-e-3.
func NewGenerateImageTool(apiKey, model string) (*GenerateImageTool, error) {
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("tools: OPENAI_API_KEY not set")
	}
	if model == "" {
		model = "dall-e-3"
	}
	return &GenerateImageTool{
		client:     openai.NewClient(apiKey),
		httpClient: &http.Client{Timeout: imageDownloadTimeout},
		model:      model,
	}, nil
}

func (t *GenerateImageTool) Name() string { return "generate_image" }
func (t *GenerateImageTool) Description() string {
	return "Generate a high-quality image from a text description using DALL-E 3. " +
		"Always craft a vivid, detailed prompt — style, lighting, mood, composition, camera angle. " +
		"After generating, display the image directly in the chat."
}
func (t *GenerateImageTool) ParametersSchema() tools.ToolSchema {
	return tools.SchemaFor[generateImageArgs]()
}
func (t *GenerateImageTool) RequiresConfirmation() bool         { return false }
func (t *GenerateImageTool) InlineResult() bool                 { return true }

// Execute generates the image, downloads it, saves it to SaveDir, and returns
// a markdown image tag pointing at URLBase/<filename>.
func (t *GenerateImageTool) Display() tools.ToolDisplay { return tools.DefaultDisplay(t.Name(), t.Description()) }
func (t *GenerateImageTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args generateImageArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("tools: generate_image: invalid args: %w", err)
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return "", fmt.Errorf("tools: generate_image: prompt is required")
	}
	if args.Size == "" {
		args.Size = "1024x1024"
	}
	if args.Style == "" {
		args.Style = "vivid"
	}
	if args.Quality == "" {
		args.Quality = "standard"
	}

	req := openai.ImageRequest{
		Model:          t.model,
		Prompt:         args.Prompt,
		N:              1,
		Size:           args.Size,
		Style:          args.Style,
		Quality:        args.Quality,
		ResponseFormat: openai.CreateImageResponseFormatURL,
	}

	resp, err := t.client.CreateImage(ctx, req)
	if err != nil {
		return "", fmt.Errorf("tools: generate_image: %w", err)
	}
	if len(resp.Data) == 0 {
		return "", fmt.Errorf("tools: generate_image: no image returned")
	}

	img := resp.Data[0]
	revisedPrompt := img.RevisedPrompt
	if revisedPrompt == "" {
		revisedPrompt = args.Prompt
	}
	alt := truncateRunes(revisedPrompt, imageAltMaxLen)

	// Download and save locally so the URL doesn't expire.
	localURL, err := t.download(ctx, img.URL)
	if err != nil {
		// Fall back to the CDN URL (expires in ~1 hour). Log so a recurring
		// local-save failure is diagnosable instead of silently degrading.
		log.Printf("tools: generate_image: local save failed, returning CDN URL: %v", err)
		return fmt.Sprintf("![%s](%s)\n\n*Prompt: %s*", alt, img.URL, revisedPrompt), nil
	}

	return fmt.Sprintf("![%s](%s)\n\n*Prompt: %s*", alt, localURL, revisedPrompt), nil
}

// download fetches an image from src, hands the bytes to Storage, and
// returns the public URL the storage backend assigned. Caps the download
// size and honors ctx for cancellation.
func (t *GenerateImageTool) download(ctx context.Context, src string) (string, error) {
	if t.Storage == nil {
		return "", fmt.Errorf("tools: generate_image: Storage not configured")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return "", err
	}
	hresp, err := t.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer hresp.Body.Close()
	if hresp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d", hresp.StatusCode)
	}

	contentType := hresp.Header.Get("Content-Type")
	ext := extensionForImage(contentType, src)
	filename := fmt.Sprintf("img-%d%s", time.Now().UnixNano(), ext)

	data, err := io.ReadAll(io.LimitReader(hresp.Body, imageMaxBytes))
	if err != nil {
		return "", err
	}
	return t.Storage.Save(ctx, filename, data, contentType)
}

// extensionForImage chooses a file extension from the response Content-Type,
// falling back to the URL path extension and finally ".png".
func extensionForImage(contentType, src string) string {
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "jpeg"), strings.Contains(ct, "jpg"):
		return ".jpg"
	case strings.Contains(ct, "webp"):
		return ".webp"
	case strings.Contains(ct, "png"):
		return ".png"
	case strings.Contains(ct, "gif"):
		return ".gif"
	}
	if u, err := url.Parse(src); err == nil {
		switch e := strings.ToLower(path.Ext(u.Path)); e {
		case ".jpg", ".jpeg", ".png", ".webp", ".gif":
			if e == ".jpeg" {
				return ".jpg"
			}
			return e
		}
	}
	return ".png"
}

// truncateRunes returns s truncated to at most n runes, appending "…" when cut.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
