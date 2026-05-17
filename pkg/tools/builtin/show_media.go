package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// mediaExtensions enumerates the URL-path suffixes show_media will accept.
// A webpage URL (e.g. .../humans-in-space/article/) has no match here and
// is rejected — that forces the caller to read_url the page and extract a
// real media URL from the body, instead of rendering a broken <img>.
var mediaExtensions = map[string]struct{}{
	".jpg": {}, ".jpeg": {}, ".png": {}, ".gif": {}, ".webp": {},
	".svg": {}, ".bmp": {}, ".avif": {},
	".mp4": {}, ".webm": {}, ".mov": {}, ".ogg": {}, ".ogv": {},
}

// ShowMediaTool instructs the frontend to render an image or video inline in the chat.
// It returns a markdown/HTML snippet that the frontend renders natively.
type ShowMediaTool struct{}

func NewShowMediaTool() *ShowMediaTool { return &ShowMediaTool{} }

const showMediaName = "show_media"
const showMediaDescription = "Display an image or video inline in the chat. Use this when the user asks to show, display, or view an image/video from a URL. Returns a renderable media embed for the frontend."

type showMediaArgs struct {
	URL       string `json:"url"                description:"Direct HTTP/HTTPS URL to the image or video file."`
	MediaType string `json:"media_type"         description:"Type of media: 'image' for pictures/GIFs, 'video' for MP4/WebM/etc." enum:"image,video"`
	Caption   string `json:"caption,omitempty"  description:"Optional caption or alt-text to display with the media."`
}

func (t *ShowMediaTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:        showMediaName,
		Description: showMediaDescription,
		Parameters:  tools.SchemaFor[showMediaArgs](),
		Inline:      true,
		Display:     tools.DefaultDisplay(showMediaName, showMediaDescription),
	}
}

func (t *ShowMediaTool) Execute(_ context.Context, argsJSON string) (tools.Result, error) {
	var args showMediaArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return tools.Result{}, fmt.Errorf("invalid arguments: %w", err)
	}

	url := strings.TrimSpace(args.URL)
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return tools.Result{}, fmt.Errorf("url must start with http:// or https://")
	}

	// Strip query / fragment before checking the extension so URLs like
	// https://cdn.example.com/photo.jpg?w=800 still pass.
	pathOnly := url
	if i := strings.IndexAny(pathOnly, "?#"); i >= 0 {
		pathOnly = pathOnly[:i]
	}
	if _, ok := mediaExtensions[strings.ToLower(path.Ext(pathOnly))]; !ok {
		return tools.Result{}, fmt.Errorf(
			"show_media requires a direct image/video URL (path ending in .jpg/.png/.gif/.webp/.svg/.mp4/.webm/etc.); got %q. "+
				"If this looks like a webpage, call read_url on it first and pick a direct media URL from the page body",
			url,
		)
	}

	caption := args.Caption
	if caption == "" {
		caption = "media"
	}

	switch args.MediaType {
	case "image":
		// Standard markdown image — rendered by marked.js as <img>
		return tools.Text(fmt.Sprintf("![%s](%s)", caption, url)), nil
	case "video":
		// Raw HTML video block — marked.js passes HTML through, DOMPurify allows <video>
		return tools.Text(fmt.Sprintf(
			`<video controls preload="metadata" style="max-width:100%%;border-radius:8px;margin:6px 0"><source src="%s">%s</video>`,
			url, caption,
		)), nil
	default:
		return tools.Result{}, fmt.Errorf("unsupported media_type %q: must be 'image' or 'video'", args.MediaType)
	}
}
