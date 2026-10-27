package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// cleat#3245 B3c: on PostgreSQL every writer writes the lease columns to
// workflow_leases alone, and the lease row's own UPDATE is the fence. The
// workflow_instances copy of those columns is written at INSERT and never
// again; B5's baseline regeneration drops it.
//
// The four tests here pin that from four sides:
//
//   - no writer touches the instance copy, and each one does change the
//     lease row (otherwise a writer that did nothing would pass);
//   - every fence decides on the lease row: with the lease generation moved
//     on and the instance copy agreeing with the stale caller, the write is
//     refused -- a fence still on workflow_instances would let it through;
//   - the settle paths lock the lease row, not the instance row;
//   - the reaper re-checks staleness on the row it updates, so a heartbeat
//     that commits while the reap waits keeps its run.

// instanceLeaseColumns are the columns workflow_instances still carries that
// a writer could change. Inserted-once columns (task_queue, priority,
// parent_workflow_id, continued_from and the rest) are left out because no
// writer ever touched them.
const instanceLeaseColumns = `status, assigned_to, heartbeat_at, next_wake_at, sticky_worker_id,
	generation, pending_terminal_status, defer_phase_deadline, signal_seq, signal_seq_at_claim,
	signal_consumed_seq, signal_consumed_at_claim, reclaim_count, started_at, completed_by,
	promise_seq, promise_seq_at_claim`

func instanceLeaseCopy(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var s string
	if err := db.QueryRow(`SELECT row_to_json(r)::text FROM (SELECT `+instanceLeaseColumns+
		` FROM workflow_instances WHERE id = $1) r`, id).Scan(&s); err != nil {
		t.Fatalf("read workflow_instances %s: %v", id, err)
	}
	return s
}

func leaseRowOf(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var s string
	if err := db.QueryRow(`SELECT row_to_json(l)::text FROM workflow_leases l WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("read workflow_leases %s: %v", id, err)
	}
	return s
}

const leaseOnlyWorker = "b3c2-worker"

// leaseOnlyRun starts a run named for the subtest and, if claim is set,
// claims it. The name is also StartNewRun's idempotency key, which outlives
// clearInstances, so it carries the subtest's name.
func leaseOnlyRun(t *testing.T, store *PostgresStore, db *sql.DB, claim bool) (id string, gen int64) {
	t.Helper()
	clearInstances(t, db)
	id = startPayloadReaderRun(t, store, "b3c2-"+t.Name())
	if !claim {
		return id, 0
	}
	wf, err := store.ClaimWorkflow(context.Background(), leaseOnlyWorker)
	if err != nil || wf == nil || wf.ID != id {
		t.Fatalf("ClaimWorkflow = %v, %v; want %s", wf, err, id)
	}
	return id, wf.Generation
}

// owesADeferPhase gives a run a recorded defer, so a terminate takes the
// two-phase arm.
func owesADeferPhase(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO event_history (workflow_id, step, event_type, tenant_id) VALUES ($1, 0, 'defer', $2)`,
		id, DefaultTenantUUID); err != nil {
		t.Fatalf("record a defer: %v", err)
	}
}

func leaseGeneration(t *testing.T, db *sql.DB, id string) int64 {
	t.Helper()
	var gen int64
	if err := db.QueryRow(`SELECT generation FROM workflow_leases WHERE id = $1`, id).Scan(&gen); err != nil {
		t.Fatalf("read lease generation: %v", err)
	}
	return gen
}

func claimedRun(store *PostgresStore, db *sql.DB) func(t *testing.T) string {
	return func(t *testing.T) string {
		id, _ := leaseOnlyRun(t, store, db, true)
		return id
	}
}

