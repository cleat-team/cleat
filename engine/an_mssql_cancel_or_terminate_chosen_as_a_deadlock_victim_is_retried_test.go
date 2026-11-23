package engine

import (
	"context"
	"testing"
	"time"
)

// cleat#4340. A cancel or a terminate takes the END run's row (UPDLOCK, via lockChainEnd) and then, inside the same transaction, reads that run's event_history to see whether it owes a defer
// phase. Another transaction holding an event_history row of that run while it asks for the run's row makes a cycle, and SQL Server picks a victim. These verbs sit inside
// withRollbackGuaranteedRetry; this test makes the verb the victim on purpose and checks it is retried, acts ONCE, and acts on the successor when addressed to the run that continued.
//
// (A first version held the successor's row and expected the verb to lock the old run first. It did not deadlock, and the counter check below said so: the walk starts at the END of the chain
// as an unlocked read found it, so a settled chain costs the verb ONE lock, not two. Only a continue that commits between that read and the lock walks, and that window cannot be held open.)
//
// How, deterministically: a chain first -> second. A test transaction T inserts an event_history row for SECOND and does not commit. The verb, addressed to FIRST, locks second, then blocks reading
// second's history. T then asks for second's row, which the verb holds: each waits for the other. T runs at DEADLOCK_PRIORITY HIGH, so SQL Server kills the verb. Like the signal and
// continue-as-new twins in an_mssql_signal_delivery_chosen_as_a_deadlock_victim_is_retried_test.go it shows the retry works, not that this is the deadlock CI once saw.
func TestAnMSSQLCancelOrTerminateChosenAsADeadlockVictimIsRetriedAndActsOnce(t *testing.T) {
	type verb struct {
		name   string
		status string
		do     func(context.Context, *MSSQLStore, string) (SettleReceipt, error)
	}
	verbs := []verb{
		{"cancel", statusCancelled, func(ctx context.Context, s *MSSQLStore, id string) (SettleReceipt, error) {
			return s.CancelWorkflowReceipt(ctx, id, "victim")
		}},
		{"terminate", statusTerminated, func(ctx context.Context, s *MSSQLStore, id string) (SettleReceipt, error) {
			return s.TerminateWorkflowReceipt(ctx, id, "victim")
		}},
	}
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
			parent := newIntentWorkflow(t, ctx, store, "deadlock-settle-parent")

			for _, v := range verbs {
				v := v
				t.Run(v.name, func(t *testing.T) {
					first := settleRunOpen(t, ctx, store, parent)
					second := continueChain(t, ctx, store, first)

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
					if _, err := tx.ExecContext(ctx, `INSERT INTO event_history (workflow_id, step, tenant_id) VALUES (@p1, 1, @p2)`, second, ms.tenantID); err != nil {
						t.Fatalf("hold an event_history row of the successor: %v", err)
					}

					type result struct {
						rec SettleReceipt
						err error
					}
					done := make(chan result, 1)
					go func() {
						rec, err := v.do(ctx, ms, first)
						done <- result{rec, err}
					}()

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
							t.Fatalf("the verb never blocked on the successor's history, so no deadlock can be made")
						}
						time.Sleep(20 * time.Millisecond)
					}

					// T asks for the run's row, which the verb holds: the cycle. It returns when SQL Server has killed the verb.
					if _, err := tx.ExecContext(ctx, `SELECT 1 FROM workflow_instances WITH (UPDLOCK, ROWLOCK) WHERE id = @p1 AND tenant_id = @p2`, second, ms.tenantID); err != nil {
						t.Fatalf("T's lock of the successor failed (T was meant to survive, at DEADLOCK_PRIORITY HIGH): %v", err)
					}
					if err := tx.Rollback(); err != nil {
						t.Fatalf("rollback: %v", err)
					}

					select {
					case r := <-done:
						if r.err != nil {
							t.Fatalf("the %s chosen as the deadlock victim returned %v, want it retried and applied", v.name, r.err)
						}
						if r.rec.SettledRun != second || !r.rec.Forwarded {
							t.Errorf("receipt = %+v, want it to say the successor %s was acted on", r.rec, second)
						}
					case <-time.After(30 * time.Second):
						t.Fatalf("the %s did not finish within 30s of T letting go", v.name)
					}
					if after := deadlocks(); after <= before {
						t.Errorf("the deadlock counter did not move (%d -> %d): no deadlock was made, so this passed without exercising the retry", before, after)
					}
					if got := statusOf(t, ctx, store, second); got != v.status {
						t.Errorf("the successor is %q, want %q", got, v.status)
					}
					if got := statusOf(t, ctx, store, first); got != statusDone {
						t.Errorf("the run that continued is %q, want it left done", got)
					}
				})
			}
		})
	}
}
