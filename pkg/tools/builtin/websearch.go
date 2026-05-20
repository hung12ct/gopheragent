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

const webSearchName = "web_search"
const webSearchDescription = "Perform a web search to find current information, news, or general knowledge using Tavily API."

type webSearchArgs struct {
	Query string `json:"query"                    description:"The search query to look up on the internet (e.g. 'latest news about AI')."`
	Topic string `json:"topic,omitempty"          description:"Search category. Use 'news' for recent events, headlines, or time-sensitive queries. Default: 'general'." enum:"general,news"`
	Days  int    `json:"days,omitempty"           description:"Only return results from the last N days. Use 1 for 'today', 7 for 'this week'. Only applies when topic is 'news'. Default: 3."`
}

// RegisterWebSearch registers a Tavily-backed web search tool. apiKey
// is required; pass "" to fall back to the TAVILY_API_KEY environment
// variable. Returns an error when no key is resolvable so adopters
// catch misconfiguration at startup instead of on the first call.
//
// The http.Client is built once per registration and shared across
// every call — typical Go HTTP-client lifecycle.
func RegisterWebSearch(reg tools.Registerer, apiKey string) error {
	if apiKey == "" {
		apiKey = os.Getenv("TAVILY_API_KEY")
	}
	if apiKey == "" {
		return fmt.Errorf("tools: web_search: TAVILY_API_KEY is missing")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	tools.RegisterFunc(reg, webSearchName, webSearchDescription,
		func(ctx context.Context, args webSearchArgs) (tools.Result, error) {
			return executeWebSearch(ctx, client, apiKey, args)
		})
	return nil
}

// executeWebSearch is the typed-arg body extracted so the
// RegisterWebSearch closure carries only the bare-minimum captures
// (client, apiKey) and the HTTP + formatting logic stays
// independently testable.
func executeWebSearch(ctx context.Context, client *http.Client, apiKey string, args webSearchArgs) (tools.Result, error) {
	if args.Topic == "" {
		args.Topic = "general"
	}

	reqBody := map[string]any{
		"api_key":        apiKey,
		"query":          args.Query,
		"search_depth":   "basic",
		"include_answer": true,
		"max_results":    5,
		"topic":          args.Topic,
	}
	if args.Topic == "news" {
		if args.Days <= 0 {
			args.Days = 3
		}
		reqBody["days"] = args.Days
	}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return tools.Result{}, fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.tavily.com/search", bytes.NewBuffer(jsonData))
	if err != nil {
		return tools.Result{}, fmt.Errorf("failed to create http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return tools.Result{}, fmt.Errorf("search API error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return tools.Result{}, fmt.Errorf("search API returned status %d: %s", resp.StatusCode, string(bodyBytes))
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
		return tools.Result{}, fmt.Errorf("failed to decode response: %w", err)
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
		return tools.Text("No results found for the query."), nil
	}

	return tools.Text(formattedResult), nil
}
