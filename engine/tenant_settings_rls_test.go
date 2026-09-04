package engine

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// Per-tenant settings on PostgreSQL, where D1 grants multi-tenancy and the
// isolation question is therefore real.
//
// Three separate claims, each with its own test, because they fail
// independently:
//
//	1. the store reads what was written                (the feature works)
//	2. one tenant cannot read another's row            (the feature is safe)
//	3. dropping a tenant drops its settings            (the feature is tidy)
//
// (2) is the one with a trap in it. CLAUDE.md's standing warning is that a
// cross-tenant assertion can pass because of a layer other than the one under
// test -- and here there are two layers, the `tenant_id = $1` predicate in
// GetTenantSettings and the RLS policy 039 installs. A test that read through
// GetTenantSettings would pass with the policy deleted. So the isolation test
// below queries with NO tenant_id in the SQL text at all, leaving the policy as
// the only thing that can hide the row, and disables the policy to watch it
// stop hiding it.

const (
	settingsTenantA = "aaaaaaaa-0000-0000-0000-00000000000a"
	settingsTenantB = "bbbbbbbb-0000-0000-0000-00000000000b"
)

// resetSettingsFixture removes any rows a previous run left behind, BEFORE
// seeding rather than after.
//
// Not t.Cleanup: cleanup functions run after the test body's deferred calls, so
// a `defer owner.Close()` in the caller closes the pool first and every delete
// silently fails against a closed connection. Three rows survived a green run
// that way, and the next run failed on a duplicate key rather than on anything
// it was testing. Pre-cleaning also survives a crashed run, which cleanup does
// not.
//
// EVERY FK CHILD OF admin.tenants IS DELETED HERE, child before parent. There
// are three, and the one this helper used to name is the only one that did not
// need naming:
//
//	tenant_settings         039  ON DELETE CASCADE  -- goes anyway
//	admin.tenant_roles      001  no cascade         -- blocks the parent delete
//	admin.tenant_api_keys   001  no cascade         -- blocks the parent delete
//
// So the helper deleted the child the database would have deleted for it and
// omitted both that it would not. Re-derive the list with
//
//	grep -rn "REFERENCES admin.tenants" migrations/postgres/*.sql
//
// and add any new child here, because the failure mode is delayed: nothing goes
// wrong until a row exists in one of them.
//
// A row DOES exist, and no test in this file writes it. 002_defaults.sql
// backfills admin.tenant_roles for EVERY tenant present when it runs, and these
// tests leak their tenants -- a clean full-suite run leaves all three behind,
// since this helper only pre-cleans. So on the next database where migrations
// are applied from scratch (a fresh one, or the recreate CLAUDE.md mandates
// after a schema change), the backfill adopts the leaked fixtures, and from
// then on the DELETE below fails permanently with
//
//	pq: update or delete on table "tenants" violates foreign key constraint
//	    "tenant_roles_tenant_id_fkey" on table "tenant_roles" (23503)
//
// Measured 2026-09-04: all three of this file's tests failed that way on a
// re-used database while passing on a fresh one, which is why CI never saw it.
func resetSettingsFixture(t *testing.T, owner *sql.DB, tenantIDs ...string) {
	t.Helper()
	for _, id := range tenantIDs {
		for _, stmt := range []string{
			`DELETE FROM tenant_settings WHERE tenant_id = $1`,
			`DELETE FROM admin.tenant_api_keys WHERE tenant_id = $1`,
			`DELETE FROM admin.tenant_roles WHERE tenant_id = $1`,
			`DELETE FROM admin.tenants WHERE tenant_id = $1`,
		} {
			if _, err := owner.Exec(stmt, id); err != nil {
				t.Fatalf("resetting fixture for %s: %s: %v", id, stmt, err)
			}
		}
	}
}

