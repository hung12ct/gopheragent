package main

import (
	"strings"
	"testing"
)

func TestExtractPlanBullets_StandardDashList(t *testing.T) {
	plan := `Here is my plan:
- Search for benchmark data
- Read the top 3 sources
- Render the comparison table`
	got := extractPlanBullets(plan)
	if len(got) != 3 {
		t.Fatalf("got %d bullets, want 3: %+v", len(got), got)
	}
	if got[0].Title != "Search for benchmark data" || got[2].Title != "Render the comparison table" {
		t.Fatalf("unexpected titles: %+v", got)
	}
	for _, b := range got {
		if b.Notes != "" {
			t.Fatalf("no sub-bullets expected, got notes=%q", b.Notes)
		}
	}
}

func TestExtractPlanBullets_MixedMarkers(t *testing.T) {
	plan := `* one
- two
• three
1. four`
	got := extractPlanBullets(plan)
	if len(got) != 4 {
		t.Fatalf("got %d, want 4: %+v", len(got), got)
	}
	for i, want := range []string{"one", "two", "three", "four"} {
		if got[i].Title != want {
			t.Fatalf("bullet[%d].Title=%q, want %q", i, got[i].Title, want)
		}
	}
}

func TestExtractPlanBullets_IndentedSubBulletsFoldIntoNotes(t *testing.T) {
	plan := `- Research GoFiber, Gin, and Echo
  - Latest stable releases
  - GitHub stars and active issues
- Render comparison table
- Cite each source`
	got := extractPlanBullets(plan)
	if len(got) != 3 {
		t.Fatalf("got %d, want 3 top-level: %+v", len(got), got)
	}
	if got[0].Title != "Research GoFiber, Gin, and Echo" {
		t.Fatalf("first title wrong: %+v", got[0])
	}
	if !strings.Contains(got[0].Notes, "Latest stable releases") || !strings.Contains(got[0].Notes, "GitHub stars and active issues") {
		t.Fatalf("sub-bullets did not fold into notes: %q", got[0].Notes)
	}
	if got[1].Notes != "" || got[2].Notes != "" {
		t.Fatalf("only first task should have notes, got %+v", got)
	}
}

func TestExtractPlanBullets_IgnoresHeadersAndProse(t *testing.T) {
	plan := `# Plan

I'll proceed in three steps.

- Step one
- Step two

Then I'll wrap up.`
	got := extractPlanBullets(plan)
	if len(got) != 2 {
		t.Fatalf("expected 2 bullets, got %+v", got)
	}
}

func TestExtractPlanBullets_EmptyOrNoBullets(t *testing.T) {
	cases := []string{
		"",
		"Just prose, no bullets at all.",
		"# Header only",
	}
	for _, c := range cases {
		if got := extractPlanBullets(c); len(got) != 0 {
			t.Fatalf("expected zero bullets for %q, got %+v", c, got)
		}
	}
}
