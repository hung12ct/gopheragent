package builtin

import (
	"context"
	"strings"
	"testing"
)

func TestIntrospectPostgresSchema_RejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name    string
		schema  string
		tables  []string
		wantSub string
	}{
		{"nil db with valid args", "public", []string{"t"}, "nil db"},
		{"empty tables", "public", nil, "at least one table"},
		{"bad schema name with semicolon", "public; DROP", []string{"t"}, "invalid Postgres schema name"},
		{"bad schema name with quote", "pub'lic", []string{"t"}, "invalid Postgres schema name"},
		{"bad table name with semicolon", "public", []string{"t; DROP"}, "invalid Postgres table name"},
		{"bad table name with double quote", "public", []string{`"t"`}, "invalid Postgres table name"},
		{"bad table name leading digit", "public", []string{"1t"}, "invalid Postgres table name"},
		{"bad table name with space", "public", []string{"my table"}, "invalid Postgres table name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := IntrospectPostgresSchema(context.Background(), nil, tc.schema, tc.tables...)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("expected error containing %q, got: %v", tc.wantSub, err)
			}
		})
	}
}
