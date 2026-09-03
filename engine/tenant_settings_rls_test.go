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
func resetSettingsFixture(t *testing.T, owner *sql.DB, tenantIDs ...string) {
	t.Helper()
	for _, id := range tenantIDs {
		if _, err := owner.Exec(`DELETE FROM tenant_settings WHERE tenant_id = $1`, id); err != nil {
			t.Fatalf("clearing settings for %s: %v", id, err)
		}
		if _, err := owner.Exec(`DELETE FROM admin.tenants WHERE tenant_id = $1`, id); err != nil {
			t.Fatalf("clearing tenant %s: %v", id, err)
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
