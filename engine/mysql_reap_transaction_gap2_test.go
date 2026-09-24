package engine

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"
)

// mysqlDSNWithInterpolateParams forces interpolateParams=true onto
// CLEAT_TEST_MYSQL regardless of whatever the ambient DSN happens to carry,
// because this test's whole subject is a behaviour specific to that setting
// -- see the doc comment on TestMySQLReapStaleInstancesDoesNotGhostCommit...
// below. A test that trusted the ambient DSN's params could silently stop
// exercising interpolateParams=true the moment someone edited the exported
// env var for an unrelated reason.
func mysqlDSNWithInterpolateParams(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("CLEAT_TEST_MYSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL integration test")
	}
	q := strings.IndexByte(dsn, '?')
	if q < 0 {
		return dsn + "?interpolateParams=true"
	}
	params := dsn[q+1:]
	kept := make([]string, 0)
	for _, kv := range strings.Split(params, "&") {
		if !strings.HasPrefix(kv, "interpolateParams=") {
			kept = append(kept, kv)
		}
	}
	kept = append(kept, "interpolateParams=true")
	// interpolateParams=true sends string args as binary-charset literals
	// unless a charset is named explicitly, and this schema has a JSON
	// column that then refuses them ("Cannot create a JSON value from a
	// string with CHARACTER SET 'binary'"). Every other MySQL test in this
	// package avoids this by never setting interpolateParams at all; this
	// one exists specifically because that setting is the point.
	hasCharset := false
	for _, kv := range kept {
		if strings.HasPrefix(kv, "charset=") {
			hasCharset = true
		}
	}
	if !hasCharset {
		kept = append(kept, "charset=utf8mb4")
	}
	return dsn[:q+1] + strings.Join(kept, "&")
}

// lockRowForUpdate opens a dedicated connection, starts a transaction, locks
// the row with SELECT ... FOR UPDATE, and returns a function that releases
// the lock. The commit runs on a background context so the release itself
// cannot be defeated by the caller's (already-expired) test context.
func lockRowForUpdate(t *testing.T, db *sql.DB, id string) (release func()) {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(), `START TRANSACTION`); err != nil {
		t.Fatalf("START TRANSACTION: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(),
		`SELECT id FROM workflow_instances WHERE id = ? FOR UPDATE`, id); err != nil {
		t.Fatalf("SELECT ... FOR UPDATE: %v", err)
	}
	return func() {
		conn.ExecContext(context.Background(), `COMMIT`)
		conn.Close()
	}
}

// TestMySQLReapStaleInstancesDoesNotGhostCommitUnderInterpolateParams is
// GAP2 from cleat#2005's review: under interpolateParams=true, an autocommit
// UPDATE that is still blocked on a row lock when its caller's context times
// out can still complete and commit server-side after the client has already
// given up -- the caller sees "context deadline exceeded" and the row is
// reclaimed anyway. That is worse than an ordinary timeout, because the
// worker that owns the run has no way to learn its run was just taken from
// under it by a call that "failed."
//
// This is not a rare timing race here: forcing the row lock to release at
// (approximately) the same instant the reap's context deadline fires
// reproduces it 10/10 against a real MySQL container on the pre-fix
// autocommit ExecContext (verified with a standalone probe before this test
// was written). The fix -- wrapping the UPDATE in an explicit transaction
// (this file's sibling, mysql_lifecycle.go's ReapStaleInstances) -- closes it
// because a transaction that is never COMMITted is not durable: MySQL
// implicitly rolls back an open transaction when its connection goes away,
// which is exactly what happens when database/sql abandons a
// context-cancelled Tx.
//
// Falsify by reverting ReapStaleInstances to a bare
// `s.db.ExecContext(ctx, ...)` (no BeginTx/Commit): this test must then fail
// with the row showing "ready"+cleared assigned_to despite ReapStaleInstances
// having returned a context error.
func TestMySQLReapStaleInstancesDoesNotGhostCommitUnderInterpolateParams(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping MySQL integration test in short mode")
	}

	// Setup (schema, workflow def, claims) goes through an ordinary
	// connection: interpolateParams=true sends []byte args -- including the
	// JSON payload columns this path writes -- as binary-charset literals,
	// which this schema's JSON columns refuse. That is orthogonal to GAP2,
	// which is specifically about ReapStaleInstances's UPDATE, so only the
	// reap call itself (and the lock that blocks it) runs over the
	// interpolateParams=true connection, against the same database.
	setupStore, teardown := mysqlIntegrationStore(t)
	defer teardown()

	interpDSN := mysqlDSNWithInterpolateParams(t)
	interpDB, err := sql.Open("mysql", interpDSN)
	if err != nil {
		t.Fatalf("open MySQL (interpolateParams=true): %v", err)
	}
	defer interpDB.Close()
	if err := interpDB.Ping(); err != nil {
		t.Fatalf("ping MySQL (interpolateParams=true): %v", err)
	}
	interpStore := NewMySQLStore(interpDB)

	const trials = 5
	for i := 0; i < trials; i++ {
		cleanupMySQLTestTables(t, setupStore)

		wfID := createReadyWorkflow(t, setupStore, "gap2-trial")
		wf := claimOne(t, setupStore, "gap2-worker")
		if wf.ID != wfID {
			t.Fatalf("claimed %q, want %q", wf.ID, wfID)
		}

		const holdAndDeadline = 60 * time.Millisecond
		release := lockRowForUpdate(t, interpDB, wf.ID)
		released := make(chan struct{})
		go func() {
			time.Sleep(holdAndDeadline)
			release()
			close(released)
		}()

		reapCtx, cancel := context.WithTimeout(context.Background(), holdAndDeadline)
		_, reapErr := interpStore.ReapStaleInstances(reapCtx, time.Nanosecond, 0)
		cancel()
		<-released

		// Give MySQL a moment to finish tearing down the abandoned
		// connection/transaction before reading the row back.
		time.Sleep(200 * time.Millisecond)

		stored, err := setupStore.GetWorkflowByID(context.Background(), wf.ID)
		if err != nil {
			t.Fatalf("GetWorkflowByID: %v", err)
		}
		if stored == nil {
			t.Fatalf("trial %d: workflow %s not found after reap attempt", i, wf.ID)
		}

		if reapErr == nil {
			// The reap genuinely won the race and completed within its
			// deadline; that is a legitimate, correctly-committed reclaim,
			// not the ghost-commit hazard this test is for. Nothing to
			// assert either way -- move on to the next trial.
			continue
		}

		if stored.Status != "running" || stored.AssignedTo != "gap2-worker" {
			t.Fatalf("trial %d: reap reported an error (%v) but the row was still reclaimed anyway "+
				"(status=%q assigned_to=%q) -- a ghost commit under interpolateParams=true",
				i, reapErr, stored.Status, stored.AssignedTo)
		}
	}
}
