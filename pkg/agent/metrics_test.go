package agent

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBudgetTracker_Snapshot_IsDetachedCopy(t *testing.T) {
	bt := NewBudgetTracker(0)
	bt.Handler()(context.Background(), "s1", usageEvent(t, 100, 50, 150))

	snap := bt.Snapshot()
	// Mutating the snapshot must not affect the tracker.
	snap["s1"] = TokenUsage{TotalTokens: 9999}
	snap["new"] = TokenUsage{TotalTokens: 42}

	live := bt.Usage("s1")
	if live.TotalTokens != 150 {
		t.Fatalf("tracker state leaked through snapshot: got %+v", live)
	}
	if bt.Usage("new").TotalTokens != 0 {
		t.Fatalf("snapshot write leaked into tracker")
	}
}

func TestBudgetTracker_MetricsHandler_EmitsUsageAndBudget(t *testing.T) {
	bt := NewBudgetTracker(1000)
	h := bt.Handler()
	h(context.Background(), "alpha", usageEvent(t, 300, 200, 500))
	h(context.Background(), "beta", usageEvent(t, 100, 50, 150))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	bt.MetricsHandler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/openmetrics-text") {
		t.Fatalf("unexpected Content-Type: %q", ct)
	}

	body := rec.Body.String()
	mustContain(t, body,
		`# TYPE gopheragent_session_prompt_tokens_total counter`,
		`gopheragent_session_prompt_tokens_total{session_key="alpha"} 300`,
		`gopheragent_session_completion_tokens_total{session_key="beta"} 50`,
		`gopheragent_session_tokens_total{session_key="alpha"} 500`,
		`gopheragent_session_budget_tokens{session_key="alpha"} 1000`,
		`gopheragent_session_budget_remaining_tokens{session_key="alpha"} 500`,
		`gopheragent_session_budget_remaining_tokens{session_key="beta"} 850`,
	)
	if !strings.HasSuffix(body, "# EOF\n") {
		t.Fatalf("OpenMetrics output must end with '# EOF\\n', got tail: %q", tail(body, 40))
	}
}

func TestBudgetTracker_MetricsHandler_OmitsRemainingWhenBudgetZero(t *testing.T) {
	bt := NewBudgetTracker(0) // track-only, enforcement disabled
	bt.Handler()(context.Background(), "s", usageEvent(t, 10, 20, 30))

	body := string(bt.renderOpenMetrics())

	// Budget gauge still emitted (as 0), but the "remaining" family must be omitted
	// — there is no meaningful remaining value when enforcement is disabled.
	if !strings.Contains(body, `gopheragent_session_budget_tokens{session_key="s"} 0`) {
		t.Fatalf("expected budget gauge with value 0, got:\n%s", body)
	}
	if strings.Contains(body, "gopheragent_session_budget_remaining_tokens") {
		t.Fatalf("remaining gauge must be omitted when Budget==0, got:\n%s", body)
	}
}

func TestBudgetTracker_MetricsHandler_EscapesLabelValues(t *testing.T) {
	bt := NewBudgetTracker(100)
	bt.Handler()(context.Background(), `weird"key\with`+"\nnewline", usageEvent(t, 1, 1, 2))

	body := string(bt.renderOpenMetrics())
	// Backslash, quote, and newline must each be escaped per the spec.
	if !strings.Contains(body, `session_key="weird\"key\\with\nnewline"`) {
		t.Fatalf("label escaping failed, body was:\n%s", body)
	}
}

func TestBudgetTracker_MetricsHandler_EmptyTrackerStillValid(t *testing.T) {
	bt := NewBudgetTracker(0)
	body := string(bt.renderOpenMetrics())
	// HELP/TYPE lines must still appear even with no series, and # EOF must close it.
	mustContain(t, body,
		"# TYPE gopheragent_session_prompt_tokens_total counter",
		"# TYPE gopheragent_session_tokens_total counter",
	)
	if !strings.HasSuffix(body, "# EOF\n") {
		t.Fatalf("empty output must still terminate with # EOF")
	}
}

func TestBudgetTracker_MetricsHandler_DeterministicOrdering(t *testing.T) {
	bt := NewBudgetTracker(100)
	h := bt.Handler()
	h(context.Background(), "c", usageEvent(t, 1, 1, 2))
	h(context.Background(), "a", usageEvent(t, 1, 1, 2))
	h(context.Background(), "b", usageEvent(t, 1, 1, 2))

	body := string(bt.renderOpenMetrics())
	// Inside the "prompt tokens" family the sessions must appear sorted: a, b, c.
	a := strings.Index(body, `gopheragent_session_prompt_tokens_total{session_key="a"}`)
	b := strings.Index(body, `gopheragent_session_prompt_tokens_total{session_key="b"}`)
	c := strings.Index(body, `gopheragent_session_prompt_tokens_total{session_key="c"}`)
	if !(a < b && b < c) {
		t.Fatalf("session keys must be sorted; got indices a=%d b=%d c=%d", a, b, c)
	}
}

func mustContain(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Fatalf("output missing expected line %q\n--- body ---\n%s", w, body)
		}
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
