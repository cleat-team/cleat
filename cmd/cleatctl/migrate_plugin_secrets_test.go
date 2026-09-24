package main

// cleatctl migrate-plugin-secrets (cleat#1992), driven the way an operator
// drives it, on all three dialects: seed the OLD schema (still carrying the
// plaintext column), run the command, and confirm the value reads back from
// tenant secrets under the exact name the plugin will look it up under.
//
// THE FIXTURE RUNS ONLY THE PRE-DROP MIGRATIONS. Running the plugin's real,
// current Migrations() would apply the column-dropping migration in the same
// step this command exists to run BEFORE, which would make the plaintext
// column this test seeds never exist -- the schema this command has to
// operate against is a real state a deployed cluster passes through, not one
// this test invents.

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/datadogexport"
	"github.com/cleat-team/cleat/plugins/pagerdutyalert"
	"github.com/google/uuid"
)

// preV4DatadogExport reports only datadog-export's migrations older than the
// one that drops api_key (migrations.go v4), so a test fixture can seed the
// plaintext column this command exists to read.
type preV4DatadogExport struct{ *datadogexport.Plugin }

func (p preV4DatadogExport) Migrations() []plugin.Migration {
	var out []plugin.Migration
	for _, m := range p.Plugin.Migrations() {
		if m.Version < 4 {
			out = append(out, m)
		}
	}
	return out
}

// preV3PagerdutyAlert is preV4DatadogExport's mirror for pagerduty-alert's v3
// (migrations.go), which drops routing_key.
type preV3PagerdutyAlert struct{ *pagerdutyalert.Plugin }

func (p preV3PagerdutyAlert) Migrations() []plugin.Migration {
	var out []plugin.Migration
	for _, m := range p.Plugin.Migrations() {
		if m.Version < 3 {
			out = append(out, m)
		}
	}
	return out
}

func dialectToPluginDialect(d testutil.Dialect) plugin.Dialect {
	switch d {
	case testutil.DialectMySQL:
		return plugin.DialectMySQL
	case testutil.DialectMSSQL:
		return plugin.DialectMSSQL
	default:
		return plugin.DialectPostgres
	}
}

func migrateSecretsRun(t *testing.T, db *sql.DB, d testutil.Dialect, args ...string) (string, string, int) {
	t.Helper()
	// dialectByName(string(d)), NOT dialect{name: string(d)}: the latter
	// leaves Dialect.query at its zero value (plugin.Dialect is a string
	// type, so that zero value is "" -- not "postgres", which is a real,
	// distinct dialect constant). beginTenantTx checks the dialect by value
	// (engine/plugindb_tenant.go: "dialect != plugin.DialectPostgres &&
	// dialect != plugin.DialectMSSQL"), and "" satisfies neither -- so the
	// zero-valued literal silently skips ALL tenant scoping, on every
	// dialect. On Postgres/MySQL this is invisible because the test role
	// bypasses RLS (Postgres) or there is no RLS to bypass (MySQL); on SQL
	// Server it is not, and the command's discovery query saw zero rows
	// through an RLS block predicate that a correctly-scoped one bypasses.
	// See migratepluginsecrets.go's own comment on why the bypass is needed
	// at all.
	cd, err := dialectByName(string(d))
	if err != nil {
		t.Fatalf("dialectByName(%q): %v", d, err)
	}
	return runCapturingExit(t, func() {
		runMigratePluginSecrets(context.Background(), db, cd, args)
	})
}

func forEachMigrateSecretsDialect(t *testing.T, fn func(t *testing.T, db *sql.DB, d testutil.Dialect)) {
	for _, d := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		t.Run(string(d), func(t *testing.T) {
			db := testutil.TestDB(t, d)
			t.Cleanup(func() { db.Close() })
			testutil.SetupFullSchema(t, db, d)
			fn(t, db, d)
		})
	}
}

// deleteTenantSecretRow removes one tenant_secrets row, the same way
// resealFixture.deleteRow (reseal_secrets_test.go) does and for the same
// reason: on SQL Server the row is invisible unless SESSION_CONTEXT
// carries its tenant, and the setting and the DELETE must share one
// connection or the DELETE affects zero rows and reports success.
//
// A STANDALONE COPY RATHER THAN A SHARED HELPER, because tenant_secrets is
// the one table both this file's tests and reseal_secrets_test.go's write
// into, and TestResealSecretsCommandConvergesAndIsIdempotent failed once
// already from exactly this collision -- a secret this file wrote, sealed
// under a master key of its own choosing, sat in tenant_secrets past this
// test's own run and turned up UNREADABLE under the reseal test's ring. Every
// secret this file creates must clean up after itself for the same reason
// dd_config/pd_config rows must (clearTenantScopedTable): the fixture that
// leaves state behind is not this test's own next run, it is whichever test
// in this package runs after it and reads the same table.
func deleteTenantSecretRow(t *testing.T, db *sql.DB, d testutil.Dialect, tenant uuid.UUID, name string) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Errorf("cleanup: acquire a connection: %v", err)
		return
	}
	defer conn.Close()
	if d == testutil.DialectMSSQL {
		if _, err := conn.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tenant.String()); err != nil {
			t.Errorf("cleanup: scope the connection: %v", err)
			return
		}
	}
	q := map[testutil.Dialect]string{
		testutil.DialectPostgres: `DELETE FROM tenant_secrets WHERE tenant_id = $1 AND name = $2`,
		testutil.DialectMySQL:    `DELETE FROM tenant_secrets WHERE tenant_id = ? AND name = ?`,
		testutil.DialectMSSQL:    `DELETE FROM tenant_secrets WHERE tenant_id = @p1 AND name = @p2`,
	}[d]
	if _, err := conn.ExecContext(ctx, q, tenant.String(), name); err != nil {
		t.Errorf("cleanup: delete %q: %v", name, err)
	}
}

