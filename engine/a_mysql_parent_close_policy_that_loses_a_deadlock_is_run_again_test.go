package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// cleat#4410. On MySQL, enforceParentClosePolicy runs AFTER a terminal verb has committed and applies the closing parent's policy to its children in ONE transaction. Another transaction
// that holds one child's row while it asks for another that this transaction already holds makes a cycle, InnoDB picks a victim and rolls it back entirely, and when the victim is this
// transaction, error 1213 is logged at WARN and dropped: the function returns nothing, the verb that called it has already succeeded, and EVERY child of the closing parent is left as it
// was. A TERMINATE child keeps running under a parent that is gone.
//
// This makes that transaction the victim on purpose, and it is built from what was measured rather than from what the issue guessed (MySQL 8.4.11, a small table). The enforcement's first UPDATE reads the tenant's
// rows through idx_instances_tenant_queue_ready and locks every row it scans, not only the closing parent's children: at the moment it blocks, performance_schema.data_locks shows it holding an X lock on that
// index and WAITING on the PRIMARY row of the parent, which is no child of anything. So its footprint reaches rows of workflows that have nothing to do with the close.
//
// How, deterministically, and WITHOUT assuming which index the enforcement scans: a test transaction T changes more rows than the enforcement will (the parent and six other
// workflows), so InnoDB, which rolls back the transaction that has done the least, picks the enforcement. The enforcement blocks on one of T's rows. The test then reads what the
// enforcement actually HOLDS from performance_schema.data_locks and has T ask for one of those rows: a PRIMARY row if it holds any (the children it has already updated), else the
// first record of the secondary index it is scanning. Each waits for the other. T survives, the enforcement is rolled back, and the question is what the two children look like afterwards.
//
// Which index the enforcement scans is the optimizer's choice and it flips (cleat#4444, found by cleat-review3 on a long-lived MySQL 8.4.11): in one plan it reads through
// idx_instances_tenant_queue_ready, in another through fk_instances_def and holds nothing on the queue index at all, so a T that always asks for "the first record of the queue index"
// asks for a row nobody holds and no cycle forms. The scenario therefore runs once per plan, the second with the queue index made INVISIBLE so the optimizer cannot use it. It shows
// the retry works, not that this is the deadlock that was seen in a race test.
func TestAMySQLParentClosePolicyThatLosesADeadlockIsRunAgainAndTerminatesEveryChild(t *testing.T) {
	for _, backend := range registeredBackends {
		if backend.Name() != "mysql" {
			continue
		}
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ms, ok := store.(*MySQLStore)
			if !ok {
				t.Fatalf("%T is not a *MySQLStore", store)
			}
			// Both plans share this one database, so a DSN that is not set skips here, once, under the same name as before.
			for i, plan := range []struct{ name, hideIndex string }{
				{"default plan", ""},
				{"queue index invisible", "idx_instances_tenant_queue_ready"},
			} {
				plan, i := plan, i
				t.Run(plan.name, func(t *testing.T) {
					closeDeadlockScenario(t, store, ms, fmt.Sprintf("wf-4410-parent-%d", i), plan.hideIndex)
				})
			}
		})
	}
}