func TestPostgresReadsTheTenantsOwnSettingsRow(t *testing.T) {
	owner := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, owner, testutil.DialectPostgres)
	defer owner.Close()
	ctx := context.Background()

	resetSettingsFixture(t, owner, settingsTenantA, settingsTenantB)
	for _, id := range []string{settingsTenantA, settingsTenantB} {
		if _, err := owner.Exec(
			`INSERT INTO admin.tenants (tenant_id, name, display_name)
			 VALUES ($1, $2, $2)`, id, "settings-"+id[:8]); err != nil {
			t.Fatalf("seeding tenant %s: %v", id, err)
		}
	}

	if _, err := owner.Exec(`
		INSERT INTO tenant_settings (tenant_id, wasm_wall_clock_ceiling_ms, host_retry_budget_ms)
		VALUES ($1, 5000, 9000)`, settingsTenantA); err != nil {
		t.Fatalf("writing tenant A's settings: %v", err)
	}

	store := NewPostgresStore(owner)

	got, err := store.WithTenant(settingsTenantA).GetTenantSettings(ctx)
	if err != nil {
		t.Fatalf("reading tenant A's settings: %v", err)
	}
	if got.WasmWallClockCeiling != 5*time.Second {
		t.Errorf("wall clock ceiling = %v, want 5s", got.WasmWallClockCeiling)
	}
	if got.HostRetryBudget != 9*time.Second {
		t.Errorf("host retry budget = %v, want 9s", got.HostRetryBudget)
	}
	if got.WasmInstanceTimeout != 0 {
		t.Errorf("instance timeout = %v, want 0 -- the column was left NULL, and "+
			"NULL must resolve to 'no override' rather than to any number",
			got.WasmInstanceTimeout)
	}

	// Tenant B wrote nothing. Its read must succeed and return the zero value,
	// not an error: a tenant with no overrides is the common case, and failing
	// here would fail every workflow on an unconfigured deployment.
	empty, err := store.WithTenant(settingsTenantB).GetTenantSettings(ctx)
	if err != nil {
		t.Fatalf("reading a tenant with no settings row returned an error: %v", err)
	}
	if empty != (TenantSettings{}) {
		t.Errorf("a tenant with no row got %+v, want the zero value.\n\n"+
			"This is also the isolation check at the API level: tenant B must "+
			"not be reading tenant A's row.", empty)
	}
}

