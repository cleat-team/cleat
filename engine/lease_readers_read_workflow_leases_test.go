package engine

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3245 B3a: the engine's remaining PostgreSQL readers of the lease
// columns -- status, continued_from and the run_* limits -- read
// workflow_leases, not workflow_instances.
//
// Every writer keeps the two copies equal, so a test that only drives normal
// writes cannot tell which table a reader consulted. Each test here makes the
// copies disagree with diverge (one UPDATE, bypassing every mirror), and where
// it can it diverges in BOTH directions: the lease says one thing and the
// instance the opposite, so a reader still on workflow_instances gives the
// opposite answer rather than merely a missing one.
//
// GetChildResult, readReplacedOutcome and the retention sweeps are covered by
// payload_readers_read_workflow_payloads_test.go, whose fixtures put status on
// workflow_leases only since B3a.

// splitStatus gives id's lease one status and its instance another.
func splitStatus(t *testing.T, db *sql.DB, id, leaseStatus, instanceStatus string) {
	t.Helper()
	diverge(t, db, "workflow_leases", `status = $2`, id, leaseStatus)
}

func clearInstances(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`DELETE FROM workflow_instances`); err != nil {
		t.Fatalf("clear workflow_instances: %v", err)
	}
}

func TestQueueDepthReadsStatusFromWorkflowLeases(t *testing.T) {
	store, db := newRunTestStore(t)
	clearInstances(t, db)
	// Asymmetric on purpose: two runs ready by their lease, one ready only by
	// its instance. A count over one diverged run each way reads 1 from either
	// table -- each reading counts the other's run -- so it cannot tell them
	// apart. Here the lease reading is 2 and the instance reading is 1.
	for _, name := range []string{"b3a-queue-depth-counted-1", "b3a-queue-depth-counted-2"} {
		splitStatus(t, db, startPayloadReaderRun(t, store, name), "ready", "running")
	}
	splitStatus(t, db, startPayloadReaderRun(t, store, "b3a-queue-depth-ignored"), "running", "ready")

	depth, err := store.QueueDepth(context.Background())
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth != 2 {
		t.Fatalf("QueueDepth = %d, want 2 (the runs whose LEASE is ready) -- 1 means it read workflow_instances.status", depth)
	}
}