// closeDeadlockScenario makes the enforcement for one closing parent the deadlock victim and checks that it was run again and terminated every child. hideIndex, when not
// empty, is made INVISIBLE for the duration, so the optimizer has to scan the enforcement's rows through another index.
func closeDeadlockScenario(t *testing.T, store WorkflowStore, ms *MySQLStore, parent, hideIndex string) {
	ctx := context.Background()
	if hideIndex != "" {
		if _, err := ms.db.ExecContext(ctx, "ALTER TABLE workflow_instances ALTER INDEX `"+hideIndex+"` INVISIBLE"); err != nil {
			t.Fatalf("make %s invisible: %v", hideIndex, err)
		}
		defer func() {
			if _, err := ms.db.ExecContext(ctx, "ALTER TABLE workflow_instances ALTER INDEX `"+hideIndex+"` VISIBLE"); err != nil {
				t.Errorf("make %s visible again: %v", hideIndex, err)
			}
		}()
	}
	// Enough decoys that T has done clearly more than the enforcement: InnoDB rolls back the transaction with less work, and a plan that locks more of the enforcement's rows
	// must still lose.
	var decoys []string
	for k := 1; k <= 6; k++ {
		decoys = append(decoys, fmt.Sprintf("%s-decoy-%d", parent, k))
	}
	deployConcurrencyTestWorkflows(t, store, append([]string{parent}, decoys...)...)

	var children []string
	for i := 0; i < 2; i++ {
		id, err := store.StartChildWorkflow(ctx, parent, "concurrency-test-workflow", `{}`, 1, "TERMINATE", 0)
		if err != nil {
			t.Fatalf("StartChildWorkflow: %v", err)
		}
		children = append(children, id)
	}
	for _, c := range children {
		if got := statusOf(t, ctx, store, c); statusIsClosed(got) {
			t.Fatalf("child %s starts %q, want it open", c, got)
		}
	}

	// The precondition is this store's OWN retry count, not a server-wide counter (cleat#4444): INNODB_METRICS lock_deadlocks moves for any session on the server, and
	// with a real deadlock cycling in another database (11 in 5 s) a test that made none, T taking no lock, still passed 1 of 20 runs. This store runs only this test's
	// enforcement, so a retry counted here is this test's deadlock or lock-wait loss and nothing else's.
	retries := new(atomic.Int64)
	ms.lockConflictRetries = retries

	tx, err := ms.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	args := []any{parent}
	marks := "?"
	for _, d := range decoys {
		args = append(args, d)
		marks += ", ?"
	}
	args = append(args, ms.tenantID)
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_instances SET error_msg = 'held by T' WHERE id IN (`+marks+`) AND tenant_id = ?`, args...); err != nil {
		t.Fatalf("T's hold of the parent's row and the decoys: %v", err)
	}

	// T's own InnoDB transaction id, so the wait below can be about T and nothing else.
	tTrx, tries := innodbTransactionID(ctx, t, tx)
	if tries > 1 {
		t.Logf("T's InnoDB transaction id %d needed %d reads of performance_schema.data_locks", tTrx, tries)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		ms.enforceParentClosePolicy(ctx, parent, "parent terminated by test")
	}()

	waiter, waited := waitForMySQLLockWaitOn(ctx, ms.db, tTrx, 20*time.Second)
	if !waited {
		t.Fatalf("no transaction ever waited on T's locks (T is InnoDB transaction %d), so the enforcement never reached a row T holds and no deadlock can be made", tTrx)
	}

	// What the enforcement HOLDS decides what T asks for, not an assumption about its plan.
	held := recordLocksHeldBy(ctx, t, ms.db, waiter)
	lockSQL, lockArgs, how := rowTToLock(ctx, t, ms.db, held)
	t.Logf("the enforcement (InnoDB transaction %d) holds %d granted record lock(s) on workflow_instances, on indexes %v; T asks for %s", waiter, len(held), distinctIndexes(held), how)

	// T asks for a row the enforcement already holds: the cycle. It returns when InnoDB has rolled the enforcement back.
	var got string
	if err := tx.QueryRowContext(ctx, lockSQL, lockArgs...).Scan(&got); err != nil {
		t.Fatalf("T's lock of a row the enforcement holds failed (T was meant to survive, having done more work than the enforcement): %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("the enforcement did not return within 30s of T letting go")
	}
	if n := retries.Load(); n < 1 {
		t.Errorf("the enforcement was never run again (retryingLockConflict retries on this store: %d): no deadlock was made, so this passed without exercising the retry", n)
	}
	for _, c := range children {
		if got := statusOf(t, ctx, store, c); got != statusTerminated {
			t.Errorf("child %s is %q after its parent closed with TERMINATE, want %q: the enforcement lost its deadlock and was not run again", c, got, statusTerminated)
		}
	}
}

// heldRecordLock is one granted exclusive record lock on workflow_instances: the index it is on and the key it covers (performance_schema.data_locks LOCK_DATA).
type heldRecordLock struct{ index, data string }

// recordLocksHeldBy lists the granted exclusive record locks on workflow_instances held by the InnoDB transaction trx, PRIMARY rows first. The supremum pseudo-record is a gap
// lock and is left out.
func recordLocksHeldBy(ctx context.Context, t *testing.T, db *sql.DB, trx uint64) []heldRecordLock {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT INDEX_NAME, LOCK_DATA FROM performance_schema.data_locks
		WHERE ENGINE_TRANSACTION_ID = ? AND OBJECT_NAME = 'workflow_instances' AND LOCK_TYPE = 'RECORD' AND LOCK_STATUS = 'GRANTED'
		  AND LOCK_MODE LIKE 'X%' AND LOCK_DATA NOT LIKE 'supremum%'
		ORDER BY (INDEX_NAME = 'PRIMARY') DESC, INDEX_NAME, LOCK_DATA`, trx)
	if err != nil {
		t.Fatalf("read the locks held by InnoDB transaction %d: %v", trx, err)
	}
	defer rows.Close()
	var out []heldRecordLock
	for rows.Next() {
		var idx, data sql.NullString
		if err := rows.Scan(&idx, &data); err != nil {
			t.Fatalf("scan a held lock: %v", err)
		}
		out = append(out, heldRecordLock{idx.String, data.String})
	}
	return out
}

func distinctIndexes(held []heldRecordLock) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range held {
		if !seen[h.index] {
			seen[h.index] = true
			out = append(out, h.index)
		}
	}
	return out
}

