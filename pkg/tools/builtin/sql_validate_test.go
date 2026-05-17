package builtin

import (
	"strings"
	"testing"
)

func TestValidateReadOnly_AllowsSelectWithLeadingComment(t *testing.T) {
	if err := ValidateReadOnly("/* pick top customers */ SELECT id FROM c"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidateReadOnly_AllowsWithCTE(t *testing.T) {
	if err := ValidateReadOnly("WITH t AS (SELECT 1) SELECT * FROM t"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidateReadOnly_AllowsExplain(t *testing.T) {
	if err := ValidateReadOnly("EXPLAIN SELECT 1"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidateReadOnly_RejectsDML(t *testing.T) {
	cases := []string{
		"UPDATE t SET x = 1",
		"DELETE FROM t",
		"DROP TABLE t",
		"INSERT INTO t VALUES (1)",
		"CREATE TABLE t (id INT)",
		"ALTER TABLE t ADD COLUMN x INT",
		"TRUNCATE TABLE t",
		"MERGE INTO t USING s ON t.id = s.id",
		"GRANT SELECT ON t TO u",
	}
	for _, sql := range cases {
		if err := ValidateReadOnly(sql); err == nil {
			t.Fatalf("expected error for %q, got nil", sql)
		}
	}
}

func TestValidateReadOnly_RejectsCommentDisguisedDML(t *testing.T) {
	// "/* SELECT */ DROP" must be classified as DROP after comment strip.
	err := ValidateReadOnly("/* SELECT */ DROP TABLE users")
	if err == nil {
		t.Fatal("expected error for comment-disguised DROP, got nil")
	}
	if !strings.Contains(err.Error(), "DROP") {
		t.Fatalf("error should name DROP, got %v", err)
	}
}

func TestValidateReadOnly_RejectsMultipleStatements(t *testing.T) {
	err := ValidateReadOnly("SELECT 1; DROP TABLE x")
	if err == nil {
		t.Fatal("expected multi-statement error, got nil")
	}
	if !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("error should mention multiple statements, got %v", err)
	}
}

func TestValidateReadOnly_SingleTrailingSemicolonIsOK(t *testing.T) {
	// A single statement with a trailing ';' is not multi-statement.
	if err := ValidateReadOnly("SELECT 1;"); err != nil {
		t.Fatalf("trailing semicolon should be allowed, got %v", err)
	}
}

func TestValidateReadOnly_EmptyIsError(t *testing.T) {
	if err := ValidateReadOnly("   \n\t "); err == nil {
		t.Fatal("expected error for empty SQL")
	}
}

func TestValidateReadOnly_SemicolonInsideStringIsNotSplit(t *testing.T) {
	if err := ValidateReadOnly(`SELECT ';' AS tag FROM t`); err != nil {
		t.Fatalf("semicolon in string literal must not count as multi-statement, got %v", err)
	}
}

func TestClassifySQL_ClassifiesReads(t *testing.T) {
	for _, sql := range []string{
		"SELECT 1",
		"  with t as (select 1) select * from t",
		"EXPLAIN SELECT 1",
		"SHOW TABLES",
		"DESCRIBE t",
		"DESC t",
	} {
		kind, err := ClassifySQL(sql)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", sql, err)
		}
		if kind != SQLKindRead {
			t.Fatalf("expected Read for %q, got %v", sql, kind)
		}
	}
}

func TestClassifySQL_ClassifiesMutations(t *testing.T) {
	for _, sql := range []string{
		"INSERT INTO t VALUES (1)",
		"update t set x = 1 where id = 5",
		"DELETE FROM t WHERE id = 2",
		"MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN UPDATE SET x = s.x",
	} {
		kind, err := ClassifySQL(sql)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", sql, err)
		}
		if kind != SQLKindMutation {
			t.Fatalf("expected Mutation for %q, got %v", sql, kind)
		}
	}
}

func TestClassifySQL_ClassifiesDDL(t *testing.T) {
	for _, sql := range []string{
		"DROP TABLE t",
		"CREATE TABLE t (id INT)",
		"ALTER TABLE t ADD COLUMN x INT",
		"TRUNCATE TABLE t",
		"GRANT SELECT ON t TO u",
		"REVOKE ALL ON t FROM u",
		"COMMENT ON TABLE t IS 'note'",
	} {
		kind, err := ClassifySQL(sql)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", sql, err)
		}
		if kind != SQLKindDDL {
			t.Fatalf("expected SQLKindDDL for %q, got %v", sql, kind)
		}
	}
}

func TestClassifySQL_RejectsUnknownVerbs(t *testing.T) {
	for _, sql := range []string{
		"VACUUM t",
		"CALL sp_do_stuff()",
	} {
		kind, err := ClassifySQL(sql)
		if err == nil {
			t.Fatalf("expected error for %q, got nil (kind=%v)", sql, kind)
		}
		if kind != SQLKindUnknown {
			t.Fatalf("expected SQLKindUnknown for %q, got %v", sql, kind)
		}
	}
}

func TestClassifySQL_RejectsMultiStatementAndEmpty(t *testing.T) {
	if _, err := ClassifySQL("SELECT 1; DELETE FROM t"); err == nil {
		t.Fatal("expected multi-statement error")
	}
	if _, err := ClassifySQL("   "); err == nil {
		t.Fatal("expected empty-statement error")
	}
}

func TestStripSQLComments_RemovesLineAndBlock(t *testing.T) {
	in := "-- header\nSELECT 1 /* inline */ FROM t -- trailing"
	got := StripSQLComments(in)
	if strings.Contains(got, "--") || strings.Contains(got, "/*") {
		t.Fatalf("comments not stripped: %q", got)
	}
	if !strings.Contains(got, "SELECT 1") || !strings.Contains(got, "FROM t") {
		t.Fatalf("content lost: %q", got)
	}
}

func TestStripSQLComments_PreservesCommentInsideString(t *testing.T) {
	in := `SELECT 'not -- a comment' FROM t`
	if got := StripSQLComments(in); got != in {
		t.Fatalf("content inside quotes must be preserved, got %q", got)
	}
}

func TestEnsureLimit_AppendsWhenAbsent(t *testing.T) {
	got := EnsureLimit("SELECT * FROM t", 50)
	if !strings.Contains(got, "LIMIT 50") {
		t.Fatalf("expected LIMIT 50 appended, got %q", got)
	}
}

func TestEnsureLimit_NoopWhenPresent(t *testing.T) {
	orig := "SELECT id FROM t LIMIT 10"
	if got := EnsureLimit(orig, 50); got != orig {
		t.Fatalf("existing LIMIT must be preserved, got %q", got)
	}
}

func TestEnsureLimit_NoopOnZero(t *testing.T) {
	orig := "SELECT * FROM t"
	if got := EnsureLimit(orig, 0); got != orig {
		t.Fatalf("max<=0 must be a noop, got %q", got)
	}
}

func TestEnsureLimit_SkipsInnerLimitOnly(t *testing.T) {
	// Inner LIMIT must NOT prevent outer LIMIT from being added.
	orig := "SELECT * FROM (SELECT id FROM t LIMIT 5) x"
	got := EnsureLimit(orig, 100)
	if !strings.Contains(got, "LIMIT 100") {
		t.Fatalf("outer LIMIT should be appended despite inner subquery LIMIT, got %q", got)
	}
}

func TestEnsureLimit_NoopOnNonSelect(t *testing.T) {
	// Only SELECT/WITH get a LIMIT — EXPLAIN and SHOW are left alone.
	for _, in := range []string{"EXPLAIN SELECT 1", "SHOW TABLES"} {
		if got := EnsureLimit(in, 100); got != in {
			t.Fatalf("LIMIT must not be added to %q, got %q", in, got)
		}
	}
}

func TestEnsureLimit_StripsTrailingSemicolon(t *testing.T) {
	got := EnsureLimit("SELECT * FROM t;", 10)
	if strings.Contains(got, ";\n") || strings.HasSuffix(got, ";") {
		t.Fatalf("trailing semicolon should be trimmed before LIMIT, got %q", got)
	}
	if !strings.Contains(got, "LIMIT 10") {
		t.Fatalf("expected LIMIT appended, got %q", got)
	}
}

func TestSplitStatements_IgnoresSemicolonsInStrings(t *testing.T) {
	got := SplitStatements(`SELECT ';' ; SELECT 2`)
	if len(got) != 2 {
		t.Fatalf("expected 2 statements, got %d: %#v", len(got), got)
	}
}
