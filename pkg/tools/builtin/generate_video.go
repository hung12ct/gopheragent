package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

const (
	defaultVideoModel    = "veo-2.0-generate-001"
	defaultPollInterval  = 10 * time.Second
	defaultPollTimeout   = 5 * time.Minute
	videoDownloadTimeout = 5 * time.Minute
	videoMaxBytes        = 200 * 1024 * 1024 // 200 MB cap on downloaded video bodies
)

// GenerateVideoTool generates short video clips using Google Veo 2 via the
// Gemini API. Generation is asynchronous on Google's side — this tool polls
// until the operation completes (up to 5 minutes). Implements InlineRenderer so
// the video embed streams directly to the frontend chat.
//
// Set SaveDir and URLBase before use — same semantics as GenerateImageTool.
type generateVideoArgs struct {
	Prompt          string `json:"prompt"                     description:"Detailed scene description. Include action, camera motion, lighting, style, and mood. Example: 'Slow dolly-in on a glowing neon cityscape at night, rain-slicked streets reflecting pink and blue lights, cinematic film grain.'"`
	AspectRatio     string `json:"aspect_ratio,omitempty"     description:"16:9 for landscape/cinematic, 9:16 for vertical/social media." enum:"16:9,9:16"`
	DurationSeconds int    `json:"duration_seconds,omitempty" description:"Length of the clip in seconds. Supported range depends on the model (Veo 2: 5–8, Veo 3: typically 8). Defaults to 8 when omitted; the provider will reject out-of-range values."`
}

type GenerateVideoTool struct {
	client     *genai.Client
	httpClient *http.Client
	apiKey     string // kept for authenticated downloads from the Gemini Files API
	model      string
	SaveDir    string // local directory to save generated video files
	URLBase    string // URL prefix served by the host HTTP server, e.g. "/media"
}

// NewGenerateVideoTool builds a video generation tool.
// apiKey defaults to GEMINI_API_KEY; model defaults to veo-2.0-generate-001.
func NewGenerateVideoTool(apiKey, model string) (*GenerateVideoTool, error) {
	if apiKey == "" {
		apiKey = os.Getenv("GEMINI_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("tools: GEMINI_API_KEY not set")
	}
	if model == "" {
		model = defaultVideoModel
	}
	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey: apiKey,
	})
	if err != nil {
		return nil, fmt.Errorf("tools: generate_video: %w", err)
	}
	return &GenerateVideoTool{
		client:     client,
		httpClient: &http.Client{Timeout: videoDownloadTimeout},
		apiKey:     apiKey,
		model:      model,
	}, nil
}

func (t *GenerateVideoTool) Name() string { return "generate_video" }
func (t *GenerateVideoTool) Description() string {
	return "Generate a short video clip (5–8 seconds) from a text description using Veo 2. " +
		"Include camera movement (pan, zoom, dolly), subject action, environment, and mood. " +
		"Generation takes 1–3 minutes — tell the user to wait. " +
		"After generating, the video will appear inline in the chat."
}
func (t *GenerateVideoTool) ParametersSchema() tools.ToolSchema {
	return tools.SchemaFor[generateVideoArgs]()
}
func (t *GenerateVideoTool) RequiresConfirmation() bool         { return false }
func (t *GenerateVideoTool) InlineResult() bool                 { return true }

