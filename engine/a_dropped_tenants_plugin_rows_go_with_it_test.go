package engine

import (
	"context"
	"database/sql"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// admin.drop_tenant deleted from ten core relations and no plugin table, so a
// dropped tenant's rows in kv_store and every other plugin table survived
// indefinitely -- and since #1280 gave those tables a policy keyed on the
// tenant, they survived unreadable as well as undeleted. cleat#1289.
//
// The fix reads admin.plugin_tables, which plugin.RunMigrations now fills from
// each migration's TenantScoped declaration, so these two tests are the two
// halves: one that the registration happens, one that the drop uses it.

type dropTenantProbePlugin struct{}

func (dropTenantProbePlugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{Name: "drop-tenant-probe", Version: "1"}
}

func (dropTenantProbePlugin) Init(context.Context, *plugin.Environment) error { return nil }

func (dropTenantProbePlugin) Migrations() []plugin.Migration {
	return []plugin.Migration{{
		Version: 1,
		Up: `CREATE TABLE IF NOT EXISTS drop_tenant_probe (
			tenant_id UUID NOT NULL,
			k TEXT NOT NULL,
			PRIMARY KEY (tenant_id, k))`,
		TenantScoped: []string{"drop_tenant_probe"},
	}}
}

// runDropTenantProbeMigrations applies the probe plugin from scratch.
//
// It clears the plugin_migrations row first, and that is not tidiness. The
// bookkeeping row outlives the table: drop the table alone and RunMigrations
// sees version 1 as applied, skips it, and every assertion below then runs
// against a table that does not exist -- or, worse, against one left over from
// an earlier run with its rows still in it. Caught exactly that way.
func runDropTenantProbeMigrations(t *testing.T, ctx context.Context, adminDB *sql.DB) {
	t.Helper()
	// plugin_migrations is created by RunMigrations itself, so on a database
	// that has never run a plugin migration it does not exist yet and a bare
	// DELETE is `relation "plugin_migrations" does not exist`. That is not a
	// CI-only nicety: it passed on every local run, where an earlier run had
	// left the table behind, and failed on the first FRESH database CI built.
	// Guarding is also the honest statement -- if the table is not there,
	// there is no bookkeeping row to reset.
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS drop_tenant_probe`,
		`DO $$ BEGIN
			IF to_regclass('plugin_migrations') IS NOT NULL THEN
				DELETE FROM plugin_migrations WHERE plugin_name = 'drop-tenant-probe';
			END IF;
		END $$`,
		`DELETE FROM admin.plugin_tables WHERE plugin_name = 'drop-tenant-probe'`,
	} {
		if _, err := adminDB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("reset probe plugin (%s): %v", stmt, err)
		}
	}
}

func TestRunMigrationsRegistersATenantScopedTable(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	apply032DropTenantMigration(t, adminDB)
	ctx := context.Background()

	runDropTenantProbeMigrations(t, ctx, adminDB)
	// Both, and the registry row is the one that matters. Dropping the table
	// alone leaves an admin.plugin_tables row naming a table that is gone,
	// which every later admin.drop_tenant in this database then has to step
	// over -- and before the to_regclass guard it aborted on. A sibling test
	// here did exactly that, and the falsification of
	// TestDropTenantSurvivesARegistryRowWhoseTableIsGone went red naming the
	// LEFTOVER row rather than the one it seeds.
	defer adminDB.Exec(`DELETE FROM admin.plugin_tables WHERE plugin_name = 'drop-tenant-probe'`)
	defer adminDB.Exec(`DROP TABLE IF EXISTS drop_tenant_probe`)
	if err := plugin.RunMigrations(ctx, adminDB, plugin.DialectPostgres, nil,
		[]*plugin.LoadedPlugin{{Plugin: dropTenantProbePlugin{}, Healthy: true}}); err != nil {
		t.Fatalf("run plugin migrations: %v", err)
	}

	var schema string
	var scoped bool
	err := adminDB.QueryRowContext(ctx,
		`SELECT schema_name, tenant_scoped FROM admin.plugin_tables
		 WHERE plugin_name = 'drop-tenant-probe' AND table_name = 'drop_tenant_probe'`).
		Scan(&schema, &scoped)
	if err != nil {
		t.Fatalf("a table declared TenantScoped has no admin.plugin_tables row, so admin.drop_tenant "+
			"cannot find it and its rows outlive the tenant that owns them: %v", err)
	}
	if !scoped {
		t.Errorf("registered with tenant_scoped = false; admin.drop_tenant only deletes from rows where it is true")
	}
	// The schema has to be recorded, not assumed: --schema puts plugin tables
	// somewhere other than public, admin.plugin_tables stays in the admin
	// schema either way, and admin.drop_tenant carries no search_path of its
	// own (cleat#1363) -- so an unqualified name would resolve against the
	// caller's.
	if schema != "public" {
		t.Errorf("registered schema_name = %q, want %q for a default install", schema, "public")
	}
}

func TestDropTenantDeletesAPluginsTenantRows(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	apply032DropTenantMigration(t, adminDB)
	ctx := context.Background()

	runDropTenantProbeMigrations(t, ctx, adminDB)
	// Both, and the registry row is the one that matters. Dropping the table
	// alone leaves an admin.plugin_tables row naming a table that is gone,
	// which every later admin.drop_tenant in this database then has to step
	// over -- and before the to_regclass guard it aborted on. A sibling test
	// here did exactly that, and the falsification of
	// TestDropTenantSurvivesARegistryRowWhoseTableIsGone went red naming the
	// LEFTOVER row rather than the one it seeds.
	defer adminDB.Exec(`DELETE FROM admin.plugin_tables WHERE plugin_name = 'drop-tenant-probe'`)
	defer adminDB.Exec(`DROP TABLE IF EXISTS drop_tenant_probe`)
	if err := plugin.RunMigrations(ctx, adminDB, plugin.DialectPostgres, nil,
		[]*plugin.LoadedPlugin{{Plugin: dropTenantProbePlugin{}, Healthy: true}}); err != nil {
		t.Fatalf("run plugin migrations: %v", err)
	}

	const victim = "02d00000-0000-4000-8000-0000000012a9"
	const bystander = "02d00000-0000-4000-8000-0000000012b9"
	defer cleanupDropTenantExtras(t, ctx, adminDB, victim)
	defer cleanupDropTenantExtras(t, ctx, adminDB, bystander)

	for _, tn := range []string{victim, bystander} {
		if _, err := adminDB.ExecContext(ctx,
			`INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			tn, "drop-tenant-probe-"+tn[len(tn)-4:]); err != nil {
			t.Fatalf("seed tenant %s: %v", tn, err)
		}
		if _, err := adminDB.ExecContext(ctx,
			`INSERT INTO drop_tenant_probe (tenant_id, k) VALUES ($1, 'secret')`, tn); err != nil {
			t.Fatalf("seed plugin row for %s: %v", tn, err)
		}
	}

	// Every count below comes from adminDB, the superuser/owner connection, so
	// row-level security cannot filter it. A policy that merely HIDES a row
	// would otherwise make this test pass while the row is still there --
	// which is the exact state #1280 left a dropped tenant's plugin rows in.
	count := func(tn string) int {
		t.Helper()
		var n int
		if err := adminDB.QueryRowContext(ctx,
			`SELECT count(*) FROM drop_tenant_probe WHERE tenant_id = $1`, tn).Scan(&n); err != nil {
			t.Fatalf("count plugin rows for %s: %v", tn, err)
		}
		return n
	}

	// A table that was never seeded and a table that was correctly emptied
	// both count zero afterwards. cleat#1265 published "4 of 4 clean" off a
	// run where two of six seeds had failed, for exactly this reason.
	if got := count(victim); got != 1 {
		t.Fatalf("PRECONDITION FAILED: the victim has %d plugin rows before the drop, want 1 -- "+
			"a zero count afterwards would mean nothing", got)
	}
	if got := count(bystander); got != 1 {
		t.Fatalf("PRECONDITION FAILED: the bystander has %d plugin rows before the drop, want 1", got)
	}

	if _, err := adminDB.ExecContext(ctx, `SELECT admin.drop_tenant($1)`, victim); err != nil {
		t.Fatalf("admin.drop_tenant(victim): %v", err)
	}

	// The other half of the #1265 lesson, and the one that is easy to leave
	// out: prove the sweep RAN. A drop_tenant that silently did nothing
	// produces a surviving plugin row too, and reads as the same bug.
	var tenantRow int
	if err := adminDB.QueryRowContext(ctx,
		`SELECT count(*) FROM admin.tenants WHERE tenant_id = $1`, victim).Scan(&tenantRow); err != nil {
		t.Fatalf("count admin.tenants: %v", err)
	}
	if tenantRow != 0 {
		t.Fatalf("PRECONDITION FAILED: admin.drop_tenant left the victim's admin.tenants row behind, "+
			"so it did not run to completion and the plugin-row count below says nothing (count=%d)", tenantRow)
	}

	if got := count(victim); got != 0 {
		t.Errorf("the victim's rows in a TenantScoped plugin table survived admin.drop_tenant (count=%d). "+
			"Since #1280 gave that table a policy keyed on the tenant, they are now unreadable as well "+
			"as undeleted: nothing can see them and nothing will remove them", got)
	}
	if got := count(bystander); got != 1 {
		t.Errorf("dropping one tenant changed another tenant's plugin rows (bystander count=%d, want 1)", got)
	}
}

