package builtin

import "testing"

func resultAB12() *SQLResult {
	return &SQLResult{
		SQL:     "SELECT a, b FROM t",
		Columns: []string{"a", "b"},
		Rows: []map[string]any{
			{"a": float64(1), "b": float64(2)},
		},
	}
}

func resultAB12DifferentSQL() *SQLResult {
	// Same rows, different SQL — should hash identically because hashResult
	// intentionally ignores the SQL text (two equivalent queries must cluster).
	r := resultAB12()
	r.SQL = "SELECT a, b FROM t WHERE 1=1"
	return r
}

func resultDifferent() *SQLResult {
	return &SQLResult{
		SQL:     "SELECT a, b FROM t",
		Columns: []string{"a", "b"},
		Rows: []map[string]any{
			{"a": float64(99), "b": float64(100)},
		},
	}
}

func TestHashResult_EquivalentResultsMatch(t *testing.T) {
	if hashResult(resultAB12()) != hashResult(resultAB12DifferentSQL()) {
		t.Fatal("identical rows must hash identically regardless of SQL text")
	}
}

func TestHashResult_DifferentRowsDifferentHash(t *testing.T) {
	if hashResult(resultAB12()) == hashResult(resultDifferent()) {
		t.Fatal("different rows must produce different hashes")
	}
}

func TestHashResult_RowOrderInsensitive(t *testing.T) {
	a := &SQLResult{Columns: []string{"x"}, Rows: []map[string]any{{"x": float64(1)}, {"x": float64(2)}}}
	b := &SQLResult{Columns: []string{"x"}, Rows: []map[string]any{{"x": float64(2)}, {"x": float64(1)}}}
	if hashResult(a) != hashResult(b) {
		t.Fatal("row order should not affect hash (sort before hashing)")
	}
}

func TestPickByMajority_LargestCluster(t *testing.T) {
	cands := []sqlCandidate{
		{index: 0, finalResp: "wrong1", lastResult: resultDifferent()},
		{index: 1, finalResp: "right1", lastResult: resultAB12()},
		{index: 2, finalResp: "right2", lastResult: resultAB12DifferentSQL()},
	}
	got := pickByMajority(cands)
	if got == nil {
		t.Fatal("expected a winner")
	}
	if got.index != 1 {
		t.Fatalf("expected candidate 1 (first of winning cluster), got %d (%q)", got.index, got.finalResp)
	}
}

func TestPickByMajority_AllErrorsReturnsFirst(t *testing.T) {
	cands := []sqlCandidate{
		{index: 0, finalResp: "err0"},
		{index: 1, finalResp: "err1"},
	}
	got := pickByMajority(cands)
	if got == nil || got.index != 0 {
		t.Fatalf("expected fallback to index 0 when all error, got %+v", got)
	}
}

func TestPickByMajority_TieBreakFirstSeenCluster(t *testing.T) {
	// Two singleton clusters: ties resolve to the first-seen cluster
	// (candidate index 0).
	cands := []sqlCandidate{
		{index: 0, finalResp: "a", lastResult: resultAB12()},
		{index: 1, finalResp: "b", lastResult: resultDifferent()},
	}
	got := pickByMajority(cands)
	if got == nil || got.index != 0 {
		t.Fatalf("expected index 0 on tie, got %+v", got)
	}
}

func TestPickByMajority_SkipsErroredCandidates(t *testing.T) {
	erroredResult := &SQLResult{SQL: "bad", Error: "syntax error"}
	cands := []sqlCandidate{
		{index: 0, finalResp: "err", lastResult: erroredResult},
		{index: 1, finalResp: "good", lastResult: resultAB12()},
	}
	got := pickByMajority(cands)
	if got == nil || got.index != 1 {
		t.Fatalf("errored results must be excluded from clustering, got %+v", got)
	}
}

func TestPickByMajority_Empty(t *testing.T) {
	if got := pickByMajority(nil); got != nil {
		t.Fatalf("empty input should return nil, got %+v", got)
	}
}
