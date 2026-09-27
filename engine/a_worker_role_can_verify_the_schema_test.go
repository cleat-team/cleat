package engine

// A normal worker start VERIFIES the schema instead of migrating it (cleat#2117), and
// it does so on the RUNTIME connection -- the unprivileged role row-level security
// applies to, which is deliberately not the one that can run DDL. If that role cannot
// read schema_migrations and plugin_migrations, verification fails for every real
// deployment (a superuser test connection would never show it), so this asks as the
// role a worker connects as.

import (
	"context"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/migration"
	"github.com/cleat-team/cleat/plugin"
)

func TestTheUnprivilegedWorkerRoleCanVerifyTheSchema(t *testing.T) {
	owner := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { owner.Close() })
	testutil.SetupFullSchema(t, owner, testutil.DialectPostgres)
	applyPostgresProcedures(t, owner)
	applyAppRoleMigration(t, owner)
	worker := appRoleDB(t, owner)

	var current string
	if err := worker.QueryRow(`SELECT current_user`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	var super bool
	if err := worker.QueryRow(`SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil {
		t.Fatal(err)
	}
	if super {
		t.Fatalf("connected as %q, a superuser: this would pass whatever the grants say", current)
	}

	ctx := context.Background()
	runner := migration.NewRunner(worker, migration.DialectPostgres, "../migrations")
	st, err := runner.Verify(ctx)
	if err != nil {
		t.Fatalf("Verify as %s: %v", current, err)
	}
	if st.TrackingTableMissing || st.Behind() {
		t.Fatalf("Verify as %s reports a migrated schema as missing or behind: %+v", current, st)
	}

	// The plugin half, with a plugin that really has a migration: with none,
	// VerifyMigrations returns before reading anything and this would prove nothing.
	p := &verifyProbePlugin{migrations: []plugin.Migration{{Version: 1, Up: `CREATE TABLE cleat_2117_verify_probe (id int)`}}}
	lp := []*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}
	t.Cleanup(func() { _, _ = owner.Exec(`DROP TABLE IF EXISTS cleat_2117_verify_probe`) })
	if err := plugin.RunMigrations(ctx, owner, plugin.DialectPostgres, nil, lp); err != nil {
		t.Fatalf("apply the probe plugin's migration: %v", err)
	}
	ps, err := plugin.VerifyMigrations(ctx, worker, plugin.DialectPostgres, lp)
	if err != nil {
		t.Fatalf("plugin.VerifyMigrations as %s: %v", current, err)
	}
	if ps.Behind() || ps.TableMissing {
		t.Fatalf("plugin.VerifyMigrations as %s reports an applied plugin migration as pending: %+v", current, ps)
	}

	// Known-positive: the same read DOES report a migration that is not applied.
	p.migrations = append(p.migrations, plugin.Migration{Version: 2, Up: `SELECT 1`})
	ps, err = plugin.VerifyMigrations(ctx, worker, plugin.DialectPostgres, lp)
	if err != nil {
		t.Fatal(err)
	}
	if !ps.Behind() || len(ps.Pending) != 1 || ps.Pending[0] != p.Info().Name+" v2" {
		t.Fatalf("an unapplied plugin migration was not reported: %+v", ps)
	}
}

type verifyProbePlugin struct{ migrations []plugin.Migration }

func (p *verifyProbePlugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{Name: "cleat-2117-verify-probe"}
}
func (p *verifyProbePlugin) Init(ctx context.Context, env *plugin.Environment) error { return nil }
func (p *verifyProbePlugin) Migrations() []plugin.Migration                          { return p.migrations }
