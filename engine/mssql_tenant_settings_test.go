package engine

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// Per-tenant settings on SQL Server, the other dialect where D1 grants
// multi-tenancy and the isolation question is therefore real.
//
// The PostgreSQL equivalent is engine/tenant_settings_rls_test.go. The shape of
// the isolation proof is the same and the mechanism is not: PostgreSQL has an
// RLS policy that can be widened to USING (true), SQL Server has a security
// policy that can be turned off with STATE = OFF. What matters in both is that
// the query used to observe the effect names no tenant_id at all, so the
// policy is the only layer that can be doing the filtering.
//
// A filter predicate hides rows rather than raising, so a broken one looks
// exactly like a tenant with no overrides -- the flag defaults, and a green
// test. That is why the disable-and-watch step below is not optional.
func TestMSSQLTenantSettingsAreIsolatedByTheSecurityPolicy(t *testing.T) {
	dsn := os.Getenv("CLEAT_TEST_MSSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL integration test in short mode")
	}

	ctx := context.Background()
	adminDB := testutil.MSSQLTestDB(t)
	t.Cleanup(func() { adminDB.Close() })
	testutil.SetupMSSQLFullSchema(t, adminDB)

	const (
		tenantA = "eeeeeeee-eeee-4eee-eeee-eeeeeeeeeeee"
		tenantB = "ffffffff-ffff-4fff-ffff-ffffffffffff"
	)

	// Pre-clean rather than clean up afterwards: a cleanup registered here runs
	// after this function's deferred closes, so it would delete through a
	// closed pool. The PostgreSQL sibling learned that the expensive way.
	for _, id := range []string{tenantA, tenantB} {
		if _, err := adminDB.ExecContext(ctx,
			`DELETE FROM dbo.tenant_settings WHERE tenant_id = @p1`, id); err != nil {
			t.Fatalf("clearing settings for %s: %v", id, err)
		}
		if _, err := adminDB.ExecContext(ctx,
			`DELETE FROM admin.tenants WHERE tenant_id = @p1`, id); err != nil {
			t.Fatalf("clearing tenant %s: %v", id, err)
		}
	}
	for i, id := range []string{tenantA, tenantB} {
		if _, err := adminDB.ExecContext(ctx,
			`INSERT INTO admin.tenants (tenant_id, name, display_name) VALUES (@p1, @p2, @p2)`,
			id, "settings-mssql-"+string(rune('a'+i))); err != nil {
			t.Fatalf("seeding tenant %s: %v", id, err)
		}
	}
	if _, err := adminDB.ExecContext(ctx, `
		INSERT INTO dbo.tenant_settings (tenant_id, wasm_wall_clock_ceiling_ms)
		VALUES (@p1, 5000), (@p2, 7000)`, tenantA, tenantB); err != nil {
		t.Fatalf("seeding settings rows: %v", err)
	}

	factory := NewMSSQLStoreFactory(dsn)
	defer factory.Close()

	storeA, closerA, err := factory.OpenStore(ctx, tenantA)
	if err != nil {
		t.Fatalf("OpenStore(A): %v", err)
	}
	defer closerA.Close()
	storeB, closerB, err := factory.OpenStore(ctx, tenantB)
	if err != nil {
		t.Fatalf("OpenStore(B): %v", err)
	}
	defer closerB.Close()

	readerA, ok := storeA.(TenantSettingsReader)
	if !ok {
		t.Fatal("the MSSQL store no longer implements TenantSettingsReader, so " +
			"every tenant on SQL Server silently gets the worker flags")
	}
	readerB := storeB.(TenantSettingsReader)

	gotA, err := readerA.GetTenantSettings(ctx)
	if err != nil {
		t.Fatalf("reading tenant A's settings: %v", err)
	}
	gotB, err := readerB.GetTenantSettings(ctx)
	if err != nil {
		t.Fatalf("reading tenant B's settings: %v", err)
	}
	if gotA.WasmWallClockCeiling != 5*time.Second {
		t.Errorf("tenant A's wall clock ceiling = %v, want 5s", gotA.WasmWallClockCeiling)
	}
	if gotB.WasmWallClockCeiling != 7*time.Second {
		t.Errorf("tenant B's wall clock ceiling = %v, want 7s", gotB.WasmWallClockCeiling)
	}
	if gotA.WasmWallClockCeiling == gotB.WasmWallClockCeiling {
		t.Fatalf("both tenants resolved to %v.\n\n"+
			"Asserting the difference, not that one tenant's value is honoured: a "+
			"read that ignored the tenant entirely would satisfy one of the two "+
			"checks above and fail here.", gotA.WasmWallClockCeiling)
	}

	// The isolation claim, on the pool the store actually uses. No tenant_id in
	// the query text, so dbo.TenantFilter_Settings is the only thing that can
	// restrict the result.
	poolA, err := factory.getOrCreateTenantPool(ctx, tenantA)
	if err != nil {
		t.Fatalf("getOrCreateTenantPool(A): %v", err)
	}
	countVisible := func(t *testing.T) int {
		t.Helper()
		var n int
		if err := poolA.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM dbo.tenant_settings`).Scan(&n); err != nil {
			t.Fatalf("counting settings rows on tenant A's pool: %v", err)
		}
		return n
	}

	// Confirm the policy is on before believing anything it does. A policy that
	// does not exist and a policy that filters perfectly are indistinguishable
	// from the count alone when the other tenant's row happens to be absent.
	var enabled bool
	if err := adminDB.QueryRowContext(ctx,
		`SELECT is_enabled FROM sys.security_policies WHERE name = N'TenantFilter_Settings'`,
	).Scan(&enabled); err != nil {
		t.Fatalf("looking up TenantFilter_Settings: %v\n\n"+
			"migrations/mssql/042_tenant_settings.sql creates it. If it is missing, "+
			"dbo.tenant_settings has no isolation on this dialect at all.", err)
	}
	if !enabled {
		t.Fatal("TenantFilter_Settings exists but is disabled, so it filters nothing")
	}

	if n := countVisible(t); n != 1 {
		t.Fatalf("tenant A sees %d settings rows, want exactly 1 (its own). Two "+
			"tenants have rows, so 2 means the policy is not filtering and 0 means "+
			"the session context is missing.", n)
	}

	// Break the specific layer and watch.
	if _, err := adminDB.ExecContext(ctx,
		`ALTER SECURITY POLICY dbo.TenantFilter_Settings WITH (STATE = OFF)`); err != nil {
		t.Fatalf("disabling the policy for the negative control: %v", err)
	}
	unfiltered := countVisible(t)
	if _, err := adminDB.ExecContext(ctx,
		`ALTER SECURITY POLICY dbo.TenantFilter_Settings WITH (STATE = ON)`); err != nil {
		t.Fatalf("re-enabling the policy: %v -- LEAVING dbo.tenant_settings "+
			"UNPROTECTED, recreate the test database", err)
	}

	if unfiltered < 2 {
		t.Errorf("with TenantFilter_Settings disabled, tenant A's pool still saw "+
			"only %d row(s).\n\n"+
			"The count above therefore proved nothing about the policy: something "+
			"else was hiding tenant B's row, and this test would stay green if 042 "+
			"shipped without the policy at all.", unfiltered)
	}
	if n := countVisible(t); n != 1 {
		t.Errorf("after re-enabling the policy tenant A sees %d rows, want 1", n)
	}
}
