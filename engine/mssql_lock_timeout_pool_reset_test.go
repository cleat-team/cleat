package engine

import (
	"context"
	"os"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestMSSQLLockTimeoutDoesNotSurviveAPooledConnectionRecycle is the
// defense-in-depth half of cleat#1963's LOCK_TIMEOUT fix, proven rather than
// assumed per the coordinator's review of #2088.
//
// claimWorkflowsOnce resets LOCK_TIMEOUT itself (a defer, guaranteed to run
// on every return path -- see the comment above it in mssql_lifecycle.go).
// This test checks the layer underneath that: if a caller EVER left
// LOCK_TIMEOUT set on a connection returned to the pool -- a bug in this
// package, or a future caller with the same pattern that gets the ordering
// wrong -- would that connection poison whatever the pool hands it to next?
//
// This package already established (a_deleted_row_and_an_invisible_row_are_
// told_apart_test.go, mssql_store.go's ResetSession doc comment) that
// database/sql calls ResetSession on every recycle and go-mssqldb answers it
// with sp_reset_connection, which clears SESSION_CONTEXT. sp_reset_connection
// is documented to reset SET options generally, not SESSION_CONTEXT
// specifically -- this test is the same measurement extended to
// LOCK_TIMEOUT, on a pool forced to reuse the exact same physical connection
// (SetMaxOpenConns(1)).
func TestMSSQLLockTimeoutDoesNotSurviveAPooledConnectionRecycle(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	db := testutil.MSSQLTestDB(t)
	ctx := context.Background()

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// First use: set LOCK_TIMEOUT and deliberately do NOT reset it --
	// simulating exactly the bug this fix's defer exists to prevent -- then
	// let the connection return to the pool.
	conn1, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn1: %v", err)
	}
	if _, err := conn1.ExecContext(ctx, "SET LOCK_TIMEOUT 500"); err != nil {
		t.Fatalf("set lock timeout on conn1: %v", err)
	}
	var setValue int
	if err := conn1.QueryRowContext(ctx, "SELECT @@LOCK_TIMEOUT").Scan(&setValue); err != nil {
		t.Fatalf("read back on conn1: %v", err)
	}
	if setValue != 500 {
		t.Fatalf("@@LOCK_TIMEOUT on conn1 = %d, want 500 -- the SET itself didn't take, "+
			"nothing below measures anything until this holds", setValue)
	}
	if err := conn1.Close(); err != nil {
		t.Fatalf("close conn1: %v", err)
	}

	// Known-positive for the harness itself: SetMaxOpenConns(1) is only a
	// forcing function if the pool actually reuses the physical connection
	// rather than opening a second one that would trivially read -1 as its
	// own fresh default and prove nothing about recycling.
	if stats := db.Stats(); stats.OpenConnections != 1 {
		t.Fatalf("db.Stats().OpenConnections = %d after conn1.Close(), want 1 -- SetMaxOpenConns(1) "+
			"isn't forcing single-connection reuse the way this test assumes", stats.OpenConnections)
	}

	// Second use: with MaxOpenConns(1) and one connection already idle, this
	// MUST be the same physical connection recycled, not a fresh one.
	conn2, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn2: %v", err)
	}
	defer conn2.Close()
	if stats := db.Stats(); stats.OpenConnections != 1 {
		t.Fatalf("db.Stats().OpenConnections = %d after acquiring conn2, want 1 -- a second physical "+
			"connection was opened instead of the idle one being reused, so the read below would prove "+
			"nothing about recycling", stats.OpenConnections)
	}
	var recycledValue int
	if err := conn2.QueryRowContext(ctx, "SELECT @@LOCK_TIMEOUT").Scan(&recycledValue); err != nil {
		t.Fatalf("read back on conn2: %v", err)
	}
	if recycledValue != -1 {
		t.Fatalf("@@LOCK_TIMEOUT on the recycled connection = %d, want -1 (SQL Server's default, "+
			"\"wait indefinitely\") -- a caller that left LOCK_TIMEOUT set (which this fix's own "+
			"defer is designed never to do) would silently make some unrelated later statement on "+
			"this pool fail with error 1222 for no reason it could see. If this test fails, the fix "+
			"in mssql_lifecycle.go needs to stop relying on pool recycling and reset unconditionally "+
			"in a defer -- which it already does; this test exists to prove that belt-and-braces "+
			"layer actually holds rather than assuming it from database/sql's documented contract",
			recycledValue)
	}
}
