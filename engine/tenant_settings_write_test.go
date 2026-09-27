package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestAStaleWriteIsRefusedRatherThanApplied is the property the whole
// read-modify-write shape exists for.
//
// Without it the write is last-write-wins and loses changes in SILENCE: two
// operators, one raising a limit and one lowering a different one, and
// whichever commits second erases the other's field with neither seeing an
// error. Nothing fails, and the row simply stops reflecting one of the two
// decisions.
//
// Cadence's configStorePersistenceTest.go::TestUpdateVersionCollisionFailure is
// the upstream case for exactly this, and it was unportable to cleat only
// because the operation did not exist yet. cleat#1187.
func TestAStaleWriteIsRefusedRatherThanApplied(t *testing.T) {
	store, teardown := postgresOnly(t)
	defer teardown()
	ctx := context.Background()

	// First write: no row yet.
	_, rev0, err := store.ReadTenantSettingsForUpdate(ctx)
	if err != nil {
		t.Fatalf("initial read: %v", err)
	}
	if rev0.Existed {
		t.Fatalf("expected no row to start from, got one")
	}
	if err := store.WriteTenantSettings(ctx,
		TenantSettings{HostRetryBudget: 5 * time.Second}, rev0); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Operator A reads.
	aSettings, aRev, err := store.ReadTenantSettingsForUpdate(ctx)
	if err != nil {
		t.Fatalf("A read: %v", err)
	}

	// Operator B reads the same row, changes a DIFFERENT field, and commits.
	bSettings, bRev, err := store.ReadTenantSettingsForUpdate(ctx)
	if err != nil {
		t.Fatalf("B read: %v", err)
	}
	bSettings.WasmWallClockCeiling = 30 * time.Second
	if err := store.WriteTenantSettings(ctx, bSettings, bRev); err != nil {
		t.Fatalf("B write: %v", err)
	}

	// A now writes, holding the revision it read before B committed.
	aSettings.HostRetryBudget = 9 * time.Second
	err = store.WriteTenantSettings(ctx, aSettings, aRev)
	if !errors.Is(err, ErrTenantSettingsConflict) {
		t.Fatalf("A's stale write returned %v, want ErrTenantSettingsConflict.\n\n"+
			"Without the precondition this succeeds and erases B's "+
			"WasmWallClockCeiling, with neither operator seeing an error.", err)
	}

	// And B's change is intact -- the refusal has to mean nothing was applied,
	// not that an error was returned after a partial write.
	after, _, err := store.ReadTenantSettingsForUpdate(ctx)
	if err != nil {
		t.Fatalf("read after the refusal: %v", err)
	}
	if after.WasmWallClockCeiling != 30*time.Second {
		t.Errorf("B's change was lost: WasmWallClockCeiling = %v, want 30s", after.WasmWallClockCeiling)
	}
	if after.HostRetryBudget != 5*time.Second {
		t.Errorf("A's refused write was partially applied: HostRetryBudget = %v, want the "+
			"original 5s", after.HostRetryBudget)
	}
}

// TestAFirstWriteRacedByAnotherFirstWriteIsRefused.
//
// Two operators both see "no row" and both write. One must lose, or the second
// silently replaces the first's whole row rather than colliding with it -- and
// "there was no row" is a precondition like any other.
func TestAFirstWriteRacedByAnotherFirstWriteIsRefused(t *testing.T) {
	store, teardown := postgresOnly(t)
	defer teardown()
	ctx := context.Background()

	_, aRev, err := store.ReadTenantSettingsForUpdate(ctx)
	if err != nil {
		t.Fatalf("A read: %v", err)
	}
	_, bRev, err := store.ReadTenantSettingsForUpdate(ctx)
	if err != nil {
		t.Fatalf("B read: %v", err)
	}
	if aRev.Existed || bRev.Existed {
		t.Fatalf("expected both to see no row")
	}

	if err := store.WriteTenantSettings(ctx, TenantSettings{HostRetryBudget: time.Second}, bRev); err != nil {
		t.Fatalf("B's first write: %v", err)
	}
	err = store.WriteTenantSettings(ctx, TenantSettings{HostRetryBudget: 2 * time.Second}, aRev)
	if !errors.Is(err, ErrTenantSettingsConflict) {
		t.Fatalf("A's first write returned %v after B created the row, want "+
			"ErrTenantSettingsConflict", err)
	}
	after, _, _ := store.ReadTenantSettingsForUpdate(ctx)
	if after.HostRetryBudget != time.Second {
		t.Errorf("B's row was replaced rather than defended: %v", after.HostRetryBudget)
	}
}

