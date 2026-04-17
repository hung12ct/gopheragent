package builtin

import (
	"fmt"
	"strings"
)

// readOnlyHeads lists the first-token keywords that are allowed as the start
// of a statement. Anything else (UPDATE, DELETE, DROP, etc.) is rejected.
var readOnlyHeads = map[string]bool{
	"SELECT":   true,
	"WITH":     true,
	"EXPLAIN":  true,
	"DESCRIBE": true,
	"DESC":     true,
	"SHOW":     true,
}

// ValidateReadOnly checks that sql is exactly one read-only statement.
// It rejects:
//   - multi-statement input ("SELECT 1; DROP TABLE x")
//   - any statement whose first keyword is not in readOnlyHeads
//
// Comments are stripped before analysis, so "/* update */ SELECT" is
// correctly classified as a SELECT. Returns nil when the input is a single
// read-only statement.
func ValidateReadOnly(sql string) error {
	stripped := StripSQLComments(sql)
	statements := SplitStatements(stripped)
	nonEmpty := statements[:0]
	for _, s := range statements {
		if strings.TrimSpace(s) != "" {
			nonEmpty = append(nonEmpty, s)
		}
	}
	if len(nonEmpty) == 0 {
		return fmt.Errorf("tools: empty SQL statement")
	}
	if len(nonEmpty) > 1 {
		return fmt.Errorf("tools: multiple statements are not permitted (got %d)", len(nonEmpty))
	}
	head := firstKeyword(nonEmpty[0])
	if !readOnlyHeads[head] {
		return fmt.Errorf("tools: statement type %q is not permitted; only read-only queries (SELECT, WITH, EXPLAIN, SHOW, DESCRIBE) are allowed", head)
	}
	return nil
}

// StripSQLComments removes -- line comments and /* */ block comments from s.
// Content inside single-quoted strings, double-quoted identifiers, and
// backtick-quoted identifiers is preserved verbatim. Doubled quotes inside a
// string literal (SQL's escape) are respected.
func StripSQLComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			// Copy the quoted run unchanged.
			end := scanQuoted(s, i)
			b.WriteString(s[i:end])
			i = end
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			// Line comment → skip to end of line.
			j := i + 2
			for j < len(s) && s[j] != '\n' {
				j++
			}
			i = j
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			// Block comment → skip to */.
			j := i + 2
			for j+1 < len(s) && !(s[j] == '*' && s[j+1] == '/') {
				j++
			}
			if j+1 < len(s) {
				j += 2
			} else {
				j = len(s)
			}
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// SplitStatements splits s on top-level semicolons. Semicolons inside
// quoted strings/identifiers are ignored. A trailing semicolon does not
// produce a final empty statement.
func SplitStatements(s string) []string {
	var out []string
	var cur strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			end := scanQuoted(s, i)
			cur.WriteString(s[i:end])
			i = end
		case c == ';':
			out = append(out, cur.String())
			cur.Reset()
			i++
		default:
			cur.WriteByte(c)
			i++
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, cur.String())
	}
	return out
}

// EnsureLimit returns sql with a trailing LIMIT N clause when:
//   - max > 0
//   - the first keyword is SELECT or WITH
//   - the statement does not already contain a top-level LIMIT
//
// It is a best-effort lexical transform — it will not touch subquery LIMITs
// and will not rewrite an already-limited outer query. Comments are stripped
// for the LIMIT check but preserved in the returned SQL.
func EnsureLimit(sql string, max int) string {
	if max <= 0 {
		return sql
	}
	trimmed := strings.TrimRight(sql, "; \t\r\n")
	head := firstKeyword(StripSQLComments(trimmed))
	if head != "SELECT" && head != "WITH" {
		return sql
	}
	if hasTopLevelLimit(StripSQLComments(trimmed)) {
		return sql
	}
	return trimmed + fmt.Sprintf("\nLIMIT %d", max)
}

// firstKeyword returns the first SQL keyword in s, uppercased. Leading
// whitespace and an opening parenthesis (common in parenthesised SELECTs)
// are skipped.
func firstKeyword(s string) string {
	i := 0
	for i < len(s) && (isSpace(s[i]) || s[i] == '(') {
		i++
	}
	j := i
	for j < len(s) && isIdentChar(s[j]) {
		j++
	}
	return strings.ToUpper(s[i:j])
}

// hasTopLevelLimit reports whether s contains a LIMIT keyword at parenthesis
// depth zero. Quoted content is skipped.
func hasTopLevelLimit(s string) bool {
	depth := 0
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			i = scanQuoted(s, i)
		case c == '(':
			depth++
			i++
		case c == ')':
			if depth > 0 {
				depth--
			}
			i++
		case depth == 0 && (c == 'L' || c == 'l') && matchesWordAt(s, i, "LIMIT"):
			return true
		default:
			i++
		}
	}
	return false
}

// matchesWordAt reports whether s has `word` (case-insensitive) starting at
// byte offset i and bounded by non-identifier characters.
func matchesWordAt(s string, i int, word string) bool {
	if i > 0 && isIdentChar(s[i-1]) {
		return false
	}
	if i+len(word) > len(s) {
		return false
	}
	for k := range len(word) {
		if toUpperASCII(s[i+k]) != word[k] {
			return false
		}
	}
	end := i + len(word)
	if end < len(s) && isIdentChar(s[end]) {
		return false
	}
	return true
}

// scanQuoted returns the byte offset immediately after the quoted run that
// starts at s[i]. Supports SQL's doubled-quote escape ('' inside '...',
// "" inside "...", `` inside `...`).
func scanQuoted(s string, i int) int {
	if i >= len(s) {
		return i
	}
	q := s[i]
	j := i + 1
	for j < len(s) {
		if s[j] == q {
			if j+1 < len(s) && s[j+1] == q {
				j += 2
				continue
			}
			return j + 1
		}
		j++
	}
	return len(s)
}

func isSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', '\f':
		return true
	}
	return false
}

func isIdentChar(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '_'
}

func toUpperASCII(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - 32
	}
	return c
}
