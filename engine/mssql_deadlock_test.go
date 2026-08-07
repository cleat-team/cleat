package engine

// IMPROVEMENT-PLAN.md 2.26: mssqlRetry and the whole engine/mssql_errors.go
// classification family have no production caller, and the support position is
// that "on SQL Server, a deadlock is a hard error today".
//
// 2.26's own history is the reason to be careful here. The classifier was
// wrong in both directions at once and its tests agreed with it perfectly,
// because both were built on the same wrong model -- that a SQL Server error
// *is* text. engine/mssql_errors_test.go feeds only fmt.Errorf strings and
// never an mssql.Error, so it could confirm the implementation without ever
// touching the requirement.
//
// This file closes that loop from the other end: provoke a real deadlock on a
// real SQL Server, take the error the driver actually returns, and assert the
// predicates classify *that*. No fabricated error values.

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/cleat-team/cleat/engine/testutil"
)

// provokeMSSQLDeadlock forces a genuine 1205 by having two transactions take
// row locks in opposite order, and returns the victim's error.
//
// SQL Server chooses the victim itself, so the test asserts on whichever
// transaction lost rather than assuming.
func provokeMSSQLDeadlock(t *testing.T, db *sql.DB) error {
	t.Helper()

	ctx := context.Background()
	const (
		rowA = "deadlock-row-a"
		rowB = "deadlock-row-b"
	)
	if _, err := db.ExecContext(ctx, `
		IF NOT EXISTS (SELECT 1 FROM workflow_defs WHERE name = 'deadlock-def' AND version = 1)
		INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		VALUES ('deadlock-def', 1, 0x0061736d, 1, 1, @p1)`, DefaultTenantUUID); err != nil {
		t.Fatalf("seed workflow_def: %v", err)
	}
	for _, id := range []string{rowA, rowB} {
		if _, err := db.ExecContext(ctx, `
			IF NOT EXISTS (SELECT 1 FROM workflow_instances WHERE id = @p1)
			INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id)
			VALUES (@p1, 'deadlock-def', 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default', @p2)`,
			id, DefaultTenantUUID); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	t.Cleanup(func() {
		db.ExecContext(context.Background(),
			`DELETE FROM workflow_instances WHERE id IN (@p1, @p2)`, rowA, rowB)
		db.ExecContext(context.Background(),
			`DELETE FROM workflow_defs WHERE name = 'deadlock-def'`)
	})

	// Each transaction locks its first row, waits for the other to do the
	// same, then reaches for the row the other holds.
	bothLocked := make(chan struct{})
	var once sync.Once
	var lockedCount int
	var mu sync.Mutex
	reachedFirst := func() {
		mu.Lock()
		lockedCount++
		n := lockedCount
		mu.Unlock()
		if n == 2 {
			once.Do(func() { close(bothLocked) })
		}
	}

	run := func(first, second string) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		if _, err := tx.ExecContext(ctx,
			`UPDATE workflow_instances SET error_msg = 'lock' WHERE id = @p1`, first); err != nil {
			return err
		}
		reachedFirst()

		select {
		case <-bothLocked:
		case <-time.After(10 * time.Second):
			return errors.New("timed out waiting for both transactions to take their first lock")
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE workflow_instances SET error_msg = 'lock' WHERE id = @p1`, second); err != nil {
			return err
		}
		return tx.Commit()
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = run(rowA, rowB) }()
	go func() { defer wg.Done(); errs[1] = run(rowB, rowA) }()
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// TestMSSQLDeadlock_ClassifiedFromTheRealDriverError is the check 2.26 says
// was never made: the predicates are exercised against the error type the
// driver actually returns, not a fmt.Errorf string that happens to contain the
// right words.
func TestMSSQLDeadlock_ClassifiedFromTheRealDriverError(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL integration test in short mode")
	}

	db := testutil.MSSQLTestDB(t)
	defer db.Close()
	testutil.SetupMSSQLFullSchema(t, db)

	// The deadlock has to be provoked on a handle that can actually lock the
	// rows. A plain pool is subject to the security policies on a database
	// built from the shipped migrations, so both UPDATEs below matched nothing,
	// took no row locks, and the two transactions committed happily side by
	// side -- the test then failed with "no transaction was chosen as a
	// deadlock victim", which is a truthful report of a fixture that could not
	// do its job.
	//
	// Note what would have happened without that Fatal: no deadlock, no error,
	// and a test named for classifying a real 1205 that never saw one. The
	// assertion earning its keep is the reason the fixture's failure was
	// visible at all.
	err := provokeMSSQLDeadlock(t, testutil.MSSQLAdminDB(t, db))
	if err == nil {
		t.Fatal("no transaction was chosen as a deadlock victim -- the test did not provoke a deadlock, " +
			"so it proves nothing about the classifier")
	}

	// What the driver actually hands back, recorded so a future change in
	// go-mssqldb's error shape is visible in the failure output rather than
	// silently reclassifying every deadlock.
	var mssqlErr mssql.Error
	if !errors.As(err, &mssqlErr) {
		t.Fatalf("deadlock error is %T (%v), not an mssql.Error -- the number-based predicates cannot see it", err, err)
	}
	t.Logf("driver error: Number=%d Message=%q", mssqlErr.Number, mssqlErr.Message)

	if mssqlErr.Number != mssqlErrDeadlockVictim {
		t.Fatalf("deadlock victim reported error %d, want %d", mssqlErr.Number, mssqlErrDeadlockVictim)
	}
	if !isMSSQLDeadlock(err) {
		t.Errorf("isMSSQLDeadlock(real 1205) = false")
	}
	if !isMSSQLRetryable(err) {
		t.Errorf("isMSSQLRetryable(real 1205) = false")
	}
	if !isMSSQLRollbackGuaranteed(err) {
		t.Errorf("isMSSQLRollbackGuaranteed(real 1205) = false -- a deadlock victim's transaction "+
			"is definitively rolled back by the server, which is what makes replaying it sound; "+
			"error text was %q", mssqlErr.Message)
	}
}
