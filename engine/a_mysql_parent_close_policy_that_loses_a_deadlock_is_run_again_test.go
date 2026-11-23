package engine

import (
	"context"
	"database/sql"
	"errors"
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
// How, deterministically: a test transaction T changes four rows (the parent and two other workflows), so InnoDB, which rolls back the transaction that has done the least, picks the enforcement. The
// enforcement blocks on the parent's row while holding the first record of that index. T then asks for that first record: each waits for the other. T survives, the enforcement is rolled back, and the
// question is what the two children look like afterwards. It shows the retry works, not that this is the deadlock that was seen in a race test.
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
			ctx := context.Background()
			parent := "wf-4410-parent"
			deployConcurrencyTestWorkflows(t, store, parent, "wf-4410-decoy-1", "wf-4410-decoy-2")

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

			deadlocks := func() int64 {
				var n int64
				if err := ms.db.QueryRowContext(ctx, `SELECT COUNT FROM information_schema.INNODB_METRICS WHERE NAME = 'lock_deadlocks'`).Scan(&n); err != nil {
					t.Fatalf("read the deadlock counter: %v", err)
				}
				return n
			}
			before := deadlocks()

			tx, err := ms.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback()
			// T does more than the enforcement will: it changes three rows (the parent and two other workflows), so InnoDB, which rolls back the transaction that has done the least, picks the enforcement.
			if _, err := tx.ExecContext(ctx, `UPDATE workflow_instances SET error_msg = 'held by T' WHERE id IN (?, ?, ?) AND tenant_id = ?`,
				parent, "wf-4410-decoy-1", "wf-4410-decoy-2", ms.tenantID); err != nil {
				t.Fatalf("T's hold of the parent's row: %v", err)
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

			if !waitForMySQLLockWaitOn(ctx, ms.db, tTrx, 20*time.Second) {
				t.Fatalf("no transaction ever waited on T's locks (T is InnoDB transaction %d), so the enforcement never reached a row T holds and no deadlock can be made", tTrx)
			}

			// T asks for the FIRST record of the index the enforcement is reading through, which the enforcement already holds: the cycle. It returns when InnoDB has rolled the enforcement back.
			var got string
			if err := tx.QueryRowContext(ctx, `SELECT id FROM workflow_instances FORCE INDEX (idx_instances_tenant_queue_ready) WHERE tenant_id = ? ORDER BY task_queue, status, priority, next_wake_at LIMIT 1 FOR UPDATE`,
				ms.tenantID).Scan(&got); err != nil {
				t.Fatalf("T's lock of the index's first record failed (T was meant to survive, having done more work than the enforcement): %v", err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatalf("rollback: %v", err)
			}

			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatalf("the enforcement did not return within 30s of T letting go")
			}
			if after := deadlocks(); after <= before {
				t.Errorf("the deadlock counter did not move (%d -> %d): no deadlock was made, so this passed without exercising the retry", before, after)
			}
			for _, c := range children {
				if got := statusOf(t, ctx, store, c); got != statusTerminated {
					t.Errorf("child %s is %q after its parent closed with TERMINATE, want %q: the enforcement lost its deadlock and was not run again", c, got, statusTerminated)
				}
			}
		})
	}
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

// waitForMySQLLockWaitOn reports whether some transaction is waiting on a lock held by the InnoDB transaction blocker within d. It reads
// performance_schema.data_lock_waits, and it asks about ONE blocker on purpose (cleat#4410). The first version counted every row of that table, which is
// server-wide: on a shared MySQL server an unrelated test's lock wait made it return before the enforcement held the index record T asks for next, so T got
// that record without a cycle, no deadlock formed, and the test failed its own precondition ("the deadlock counter did not move") about 1 run in 15 in CI.
// Measured 2026-11-23 on MySQL 8.4: that version passed 20 of 20 on a quiet server and 1 of 20 with an unrelated pair of connections holding and waiting on a
// row in another database; keyed on T it is the enforcement's own wait or nothing. (information_schema.INNODB_TRX did not list the waiter at all.)
func waitForMySQLLockWaitOn(ctx context.Context, db *sql.DB, blocker uint64, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM performance_schema.data_lock_waits WHERE BLOCKING_ENGINE_TRANSACTION_ID = ?`, blocker).Scan(&n); err == nil && n > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
