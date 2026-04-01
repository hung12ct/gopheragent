package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// WebSearchTool uses the Tavily API to perform web searches and return answers.
type WebSearchTool struct {
	apiKey string
}

// NewWebSearchTool creates a new web search tool. If apiKey is empty, it attempts
// to load it from the TAVILY_API_KEY environment variable.
func NewWebSearchTool(apiKey string) (*WebSearchTool, error) {
	if apiKey == "" {
		apiKey = os.Getenv("TAVILY_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("TAVILY_API_KEY environment variable is missing")
	}
	return &WebSearchTool{apiKey: apiKey}, nil
}

func (t *WebSearchTool) Name() string {
	return "web_search"
}

func (t *WebSearchTool) Description() string {
	return "Perform a web search to find current information, news, or general knowledge using Tavily API."
}

func (t *WebSearchTool) ParametersSchema() tools.ToolSchema {
	return tools.ToolSchema{
		Type: "object",
		Properties: map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "The search query to look up on the internet (e.g. 'latest news about AI').",
			},
		},
		Required: []string{"query"},
	}
}

func (t *WebSearchTool) RequiresConfirmation() bool {
	return false
}

func (t *WebSearchTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	reqBody := map[string]interface{}{
		"api_key":        t.apiKey,
		"query":          args.Query,
		"search_depth":   "basic",
		"include_answer": true,
		"max_results":    3,
	}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.tavily.com/search", bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf("failed to create http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("search API error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("search API returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var tavilyResp struct {
		Answer  string `json:"answer"`
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&tavilyResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	var formattedResult string
	if tavilyResp.Answer != "" {
		formattedResult += fmt.Sprintf("Summary: %s\n\n", tavilyResp.Answer)
	}

	formattedResult += "Top sources:\n"
	for idx, r := range tavilyResp.Results {
		formattedResult += fmt.Sprintf("[%d] %s (%s):\n%s\n\n", idx+1, r.Title, r.URL, r.Content)
	}

	if formattedResult == "Top sources:\n" {
		return "No results found for the query.", nil
	}

	return formattedResult, nil
}