func TestATenantCannotSeeAnotherTenantsSettingsRow(t *testing.T) {
	owner := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, owner, testutil.DialectPostgres)
	defer owner.Close()

	applyAppRoleMigration(t, owner)
	appDB := appRoleDB(t, owner)

	resetSettingsFixture(t, owner, settingsTenantA, settingsTenantB)
	for _, id := range []string{settingsTenantA, settingsTenantB} {
		if _, err := owner.Exec(
			`INSERT INTO admin.tenants (tenant_id, name, display_name)
			 VALUES ($1, $2, $2)`, id, "settings-rls-"+id[:8]); err != nil {
			t.Fatalf("seeding tenant %s: %v", id, err)
		}
	}
	if _, err := owner.Exec(`
		INSERT INTO tenant_settings (tenant_id, wasm_wall_clock_ceiling_ms)
		VALUES ($1, 5000), ($2, 7000)`,
		settingsTenantA, settingsTenantB); err != nil {
		t.Fatalf("seeding settings rows: %v", err)
	}

	// Confirm the premise before measuring anything: cleat_app must be able to
	// SELECT this table at all. 039 issues no GRANT, relying on
	// 005_app_role.sql's ALTER DEFAULT PRIVILEGES to cover tables the migration
	// role creates later. That is a claim about PostgreSQL behaviour and not
	// visible in either file, so it is checked rather than assumed -- if it
	// were false, every assertion below would "pass" on a permission error.
	countVisible := func(t *testing.T, tenantID string) int {
		t.Helper()
		tx, err := appDB.Begin()
		if err != nil {
			t.Fatalf("begin as cleat_app: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`SELECT set_config('cleat.tenant_id', $1, true)`, tenantID); err != nil {
			t.Fatalf("setting the RLS tenant context: %v", err)
		}
		// No tenant_id anywhere in this query. The RLS policy is the only thing
		// that can restrict what comes back, so a count of 1 means the policy
		// filtered -- not that the query did.
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM tenant_settings`).Scan(&n); err != nil {
			t.Fatalf("counting settings rows as cleat_app: %v\n\n"+
				"If this is a permission error, 005_app_role.sql's ALTER DEFAULT "+
				"PRIVILEGES did not reach migrations/postgres/039_tenant_settings.sql "+
				"and that migration needs an explicit GRANT.", err)
		}
		return n
	}

	if n := countVisible(t, settingsTenantA); n != 1 {
		t.Fatalf("tenant A sees %d settings rows, want exactly 1 (its own).\n\n"+
			"Two tenants have rows. Seeing 2 means tenant_isolation_settings is "+
			"not filtering; seeing 0 means the fixture did not land.", n)
	}
	if n := countVisible(t, settingsTenantB); n != 1 {
		t.Fatalf("tenant B sees %d settings rows, want exactly 1", n)
	}

	// Break the specific layer and watch: swap the tenant predicate for a
	// wide-open one and confirm the same query now sees BOTH rows.
	//
	// Replacing the predicate rather than dropping the policy, and the
	// difference is the whole value of the control. PostgreSQL defaults to DENY
	// when RLS is enabled with no policy at all, so a dropped policy makes this
	// query return 0 -- a change, but not the change that distinguishes "the
	// tenant predicate is filtering" from "some other layer is". The first
	// version of this test asserted >= 2 after a DROP and failed against a
	// correct implementation, which is the control doing its job on itself.
	//
	// USING (true) is also the exact scenario CLAUDE.md names: a cross-tenant
	// assertion that passes against a wide-open security policy. If the counts
	// above survive this, they were never coming from the policy.
	if _, err := owner.Exec(`DROP POLICY tenant_isolation_settings ON tenant_settings`); err != nil {
		t.Fatalf("widening the policy for the negative control: %v", err)
	}
	if _, err := owner.Exec(
		`CREATE POLICY tenant_isolation_settings ON tenant_settings FOR ALL USING (true)`); err != nil {
		t.Fatalf("widening the policy for the negative control: %v", err)
	}
	unfiltered := countVisible(t, settingsTenantA)
	if _, err := owner.Exec(`DROP POLICY tenant_isolation_settings ON tenant_settings`); err != nil {
		t.Fatalf("restoring the policy: %v -- LEAVING THE TABLE WIDE OPEN, "+
			"recreate the test database", err)
	}
	if _, err := owner.Exec(`
		CREATE POLICY tenant_isolation_settings ON tenant_settings
			FOR ALL USING (tenant_id = cleat.assert_tenant_set())`); err != nil {
		t.Fatalf("restoring the policy: %v -- LEAVING THE TABLE UNPROTECTED, "+
			"recreate the test database", err)
	}

	if unfiltered < 2 {
		t.Errorf("with tenant_isolation_settings widened to USING (true), tenant A "+
			"still saw only %d row(s).\n\n"+
			"The isolation assertions above therefore proved nothing about the "+
			"policy -- some other layer was hiding tenant B's row, and this test "+
			"would stay green if 039 shipped with no policy at all. That is the "+
			"exact shape CLAUDE.md warns about.", unfiltered)
	}

	if n := countVisible(t, settingsTenantA); n != 1 {
		t.Errorf("after restoring the policy tenant A sees %d rows, want 1", n)
	}
}

func TestDroppingATenantDropsItsSettings(t *testing.T) {
	owner := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, owner, testutil.DialectPostgres)
	applyPostgresProcedures(t, owner)
	defer owner.Close()

	const id = "cccccccc-0000-0000-0000-00000000000c"
	resetSettingsFixture(t, owner, id)
	if _, err := owner.Exec(
		`INSERT INTO admin.tenants (tenant_id, name, display_name)
		 VALUES ($1, 'settings-cascade', 'settings-cascade')`, id); err != nil {
		t.Fatalf("seeding the tenant: %v", err)
	}
	if _, err := owner.Exec(`
		INSERT INTO tenant_settings (tenant_id, wasm_wall_clock_ceiling_ms)
		VALUES ($1, 5000)`, id); err != nil {
		t.Fatalf("seeding the settings row: %v", err)
	}

	if _, err := owner.Exec(`SELECT admin.drop_tenant($1)`, id); err != nil {
		t.Fatalf("admin.drop_tenant: %v", err)
	}

	var n int
	if err := owner.QueryRow(
		`SELECT count(*) FROM tenant_settings WHERE tenant_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("counting settings rows after the drop: %v", err)
	}
	if n != 0 {
		t.Errorf("%d settings row(s) survived admin.drop_tenant.\n\n"+
			"039 relies on ON DELETE CASCADE rather than on a fifteenth DELETE "+
			"inside admin.drop_tenant (032). Two things could break that and both "+
			"matter: the foreign key could be missing, or -- the subtler one -- "+
			"referential-integrity actions could turn out to be subject to the "+
			"FORCE'd row-level security policy on this table, which is the "+
			"assumption 039's comment states and this test exists to check.", n)
	}
}