// clearTenantScopedTable removes every row of a TenantScoped plugin table
// before seeding. testutil.TestDB hands back a database that PERSISTS
// between runs (CLAUDE.md, "Ground rules for changes": SuiteTestDB's own
// warning applies here too, and dd_config/pd_config are exactly the kind of
// table an earlier failed run of THIS test leaves rows in), and a leftover
// row from a previous run is counted by the command under test exactly like
// a real one -- read once, this reported "would migrate 2 row(s)" where the
// test expected 1.
//
// plugin.AcrossAllTenants, for the same reason seedTenantScopedRow does not
// use a raw db.ExecContext: on SQL Server the block predicate applies to
// every principal, so an unscoped DELETE silently removes nothing.
func clearTenantScopedTable(t *testing.T, db *sql.DB, pd plugin.Dialect, table string) {
	t.Helper()
	adapter := &engine.SQLDBAdapter{DB: db, Dialect: pd}
	ctx := plugin.AcrossAllTenants(context.Background(), "test: clearing "+table+" before seeding")
	if _, err := adapter.Exec(ctx, `DELETE FROM `+table); err != nil {
		t.Fatalf("clear %s: %v", table, err)
	}
}

// seedTenantScopedRow inserts one row into a TenantScoped plugin table
// through plugin.ForTenant + SQLDBAdapter, the same path a plugin's own
// route handler writes through (plugins/datadogexport/routes.go,
// plugins/pagerdutyalert/routes.go) -- NOT a raw db.ExecContext. dd_config
// and pd_config carry a row-level policy from their own v2/v3 migrations
// (included by preV4DatadogExport/preV3PagerdutyAlert, which only exclude
// the LAST, column-dropping version), and on SQL Server that policy is a
// BLOCK predicate enforced against sysadmin and dbo alike -- an insert with
// no SESSION_CONTEXT('tenant_id') set is refused outright, which
// beginTenantTx (reached through SQLDBAdapter.Exec) is what sets.
func seedTenantScopedRow(t *testing.T, db *sql.DB, pd plugin.Dialect, tenant uuid.UUID, query string, args ...any) {
	t.Helper()
	adapter := &engine.SQLDBAdapter{DB: db, Dialect: pd}
	if _, err := adapter.Exec(plugin.ForTenant(context.Background(), tenant), plugin.Rebind(query, pd), args...); err != nil {
		t.Fatalf("seed row: %v", err)
	}
}

