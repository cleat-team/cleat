package catalogdiff

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/cleat-team/cleat/migration"
)

// snapshotPostgres builds a Catalog for a PostgreSQL database. It is the
// dialect docs/schema-partitioning-design.md's differential-verification
// section is written about, and the only dialect the two mandated
// known-positives (TestDiffCatchesAWrongRoutineBody,
// TestDiffCatchesADroppedForce) exercise -- RLS and FORCE are PostgreSQL
// concepts with no MySQL or SQL Server equivalent in this schema (tenant
// isolation on those dialects is a different mechanism entirely; see the
// design doc's work plan on the SQL Server contained-user role).
func snapshotPostgres(ctx context.Context, db *sql.DB) (*Catalog, error) {
	cat := &Catalog{
		Dialect:  migration.DialectPostgres,
		Tables:   map[string]*Table{},
		Routines: map[string]string{},
	}

	type tableRow struct {
		oid                         int64
		schema, name                string
		relrowsecurity, relforcerls bool
	}
	rows, err := db.QueryContext(ctx, `
		SELECT c.oid, n.nspname, c.relname, c.relrowsecurity, c.relforcerowsecurity
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'r'
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		ORDER BY 1
	`)
	if err != nil {
		return nil, fmt.Errorf("catalogdiff: listing tables: %w", err)
	}
	var tables []tableRow
	for rows.Next() {
		var r tableRow
		if err := rows.Scan(&r.oid, &r.schema, &r.name, &r.relrowsecurity, &r.relforcerls); err != nil {
			rows.Close()
			return nil, fmt.Errorf("catalogdiff: scanning table row: %w", err)
		}
		if r.name == schemaMigrationsTable {
			continue
		}
		tables = append(tables, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalogdiff: listing tables: %w", err)
	}
	rows.Close()

	for _, tr := range tables {
		qname := tr.schema + "." + tr.name
		t := &Table{
			Name:        qname,
			RowSecurity: &RowSecurity{Enabled: tr.relrowsecurity, Forced: tr.relforcerls},
		}

		colRows, err := db.QueryContext(ctx, `
			SELECT column_name, data_type, is_nullable, COALESCE(column_default, '')
			FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = $2
			ORDER BY ordinal_position
		`, tr.schema, tr.name)
		if err != nil {
			return nil, fmt.Errorf("catalogdiff: columns for %s: %w", qname, err)
		}
		for colRows.Next() {
			var name, dtype, nullable, def string
			if err := colRows.Scan(&name, &dtype, &nullable, &def); err != nil {
				colRows.Close()
				return nil, fmt.Errorf("catalogdiff: scanning column for %s: %w", qname, err)
			}
			t.Columns = append(t.Columns, Column{Name: name, DataType: dtype, Nullable: nullable == "YES", Default: def})
		}
		if err := colRows.Err(); err != nil {
			return nil, fmt.Errorf("catalogdiff: columns for %s: %w", qname, err)
		}
		colRows.Close()

		idxRows, err := db.QueryContext(ctx, `
			SELECT indexname, indexdef FROM pg_indexes
			WHERE schemaname = $1 AND tablename = $2
			ORDER BY indexname
		`, tr.schema, tr.name)
		if err != nil {
			return nil, fmt.Errorf("catalogdiff: indexes for %s: %w", qname, err)
		}
		for idxRows.Next() {
			var name, def string
			if err := idxRows.Scan(&name, &def); err != nil {
				idxRows.Close()
				return nil, fmt.Errorf("catalogdiff: scanning index for %s: %w", qname, err)
			}
			t.Indexes = append(t.Indexes, Index{Name: name, Definition: def})
		}
		if err := idxRows.Err(); err != nil {
			return nil, fmt.Errorf("catalogdiff: indexes for %s: %w", qname, err)
		}
		idxRows.Close()

		conRows, err := db.QueryContext(ctx, `
			SELECT conname, pg_get_constraintdef(oid)
			FROM pg_constraint
			WHERE conrelid = $1::oid
			ORDER BY conname
		`, tr.oid)
		if err != nil {
			return nil, fmt.Errorf("catalogdiff: constraints for %s: %w", qname, err)
		}
		for conRows.Next() {
			var name, def string
			if err := conRows.Scan(&name, &def); err != nil {
				conRows.Close()
				return nil, fmt.Errorf("catalogdiff: scanning constraint for %s: %w", qname, err)
			}
			t.Constraints = append(t.Constraints, Constraint{Name: name, Definition: def})
		}
		if err := conRows.Err(); err != nil {
			return nil, fmt.Errorf("catalogdiff: constraints for %s: %w", qname, err)
		}
		conRows.Close()

		polRows, err := db.QueryContext(ctx, `
			SELECT polname,
			       CASE polcmd WHEN 'r' THEN 'SELECT' WHEN 'a' THEN 'INSERT'
			                    WHEN 'w' THEN 'UPDATE' WHEN 'd' THEN 'DELETE' ELSE '*' END,
			       COALESCE(pg_get_expr(polqual, polrelid), ''),
			       COALESCE(pg_get_expr(polwithcheck, polrelid), '')
			FROM pg_policy
			WHERE polrelid = $1::oid
			ORDER BY polname
		`, tr.oid)
		if err != nil {
			return nil, fmt.Errorf("catalogdiff: policies for %s: %w", qname, err)
		}
		for polRows.Next() {
			var name, cmd, using, check string
			if err := polRows.Scan(&name, &cmd, &using, &check); err != nil {
				polRows.Close()
				return nil, fmt.Errorf("catalogdiff: scanning policy for %s: %w", qname, err)
			}
			t.Policies = append(t.Policies, Policy{Name: name, Command: cmd, Using: using, Check: check})
		}
		if err := polRows.Err(); err != nil {
			return nil, fmt.Errorf("catalogdiff: policies for %s: %w", qname, err)
		}
		polRows.Close()

		cat.Tables[qname] = t
	}

	// Routines: keyed by (schema, name, argument identity) so overloaded
	// functions of the same name do not collide in the map. pg_get_functiondef
	// includes the full CREATE ... AS $$ ... $$ body, so a changed body is a
	// changed value at this key -- exactly what TestDiffCatchesAWrongRoutineBody
	// needs.
	rtRows, err := db.QueryContext(ctx, `
		SELECT n.nspname, p.proname,
		       COALESCE(pg_get_function_identity_arguments(p.oid), ''),
		       pg_get_functiondef(p.oid)
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
		ORDER BY 1, 2, 3
	`)
	if err != nil {
		return nil, fmt.Errorf("catalogdiff: listing routines: %w", err)
	}
	for rtRows.Next() {
		var schema, name, args, def string
		if err := rtRows.Scan(&schema, &name, &args, &def); err != nil {
			rtRows.Close()
			return nil, fmt.Errorf("catalogdiff: scanning routine: %w", err)
		}
		key := fmt.Sprintf("%s.%s(%s)", schema, name, args)
		cat.Routines[key] = def
	}
	if err := rtRows.Err(); err != nil {
		return nil, fmt.Errorf("catalogdiff: listing routines: %w", err)
	}
	rtRows.Close()

	grantRows, err := db.QueryContext(ctx, `
		SELECT grantee, table_schema, table_name, privilege_type
		FROM information_schema.role_table_grants
		WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
		ORDER BY 1, 2, 3, 4
	`)
	if err != nil {
		return nil, fmt.Errorf("catalogdiff: listing grants: %w", err)
	}
	for grantRows.Next() {
		var grantee, schema, table, priv string
		if err := grantRows.Scan(&grantee, &schema, &table, &priv); err != nil {
			grantRows.Close()
			return nil, fmt.Errorf("catalogdiff: scanning grant: %w", err)
		}
		cat.Grants = append(cat.Grants, fmt.Sprintf("%s ON %s.%s TO %s", priv, schema, table, grantee))
	}
	if err := grantRows.Err(); err != nil {
		return nil, fmt.Errorf("catalogdiff: listing grants: %w", err)
	}
	grantRows.Close()

	return cat, nil
}
