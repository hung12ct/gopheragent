package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// MediaAnalyzer is the provider-side dependency injected into
// MediaAnalyzeTool. Implementations call the underlying multimodal model
// (Gemini vision/video, OpenAI GPT-4o, Claude vision, etc.) and return a
// natural-language description.
//
// This interface lives in the tools package (not the llm package) to keep
// MediaAnalyzeTool decoupled from any specific provider — users wire in a
// thin adapter over whatever SDK they already use.
//
// media is one of:
//   - an absolute http(s) URL pointing to an image or video
//   - a Google Cloud Storage URI ("gs://bucket/object") for Gemini video
//   - a data URI ("data:image/png;base64,...") for inline images
//   - raw base64 (the adapter decides how to interpret it)
//
// Not all providers support video. Gemini 1.5/2.0 accepts video URLs and
// gs:// URIs natively; OpenAI GPT-4o and Claude 3.x support images only.
// The adapter is responsible for surfacing a clear error when the media
// type is unsupported by the underlying model.
//
// prompt is the analysis instruction ("describe the chart", "extract all
// visible text", "summarise what happens in this video clip").
type MediaAnalyzer interface {
	Analyze(ctx context.Context, media, prompt string) (string, error)
}

// MediaAnalyzeTool passes a media item (image or video) + instruction to a
// multimodal model and returns its description. Use it for OCR, chart
// reading, screenshot understanding, video summarisation, and any other
// visual or temporal analysis task.
type MediaAnalyzeTool struct {
	analyzer MediaAnalyzer
}

// NewMediaAnalyzeTool wires up an analyzer. analyzer must not be nil.
func NewMediaAnalyzeTool(analyzer MediaAnalyzer) *MediaAnalyzeTool {
	return &MediaAnalyzeTool{analyzer: analyzer}
}

func (t *MediaAnalyzeTool) Name() string { return "media_analyze" }

func (t *MediaAnalyzeTool) Description() string {
	return "Analyze an image or video using a multimodal model. Accepts an http(s) URL, a Google Cloud Storage URI (gs://), or a data URI. Use for OCR, chart reading, screenshot understanding, video summarisation, or any visual question-answering. Note: video support depends on the underlying provider (Gemini supports video; OpenAI and Claude support images only)."
}

type mediaAnalyzeArgs struct {
	Media  string `json:"media"  description:"The media to analyze — an http(s) URL, a gs:// Cloud Storage URI (for Gemini video), or a data: URI for inline images."`
	Prompt string `json:"prompt" description:"What to extract or describe. Be specific (e.g. 'summarise the key events in this video', 'extract all text', 'describe the chart axes and trend')."`
}

func (t *MediaAnalyzeTool) ParametersSchema() tools.ToolSchema {
	return tools.SchemaFor[mediaAnalyzeArgs]()
}

// RequiresConfirmation is false — analysis is read-only.
func (t *MediaAnalyzeTool) RequiresConfirmation() bool { return false }

func (t *MediaAnalyzeTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args mediaAnalyzeArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("tools: invalid arguments: %w", err)
	}
	args.Media = strings.TrimSpace(args.Media)
	args.Prompt = strings.TrimSpace(args.Prompt)
	if args.Media == "" {
		return "", fmt.Errorf("tools: media is required")
	}
	if args.Prompt == "" {
		return "", fmt.Errorf("tools: prompt is required")
	}
	if t.analyzer == nil {
		return "", fmt.Errorf("tools: media_analyze has no analyzer configured")
	}
	return t.analyzer.Analyze(ctx, args.Media, args.Prompt)
}
