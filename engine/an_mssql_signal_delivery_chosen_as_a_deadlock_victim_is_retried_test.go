package engine

import (
	"context"
	"testing"
	"time"
)

// cleat#4340. SQL Server chose a signal delivery as a deadlock victim once in a CI run of the race test (1 delivery in 96, "Transaction (Process ID 82) was deadlocked ... chosen as the deadlock
// victim", at the statement that locks the run). The mechanism was NOT established: the deadlock was not reproduced locally in about 100 runs of that test, including under CPU starvation, so the
// locking argument that said it could not happen was wrong in a way nobody has seen the inside of.
//
// What can be said is what a victim is: SQL Server has rolled its transaction back entirely. A delivery is ONE transaction (the idempotency key, the chain walk and the signal), so running it
// again is exactly-once safe, and the store already retries a victim at a transaction boundary for the same reason (withRollbackGuaranteedRetry, mssqlTxRetries). This test makes a victim on
// purpose and checks the delivery is retried and delivers once. It does NOT show that the deadlock CI saw is the one made here.
//
// How the victim is made, deterministically: a test transaction T holds the run's row (UPDLOCK). A KEYED delivery starts, claims its idempotency key (an uncommitted row), and blocks on the run's
// row. T then tries to claim the SAME key, which blocks on the delivery's uncommitted key row. Each waits for the other. T runs at DEADLOCK_PRIORITY HIGH, so SQL Server picks the delivery.
func TestAnMSSQLSignalDeliveryChosenAsADeadlockVictimIsRetriedAndDeliveredOnce(t *testing.T) {
	for _, backend := range registeredBackends {
		if backend.Name() != "mssql" {
			continue
		}
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ms, ok := store.(*MSSQLStore)
			if !ok {
				t.Fatalf("%T is not a *MSSQLStore", store)
			}
			ctx := context.Background()
			parent := newIntentWorkflow(t, ctx, store, "deadlock-parent")
			run := settleRunOpen(t, ctx, store, parent)
			const key = "deadlock-victim-key"

			// The server-wide deadlock count, so a pass cannot mean "no deadlock happened and the delivery just waited".
			deadlocks := func() int64 {
				var n int64
				if err := ms.db.QueryRowContext(ctx, `SELECT cntr_value FROM sys.dm_os_performance_counters WHERE counter_name = 'Number of Deadlocks/sec' AND instance_name = '_Total'`).Scan(&n); err != nil {
					t.Fatalf("read the deadlock counter: %v", err)
				}
				return n
			}
			before := deadlocks()

			tx, err := ms.beginTxWithContext(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, `SET DEADLOCK_PRIORITY HIGH`); err != nil {
				t.Fatalf("SET DEADLOCK_PRIORITY: %v", err)
			}
			if _, err := tx.ExecContext(ctx, `SELECT 1 FROM workflow_instances WITH (UPDLOCK, ROWLOCK) WHERE id = @p1 AND tenant_id = @p2`, run, ms.tenantID); err != nil {
				t.Fatalf("take the run's row: %v", err)
			}

			type result struct {
				replay bool
				err    error
			}
			done := make(chan result, 1)
			go func() {
				replay, err := ms.DeliverSignalIdempotent(ctx, run, "ping", `"v"`, key)
				done <- result{replay, err}
			}()

			// Wait until the delivery is blocked on the run's row (it has claimed its key by then), rather than guessing a delay.
			deadline := time.Now().Add(20 * time.Second)
			for {
				var blocked int
				if err := ms.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.dm_exec_requests WHERE blocking_session_id <> 0`).Scan(&blocked); err != nil {
					t.Fatalf("read blocked requests: %v", err)
				}
				if blocked > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the delivery never blocked on the run's row, so no deadlock can be made")
				}
				time.Sleep(20 * time.Millisecond)
			}

			// T now claims the delivery's key, which waits on the delivery's uncommitted key row: the cycle. This statement returns when SQL Server has killed the delivery.
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id)
				SELECT @p1, @p2, DATEADD(SECOND, 60, SYSUTCDATETIME()), @p3
				WHERE NOT EXISTS (SELECT 1 FROM idempotency_keys WITH (UPDLOCK, HOLDLOCK) WHERE key_hash = @p1 AND tenant_id = @p3)
			`, signalIdempotencyHash(key), run, ms.tenantID); err != nil {
				t.Fatalf("T's own claim of the key failed (T was meant to survive, at DEADLOCK_PRIORITY HIGH): %v", err)
			}
			// T lets go of both the run and the key it just took, so the delivery's retry can proceed.
			if err := tx.Rollback(); err != nil {
				t.Fatalf("rollback: %v", err)
			}

			select {
			case r := <-done:
				if r.err != nil {
					t.Fatalf("the delivery chosen as the deadlock victim returned %v, want it retried and delivered", r.err)
				}
				if r.replay {
					t.Errorf("the retried delivery reported replay=true: the victim's key was not rolled back with it")
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("the delivery did not finish within 30s of T letting go")
			}
			if after := deadlocks(); after <= before {
				t.Errorf("the deadlock counter did not move (%d -> %d): no deadlock was made, so this passed without exercising the retry", before, after)
			}
			if got := drainSignalPayloads(t, ctx, store, run, "ping"); len(got) != 1 || got[0] != "v" {
				t.Errorf("the run holds %v, want exactly [v]: a retried delivery must deliver once", got)
			}
		})
	}
}
