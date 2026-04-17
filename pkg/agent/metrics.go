package agent

import (
	"bytes"
	"fmt"
	"maps"
	"net/http"
	"sort"
	"strings"
)

// Snapshot returns a copy of the per-session token usage recorded by the
// tracker. The returned map is safe for the caller to read and mutate; it
// reflects the state at the moment of the call.
func (bt *BudgetTracker) Snapshot() map[string]TokenUsage {
	bt.mu.RLock()
	defer bt.mu.RUnlock()
	out := make(map[string]TokenUsage, len(bt.usage))
	maps.Copy(out, bt.usage)
	return out
}

// MetricsHandler returns an http.Handler that exposes the tracker's per-session
// token usage in OpenMetrics text format. The output is also valid Prometheus
// text format, so any Prometheus-compatible scraper (including Grafana Agent,
// Grafana Cloud, VictoriaMetrics, etc.) can ingest it directly:
//
//	http.Handle("/metrics", bt.MetricsHandler())
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
// tracker is used with short-lived per-request keys, call Reset when a session
// ends so the exporter does not carry a growing label set.
func (bt *BudgetTracker) MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/openmetrics-text; version=1.0.0; charset=utf-8")
		_, _ = w.Write(bt.renderOpenMetrics())
	})
}

// renderOpenMetrics produces the scrape body. Kept separate from the handler
// so tests can assert on the exact text without spinning up an HTTP server.
func (bt *BudgetTracker) renderOpenMetrics() []byte {
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
		func(u TokenUsage) int { return u.PromptTokens },
	)
	writeFamily(&buf,
		"gopheragent_session_completion_tokens_total",
		"counter",
		"Cumulative completion tokens produced for the session.",
		keys, snap,
		func(u TokenUsage) int { return u.CompletionTokens },
	)
	writeFamily(&buf,
		"gopheragent_session_tokens_total",
		"counter",
		"Cumulative total tokens (prompt + completion) accounted against the session budget.",
		keys, snap,
		func(u TokenUsage) int { return u.TotalTokens },
	)
	writeFamily(&buf,
		"gopheragent_session_budget_tokens",
		"gauge",
		"Configured token budget for the session (0 means enforcement is disabled).",
		keys, snap,
		func(_ TokenUsage) int { return budget },
	)

	if budget > 0 {
		writeFamily(&buf,
			"gopheragent_session_budget_remaining_tokens",
			"gauge",
			"Tokens remaining before the budget is exhausted (negative when over).",
			keys, snap,
			func(u TokenUsage) int { return budget - u.TotalTokens },
		)
	}

	buf.WriteString("# EOF\n")
	return buf.Bytes()
}

func writeFamily(
	buf *bytes.Buffer,
	name, metricType, help string,
	keys []string, snap map[string]TokenUsage,
	value func(TokenUsage) int,
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
