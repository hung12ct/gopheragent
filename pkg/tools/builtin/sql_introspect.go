package builtin

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

var mysqlIdentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// IntrospectMySQLSchema queries INFORMATION_SCHEMA on a live MySQL/MariaDB
// connection and returns a Schema populated with column metadata for the
// named tables. It is the deterministic alternative to a hand-maintained
// schema doc — schema drift between docs and the live DB is the most common
// cause of "Unknown column" errors from CallSQLAgentTool.
//
// schemaName scopes the lookup; pass "" to use the connection's current
// database (resolved via DATABASE()). tableNames must be non-empty.
//
// All identifiers are validated against [A-Za-z_][A-Za-z0-9_]* before being
// inlined into the query. Invalid identifiers are rejected at the boundary,
// which lets the helper run on any MySQL driver without dialect-specific
// placeholder syntax. Tables that do not exist in the database are silently
// omitted from the result.
//
// Column descriptions are populated from MySQL COLUMN_COMMENT when present.
// PrimaryKey is populated from KEY_COLUMN_USAGE when the table has one.
// Foreign keys are not currently introspected; declare them via WithSchema
// if your model needs join hints.
//
// Typical use:
//
//	schema, err := builtin.IntrospectMySQLSchema(ctx, db, "",
//	    "daily_creatives_impressions_v2", "creatives", "apps")
//	if err != nil { ... }
//	sqlTool := builtin.NewCallSQLAgentTool(db, "", sm, provider).WithSchema(schema)
func IntrospectMySQLSchema(ctx context.Context, db *sql.DB, schemaName string, tableNames ...string) (Schema, error) {
	if len(tableNames) == 0 {
		return Schema{}, fmt.Errorf("tools: introspect: at least one table name is required")
	}
	if schemaName != "" && !mysqlIdentRE.MatchString(schemaName) {
		return Schema{}, fmt.Errorf("tools: introspect: invalid MySQL schema name %q (must match [A-Za-z_][A-Za-z0-9_]*)", schemaName)
	}
	for _, t := range tableNames {
		if !mysqlIdentRE.MatchString(t) {
			return Schema{}, fmt.Errorf("tools: introspect: invalid MySQL table name %q (must match [A-Za-z_][A-Za-z0-9_]*)", t)
		}
	}
	if db == nil {
		return Schema{}, fmt.Errorf("tools: introspect: nil db")
	}

	if schemaName == "" {
		var current sql.NullString
		if err := db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&current); err != nil {
			return Schema{}, fmt.Errorf("tools: introspect: resolve current database: %w", err)
		}
		if !current.Valid || current.String == "" {
			return Schema{}, fmt.Errorf("tools: introspect: no current database — pass schemaName explicitly")
		}
		schemaName = current.String
	}

	tables, err := fetchMySQLColumns(ctx, db, schemaName, tableNames)
	if err != nil {
		return Schema{}, err
	}
	if err := annotateMySQLPrimaryKeys(ctx, db, schemaName, tables); err != nil {
		return Schema{}, err
	}
	return Schema{Tables: orderTablesAs(tables, tableNames)}, nil
}

// fetchMySQLColumns runs the INFORMATION_SCHEMA.COLUMNS query and groups
// the rows into a per-table map keyed by table name.
func fetchMySQLColumns(ctx context.Context, db *sql.DB, schemaName string, tableNames []string) (map[string]*TableSchema, error) {
	q := fmt.Sprintf(`
		SELECT TABLE_NAME, COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COLUMN_COMMENT
		FROM INFORMATION_SCHEMA.COLUMNS
		WHERE TABLE_SCHEMA = '%s' AND TABLE_NAME IN (%s)
		ORDER BY TABLE_NAME, ORDINAL_POSITION`,
		schemaName, quotedIdentList(tableNames),
	)
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("tools: introspect: query INFORMATION_SCHEMA.COLUMNS: %w", err)
	}
	defer rows.Close()

	tables := make(map[string]*TableSchema)
	for rows.Next() {
		var tableName, colName, colType, isNullable, colComment string
		if err := rows.Scan(&tableName, &colName, &colType, &isNullable, &colComment); err != nil {
			return nil, fmt.Errorf("tools: introspect: scan COLUMNS row: %w", err)
		}
		ts, ok := tables[tableName]
		if !ok {
			ts = &TableSchema{Name: tableName}
			tables[tableName] = ts
		}
		ts.Columns = append(ts.Columns, ColumnSchema{
			Name:        colName,
			Type:        colType,
			Description: colComment,
			Nullable:    strings.EqualFold(isNullable, "YES"),
		})
	}
	return tables, rows.Err()
}

// annotateMySQLPrimaryKeys reads KEY_COLUMN_USAGE for the discovered tables
// and appends each PRIMARY column to the matching TableSchema in order.
func annotateMySQLPrimaryKeys(ctx context.Context, db *sql.DB, schemaName string, tables map[string]*TableSchema) error {
	if len(tables) == 0 {
		return nil
	}
	names := make([]string, 0, len(tables))
	for n := range tables {
		names = append(names, n)
	}
	q := fmt.Sprintf(`
		SELECT TABLE_NAME, COLUMN_NAME
		FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = '%s'
		  AND CONSTRAINT_NAME = 'PRIMARY'
		  AND TABLE_NAME IN (%s)
		ORDER BY TABLE_NAME, ORDINAL_POSITION`,
		schemaName, quotedIdentList(names),
	)
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("tools: introspect: query KEY_COLUMN_USAGE: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tableName, colName string
		if err := rows.Scan(&tableName, &colName); err != nil {
			return fmt.Errorf("tools: introspect: scan KEY_COLUMN_USAGE row: %w", err)
		}
		if ts, ok := tables[tableName]; ok {
			ts.PrimaryKey = append(ts.PrimaryKey, colName)
		}
	}
	return rows.Err()
}

// orderTablesAs returns the discovered tables in the same order they were
// requested. Duplicates in the request are kept once; missing tables are
// dropped.
func orderTablesAs(found map[string]*TableSchema, requested []string) []TableSchema {
	seen := make(map[string]bool, len(requested))
	out := make([]TableSchema, 0, len(found))
	for _, want := range requested {
		if seen[want] {
			continue
		}
		seen[want] = true
		if ts, ok := found[want]; ok {
			out = append(out, *ts)
		}
	}
	return out
}

// quotedIdentList renders a slice of pre-validated identifiers as a
// comma-separated single-quoted list suitable for an INFORMATION_SCHEMA
// IN-clause: ['a', 'b', 'c']. Caller must validate the inputs.
func quotedIdentList(idents []string) string {
	q := make([]string, len(idents))
	for i, n := range idents {
		q[i] = "'" + n + "'"
	}
	return strings.Join(q, ", ")
}
