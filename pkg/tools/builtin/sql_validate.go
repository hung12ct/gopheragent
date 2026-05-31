package builtin

import (
	"fmt"
	"strings"
)

// SQLKind classifies a parsed single SQL statement by its leading verb.
// Unknown covers anything that doesn't map to a known verb. ClassifySQL
// never returns Unknown alongside a nil error; the kind/error pair is
// always consistent. The executor decides which kinds are permitted via
// WithAllowMutations / WithAllowDDL — classification itself stays neutral.
type SQLKind int

const (
	SQLKindUnknown SQLKind = iota
	SQLKindRead
	SQLKindMutation
	// SQLKindDDL covers schema- and permission-changing statements
	// (CREATE / DROP / ALTER / TRUNCATE / GRANT / REVOKE / COMMENT).
	// Default-rejected at the executor; opt-in via WithAllowDDL(true).
	SQLKindDDL
)

// readOnlyHeads lists the first-token keywords that classify as Read.
var readOnlyHeads = map[string]bool{
	"SELECT":   true,
	"WITH":     true,
	"EXPLAIN":  true,
	"DESCRIBE": true,
	"DESC":     true,
	"SHOW":     true,
}

// mutationHeads lists the DML verbs that classify as Mutation. DDL and
// permission statements are intentionally excluded — those carry larger
// blast radius and have their own opt-in flag (WithAllowDDL).
var mutationHeads = map[string]bool{
	"INSERT": true,
	"UPDATE": true,
	"DELETE": true,
	"MERGE":  true,
}

// ddlHeads lists schema- and permission-changing verbs. Kept orthogonal
// to mutationHeads so adopters can opt in to DML and DDL independently —
// their blast radii are very different (rows vs schemas/permissions).
var ddlHeads = map[string]bool{
	"CREATE":   true,
	"DROP":     true,
	"ALTER":    true,
	"TRUNCATE": true,
	"GRANT":    true,
	"REVOKE":   true,
	"COMMENT":  true,
}

// ClassifySQL parses sql, rejects multi-statement input, and returns the
// kind of the single statement. Comments are stripped before analysis, so
// "/* update */ SELECT" still classifies as Read. An empty or
// multi-statement input, or an unrecognised verb, yields SQLKindUnknown
// with a wrapped error.
func ClassifySQL(sql string) (SQLKind, error) {
	stripped := StripSQLComments(sql)
	statements := SplitStatements(stripped)
	nonEmpty := statements[:0]
	for _, s := range statements {
		if strings.TrimSpace(s) != "" {
			nonEmpty = append(nonEmpty, s)
		}
	}
	if len(nonEmpty) == 0 {
		return SQLKindUnknown, fmt.Errorf("tools: empty SQL statement")
	}
	if len(nonEmpty) > 1 {
		return SQLKindUnknown, fmt.Errorf("tools: multiple statements are not permitted (got %d)", len(nonEmpty))
	}
	head := firstKeyword(nonEmpty[0])
	switch {
	case readOnlyHeads[head]:
		return SQLKindRead, nil
	case mutationHeads[head]:
		return SQLKindMutation, nil
	case ddlHeads[head]:
		return SQLKindDDL, nil
	default:
		return SQLKindUnknown, fmt.Errorf("tools: statement type %q is not permitted; allowed verbs are SELECT, WITH, EXPLAIN, SHOW, DESCRIBE (and INSERT, UPDATE, DELETE, MERGE when mutations are enabled; CREATE, DROP, ALTER, TRUNCATE, GRANT, REVOKE, COMMENT when DDL is enabled)", head)
	}
}

// ValidateReadOnly checks that sql is exactly one read-only statement.
// Equivalent to `kind, err := ClassifySQL(sql); kind == SQLKindRead`.
// Returns nil when the input is a single read-only statement; rejects
// multi-statement input and any non-read verb (including the mutation
// verbs covered by ClassifySQL — call ClassifySQL directly to permit
// mutations).
func ValidateReadOnly(sql string) error {
	kind, err := ClassifySQL(sql)
	if err != nil {
		return err
	}
	if kind != SQLKindRead {
		head := firstKeyword(StripSQLComments(sql))
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
			for j+1 < len(s) && (s[j] != '*' || s[j+1] != '/') {
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
		switch c {
		case '\'', '"', '`':
			end := scanQuoted(s, i)
			cur.WriteString(s[i:end])
			i = end
		case ';':
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

// HasBareStarProjection reports whether the outermost SELECT's projection
// list contains a bare "*" or "<alias>.*" wildcard. Subquery SELECTs at
// paren depth > 0 are not inspected — only the outer query is enforced,
// which is where SELECT * causes the practical harm (unbounded row width,
// hallucinated column references on the model's next turn). Statements
// whose leading verb is not SELECT or WITH return false unconditionally
// (EXPLAIN / SHOW / DESCRIBE return plan / metadata rows, not user data).
func HasBareStarProjection(sql string) bool {
	s := StripSQLComments(sql)
	head := firstKeyword(s)
	if head != "SELECT" && head != "WITH" {
		return false
	}
	selectStart := findOuterSelectBody(s)
	if selectStart < 0 {
		return false
	}
	// Skip optional DISTINCT / ALL qualifier.
	i := selectStart
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	if matchesWordAt(s, i, "DISTINCT") {
		i += len("DISTINCT")
	} else if matchesWordAt(s, i, "ALL") {
		i += len("ALL")
	}
	// Walk projection list at depth 0; split on commas; stop at outer FROM.
	pieceStart := i
	depth := 0
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
		case depth == 0 && c == ',':
			if isWildcardProjection(s[pieceStart:i]) {
				return true
			}
			pieceStart = i + 1
			i++
		case depth == 0 && matchesWordAt(s, i, "FROM"):
			return isWildcardProjection(s[pieceStart:i])
		default:
			i++
		}
	}
	return isWildcardProjection(s[pieceStart:])
}

// findOuterSelectBody returns the byte offset immediately after the first
// "SELECT" keyword at paren depth 0, or -1 if none is found. Used by
// HasBareStarProjection to locate the outermost projection list (skipping
// SELECTs nested inside CTE bodies, subqueries, etc.).
func findOuterSelectBody(s string) int {
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
		case depth == 0 && matchesWordAt(s, i, "SELECT"):
			return i + len("SELECT")
		default:
			i++
		}
	}
	return -1
}

// isWildcardProjection reports whether a single comma-separated projection
// piece is a bare "*" or "<alias>.*". Trims surrounding whitespace; a
// trailing "AS alias" on the wildcard still counts (the alias does not
// constrain the row width). Anything starting with an operand before the
// star (e.g. "price * 2") is multiplication and returns false.
func isWildcardProjection(piece string) bool {
	p := strings.TrimSpace(piece)
	if p == "" {
		return false
	}
	if p[0] == '*' {
		return len(p) == 1 || !isIdentChar(p[1])
	}
	dot := strings.IndexByte(p, '.')
	if dot <= 0 {
		return false
	}
	for k := range dot {
		if !isIdentChar(p[k]) {
			return false
		}
	}
	rest := strings.TrimLeft(p[dot+1:], " \t")
	if rest == "" {
		return false
	}
	return rest[0] == '*' && (len(rest) == 1 || !isIdentChar(rest[1]))
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
// starts at s[i]. Supports SQL's doubled-quote escape, where the delimiter
// is repeated to embed a literal copy of itself inside a single-quoted,
// double-quoted, or backtick-quoted run.
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