func TestReapExpiredConcurrencyKeysReadsStatusFromWorkflowLeases(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()
	clearInstances(t, db)
	for _, table := range []string{"concurrency_keys", "queue_holders"} {
		if _, err := db.Exec(`DELETE FROM ` + table); err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}

	finished := startPayloadReaderRun(t, store, "b3a-reap-key-finished")
	splitStatus(t, db, finished, "done", "running")
	live := startPayloadReaderRun(t, store, "b3a-reap-key-live")
	splitStatus(t, db, live, "running", "done")

	// Unexpired, so only the run-state arm can free any of these rows. The
	// reaper frees concurrency_keys and queue_holders by two separate
	// statements, so each gets a row per run.
	expires := time.Now().UTC().Add(time.Hour)
	for _, id := range []string{finished, live} {
		if _, err := db.Exec(`
			INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at, tenant_id)
			VALUES ($1, $2, $3, $4, $5)`,
			retentionKeyHash(id), id, id, expires, DefaultTenantUUID); err != nil {
			t.Fatalf("seed concurrency_keys for %s: %v", id, err)
		}
		if _, err := db.Exec(`
			INSERT INTO queue_holders (tenant_id, queue_name, workflow_id, expires_at)
			VALUES ($1, 'b3a-queue', $2, $3)`,
			DefaultTenantUUID, id, expires); err != nil {
			t.Fatalf("seed queue_holders for %s: %v", id, err)
		}
	}

	if _, err := store.ReapExpiredConcurrencyKeys(ctx); err != nil {
		t.Fatalf("ReapExpiredConcurrencyKeys: %v", err)
	}
	for _, table := range []string{"concurrency_keys", "queue_holders"} {
		for _, c := range []struct {
			id   string
			want int
		}{{finished, 0}, {live, 1}} {
			var n int
			if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE workflow_id = $1`, c.id).Scan(&n); err != nil {
				t.Fatalf("count %s: %v", table, err)
			}
			if n != c.want {
				t.Errorf("%s rows for %s = %d, want %d -- the reaper judged run state by workflow_instances.status", table, c.id, n, c.want)
			}
		}
	}
}

func TestActiveInstanceCountersReadStatusFromWorkflowLeases(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()
	clearInstances(t, db)
	const active, settled = "b3a-active-by-lease", "b3a-settled-by-lease"
	a := startPayloadReaderRun(t, store, active)
	splitStatus(t, db, a, "running", "done")
	s := startPayloadReaderRun(t, store, settled)
	splitStatus(t, db, s, "done", "running")

	for _, c := range []struct {
		name string
		want int
	}{{active, 1}, {settled, 0}} {
		n, err := store.CountActiveInstances(ctx, c.name, 1)
		if err != nil || n != c.want {
			t.Errorf("CountActiveInstances(%s) = %d, %v; want %d -- it read workflow_instances.status", c.name, n, err, c.want)
		}
	}

	counts, err := store.GetActiveInstanceCountsByVersion(ctx)
	if err != nil {
		t.Fatalf("GetActiveInstanceCountsByVersion: %v", err)
	}
	if counts[active+":1"] != 1 || counts[settled+":1"] != 0 {
		t.Errorf("GetActiveInstanceCountsByVersion = %v, want %s:1=1 and no %s -- it read workflow_instances.status", counts, active, settled)
	}

	versions, err := NewWorkflowLoader(db, nil, nil).ActiveVersions(ctx)
	if err != nil {
		t.Fatalf("ActiveVersions: %v", err)
	}
	if len(versions[active]) != 1 || len(versions[settled]) != 0 {
		t.Errorf("ActiveVersions = %v, want %s and not %s -- it read workflow_instances.status", versions, active, settled)
	}
}

func TestGetRunLimitsReadsWorkflowLeases(t *testing.T) {
	store, db := newRunTestStore(t)
	id := startPayloadReaderRun(t, store, "b3a-run-limits")
	diverge(t, db, "workflow_leases", `run_wasm_instance_timeout_ms = 1234`, id)

	got, err := store.GetRunLimits(context.Background(), id)
	if err != nil {
		t.Fatalf("GetRunLimits: %v", err)
	}
	if got.WasmInstanceTimeout != 1234*time.Millisecond {
		t.Fatalf("GetRunLimits().WasmInstanceTimeout = %v, want 1.234s -- 999ms means it read workflow_instances", got.WasmInstanceTimeout)
	}
}

func TestSuccessorOfRunReadsContinuedFromWorkflowLeases(t *testing.T) {
	store, db := newRunTestStore(t)
	clearInstances(t, db)
	pred := startPayloadReaderRun(t, store, "b3a-successor-pred")
	byLease := startPayloadReaderRun(t, store, "b3a-successor-by-lease")
	byInstance := startPayloadReaderRun(t, store, "b3a-successor-by-instance")
	diverge(t, db, "workflow_leases", `continued_from = $2`, byLease, pred)

	got, err := store.successorOfRun(context.Background(), pred)
	if err != nil {
		t.Fatalf("successorOfRun: %v", err)
	}
	if got != byLease {
		t.Fatalf("successorOfRun(%s) = %q, want %s (named by its lease) -- %s is named only by workflow_instances.continued_from",
			pred, got, byLease, byInstance)
	}
}

func TestPreemptiveSettleReadsStatusFromWorkflowLeases(t *testing.T) {
	store, db := newRunTestStore(t)
	id := startPayloadReaderRun(t, store, "b3a-preempt")
	// Settled by its lease, live by its instance: a reader still on
	// workflow_instances would cancel a finished run.
	splitStatus(t, db, id, "done", "ready")

	err := store.CancelWorkflow(context.Background(), id, "b3a")
	if !errors.Is(err, ErrAdminStateConflict) {
		t.Fatalf("CancelWorkflow over a lease that says done = %v, want ErrAdminStateConflict -- it read workflow_instances.status", err)
	}
}

func TestAdminForceResolveReadsStatusFromWorkflowLeases(t *testing.T) {
	store, db := newRunTestStore(t)
	id := startPayloadReaderRun(t, store, "b3a-force-resolve")
	splitStatus(t, db, id, "done", "ready")

	err := store.AdminForceFail(context.Background(), id, 0, "b3a", "E_B3A", "operator")
	if !errors.Is(err, ErrAdminStateConflict) {
		t.Fatalf("AdminForceFail over a lease that says done = %v, want ErrAdminStateConflict -- it read workflow_instances.status", err)
	}
}

// admin.in_flight_workflow_ids() is read by the blobstore and jobqueue sweeps
// across every tenant, from a connection that has no tenant set -- so it is
// called here as testutil's RLS role, which row-level security applies to.
// A function whose owner could not see workflow_leases would return nothing
// here, and the sweeps would treat every in-flight run as finished.
func TestInFlightWorkflowIDsReadsStatusFromWorkflowLeases(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()
	clearInstances(t, db)
	inFlight := startPayloadReaderRun(t, store, "b3a-in-flight-by-lease")
	splitStatus(t, db, inFlight, "running", "done")
	finished := startPayloadReaderRun(t, store, "b3a-finished-by-lease")
	splitStatus(t, db, finished, "done", "running")

	rlsDB := testutil.OpenPostgresRLSTestDB(t, db)
	defer func() { _ = rlsDB.Close() }()
	rows, err := rlsDB.QueryContext(ctx, `SELECT id FROM admin.in_flight_workflow_ids()`)
	if err != nil {
		t.Fatalf("admin.in_flight_workflow_ids(): %v", err)
	}
	ids, err := scanWorkflowIDs(rows)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if !got[inFlight] || got[finished] {
		t.Fatalf("admin.in_flight_workflow_ids() = %v, want %s and not %s -- the function read workflow_instances.status, or could not see workflow_leases",
			ids, inFlight, finished)
	}
}

// TestALockedStatusReadSeesAWriterThatCommittedWhileItWaited pins the
// freshness of the status that preemptivelySettle and adminForceResolve read
// under their row lock (cleat-review on #3399).
//
// Under READ COMMITTED, a single `SELECT ... FOR UPDATE OF w` that waits on
// w's lock re-checks only w's NEW version after the wait (EvalPlanQual). A
// joined workflow_leases row, and every subquery, keep the statement's
// original snapshot. So reading l.status in the locking statement returns the
// status from BEFORE a writer that committed while this one waited, whatever
// order that writer updated the two tables in.
//
// Here the writer settles the run to 'done' on both rows and holds its commit.
// Each path blocks on the instance row; once the writer commits, it must see
// 'done' and refuse. With a stale read it would see 'ready' and go on to
// overwrite a finished run.
func TestALockedStatusReadSeesAWriterThatCommittedWhileItWaited(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()

	paths := []struct {
		name string
		run  func(id string) error
	}{
		{"CancelWorkflow", func(id string) error { return store.CancelWorkflow(ctx, id, "b3a") }},
		{"AdminForceFail", func(id string) error {
			return store.AdminForceFail(ctx, id, 0, "b3a", "E_B3A", "operator")
		}},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			id := startPayloadReaderRun(t, store, "b3a-locked-read-"+p.name)

			writer, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin writer: %v", err)
			}
			defer writer.Rollback()
			// The admin paths lock the instance row and read status from the
			// lease row, so the writer holds the first and settles the second.
			// event_count = event_count changes nothing; it only takes the lock.
			if _, err := writer.ExecContext(ctx, `UPDATE workflow_instances SET event_count = event_count WHERE id = $1`, id); err != nil {
				t.Fatalf("writer: lock the instance row: %v", err)
			}
			if _, err := writer.ExecContext(ctx, `UPDATE workflow_leases SET status = 'done' WHERE id = $1`, id); err != nil {
				t.Fatalf("writer: settle workflow_leases: %v", err)
			}

			done := make(chan error, 1)
			go func() { done <- p.run(id) }()
			select {
			case err := <-done:
				t.Fatalf("%s returned (%v) while the writer held the instance row -- it did not wait, so this test measured nothing", p.name, err)
			case <-time.After(400 * time.Millisecond):
			}
			if err := writer.Commit(); err != nil {
				t.Fatalf("writer commit: %v", err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, ErrAdminStateConflict) {
					t.Fatalf("%s after a writer settled the run while it waited = %v, want ErrAdminStateConflict -- it read status from a snapshot older than its lock", p.name, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s still blocked 10s after the writer committed", p.name)
			}
		})
	}
}
