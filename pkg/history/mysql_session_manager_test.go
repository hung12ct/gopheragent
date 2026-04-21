package history

import (
	"strings"
	"testing"
)

func TestNewMySQLSessionManagerWithOptions_InvalidTableName(t *testing.T) {
	cases := []struct {
		name  string
		table string
	}{
		{"empty", ""},
		{"leading digit", "1sessions"},
		{"semicolon injection", "sessions; DROP TABLE"},
		{"backtick", "`sessions`"},
		{"space", "my sessions"},
		{"quote", "sessions'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewMySQLSessionManagerWithOptions(nil, "", WithMySQLTableName(tc.table))
			if err == nil {
				t.Fatalf("expected error for invalid table name %q", tc.table)
			}
			if !strings.Contains(err.Error(), "invalid MySQL table name") {
				t.Errorf("expected invalid-table-name error, got: %v", err)
			}
		})
	}
}

func TestWithMySQLTableName_AppliesOption(t *testing.T) {
	var cfg mysqlOptions
	cfg.tableName = DefaultMySQLTableName
	WithMySQLTableName("boa_sessions")(&cfg)
	if cfg.tableName != "boa_sessions" {
		t.Errorf("expected tableName 'boa_sessions', got %q", cfg.tableName)
	}
}