// The operator's happy path for datadog-export: seed the pre-drop schema
// with a plaintext api_key, run the command, and read the value back from
// tenant secrets under DatadogAPIKeySecretName(id) -- exactly the name
// exportForConfig looks it up under.
func TestMigratePluginSecretsMovesAnExistingDatadogAPIKey(t *testing.T) {
	forEachMigrateSecretsDialect(t, func(t *testing.T, db *sql.DB, d testutil.Dialect) {
		ctx := context.Background()
		pd := dialectToPluginDialect(d)
		p := preV4DatadogExport{&datadogexport.Plugin{}}
		if err := plugin.RunMigrations(ctx, db, pd, nil,
			[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
			t.Fatalf("pre-v4 datadog-export migrations: %v", err)
		}
		clearTenantScopedTable(t, db, pd, "dd_config")

		// engine.DefaultTenantUUID, not a fresh uuid.New(): tenant_secrets has
		// a foreign key to the tenants table, and SetupFullSchema seeds only
		// this one.
		tenant := uuid.MustParse(engine.DefaultTenantUUID)
		cfgID := uuid.New()
		seedTenantScopedRow(t, db, pd, tenant,
			`INSERT INTO dd_config (tenant_id, id, name, api_key) VALUES ($1, $2, $3, $4)`,
			tenant.String(), cfgID.String(), "seed", "dd-plaintext-key")
		secretName := datadogexport.DatadogAPIKeySecretName(cfgID)
		t.Cleanup(func() { deleteTenantSecretRow(t, db, d, tenant, secretName) })

		setRingEnv(t, ringKeyB64(0x51), "1", "", "")

		// Dry run: reports the row, writes nothing.
		out, errOut, code := migrateSecretsRun(t, db, d, "--plugin", "datadog-export", "--dry-run")
		if code != 0 {
			t.Fatalf("dry run exited %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
		}
		if !strings.Contains(out, "would migrate 1 row") {
			t.Errorf("dry run should report the one row found; got:\n%s", out)
		}
		store := engine.NewSecretStoreWithRing(db, string(d), mustRing(t, 0x51, 1))
		if _, err := store.GetSecret(tenantctx.With(ctx, tenant), tenant.String(), secretName); err == nil {
			t.Fatal("a DRY RUN wrote a readable secret")
		}

		// The real run.
		out, errOut, code = migrateSecretsRun(t, db, d, "--plugin", "datadog-export")
		if code != 0 {
			t.Fatalf("migrate exited %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
		}
		if !strings.Contains(out, "migrated 1 row") {
			t.Errorf("should report the one row migrated; got:\n%s", out)
		}
		got, err := store.GetSecret(tenantctx.With(ctx, tenant), tenant.String(), secretName)
		if err != nil || got != "dd-plaintext-key" {
			t.Fatalf("GetSecret(%q) = %q, %v, want \"dd-plaintext-key\", nil", secretName, got, err)
		}

		// Idempotent: running again does no harm and the value is unchanged.
		out, errOut, code = migrateSecretsRun(t, db, d, "--plugin", "datadog-export")
		if code != 0 || !strings.Contains(out, "migrated 1 row") {
			t.Fatalf("a second run: exit %d, want 0 with the same row migrated again\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
		}
		if got, err := store.GetSecret(tenantctx.With(ctx, tenant), tenant.String(), secretName); err != nil || got != "dd-plaintext-key" {
			t.Errorf("after a second run: GetSecret = %q, %v, want unchanged", got, err)
		}
	})
}

// pagerduty-alert's mirror of the test above, for routing_key.
func TestMigratePluginSecretsMovesAnExistingPagerdutyRoutingKey(t *testing.T) {
	forEachMigrateSecretsDialect(t, func(t *testing.T, db *sql.DB, d testutil.Dialect) {
		ctx := context.Background()
		pd := dialectToPluginDialect(d)
		p := preV3PagerdutyAlert{&pagerdutyalert.Plugin{}}
		if err := plugin.RunMigrations(ctx, db, pd, nil,
			[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
			t.Fatalf("pre-v3 pagerduty-alert migrations: %v", err)
		}
		clearTenantScopedTable(t, db, pd, "pd_config")

		tenant := uuid.MustParse(engine.DefaultTenantUUID)
		cfgID := uuid.New()
		seedTenantScopedRow(t, db, pd, tenant,
			`INSERT INTO pd_config (tenant_id, id, name, routing_key) VALUES ($1, $2, $3, $4)`,
			tenant.String(), cfgID.String(), "seed", "pd-plaintext-key")
		secretName := pagerdutyalert.PagerdutyRoutingKeySecretName(cfgID)
		t.Cleanup(func() { deleteTenantSecretRow(t, db, d, tenant, secretName) })

		setRingEnv(t, ringKeyB64(0x52), "1", "", "")

		out, errOut, code := migrateSecretsRun(t, db, d, "--plugin", "pagerduty-alert")
		if code != 0 {
			t.Fatalf("migrate exited %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
		}
		if !strings.Contains(out, "migrated 1 row") {
			t.Errorf("should report the one row migrated; got:\n%s", out)
		}
		store := engine.NewSecretStoreWithRing(db, string(d), mustRing(t, 0x52, 1))
		got, err := store.GetSecret(tenantctx.With(ctx, tenant), tenant.String(), secretName)
		if err != nil || got != "pd-plaintext-key" {
			t.Fatalf("GetSecret(%q) = %q, %v, want \"pd-plaintext-key\", nil", secretName, got, err)
		}
	})
}

func mustRing(t *testing.T, fill byte, version int) *engine.KeyRing {
	t.Helper()
	ring, err := engine.NewKeyRing(engine.VersionedKey{Version: version, Key: ringKeyRaw(fill)})
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	return ring
}

// The refusals need no database, so they run once, the same as
// TestResealSecretsCommandRefusals.
func TestMigratePluginSecretsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      [4]string
		args     []string
		wantCode int
		wantErr  string
	}{
		{"no plugin named", [4]string{ringKeyB64(0x53), "1", "", ""}, nil, 1, "--plugin is required"},
		{"unknown plugin", [4]string{ringKeyB64(0x53), "1", "", ""}, []string{"--plugin", "not-a-plugin"}, 1, `unknown --plugin "not-a-plugin"`},
		{"no key configured", [4]string{"", "", "", ""}, []string{"--plugin", "datadog-export"}, 1, "CLEAT_SECRET_MASTER_KEY is not set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setRingEnv(t, tc.env[0], tc.env[1], tc.env[2], tc.env[3])
			_, errOut, code := runCapturingExit(t, func() {
				// A nil *sql.DB is enough: every case here stops before the
				// first query, and a case that did not would panic, which is
				// a louder failure than a skip.
				runMigratePluginSecrets(context.Background(), nil, dialect{name: "postgres"}, tc.args)
			})
			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, tc.wantCode, errOut)
			}
			if tc.wantErr != "" && !strings.Contains(errOut, tc.wantErr) {
				t.Errorf("stderr should contain %q; got:\n%s", tc.wantErr, errOut)
			}
		})
	}
}
