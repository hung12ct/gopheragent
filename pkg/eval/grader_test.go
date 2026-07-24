package eval

import (
	"context"
	"testing"
)

func trWith(answer string) *Transcript {
	return &Transcript{FinalAnswer: answer, TerminatedBy: "done"}
}

func TestContains(t *testing.T) {
	g := Contains("Tokyo", "21")
	if grade := g.Grade(context.Background(), trWith("It is 21° in Tokyo.")); !grade.Pass {
		t.Fatalf("expected pass, got %+v", grade)
	}
	if grade := g.Grade(context.Background(), trWith("It is 21° in Osaka.")); grade.Pass {
		t.Fatalf("expected fail on missing substring")
	}
}

func TestContainsFold(t *testing.T) {
	g := ContainsFold("tokyo")
	if grade := g.Grade(context.Background(), trWith("Weather in TOKYO")); !grade.Pass {
		t.Fatalf("fold match failed: %+v", grade)
	}
}

func TestRegexp(t *testing.T) {
	g := Regexp(`\d+\s*°`)
	if grade := g.Grade(context.Background(), trWith("21 ° today")); !grade.Pass {
		t.Fatalf("regex should match: %+v", grade)
	}
	if grade := g.Grade(context.Background(), trWith("no temperature")); grade.Pass {
		t.Fatalf("regex should not match")
	}
}

func TestExactTrims(t *testing.T) {
	g := Exact("Tokyo")
	if grade := g.Grade(context.Background(), trWith("  Tokyo\n")); !grade.Pass {
		t.Fatalf("exact should trim and match: %+v", grade)
	}
	if grade := g.Grade(context.Background(), trWith("Tokyo, Japan")); grade.Pass {
		t.Fatalf("exact should not match superset")
	}
}

func TestNoError(t *testing.T) {
	g := NoError()
	if grade := g.Grade(context.Background(), &Transcript{TerminatedBy: "done"}); !grade.Pass {
		t.Fatalf("clean run should pass no_error")
	}
	if grade := g.Grade(context.Background(), &Transcript{TerminatedBy: "max_iters"}); grade.Pass {
		t.Fatalf("max_iters should fail no_error")
	}
}

func TestAllAndAny(t *testing.T) {
	ctx := context.Background()
	both := All(Contains("a"), Contains("b"))
	if grade := both.Grade(ctx, trWith("a and b")); !grade.Pass {
		t.Fatalf("All should pass when both match")
	}
	if grade := both.Grade(ctx, trWith("only a")); grade.Pass {
		t.Fatalf("All should fail when one misses")
	}
	either := Any(Contains("x"), Contains("b"))
	if grade := either.Grade(ctx, trWith("has b")); !grade.Pass {
		t.Fatalf("Any should pass when one matches")
	}
	if grade := either.Grade(ctx, trWith("neither here")); grade.Pass {
		t.Fatalf("Any should fail when none match")
	}
}

func TestGraderFuncDefaultsName(t *testing.T) {
	g := GraderFunc("custom", func(_ context.Context, _ *Transcript) Grade {
		return Grade{Pass: true, Score: 1}
	})
	grade := g.Grade(context.Background(), trWith(""))
	if grade.Grader != "custom" {
		t.Fatalf("grader name not defaulted: %q", grade.Grader)
	}
}