func staleRun(store *PostgresStore, db *sql.DB) func(t *testing.T) string {
	return func(t *testing.T) string {
		id, _ := leaseOnlyRun(t, store, db, true)
		if _, err := db.Exec(`UPDATE workflow_leases SET heartbeat_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
			t.Fatalf("make the heartbeat stale: %v", err)
		}
		return id
	}
}

// terminatingClaimedRun is a run in its defer phase, claimed again so a
// worker can finish it.
func terminatingClaimedRun(store *PostgresStore, db *sql.DB) func(t *testing.T) string {
	return func(t *testing.T) string {
		id, _ := leaseOnlyRun(t, store, db, true)
		owesADeferPhase(t, db, id)
		if err := store.TerminateWorkflow(context.Background(), id, "b3c2"); err != nil {
			t.Fatalf("TerminateWorkflow: %v", err)
		}
		wf, err := store.ClaimWorkflow(context.Background(), leaseOnlyWorker)
		if err != nil || wf == nil || wf.ID != id {
			t.Fatalf("claim the defer phase = %v, %v; want %s", wf, err, id)
		}
		return id
	}
}

// childOfClaimedParent returns a child started under a claimed parent, with
// the given close policy. The compared run is the child.
func childOfClaimedParent(store *PostgresStore, db *sql.DB, policy string) func(t *testing.T) string {
	return func(t *testing.T) string {
		parent, _ := leaseOnlyRun(t, store, db, true)
		child, err := store.StartChildWorkflow(context.Background(), parent, "b3c2-"+t.Name(), `{}`, 1, policy, 0)
		if err != nil {
			t.Fatalf("StartChildWorkflow: %v", err)
		}
		return child
	}
}

func finalizeTheParent(store *PostgresStore, db *sql.DB) func(t *testing.T, child string) error {
	return func(t *testing.T, child string) error {
		var parent string
		if err := db.QueryRow(`SELECT parent_workflow_id FROM workflow_leases WHERE id = $1`, child).Scan(&parent); err != nil {
			t.Fatalf("find the parent: %v", err)
		}
		return store.FinalizeWorkflowSegment(context.Background(), parent, leaseOnlyWorker, leaseGeneration(t, db, parent),
			nil, "done", `{}`, "", "", nil, time.Time{})
	}
}

// TestTheSettlePathsLockTheLeaseRowAndNotTheInstanceRow covers
// preemptivelySettle (TerminateWorkflow) and adminForceResolve
// (AdminForceComplete), which take a row lock before reading the state they
// act on. Since cleat#3245 B3c that lock is on workflow_leases.
func TestTheSettlePathsLockTheLeaseRowAndNotTheInstanceRow(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()

	// writesInstance: the admin path's audit event bumps
	// workflow_instances.event_count, so it does lock the instance row, by
	// design and before the lease row -- see
	// TestTheAdminPathsLockTheInstanceRowBeforeTheLeaseRow.
	paths := []struct {
		name           string
		run            func(id string) error
		writesInstance bool
	}{
		{"TerminateWorkflow", func(id string) error { return store.TerminateWorkflow(ctx, id, "b3c2") }, false},
		{"AdminForceComplete", func(id string) error {
			return store.AdminForceComplete(ctx, id, leaseGeneration(t, db, id), `{}`, "b3c2")
		}, true},
	}
	for _, p := range paths {
		for _, lock := range []struct {
			name, sql  string
			wantBlocks bool
		}{
			{"lease row held", `SELECT id FROM workflow_leases WHERE id = $1 FOR UPDATE`, true},
			{"instance row held", `SELECT id FROM workflow_instances WHERE id = $1 FOR UPDATE`, p.writesInstance},
		} {
			t.Run(p.name+"/"+lock.name, func(t *testing.T) {
				id, _ := leaseOnlyRun(t, store, db, true)
				holder, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatalf("begin holder: %v", err)
				}
				defer holder.Rollback()
				if _, err := holder.ExecContext(ctx, lock.sql, id); err != nil {
					t.Fatalf("holder lock: %v", err)
				}
				done := make(chan error, 1)
				go func() { done <- p.run(id) }()
				if lock.wantBlocks {
					select {
					case err := <-done:
						t.Fatalf("%s returned (%v) while another transaction held the %s -- it no longer locks it", p.name, err, lock.name)
					case <-time.After(400 * time.Millisecond):
					}
					if err := holder.Rollback(); err != nil {
						t.Fatalf("release holder: %v", err)
					}
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("%s: %v", p.name, err)
					}
				case <-time.After(10 * time.Second):
					if lock.wantBlocks {
						t.Fatalf("%s still blocked 10s after the holder released", p.name)
					}
					t.Fatalf("%s blocked behind a lock on the workflow_instances row -- it still locks that row", p.name)
				}
			})
		}
	}
}

// TestTheReaperRechecksStalenessAfterWaiting holds a heartbeat open on a run
// whose committed heartbeat is stale, starts the reaper, and commits the
// heartbeat while the reaper waits on the row. Under READ COMMITTED a waiting
// UPDATE re-checks only its own WHERE against the new row version, never the
// subquery that picked its candidates, so it is the outer predicate that must
// see the fresh heartbeat.
func TestTheReaperRechecksStalenessAfterWaiting(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()
	for _, reaper := range []struct {
		name string
		reap func() (int, error)
	}{
		{"ReapStaleInstances", func() (int, error) { return store.ReapStaleInstances(ctx, time.Minute, 0) }},
		{"ReapStaleInstancesExcept", func() (int, error) { return store.ReapStaleInstancesExcept(ctx, time.Minute, 0, nil) }},
	} {
		t.Run(reaper.name, func(t *testing.T) {
			id := staleRun(store, db)(t)
			heartbeat, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin heartbeat: %v", err)
			}
			defer heartbeat.Rollback()
			if _, err := heartbeat.ExecContext(ctx, `UPDATE workflow_leases SET heartbeat_at = now() WHERE id = $1`, id); err != nil {
				t.Fatalf("heartbeat: %v", err)
			}
			type result struct {
				n   int
				err error
			}
			done := make(chan result, 1)
			go func() { n, err := reaper.reap(); done <- result{n, err} }()
			select {
			case r := <-done:
				t.Fatalf("%s returned (%d, %v) without waiting on the row the heartbeat holds", reaper.name, r.n, r.err)
			case <-time.After(400 * time.Millisecond):
			}
			if err := heartbeat.Commit(); err != nil {
				t.Fatalf("commit heartbeat: %v", err)
			}
			select {
			case r := <-done:
				if r.err != nil {
					t.Fatalf("%s: %v", reaper.name, r.err)
				}
				if r.n != 0 {
					t.Fatalf("%s reclaimed %d run(s) after a heartbeat committed while it waited -- "+
						"it re-checked only the candidate subquery, not the row it updated", reaper.name, r.n)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s still blocked 10s after the heartbeat committed", reaper.name)
			}
			var status string
			if err := db.QueryRow(`SELECT status FROM workflow_leases WHERE id = $1`, id).Scan(&status); err != nil {
				t.Fatalf("read the lease: %v", err)
			}
			if status != "running" {
				t.Errorf("lease status = %q after the reap, want still running", status)
			}
		})
	}
}

// TestTheAdminPathsLockTheInstanceRowBeforeTheLeaseRow pins the order that
// keeps the admin verbs from deadlocking with a worker finalizing the same
// run. Paths that append events in the same transaction as a lease write
// (FinalizeWorkflowSegment, ContinueAsNew, FinalizeDeferPhase) take the
// instance row first, by bumping event_count. The holder here does exactly
// that, then reaches for the lease row while the admin path waits. With the
// admin path waiting on the instance row, the holder gets the lease row at
// once. With the admin path holding the lease row, the two deadlock.
func TestTheAdminPathsLockTheInstanceRowBeforeTheLeaseRow(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()

	paths := []struct {
		name  string
		setup func(t *testing.T) string
		run   func(id string) error
	}{
		{"AdminForceComplete", claimedRun(store, db), func(id string) error {
			return store.AdminForceComplete(ctx, id, leaseGeneration(t, db, id), `{}`, "b3c2")
		}},
		{"AdminForceFail two-phase", func(t *testing.T) string {
			id, _ := leaseOnlyRun(t, store, db, true)
			owesADeferPhase(t, db, id)
			return id
		}, func(id string) error {
			return store.AdminForceFail(ctx, id, leaseGeneration(t, db, id), "boom", "E", "b3c2")
		}},
		{"AdminReReplay", func(t *testing.T) string {
			id, gen := leaseOnlyRun(t, store, db, true)
			if err := store.FailWorkflow(ctx, id, leaseOnlyWorker, gen, "boom", "E", "op", nil); err != nil {
				t.Fatalf("FailWorkflow: %v", err)
			}
			return id
		}, func(id string) error {
			return store.AdminReReplay(ctx, id, leaseGeneration(t, db, id), "b3c2")
		}},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			id := p.setup(t)
			gen := leaseGeneration(t, db, id)
			holder, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin holder: %v", err)
			}
			defer holder.Rollback()
			if _, err := holder.ExecContext(ctx, `UPDATE workflow_instances SET event_count = event_count WHERE id = $1`, id); err != nil {
				t.Fatalf("holder takes the instance row: %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- p.run(id) }()
			select {
			case err := <-done:
				t.Fatalf("%s returned (%v) while another transaction held the instance row -- it no longer takes the instance row first", p.name, err)
			case <-time.After(400 * time.Millisecond):
			}
			leaseCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if _, err := holder.ExecContext(leaseCtx, `UPDATE workflow_leases SET heartbeat_at = heartbeat_at WHERE id = $1 AND generation = $2`, id, gen); err != nil {
				t.Fatalf("the holder could not take the lease row while %s waited (%v) -- %s took the lease row before the instance row, the order that deadlocks with a finalizing worker", p.name, err, p.name)
			}
			if err := holder.Commit(); err != nil {
				t.Fatalf("commit holder: %v", err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("%s: %v", p.name, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s still blocked 10s after the holder committed", p.name)
			}
		})
	}
}