// A registry row can outlive its table -- a plugin dropped from the build, a
// reversed migration -- and nothing deletes the row. Before the to_regclass
// guard, the first such row made admin.drop_tenant raise
// `relation "public.x" does not exist` and tenant deletion stopped working
// for EVERY tenant, with a remedy no operator would guess.
//
// This is not a hypothetical: a stale row left by an earlier probe is exactly
// how it was found, on the first run after the loop was added.
func TestDropTenantSurvivesARegistryRowWhoseTableIsGone(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	apply032DropTenantMigration(t, adminDB)
	ctx := context.Background()

	const victim = "02d00000-0000-4000-8000-0000000012c9"
	defer cleanupDropTenantExtras(t, ctx, adminDB, victim)
	if _, err := adminDB.ExecContext(ctx,
		`INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, 'drop-tenant-stale')
		 ON CONFLICT DO NOTHING`, victim); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	if _, err := adminDB.ExecContext(ctx,
		`INSERT INTO admin.plugin_tables (plugin_name, schema_name, table_name, tenant_scoped)
		 VALUES ('gone-plugin', 'public', 'table_that_does_not_exist', true)
		 ON CONFLICT (plugin_name, schema_name, table_name) DO UPDATE SET tenant_scoped = true`); err != nil {
		t.Fatalf("seed stale registry row: %v", err)
	}
	defer adminDB.Exec(`DELETE FROM admin.plugin_tables WHERE plugin_name = 'gone-plugin'`)

	// The control: the row has to actually name a missing table, or this
	// passes for the wrong reason.
	var exists *string
	if err := adminDB.QueryRowContext(ctx,
		`SELECT to_regclass('public.table_that_does_not_exist')::text`).Scan(&exists); err != nil {
		t.Fatalf("to_regclass: %v", err)
	}
	if exists != nil {
		t.Fatalf("PRECONDITION FAILED: public.table_that_does_not_exist exists (%q), so the stale-row "+
			"case is not being exercised", *exists)
	}

	if _, err := adminDB.ExecContext(ctx, `SELECT admin.drop_tenant($1)`, victim); err != nil {
		t.Fatalf("admin.drop_tenant aborted on a registry row whose table is gone: %v\n\n"+
			"That blocks deletion of every tenant, not just this one, until somebody finds and "+
			"removes the row by hand.", err)
	}

	var tenantRow int
	if err := adminDB.QueryRowContext(ctx,
		`SELECT count(*) FROM admin.tenants WHERE tenant_id = $1`, victim).Scan(&tenantRow); err != nil {
		t.Fatalf("count admin.tenants: %v", err)
	}
	if tenantRow != 0 {
		t.Errorf("the drop returned without error but left the tenant behind (count=%d)", tenantRow)
	}
}
