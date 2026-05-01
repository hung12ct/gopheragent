package builtin

import (
	"context"
	"strings"
	"testing"
)

func TestIntrospectMySQLSchema_RejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name    string
		schema  string
		tables  []string
		wantSub string
	}{
		{"nil db with valid args", "db", []string{"t"}, "nil db"},
		{"empty tables", "db", nil, "at least one table"},
		{"bad schema name", "db; DROP", []string{"t"}, "invalid MySQL schema name"},
		{"bad table name semicolon", "db", []string{"t; DROP"}, "invalid MySQL table name"},
		{"bad table name backtick", "db", []string{"`t`"}, "invalid MySQL table name"},
		{"bad table name leading digit", "db", []string{"1t"}, "invalid MySQL table name"},
		{"bad table name with space", "db", []string{"my table"}, "invalid MySQL table name"},
		{"bad table name with quote", "db", []string{"t'"}, "invalid MySQL table name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := IntrospectMySQLSchema(context.Background(), nil, tc.schema, tc.tables...)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("expected error containing %q, got: %v", tc.wantSub, err)
			}
		})
	}
}

func TestOrderTablesAs(t *testing.T) {
	found := map[string]*TableSchema{
		"users":    {Name: "users"},
		"products": {Name: "products"},
	}
	got := orderTablesAs(found, []string{"products", "users", "products", "missing"})
	if len(got) != 2 {
		t.Fatalf("expected 2 tables, got %d", len(got))
	}
	if got[0].Name != "products" || got[1].Name != "users" {
		t.Fatalf("unexpected order: %s, %s (want products, users)", got[0].Name, got[1].Name)
	}
}

func TestQuotedIdentList(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"a"}, "'a'"},
		{[]string{"a", "b"}, "'a', 'b'"},
		{[]string{"a", "b", "c"}, "'a', 'b', 'c'"},
	}
	for _, tc := range cases {
		got := quotedIdentList(tc.in)
		if got != tc.want {
			t.Errorf("quotedIdentList(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
