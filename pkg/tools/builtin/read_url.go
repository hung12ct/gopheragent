package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// Pre-compiled regexps — compiled once at startup, not per-request.
var (
	reScript = regexp.MustCompile(`(?is)<script.*?>.*?</script>`)
	reStyle  = regexp.MustCompile(`(?is)<style.*?>.*?</style>`)
	reHead   = regexp.MustCompile(`(?is)<head.*?>.*?</head>`)
	reTag    = regexp.MustCompile(`(?is)<.*?>`)
)

// ReadURLTool fetches the HTML content of a given URL and strips tags
// to provide raw text for the LLM to summarize or analyze.
type ReadURLTool struct {
	client *http.Client
}

// NewReadURLTool creates a new built-in tool that can scrape simple static webpages.
// SSRF protection is always enabled: private, loopback, and link-local addresses
// (including 169.254.169.254 cloud metadata endpoints) are blocked.
func NewReadURLTool() *ReadURLTool {
	return &ReadURLTool{
		client: &http.Client{
			Timeout:   10 * time.Second,
			Transport: newSSRFSafeTransport(nil),
		},
	}
}

func (t *ReadURLTool) Name() string {
	return "read_url"
}

func (t *ReadURLTool) Description() string {
	return "Fetch raw text content from a public webpage URL. Use this when the user asks to summarize a specific link or read an article."
}

type readURLArgs struct {
	URL string `json:"url" description:"The exact valid HTTP/HTTPS URL of the webpage to scrape."`
}

func (t *ReadURLTool) ParametersSchema() tools.ToolSchema {
	return tools.SchemaFor[readURLArgs]()
}

func (t *ReadURLTool) RequiresConfirmation() bool {
	return false
}

func (t *ReadURLTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args readURLArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("tools: read_url: invalid arguments: %w", err)
	}

	url := strings.TrimSpace(args.URL)
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "https://" + url
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("tools: read_url: build request: %w", err)
	}

	// Masquerade to avoid basic bot blockers
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("tools: read_url: fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tools: read_url: server returned status %d", resp.StatusCode)
	}

	// Limit to reading max 2MB of text to prevent agent context window explosion
	const maxReadBytes = 2 * 1024 * 1024
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxReadBytes))
	if err != nil {
		return "", fmt.Errorf("tools: read_url: read body: %w", err)
	}

	htmlContent := string(bodyBytes)

	// Naive HTML text extraction (zero-dependency approach).
	htmlContent = reScript.ReplaceAllString(htmlContent, "")
	htmlContent = reStyle.ReplaceAllString(htmlContent, "")
	htmlContent = reHead.ReplaceAllString(htmlContent, "")
	htmlContent = reTag.ReplaceAllString(htmlContent, " ")

	// 3. Clean up excessive whitespaces & blank lines
	lines := strings.Split(htmlContent, "\n")
	var finalLines []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if len(l) > 0 {
			finalLines = append(finalLines, l)
		}
	}

	textResult := strings.Join(finalLines, "\n")
	
	// Soft cutoff at 15000 runes (~3-4k tokens) — rune-safe to avoid corrupting multi-byte UTF-8.
	const maxRunes = 15000
	if utf8.RuneCountInString(textResult) > maxRunes {
		runes := []rune(textResult)
		textResult = string(runes[:maxRunes]) + "\n\n...[Content truncated for length]..."
	}

	if textResult == "" {
		return "Failed to parse meaningful text from the page (it might be a heavy JS-rendered SPA app).", nil
	}

	return fmt.Sprintf("Source URL: %s\n\nContent:\n%s", url, textResult), nil
}
