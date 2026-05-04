package builtin

import (
	"fmt"
	"sort"
	"strings"
)

// Schema is a structured representation of database metadata consumed by
// CallSQLAgentTool. Keeping the schema structured (rather than a free-form string)
// unlocks two wins:
//
//  1. Prompt-side grounding — String() renders a compact, consistent markdown
//     block with column types, descriptions, and example values, which large
//     models use as soft constraints to avoid inventing columns.
//  2. Server-side schema linking — Filter(tables...) returns a subset so an
//     orchestrator can pre-select the relevant tables before generating SQL,
//     as recommended by Google's text-to-SQL guidance for large schemas.
//
// Schema is zero-value friendly: an empty Schema renders as an empty string
// and Filter on it is a no-op.
type Schema struct {
	Tables []TableSchema
}

// TableSchema describes a single table. Description, PrimaryKey, and
// ForeignKeys are optional — when present they dramatically reduce
// hallucination on join-heavy questions.
type TableSchema struct {
	Name        string
	Description string
	Columns     []ColumnSchema
	PrimaryKey  []string
	ForeignKeys []ForeignKey
}

// ColumnSchema describes a single column. Examples should be a short list
// (3–5) of representative values drawn from the actual table — "value
// grounding" helps the model match fuzzy natural-language terms to real data
// (e.g. "active customers" → status='ACTIVE' vs 'active').
type ColumnSchema struct {
	Name        string
	Type        string
	Description string
	Nullable    bool
	Examples    []string
}

// ForeignKey describes a relationship between two tables. The agent uses
// these to pick JOIN paths without guessing.
type ForeignKey struct {
	Column    string
	RefTable  string
	RefColumn string
}

// SQLExample is a Question → SQL pair used as a few-shot demonstration.
// Notes is optional free-form reasoning appended after the SQL for
// chain-of-thought style few-shot prompting.
type SQLExample struct {
	Question string
	SQL      string
	Notes    string
}

// String renders the schema as a markdown-formatted prompt block.
// Tables with no columns are skipped.
func (s Schema) String() string {
	if len(s.Tables) == 0 {
		return ""
	}
	var b strings.Builder
	for _, t := range s.Tables {
		if len(t.Columns) == 0 {
			continue
		}
		fmt.Fprintf(&b, "### Table: %s\n", t.Name)
		if t.Description != "" {
			fmt.Fprintf(&b, "%s\n\n", t.Description)
		}
		b.WriteString("| Column | Type | Nullable | Description | Examples |\n")
		b.WriteString("|---|---|---|---|---|\n")
		for _, c := range t.Columns {
			nullable := "NO"
			if c.Nullable {
				nullable = "YES"
			}
			examples := "—"
			if len(c.Examples) > 0 {
				examples = "`" + strings.Join(c.Examples, "`, `") + "`"
			}
			desc := c.Description
			if desc == "" {
				desc = "—"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
				c.Name, c.Type, nullable, desc, examples)
		}
		if len(t.PrimaryKey) > 0 {
			fmt.Fprintf(&b, "\n**Primary key:** %s\n", strings.Join(t.PrimaryKey, ", "))
		}
		if len(t.ForeignKeys) > 0 {
			b.WriteString("\n**Foreign keys:**\n")
			for _, fk := range t.ForeignKeys {
				fmt.Fprintf(&b, "- %s → %s.%s\n", fk.Column, fk.RefTable, fk.RefColumn)
			}
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// TableNames returns all table names in declaration order.
func (s Schema) TableNames() []string {
	names := make([]string, 0, len(s.Tables))
	for _, t := range s.Tables {
		names = append(names, t.Name)
	}
	return names
}

// Filter returns a new Schema restricted to the named tables. Unknown names
// are silently skipped. Matching is case-insensitive. The returned order
// matches the input `tables` slice, with duplicates preserved once.
func (s Schema) Filter(tables ...string) Schema {
	if len(s.Tables) == 0 || len(tables) == 0 {
		return Schema{}
	}
	byName := make(map[string]TableSchema, len(s.Tables))
	for _, t := range s.Tables {
		byName[strings.ToLower(t.Name)] = t
	}
	seen := make(map[string]bool, len(tables))
	out := Schema{Tables: make([]TableSchema, 0, len(tables))}
	for _, want := range tables {
		key := strings.ToLower(strings.TrimSpace(want))
		if key == "" || seen[key] {
			continue
		}
		if t, ok := byName[key]; ok {
			out.Tables = append(out.Tables, t)
			seen[key] = true
		}
	}
	return out
}

// Summary returns a one-line-per-table overview ("table_name: desc — c1,
// c2, ...") suitable for a schema-linking picker prompt where the full
// schema would be too large to fit.
func (s Schema) Summary() string {
	if len(s.Tables) == 0 {
		return ""
	}
	// Stable output — sort alphabetically for determinism in tests.
	tables := make([]TableSchema, len(s.Tables))
	copy(tables, s.Tables)
	sort.Slice(tables, func(i, j int) bool { return tables[i].Name < tables[j].Name })

	var b strings.Builder
	for _, t := range tables {
		fmt.Fprintf(&b, "- %s", t.Name)
		if t.Description != "" {
			fmt.Fprintf(&b, ": %s", t.Description)
		}
		if len(t.Columns) > 0 {
			names := make([]string, len(t.Columns))
			for i, c := range t.Columns {
				names[i] = c.Name
			}
			fmt.Fprintf(&b, " — %s", strings.Join(names, ", "))
		}
		b.WriteString("\n")
	}
	return b.String()
}
