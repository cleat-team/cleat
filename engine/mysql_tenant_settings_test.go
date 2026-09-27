package engine

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// Per-tenant settings on MySQL, which is where the uniform API of 3.94 step 2's
// option C meets tiers.yaml D1: the table ships here too, with the same shape
// and the same read path, and it holds the single tenant's row -- the
// deployment's settings.
//
// There is no isolation test on this dialect, and that is a consequence rather
// than a gap. migrations/mysql/038_single_tenant_guard.sql refuses a second
// tenant outright, so there is no other tenant whose row could leak. The
// PostgreSQL and SQL Server siblings carry isolation proofs because on those
// dialects the premise does not hold.
//
// What IS tested here is the CHECK constraint, and specifically on this
// dialect, because MySQL is the one that can silently not enforce it: CHECK is
// parsed and ignored before 8.0.16. The CI image is 8.4.11, but "the image we
// pin today enforces it" is not the same claim as "the constraint holds", and
// the constraint is a privilege boundary -- a stored 0 reads as unbounded and
// hands the tenant more than the operator granted.
func TestMySQLTenantSettings(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MYSQL") == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
	}

	ctx := context.Background()
	db := testutil.TestDB(t, testutil.DialectMySQL)
	testutil.SetupFullSchema(t, db, testutil.DialectMySQL)
	defer db.Close()

	// The default tenant, seeded by 002_defaults and -- since 038 -- the only
	// one MySQL will accept.
	const tenant = DefaultTenantUUID

	if _, err := db.ExecContext(ctx,
		`DELETE FROM tenant_settings WHERE tenant_id = ?`, tenant); err != nil {
		t.Fatalf("clearing the settings row: %v", err)
	}

	store := NewMySQLStore(db).WithTenant(tenant)

	// No row yet: the zero value, not an error.
	empty, err := store.GetTenantSettings(ctx)
	if err != nil {
		t.Fatalf("reading a tenant with no settings row returned an error: %v", err)
	}
	if empty != (TenantSettings{}) {
		t.Errorf("a tenant with no row got %+v, want the zero value", empty)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO tenant_settings (tenant_id, wasm_wall_clock_ceiling_ms, wasm_instance_timeout_ms)
		VALUES (?, 5000, 3000)`, tenant); err != nil {
		t.Fatalf("writing the settings row: %v", err)
	}

	got, err := store.GetTenantSettings(ctx)
	if err != nil {
		t.Fatalf("reading the settings row: %v", err)
	}
	if got.WasmWallClockCeiling != 5*time.Second {
		t.Errorf("wall clock ceiling = %v, want 5s", got.WasmWallClockCeiling)
	}
	if got.WasmInstanceTimeout != 3*time.Second {
		t.Errorf("instance timeout = %v, want 3s", got.WasmInstanceTimeout)
	}
	if got.HostRetryBudget != 0 {
		t.Errorf("host retry budget = %v, want 0 -- the column was left NULL, and "+
			"NULL must resolve to 'no override' rather than to any number",
			got.HostRetryBudget)
	}

	if _, err := db.ExecContext(ctx,
		`DELETE FROM tenant_settings WHERE tenant_id = ?`, tenant); err != nil {
		t.Fatalf("clearing the settings row before the constraint check: %v", err)
	}

	for _, v := range []int64{0, -1} {
		_, err := db.ExecContext(ctx, `
			INSERT INTO tenant_settings (tenant_id, wasm_wall_clock_ceiling_ms)
			VALUES (?, ?)`, tenant, v)
		if err == nil {
			_, _ = db.ExecContext(ctx, `DELETE FROM tenant_settings WHERE tenant_id = ?`, tenant)
			t.Fatalf("MySQL accepted wasm_wall_clock_ceiling_ms = %d.\n\n"+
				"CHECK constraints are parsed and IGNORED before MySQL 8.0.16, so "+
				"this is what an old server looks like -- the column is writable "+
				"with a value that unbounds the tenant's own limit. Check the "+
				"server version before assuming the migration is wrong.", v)
		}
		if !strings.Contains(err.Error(), "ck_tenant_settings_wall_clock_positive") {
			t.Errorf("the insert of %d failed, but not on the CHECK constraint: %v", v, err)
		}
	}

	// The control. Without it, a migration that made the column impossible to
	// write at all would pass the loop above.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tenant_settings (tenant_id, wasm_wall_clock_ceiling_ms)
		VALUES (?, 5000)`, tenant); err != nil {
		t.Fatalf("the CHECK constraint rejected a legitimate value: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM tenant_settings WHERE tenant_id = ?`, tenant); err != nil {
		t.Fatalf("clearing the settings row: %v", err)
	}
}
