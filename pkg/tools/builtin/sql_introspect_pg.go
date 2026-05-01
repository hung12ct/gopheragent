package builtin

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
)

// pgIdentRE matches the conservative subset of PostgreSQL identifiers we
// allow when inlining names into INFORMATION_SCHEMA queries. Postgres
// permits `$` and quoted-arbitrary identifiers, but those would require
// runtime escaping; the strict pattern lets us keep the helper
// placeholder-free without exposing an injection surface.
var pgIdentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// IntrospectPostgresSchema queries information_schema and pg_catalog on a
// live PostgreSQL connection and returns a Schema populated with column
// metadata for the named tables. Mirrors IntrospectMySQLSchema for the
// Postgres dialect.
//
// schemaName scopes the lookup; pass "" to use the connection's
// current_schema(). tableNames must be non-empty. All identifiers are
// validated against [A-Za-z_][A-Za-z0-9_]* before being inlined into the
// query — invalid names are rejected at the boundary.
//
// Column descriptions are populated from pg_catalog.pg_description (the
// COMMENT ON COLUMN ... mechanism). PrimaryKey is populated by joining
// information_schema.table_constraints / key_column_usage and filtering
// constraint_type = 'PRIMARY KEY'. Foreign keys are not introspected; use
// WithSchema(...) to declare them manually if your model needs join hints.
//
// Typical use:
//
//	schema, err := builtin.IntrospectPostgresSchema(ctx, db, "",
//	    "events", "users", "orgs")
//	if err != nil { ... }
//	sqlTool := builtin.NewSQLAgentTool(db, "", sm, provider).WithSchema(schema)
func IntrospectPostgresSchema(ctx context.Context, db *sql.DB, schemaName string, tableNames ...string) (Schema, error) {
	if len(tableNames) == 0 {
		return Schema{}, fmt.Errorf("tools: introspect: at least one table name is required")
	}
	if schemaName != "" && !pgIdentRE.MatchString(schemaName) {
		return Schema{}, fmt.Errorf("tools: introspect: invalid Postgres schema name %q (must match [A-Za-z_][A-Za-z0-9_]*)", schemaName)
	}
	for _, t := range tableNames {
		if !pgIdentRE.MatchString(t) {
			return Schema{}, fmt.Errorf("tools: introspect: invalid Postgres table name %q (must match [A-Za-z_][A-Za-z0-9_]*)", t)
		}
	}
	if db == nil {
		return Schema{}, fmt.Errorf("tools: introspect: nil db")
	}

	if schemaName == "" {
		var current sql.NullString
		if err := db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&current); err != nil {
			return Schema{}, fmt.Errorf("tools: introspect: resolve current_schema: %w", err)
		}
		if !current.Valid || current.String == "" {
			return Schema{}, fmt.Errorf("tools: introspect: no current schema — pass schemaName explicitly")
		}
		schemaName = current.String
	}

	tables, err := fetchPostgresColumns(ctx, db, schemaName, tableNames)
	if err != nil {
		return Schema{}, err
	}
	if err := annotatePostgresPrimaryKeys(ctx, db, schemaName, tables); err != nil {
		return Schema{}, err
	}
	return Schema{Tables: orderTablesAs(tables, tableNames)}, nil
}

// fetchPostgresColumns reads information_schema.columns and joins
// pg_catalog.pg_description to recover COMMENT ON COLUMN entries.
func fetchPostgresColumns(ctx context.Context, db *sql.DB, schemaName string, tableNames []string) (map[string]*TableSchema, error) {
	q := fmt.Sprintf(`
		SELECT c.table_name,
		       c.column_name,
		       c.data_type,
		       c.is_nullable,
		       COALESCE(pgd.description, '') AS description
		FROM information_schema.columns c
		LEFT JOIN pg_catalog.pg_statio_all_tables st
		  ON c.table_schema = st.schemaname
		 AND c.table_name   = st.relname
		LEFT JOIN pg_catalog.pg_description pgd
		  ON pgd.objoid    = st.relid
		 AND pgd.objsubid  = c.ordinal_position
		WHERE c.table_schema = '%s'
		  AND c.table_name IN (%s)
		ORDER BY c.table_name, c.ordinal_position`,
		schemaName, quotedIdentList(tableNames),
	)
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("tools: introspect: query information_schema.columns: %w", err)
	}
	defer rows.Close()

	tables := make(map[string]*TableSchema)
	for rows.Next() {
		var tableName, colName, dataType, isNullable, description string
		if err := rows.Scan(&tableName, &colName, &dataType, &isNullable, &description); err != nil {
			return nil, fmt.Errorf("tools: introspect: scan columns row: %w", err)
		}
		ts, ok := tables[tableName]
		if !ok {
			ts = &TableSchema{Name: tableName}
			tables[tableName] = ts
		}
		ts.Columns = append(ts.Columns, ColumnSchema{
			Name:        colName,
			Type:        dataType,
			Description: description,
			Nullable:    isNullable == "YES",
		})
	}
	return tables, rows.Err()
}

// annotatePostgresPrimaryKeys reads information_schema.table_constraints
// joined with key_column_usage to attach primary-key column lists.
func annotatePostgresPrimaryKeys(ctx context.Context, db *sql.DB, schemaName string, tables map[string]*TableSchema) error {
	if len(tables) == 0 {
		return nil
	}
	names := make([]string, 0, len(tables))
	for n := range tables {
		names = append(names, n)
	}
	q := fmt.Sprintf(`
		SELECT kcu.table_name, kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name = kcu.constraint_name
		 AND tc.table_schema    = kcu.table_schema
		WHERE tc.constraint_type = 'PRIMARY KEY'
		  AND tc.table_schema = '%s'
		  AND tc.table_name IN (%s)
		ORDER BY kcu.table_name, kcu.ordinal_position`,
		schemaName, quotedIdentList(names),
	)
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("tools: introspect: query key_column_usage: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tableName, colName string
		if err := rows.Scan(&tableName, &colName); err != nil {
			return fmt.Errorf("tools: introspect: scan key_column_usage row: %w", err)
		}
		if ts, ok := tables[tableName]; ok {
			ts.PrimaryKey = append(ts.PrimaryKey, colName)
		}
	}
	return rows.Err()
}
