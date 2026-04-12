package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// ShowMediaTool instructs the frontend to render an image or video inline in the chat.
// It returns a markdown/HTML snippet that the frontend renders natively.
type ShowMediaTool struct{}

func NewShowMediaTool() *ShowMediaTool { return &ShowMediaTool{} }

func (t *ShowMediaTool) Name() string { return "show_media" }

func (t *ShowMediaTool) Description() string {
	return "Display an image or video inline in the chat. Use this when the user asks to show, display, or view an image/video from a URL. Returns a renderable media embed for the frontend."
}

func (t *ShowMediaTool) ParametersSchema() tools.ToolSchema {
	return tools.ToolSchema{
		Type: "object",
		Properties: map[string]interface{}{
			"url": map[string]interface{}{
				"type":        "string",
				"description": "Direct HTTP/HTTPS URL to the image or video file.",
			},
			"media_type": map[string]interface{}{
				"type":        "string",
				"enum":        []string{"image", "video"},
				"description": "Type of media: 'image' for pictures/GIFs, 'video' for MP4/WebM/etc.",
			},
			"caption": map[string]interface{}{
				"type":        "string",
				"description": "Optional caption or alt-text to display with the media.",
			},
		},
		Required: []string{"url", "media_type"},
	}
}

func (t *ShowMediaTool) RequiresConfirmation() bool { return false }
func (t *ShowMediaTool) InlineResult() bool          { return true }

func (t *ShowMediaTool) Execute(_ context.Context, argsJSON string) (string, error) {
	var args struct {
		URL       string `json:"url"`
		MediaType string `json:"media_type"`
		Caption   string `json:"caption"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	url := strings.TrimSpace(args.URL)
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("url must start with http:// or https://")
	}

	caption := args.Caption
	if caption == "" {
		caption = "media"
	}

	switch args.MediaType {
	case "image":
		// Standard markdown image — rendered by marked.js as <img>
		return fmt.Sprintf("![%s](%s)", caption, url), nil
	case "video":
		// Raw HTML video block — marked.js passes HTML through, DOMPurify allows <video>
		return fmt.Sprintf(
			`<video controls preload="metadata" style="max-width:100%%;border-radius:8px;margin:6px 0"><source src="%s">%s</video>`,
			url, caption,
		), nil
	default:
		return "", fmt.Errorf("unsupported media_type %q: must be 'image' or 'video'", args.MediaType)
	}
}
