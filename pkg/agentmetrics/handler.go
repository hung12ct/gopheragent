// Package agentmetrics exposes a Prometheus / OpenMetrics HTTP handler for a
// gopheragent BudgetTracker. It lives outside pkg/agent so applications that
// embed the agent loop in a CLI or batch tool do not transitively import
// net/http.
//
// Usage:
//
//	bt := agent.NewBudgetTracker(100_000)
//	loop.OnEvent(bt.Handler())
//	http.Handle("/metrics", agentmetrics.Handler(bt))
//
// The output is valid OpenMetrics 1.0 (and therefore Prometheus text format),
// so any Prometheus-compatible scraper can ingest it directly.
package agentmetrics

import (
	"bytes"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/agent"
)

// Handler returns an http.Handler that renders the tracker's per-session token
// usage in OpenMetrics text format.
//
// Metrics emitted:
//
//   - gopheragent_session_prompt_tokens_total{session_key}     (counter)
//   - gopheragent_session_completion_tokens_total{session_key} (counter)
//   - gopheragent_session_tokens_total{session_key}            (counter, sum of the two above)
//   - gopheragent_session_budget_tokens{session_key}           (gauge, the configured ceiling)
//   - gopheragent_session_budget_remaining_tokens{session_key} (gauge, budget - total; omitted when Budget == 0)
//
// Cardinality note: every distinct session_key produces its own series. If the
// tracker is used with short-lived per-request keys, the caller should reset
// or evict entries when a session ends so the exporter does not carry a
// growing label set.
func Handler(bt *agent.BudgetTracker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/openmetrics-text; version=1.0.0; charset=utf-8")
		_, _ = w.Write(render(bt))
	})
}

// render produces the scrape body. Kept separate from Handler so tests can
// assert on the exact text without spinning up an HTTP server.
func render(bt *agent.BudgetTracker) []byte {
	snap := bt.Snapshot()
	budget := bt.Budget

	// Sort session keys for deterministic output — makes tests and diffs sane.
	keys := make([]string, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf bytes.Buffer

	writeFamily(&buf,
		"gopheragent_session_prompt_tokens_total",
		"counter",
		"Cumulative prompt tokens consumed by the session.",
		keys, snap,
		func(u agent.TokenUsage) int { return u.PromptTokens },
	)
	writeFamily(&buf,
		"gopheragent_session_completion_tokens_total",
		"counter",
		"Cumulative completion tokens produced for the session.",
		keys, snap,
		func(u agent.TokenUsage) int { return u.CompletionTokens },
	)
	writeFamily(&buf,
		"gopheragent_session_tokens_total",
		"counter",
		"Cumulative total tokens (prompt + completion) accounted against the session budget.",
		keys, snap,
		func(u agent.TokenUsage) int { return u.TotalTokens },
	)
	writeFamily(&buf,
		"gopheragent_session_budget_tokens",
		"gauge",
		"Configured token budget for the session (0 means enforcement is disabled).",
		keys, snap,
		func(_ agent.TokenUsage) int { return budget },
	)

	if budget > 0 {
		writeFamily(&buf,
			"gopheragent_session_budget_remaining_tokens",
			"gauge",
			"Tokens remaining before the budget is exhausted (negative when over).",
			keys, snap,
			func(u agent.TokenUsage) int { return budget - u.TotalTokens },
		)
	}

	buf.WriteString("# EOF\n")
	return buf.Bytes()
}

func writeFamily(
	buf *bytes.Buffer,
	name, metricType, help string,
	keys []string, snap map[string]agent.TokenUsage,
	value func(agent.TokenUsage) int,
) {
	fmt.Fprintf(buf, "# HELP %s %s\n", name, help)
	fmt.Fprintf(buf, "# TYPE %s %s\n", name, metricType)
	for _, k := range keys {
		fmt.Fprintf(buf, "%s{session_key=\"%s\"} %d\n", name, escapeLabel(k), value(snap[k]))
	}
}

// escapeLabel escapes label values per the OpenMetrics / Prometheus text spec:
// backslash, double quote, and newline. Carriage returns are turned into \n as
// well since they are equally disallowed unescaped.
var labelEscaper = strings.NewReplacer(
	`\`, `\\`,
	`"`, `\"`,
	"\n", `\n`,
	"\r", `\n`,
)

func escapeLabel(v string) string { return labelEscaper.Replace(v) }