// Execute starts a Veo 2 generation job, polls until complete, saves the video,
// and returns an HTML <video> embed pointing at URLBase/<filename>.
func (t *GenerateVideoTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args generateVideoArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("tools: generate_video: invalid args: %w", err)
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return "", fmt.Errorf("tools: generate_video: prompt is required")
	}
	if args.AspectRatio == "" {
		args.AspectRatio = "16:9"
	}
	// Supported durations vary by model — Veo 2 accepts 5–8s, Veo 3 is
	// typically fixed at 8s. Rather than silently clamping (which can
	// hide the user's real intent), default to 8s when unspecified and
	// let the provider return a clear error for out-of-range values.
	// Keep a sane upper bound to reject obvious typos like "600".
	dur := args.DurationSeconds
	if dur <= 0 {
		dur = 8
	}
	if dur > 60 {
		dur = 60
	}
	dur32 := int32(dur)

	cfg := &genai.GenerateVideosConfig{
		NumberOfVideos:  1,
		AspectRatio:     args.AspectRatio,
		DurationSeconds: &dur32,
	}

	// Start the asynchronous generation.
	op, err := t.client.Models.GenerateVideos(ctx, t.model, args.Prompt, nil, cfg)
	if err != nil {
		return "", fmt.Errorf("tools: generate_video: start: %w", err)
	}

	// Bound the polling loop with both the caller's context and our own
	// timeout — whichever fires first wins.
	pollCtx, cancel := context.WithTimeout(ctx, defaultPollTimeout)
	defer cancel()

	// Emit an initial progress tick immediately so the UI shows motion
	// before the first poll interval elapses. Include the model name so
	// operators running a non-default model see the right identifier.
	tools.ReportProgress(ctx, fmt.Sprintf("Starting video generation with %s…", t.model))

	pollStart := time.Now()
	for !op.Done {
		select {
		case <-pollCtx.Done():
			if pollCtx.Err() == context.DeadlineExceeded {
				return "", fmt.Errorf("tools: generate_video: timed out after %v", defaultPollTimeout)
			}
			return "", pollCtx.Err()
		case <-time.After(defaultPollInterval):
		}
		elapsed := time.Since(pollStart).Round(time.Second)
		tools.ReportProgress(ctx, fmt.Sprintf("Generating video… (%s elapsed, checking again in %s)", elapsed, defaultPollInterval))
		op, err = t.client.Operations.GetVideosOperation(pollCtx, op, nil)
		if err != nil {
			return "", fmt.Errorf("tools: generate_video: poll: %w", err)
		}
	}

	if op.Error != nil {
		msg, _ := json.Marshal(op.Error)
		return "", fmt.Errorf("tools: generate_video: generation failed: %s", msg)
	}
	if op.Response == nil || len(op.Response.GeneratedVideos) == 0 {
		return "", fmt.Errorf("tools: generate_video: no video in response")
	}

	vid := op.Response.GeneratedVideos[0].Video
	if vid == nil {
		return "", fmt.Errorf("tools: generate_video: video object is nil")
	}

	tools.ReportProgress(ctx, "Generation complete, downloading video…")
	localURL, err := t.saveVideo(ctx, vid)
	if err != nil {
		return "", fmt.Errorf("tools: generate_video: save: %w", err)
	}

	// Inline HTML embed. The frontend's DOMPurify config must allow <video>
	// and <source> tags (see ADD_TAGS in the host page) for this to render.
	return fmt.Sprintf(
		`<video controls preload="metadata" style="max-width:100%%;border-radius:8px;margin:6px 0"><source src="%s" type="video/mp4">Your browser does not support video.</video>`+"\n\n*Prompt: %s*",
		localURL, args.Prompt,
	), nil
}

// saveVideo persists the video bytes or downloads from URI, returns local URL.
func (t *GenerateVideoTool) saveVideo(ctx context.Context, vid *genai.Video) (string, error) {
	if t.SaveDir == "" {
		return "", fmt.Errorf("SaveDir not configured")
	}
	if err := os.MkdirAll(t.SaveDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}

	filename := fmt.Sprintf("vid-%d.mp4", time.Now().UnixNano())
	dest := filepath.Join(t.SaveDir, filename)

	var data []byte
	switch {
	case len(vid.VideoBytes) > 0:
		data = vid.VideoBytes
	case vid.URI != "":
		// Veo 2 via the Gemini API returns a Files API URI that requires the API
		// key to download. Fetch it server-side and save locally.
		var err error
		data, err = t.downloadURI(ctx, vid.URI)
		if err != nil {
			return "", fmt.Errorf("download URI: %w", err)
		}
	default:
		return "", fmt.Errorf("video has neither bytes nor URI")
	}

	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}

	base := strings.TrimRight(t.URLBase, "/")
	return base + "/" + filename, nil
}

// downloadURI fetches a Gemini Files API URI, appending the API key so the
// request is authenticated. Works for URLs of the form:
//
//	https://generativelanguage.googleapis.com/v1beta/files/<id>:download?alt=media
//
// Honors ctx for cancellation, caps the body size, and never echoes the raw
// authenticated URL into errors.
func (t *GenerateVideoTool) downloadURI(ctx context.Context, uri string) ([]byte, error) {
	sep := "?"
	if strings.Contains(uri, "?") {
		sep = "&"
	}
	authenticated := uri + sep + "key=" + t.apiKey

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authenticated, nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, redactKeyParam(uri))
	}
	return io.ReadAll(io.LimitReader(resp.Body, videoMaxBytes))
}

// redactKeyParam returns the URL with any "key" query parameter masked out, so
// API keys appended for auth never leak into error messages or logs.
func redactKeyParam(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	if q.Get("key") != "" {
		q.Set("key", "REDACTED")
		u.RawQuery = q.Encode()
	}
	return u.String()
}