func TestTheDatabaseRefusesANonPositiveTenantLimit(t *testing.T) {
	owner := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, owner, testutil.DialectPostgres)
	defer owner.Close()

	const id = "dddddddd-0000-0000-0000-00000000000d"
	resetSettingsFixture(t, owner, id)
	if _, err := owner.Exec(
		`INSERT INTO admin.tenants (tenant_id, name, display_name)
		 VALUES ($1, 'settings-check', 'settings-check')`, id); err != nil {
		t.Fatalf("seeding the tenant: %v", err)
	}

	// Zero is the dangerous value, not merely an invalid one: `ceiling > 0` is
	// false at the point of use, so a stored 0 reads as UNBOUNDED and hands the
	// tenant more than the operator granted. The clamp cannot defend against
	// it, because there is nothing left to clamp.
	for _, v := range []int64{0, -1} {
		_, err := owner.Exec(`
			INSERT INTO tenant_settings (tenant_id, wasm_wall_clock_ceiling_ms)
			VALUES ($1, $2)`, id, v)
		if err == nil {
			t.Fatalf("the database accepted wasm_wall_clock_ceiling_ms = %d.\n\n"+
				"A tenant storing 0 unbounds its own wall-clock limit, which is "+
				"the one direction per-tenant settings must not permit.", v)
		}
		if !strings.Contains(err.Error(), "ck_tenant_settings_wall_clock_positive") {
			t.Errorf("the insert of %d failed, but not on the CHECK constraint: %v\n\n"+
				"Some other failure means this test is no longer measuring the "+
				"constraint.", v, err)
		}
	}

	// The control: a positive value must still be accepted. Without it, a
	// migration that made the column impossible to write at all would pass the
	// loop above.
	if _, err := owner.Exec(`
		INSERT INTO tenant_settings (tenant_id, wasm_wall_clock_ceiling_ms)
		VALUES ($1, 5000)`, id); err != nil {
		t.Fatalf("the CHECK constraint rejected a legitimate value: %v", err)
	}
}

