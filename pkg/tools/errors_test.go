package tools

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassifyError_MatchesEachSentinel(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		want  error
	}{
		{"user", fmt.Errorf("bad input: %w", ErrUser), ErrUser},
		{"transient", fmt.Errorf("429: %w", ErrTransient), ErrTransient},
		{"permanent", fmt.Errorf("403: %w", ErrPermanent), ErrPermanent},
		{"nil", nil, nil},
		{"uncategorized", errors.New("random"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyError(tc.err)
			if got != tc.want {
				t.Errorf("ClassifyError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestErrorSentinels_UnwrapChain(t *testing.T) {
	inner := errors.New("connection refused")
	wrapped := fmt.Errorf("rpc failed: %w (classified %w)", inner, ErrTransient)

	// Is should reach both the inner cause and the classification sentinel.
	if !errors.Is(wrapped, inner) {
		t.Error("expected errors.Is to find inner cause")
	}
	if !errors.Is(wrapped, ErrTransient) {
		t.Error("expected errors.Is to find ErrTransient")
	}
	if ClassifyError(wrapped) != ErrTransient {
		t.Errorf("ClassifyError didn't pick ErrTransient out of a multi-wrap, got %v", ClassifyError(wrapped))
	}
}

func TestErrorSentinels_AreDistinct(t *testing.T) {
	if errors.Is(ErrUser, ErrTransient) || errors.Is(ErrTransient, ErrPermanent) || errors.Is(ErrPermanent, ErrUser) {
		t.Fatal("sentinels must be pairwise distinct")
	}
}