// TestClearingIsDistinctFromLeavingAlone: 0 writes NULL, which means "use the
// operator's flag". If clearing were indistinguishable from not mentioning a
// value, a tenant could never undo an override.
func TestClearingIsDistinctFromLeavingAlone(t *testing.T) {
	store, teardown := postgresOnly(t)
	defer teardown()
	ctx := context.Background()

	_, rev, _ := store.ReadTenantSettingsForUpdate(ctx)
	if err := store.WriteTenantSettings(ctx,
		TenantSettings{HostRetryBudget: 5 * time.Second, WasmWallClockCeiling: 30 * time.Second}, rev); err != nil {
		t.Fatalf("write: %v", err)
	}
	settings, rev, _ := store.ReadTenantSettingsForUpdate(ctx)
	settings.HostRetryBudget = 0 // cleared
	if err := store.WriteTenantSettings(ctx, settings, rev); err != nil {
		t.Fatalf("clearing write: %v", err)
	}
	after, _, _ := store.ReadTenantSettingsForUpdate(ctx)
	if after.HostRetryBudget != 0 {
		t.Errorf("HostRetryBudget = %v after being cleared, want 0", after.HostRetryBudget)
	}
	if after.WasmWallClockCeiling != 30*time.Second {
		t.Errorf("clearing one value disturbed another: WasmWallClockCeiling = %v, want 30s",
			after.WasmWallClockCeiling)
	}
}

// postgresOnly gives a PostgresStore against a real database. tenant_settings
// is PostgreSQL-only in the same way CreateTenant is: the other two dialects
// have their own read paths and no write path, so a multi-backend loop here
// would assert nothing about them.
//
// Neither branch below skips, and that is deliberate. The "is a database
// configured" question is already answered one level down -- PostgresBackend
// .Setup calls testutil.TestDB, which guards on whether one was REQUESTED
// rather than on whether one is reachable. By the time we are here that
// decision is made, so both remaining branches describe things that cannot
// happen in this repo: postgres is registered unconditionally at init with
// Enabled() == true, and Setup returns NewPostgresStore, which is a
// *PostgresStore by construction.
//
// They were t.Skip and check-skips.sh was right to reject them (case (c), the
// precondition is always satisfiable). A skip is indistinguishable from a
// pass, so had either somehow fired, every assertion in this file would have
// been reported as passing while testing nothing.
func postgresOnly(t *testing.T) (*PostgresStore, func()) {
	t.Helper()
	for _, b := range registeredBackends {
		if b.Name() != "postgres" {
			continue
		}
		store, teardown := b.Setup(t)
		ps, ok := store.(*PostgresStore)
		if !ok {
			teardown()
			t.Fatalf("the postgres backend returned %T, want *PostgresStore", store)
		}
		truncateAll(t, store)
		// truncateAll does not clear tenant_settings, so a row written by an
		// earlier test in the same database survives into this one -- which is
		// how the first-write-race test saw an existing row and failed on its
		// own precondition rather than on the property. Cleared explicitly,
		// and the precondition asserted below rather than assumed.
		if _, err := ps.db.ExecContext(context.Background(),
			`DELETE FROM tenant_settings WHERE tenant_id = $1`, ps.tenantID); err != nil {
			teardown()
			t.Fatalf("clearing tenant_settings: %v", err)
		}
		return ps, teardown
	}
	t.Fatal("no postgres backend registered -- RegisterBackend(&PostgresBackend{}) runs at init, so this means the registration was removed")
	return nil, func() {}
}
