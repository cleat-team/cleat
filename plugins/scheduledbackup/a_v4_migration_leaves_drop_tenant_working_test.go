package scheduledbackup

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// TestSchedulerBackupV4RegistryFlipDoesNotBreakLaterDropTenant reproduces
// cleat-review's exact incident with migrations.go's v4 registry-flip UPDATE
// (see the long comment on that migration): an earlier draft keyed the
// UPDATE on `WHERE plugin_name = 'scheduledbackup'`, but plugin.PluginInfo.Name
// for this plugin is "scheduled-backup" (with a hyphen, see plugin.go), and
// admin.plugin_tables' primary key is (plugin_name, schema_name, table_name).
// A wrong plugin_name matches zero rows: the UPDATE succeeds (0 rows
// affected is not an error), the column still gets dropped, and
// backup_config/backup_history are left in admin.plugin_tables marked
// tenant_scoped = true for a table that no longer has a tenant_id column.
//
// The consequence measured by cleat-review was not scoped to backup rows: it
// is a TOTAL admin.drop_tenant outage, for every tenant, because
// migrations/postgres/066's sweep reads every tenant_scoped row
// unconditionally and issues `DELETE FROM backup_config WHERE tenant_id = $1`
// before touching anything belonging to the tenant actually being dropped.
//
// This test runs v1 through v4 (the real upgrade path, not a fresh-database
// v4-only apply, since the incident is specifically about a registry row v2
// created surviving v4 incorrectly), then drops a tenant that owns no backup
// rows at all -- proving the sweep does not even reach backup_config/
// backup_history's stale registry row, let alone fail on it.
func TestSchedulerBackupV4RegistryFlipDoesNotBreakLaterDropTenant(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()

	ctx := context.Background()
	dialect := plugin.Dialect(string(testutil.DialectPostgres))
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	p := &Plugin{dialect: dialect, logger: quiet}
	if err := plugin.RunMigrations(ctx, db, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
		t.Fatalf("migrations v1-v4: %v", err)
	}

	// Precondition: v4 actually flipped the registry row Postgres carries
	// for these two tables. If this is still true, the DELETE below would
	// reference a column that no longer exists -- which is precisely the
	// bug, so asserting the precondition is what makes the rest of this
	// test mean anything.
	// schema_name = 'public', not current_schema(): the registry row v2
	// creates is always stamped with the literal schema the tables live in,
	// not whatever schema a later connection's search_path happens to
	// resolve first. Tier 2 Gate's role has 'cleat' ahead of 'public' in its
	// search_path (RLS testing needs it there), so current_schema() here
	// returned 'cleat' and this precondition check found 0 of 2 rows on a
	// migration that had in fact run correctly -- cleat-review, 2026-09-24.
	rows, err := db.QueryContext(ctx, `
		SELECT table_name, tenant_scoped FROM admin.plugin_tables
		WHERE schema_name = 'public' AND table_name IN ('backup_config', 'backup_history')
	`)
	if err != nil {
		t.Fatalf("reading admin.plugin_tables: %v", err)
	}
	seen := map[string]bool{}
	for rows.Next() {
		var name string
		var scoped bool
		if err := rows.Scan(&name, &scoped); err != nil {
			t.Fatalf("scanning admin.plugin_tables: %v", err)
		}
		seen[name] = true
		if scoped {
			t.Errorf("PRECONDITION FAILED: admin.plugin_tables still marks %s tenant_scoped after v4 -- "+
				"the registry-flip UPDATE did not match, so the DELETE below would hit "+
				"'column tenant_id does not exist' for every tenant, not just one with backup rows", name)
		}
	}
	rows.Close()
	if len(seen) != 2 {
		t.Fatalf("PRECONDITION FAILED: admin.plugin_tables has %d of the 2 expected rows (backup_config, "+
			"backup_history) -- v2 never registered them, so this test cannot tell v4's flip from v2 never "+
			"having run", len(seen))
	}

	// A bystander tenant that owns NO backup rows at all. The incident this
	// test reproduces is a TOTAL outage: the sweep fails on backup_config's
	// own stale registry row before it ever gets to anything this tenant
	// actually owns.
	bystander := uuid.New()
	if _, err := plugintest.ExecRebound(t, ctx, db, dialect,
		`INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`,
		bystander, "v4-registry-flip-bystander-"+bystander.String()[:8]); err != nil {
		t.Fatalf("seed admin.tenants: %v", err)
	}

	if _, err := db.ExecContext(ctx, `SELECT admin.drop_tenant($1, 'public')`, bystander); err != nil {
		t.Fatalf("admin.drop_tenant, no backup rows involved: %v -- if this names backup_config or "+
			"backup_history and 'tenant_id does not exist', the v4 registry-flip UPDATE is not matching "+
			"the row v2 created (cleat-review's incident)", err)
	}

	// Prove the drop actually ran, not merely that nothing raised.
	var remaining int
	if err := plugintest.QueryRowRebound(t, ctx, db, dialect,
		`SELECT count(*) FROM admin.tenants WHERE tenant_id = $1`,
		bystander).Scan(&remaining); err != nil {
		t.Fatalf("count admin.tenants: %v", err)
	}
	if remaining != 0 {
		t.Fatal("admin.drop_tenant left the bystander's admin.tenants row behind, so it did not run to " +
			"completion and this test proves nothing")
	}
}