// rowTToLock chooses the statement T uses to ask for something the enforcement holds. A PRIMARY row is asked for by its id (a point lookup that must wait for the enforcement's
// lock on exactly that row). When the enforcement holds no PRIMARY row yet, it is asked for the first record of the secondary index the enforcement is scanning. If it holds
// neither, the test cannot make a cycle and says so with what it did find, rather than guessing an index.
func rowTToLock(ctx context.Context, t *testing.T, db *sql.DB, held []heldRecordLock) (query string, args []any, how string) {
	t.Helper()
	for _, h := range held {
		if h.index == "PRIMARY" {
			id := strings.Trim(h.data, "'")
			return `SELECT id FROM workflow_instances WHERE id = ? FOR UPDATE`, []any{id}, "its PRIMARY row " + id
		}
	}
	for _, h := range held {
		if h.index == "" {
			continue
		}
		cols, err := db.QueryContext(ctx, `SELECT COLUMN_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'workflow_instances' AND INDEX_NAME = ? ORDER BY SEQ_IN_INDEX`, h.index)
		if err != nil {
			t.Fatalf("read the columns of index %s: %v", h.index, err)
		}
		var order []string
		for cols.Next() {
			var c string
			if err := cols.Scan(&c); err != nil {
				cols.Close()
				t.Fatalf("scan a column of index %s: %v", h.index, err)
			}
			order = append(order, "`"+c+"`")
		}
		cols.Close()
		if len(order) == 0 {
			continue
		}
		return "SELECT id FROM workflow_instances FORCE INDEX (`" + h.index + "`) ORDER BY " + strings.Join(order, ", ") + " LIMIT 1 FOR UPDATE", nil,
			"the first record of its index " + h.index
	}
	t.Fatalf("the enforcement holds no record lock T could ask for (held: %v), so no cycle can be made; this is a failure of the check, not a finding about the engine", held)
	return "", nil, ""
}

func statusIsClosed(s string) bool {
	switch s {
	case statusDone, statusFailed, statusDeadLettered, statusTerminated, statusCancelled:
		return true
	}
	return false
}

// innodbTransactionID returns the InnoDB transaction id of tx's own connection and how many reads it took. It reads it from performance_schema.data_locks, the live lock
// table the wait below also reads, joined to this connection's thread, and not from information_schema.INNODB_TRX: measured 2026-11-23 on MySQL 8.4, INNODB_TRX
// returned no row for T on the first read in 5 of 14 runs, and in one run still none after 191 reads over 5 s, so retrying that view does not make it reliable. T
// holds record locks by then (its UPDATE has run), so it has rows in data_locks; the loop is for the case that is still not visible, for at most 5 s.
func innodbTransactionID(ctx context.Context, t *testing.T, tx *sql.Tx) (id uint64, tries int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		tries++
		err := tx.QueryRowContext(ctx, `SELECT DISTINCT l.ENGINE_TRANSACTION_ID FROM performance_schema.data_locks l JOIN performance_schema.threads th ON th.THREAD_ID = l.THREAD_ID WHERE th.PROCESSLIST_ID = CONNECTION_ID() AND l.OBJECT_NAME = 'workflow_instances' LIMIT 1`).Scan(&id)
		if err == nil {
			return id, tries
		}
		if !errors.Is(err, sql.ErrNoRows) || time.Now().After(deadline) {
			t.Fatalf("read T's InnoDB transaction id from performance_schema.data_locks (after %d reads): %v", tries, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitForMySQLLockWaitOn waits up to d for some transaction to be waiting on a lock held by the InnoDB transaction blocker, and returns that waiter's InnoDB transaction id. It reads
// performance_schema.data_lock_waits, and it asks about ONE blocker on purpose (cleat#4410). The first version counted every row of that table, which is
// server-wide: on a shared MySQL server an unrelated test's lock wait made it return before the enforcement held the index record T asks for next, so T got
// that record without a cycle, no deadlock formed, and the test failed its own precondition ("the deadlock counter did not move") about 1 run in 15 in CI.
// Measured 2026-11-23 on MySQL 8.4: that version passed 20 of 20 on a quiet server and 1 of 20 with an unrelated pair of connections holding and waiting on a
// row in another database; keyed on T it is the enforcement's own wait or nothing. (information_schema.INNODB_TRX did not list the waiter at all.)
func waitForMySQLLockWaitOn(ctx context.Context, db *sql.DB, blocker uint64, d time.Duration) (waiter uint64, ok bool) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		var w sql.NullInt64
		if err := db.QueryRowContext(ctx, `SELECT REQUESTING_ENGINE_TRANSACTION_ID FROM performance_schema.data_lock_waits WHERE BLOCKING_ENGINE_TRANSACTION_ID = ? LIMIT 1`, blocker).Scan(&w); err == nil && w.Valid {
			return uint64(w.Int64), true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0, false
}