// A fourth claim, about the helper the three above depend on rather than about
// tenant settings: resetSettingsFixture must get through a tenant that has FK
// children which do not cascade.
//
// This is the failure the file actually hit. It is invisible on a fresh
// database -- nothing writes admin.tenant_roles here, 002_defaults.sql's
// backfill does, on the next database built from scratch while these tests'
// leaked tenants are present. So the test below writes the child ROWS directly
// rather than trying to stage a migration re-run: what matters is that a child
// exists, not which pass of which migration put it there.
//
// ONE CHILD PER SUBTEST, AND ONE TENANT PER SUBTEST. Seeding both children at
// once would let a single missing DELETE be blamed on either constraint, and a
// falsification that fires two assertions proves neither.
//
// The separate tenants are the half that is easy to leave out, and leaving it
// out was measured here rather than reasoned about: with both subtests sharing
// one tenant, removing the admin.tenant_roles DELETE reddened BOTH -- the roles
// subtest failed, leaving its row behind, and the api_keys subtest then failed
// on its OWN opening reset, citing tenant_roles_tenant_id_fkey. Two red
// subtests, one cause, and the api_keys one named a constraint it does not
// test. Distinct tenants make each subtest's leftovers unreachable from the
// other, so removing one DELETE reddens exactly one subtest with exactly the
// constraint that subtest names.
func TestResetSettingsFixtureClearsTheChildrenThatDoNotCascade(t *testing.T) {
	for _, tc := range []struct {
		child      string
		tenant     string
		constraint string
		seed       string
		args       func(id string) []any
	}{
		{
			child:      "admin.tenant_roles",
			tenant:     "cccccccc-0000-0000-0000-00000000000c",
			constraint: "tenant_roles_tenant_id_fkey",
			seed: `INSERT INTO admin.tenant_roles (tenant_id, role_name, password)
			       VALUES ($1, $2, 'not-a-real-password')`,
			args: func(id string) []any { return []any{id, "cleat_test_reset_fixture_role"} },
		},
		{
			child:      "admin.tenant_api_keys",
			tenant:     "ffffffff-0000-0000-0000-00000000000f",
			constraint: "tenant_api_keys_tenant_id_fkey",
			seed:       `INSERT INTO admin.tenant_api_keys (tenant_id, key_hash) VALUES ($1, $2)`,
			args:       func(id string) []any { return []any{id, []byte("not-a-real-hash")} },
		},
	} {
		t.Run(tc.child, func(t *testing.T) {
			owner := testutil.TestDB(t, testutil.DialectPostgres)
			testutil.SetupFullSchema(t, owner, testutil.DialectPostgres)
			defer owner.Close()

			resetSettingsFixture(t, owner, tc.tenant)
			// The NAME must be per-subtest too, not just the tenant_id.
			// admin.tenants carries a UNIQUE on name, so with a shared literal
			// the second subtest failed on tenants_name_key the moment the
			// first one's tenant survived -- contamination through a different
			// column than the one the separate IDs were meant to isolate.
			name := "settings-reset-" + tc.tenant[:8]
			if _, err := owner.Exec(
				`INSERT INTO admin.tenants (tenant_id, name, display_name)
				 VALUES ($1, $2, $2)`, tc.tenant, name); err != nil {
				t.Fatalf("seeding tenant: %v", err)
			}
			if _, err := owner.Exec(tc.seed, tc.args(tc.tenant)...); err != nil {
				t.Fatalf("seeding %s: %v", tc.child, err)
			}

			// The row must really be there, or the reset below clears a tenant
			// with no children and passes without testing anything.
			var n int
			if err := owner.QueryRow(
				`SELECT count(*) FROM `+tc.child+` WHERE tenant_id = $1`, tc.tenant).Scan(&n); err != nil {
				t.Fatalf("counting %s: %v", tc.child, err)
			}
			if n != 1 {
				t.Fatalf("fixture not staged: %s holds %d rows for the tenant, want 1", tc.child, n)
			}

			// Fatalfs with a violation of tc.constraint if the helper does not
			// delete this child before the parent.
			resetSettingsFixture(t, owner, tc.tenant)

			if err := owner.QueryRow(
				`SELECT count(*) FROM admin.tenants WHERE tenant_id = $1`, tc.tenant).Scan(&n); err != nil {
				t.Fatalf("counting tenants: %v", err)
			}
			if n != 0 {
				t.Fatalf("tenant survived the reset: %d rows, want 0 (%s should have been cleared first)",
					n, tc.constraint)
			}
		})
	}
}
