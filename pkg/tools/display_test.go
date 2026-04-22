package tools

import "testing"

func TestDefaultDisplay_PopulatesLabelAndDescription(t *testing.T) {
	got := DefaultDisplay("find_game", "Search the game catalog by title or id")
	if got.Label != "find_game" {
		t.Errorf("Label = %q, want %q", got.Label, "find_game")
	}
	if got.Description != "Search the game catalog by title or id" {
		t.Errorf("Description = %q", got.Description)
	}
	if got.Category != "" || got.IconHint != "" {
		t.Errorf("Category/IconHint should be zero-valued when DefaultDisplay was used; got %q / %q", got.Category, got.IconHint)
	}
}
