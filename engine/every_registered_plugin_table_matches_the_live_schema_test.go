package engine_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
)

// TestEveryPluginTablesRegistrationMatchesTheLiveSchema is a live-database
// counterpart to TestEveryPluginTableWithATenantIDDeclaresItsTenantScope
// (every_tenant_id_plugin_table_declares_its_scope_test.go). That test reads
// migration SQL TEXT and the Go-source TenantScoped declaration; it cannot
// see whether admin.plugin_tables' tenant_scoped flag -- written once, at
// migration-apply time, by registerTenantScopedTables -- still agrees with
// the table's ACTUAL columns in a live database. A later migration that
// drops or renames tenant_id without updating the registry, or a hand-edited
// row, would pass the source-level test and fail silently here; this test
// exists to make that failure loud instead.
//
// PostgreSQL only, not because this check happens not to run on the other
// two dialects but because there is nothing of this shape to run there:
// admin.plugin_tables carries no tenant_scoped column at all on MySQL or
// SQL Server (migrations/{mysql,mssql}/001_schema.sql declare only
// (plugin_name, table_name)), and registerTenantScopedTables
// (plugin/migration.go) is PostgreSQL-only by construction -- SQL Server's
// admin.drop_tenant asks sys.columns directly with no registry involved, and
// MySQL is database-per-tenant and needs no per-table registry at all
// (cleat#2238, cleat#1635 already audited this). cleat#3260, split from
// cleat#2292.
func TestEveryPluginTablesRegistrationMatchesTheLiveSchema(t *testing.T) {
	db := engine.BootstrapScratchDB(t, "cleat_plugin_registry_live_schema_test")

	plugins, err := plugin.Discover()
	if err != nil {
		t.Fatalf("plugin.Discover: %v", err)
	}

	// Same filter as TestPluginMigrations_AllDialects (plugin_migrations_test.go):
	// healthy, has migrations, and not pgvector, which requires a PostgreSQL
	// extension this scratch database does not have installed.
	var migratable []*plugin.LoadedPlugin
	for _, lp := range plugins {
		if !lp.Healthy {
			continue
		}
		if _, ok := lp.Plugin.(plugin.HasMigrations); !ok {
			continue
		}
		if lp.Plugin.Info().Name == "pgvector" {
			continue
		}
		migratable = append(migratable, lp)
	}

	ctx := context.Background()
	if err := plugin.RunMigrations(ctx, db, plugin.DialectPostgres, nil, migratable); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	rows, err := db.QueryContext(ctx, `SELECT plugin_name, schema_name, table_name, tenant_scoped
		FROM admin.plugin_tables ORDER BY plugin_name, table_name`)
	if err != nil {
		t.Fatalf("query admin.plugin_tables: %v", err)
	}
	defer rows.Close()

	type registryRow struct {
		plugin, schema, table string
		tenantScoped          bool
	}
	var regRows []registryRow
	for rows.Next() {
		var r registryRow
		if err := rows.Scan(&r.plugin, &r.schema, &r.table, &r.tenantScoped); err != nil {
			t.Fatalf("scan admin.plugin_tables row: %v", err)
		}
		regRows = append(regRows, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate admin.plugin_tables: %v", err)
	}

	// A floor, not a count: this must have real rows to examine, or every
	// assertion below passes vacuously. TenantScoped tables exist in this
	// tree today (e.g. plugins/kvstore), so zero rows means something
	// upstream of this test broke, not that the tree has nothing to check.
	if len(regRows) == 0 {
		t.Fatalf("admin.plugin_tables has zero rows after RunMigrations; this test " +
			"examined nothing. Either no plugin declares TenantScoped any more " +
			"(check plugin/migration.go's registerTenantScopedTables caller), or " +
			"RunMigrations silently did not run plugin migrations.")
	}

	for _, r := range regRows {
		hasTenantID, err := tableHasColumn(ctx, db, r.schema, r.table, "tenant_id")
		if err != nil {
			t.Errorf("%s: checking %s.%s for a tenant_id column: %v", r.plugin, r.schema, r.table, err)
			continue
		}
		switch {
		case r.tenantScoped && !hasTenantID:
			t.Errorf("%s: admin.plugin_tables says %s.%s is tenant_scoped=true, "+
				"but the live table has no tenant_id column. The registry row is "+
				"stale -- a later migration dropped or renamed the column without "+
				"updating registerTenantScopedTables' declaration, or the row was "+
				"hand-edited.", r.plugin, r.schema, r.table)
		case !r.tenantScoped && hasTenantID:
			t.Errorf("%s: admin.plugin_tables says %s.%s is tenant_scoped=false, "+
				"but the live table HAS a tenant_id column -- it looks tenant-scoped "+
				"by convention with no row-level security policy enforcing it. Add it "+
				"to the owning migration's TenantScoped list.", r.plugin, r.schema, r.table)
		}
	}
}

// tableHasColumn reports whether the named column exists on the named table
// in the given PostgreSQL schema, read from the live information_schema --
// never from any Go-source or migration-text declaration.
func tableHasColumn(ctx context.Context, db *sql.DB, schema, table, column string) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = $2 AND column_name = $3
		)`, schema, table, column).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("query information_schema.columns for %s.%s: %w", schema, table, err)
	}
	return exists, nil
}
