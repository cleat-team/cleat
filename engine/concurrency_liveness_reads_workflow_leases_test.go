package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// cleat#3245 B3c: on PostgreSQL the run-liveness arm (cleat#1965 -- a held
// concurrency key or queue slot counts only while its run is non-terminal)
// judges the holder by workflow_leases.status.
//
// Each test seeds a holder whose two copies disagree, in both directions, so a
// predicate still on workflow_instances gives the opposite answer each time.
//
// The bare-key arm is observed through CountRunnableWorkflows, which mirrors
// the claim's candidate predicate. ClaimWorkflow cannot observe that arm:
// acquiring a bare key is INSERT ... ON CONFLICT DO NOTHING, so a dead
// holder's row blocks the claim until the reaper frees it, whichever table the
// candidate predicate read. The registered-queue arm can be observed through
// ClaimWorkflow, because admission there is a count of live holders, and
// acquiring a slot inserts a new holder row with nothing to conflict on.

// seedKeyHolderAndWaiter starts two runs sharing the concurrency key, makes the
// first one hold it, and gives the holder the two statuses.
func seedKeyHolderAndWaiter(t *testing.T, store *PostgresStore, db *sql.DB, key, leaseStatus, instanceStatus string, holds func(holder string)) (holder, waiter string) {
	t.Helper()
	clearInstances(t, db)
	for _, table := range []string{"concurrency_keys", "queue_holders", "queues"} {
		if _, err := db.Exec(`DELETE FROM ` + table); err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
	// The names are also StartNewRun's idempotency keys, which outlive the
	// DELETE above, so each case gets its own.
	holder = startPayloadReaderRun(t, store, "b3c-liveness-holder-"+t.Name())
	waiter = startPayloadReaderRun(t, store, "b3c-liveness-waiter-"+t.Name())
	hash := retentionKeyHash(key)
	for _, id := range []string{holder, waiter} {
		diverge(t, db, "workflow_instances", `concurrency_key_hash = $2`, id, hash)
		// The key's text lives only on workflow_leases (cleat#3360 dropped it
		// from workflow_instances); a claim copies it into the row it takes.
		diverge(t, db, "workflow_leases", `concurrency_key_hash = $2, concurrency_key = $3`, id, hash, key)
	}
	holds(holder)
	splitStatus(t, db, holder, leaseStatus, instanceStatus)
	return holder, waiter
}

var livenessCases = []struct {
	name                        string
	holderLease, holderInstance string
	keyFree                     bool
}{
	{"holder finished by its lease", "done", "running", true},
	{"holder live by its lease", "running", "done", false},
}

func TestTheBareKeyLivenessArmReadsStatusFromWorkflowLeases(t *testing.T) {
	// One store per test, opened before the cases, so a missing PostgreSQL DSN
	// skips the test itself rather than each case; seedKeyHolderAndWaiter
	// resets the tables every case uses.
	store, db := newRunTestStore(t)
	for _, c := range livenessCases {
		t.Run(c.name, func(t *testing.T) {
			const key = "b3c-bare-key"
			seedKeyHolderAndWaiter(t, store, db, key, c.holderLease, c.holderInstance, func(holder string) {
				if _, err := db.Exec(`
					INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at, tenant_id)
					VALUES ($1, $2, $3, $4, $5)`,
					retentionKeyHash(key), key, holder, time.Now().UTC().Add(time.Hour), DefaultTenantUUID); err != nil {
					t.Fatalf("seed the holder's key: %v", err)
				}
			})

			n, err := store.CountRunnableWorkflows(context.Background())
			if err != nil {
				t.Fatalf("CountRunnableWorkflows: %v", err)
			}
			want := 0
			if c.keyFree {
				want = 1 // the waiter; the holder is not 'ready' in either case
			}
			if n != want {
				t.Fatalf("CountRunnableWorkflows = %d, want %d -- the bare-key liveness arm judged the holder by workflow_instances.status", n, want)
			}
		})
	}
}

func TestTheRegisteredQueueLivenessArmReadsStatusFromWorkflowLeases(t *testing.T) {
	// One store per test, opened before the cases, so a missing PostgreSQL DSN
	// skips the test itself rather than each case; seedKeyHolderAndWaiter
	// resets the tables every case uses.
	store, db := newRunTestStore(t)
	for _, c := range livenessCases {
		t.Run(c.name, func(t *testing.T) {
			const queue = "b3c-queue"
			_, waiter := seedKeyHolderAndWaiter(t, store, db, queue, c.holderLease, c.holderInstance, func(holder string) {
				if _, err := db.Exec(`INSERT INTO queues (tenant_id, name, concurrency_limit) VALUES ($1, $2, 1)`,
					DefaultTenantUUID, queue); err != nil {
					t.Fatalf("seed queue: %v", err)
				}
				if _, err := db.Exec(`
					INSERT INTO queue_holders (tenant_id, queue_name, workflow_id, expires_at)
					VALUES ($1, $2, $3, $4)`,
					DefaultTenantUUID, queue, holder, time.Now().UTC().Add(time.Hour)); err != nil {
					t.Fatalf("seed the holder's slot: %v", err)
				}
			})

			// CountRunnableWorkflows carries its own copy of the registered
			// arm, so it is asserted before the claim takes the slot.
			n, err := store.CountRunnableWorkflows(context.Background())
			if err != nil {
				t.Fatalf("CountRunnableWorkflows: %v", err)
			}
			if want := map[bool]int{true: 1, false: 0}[c.keyFree]; n != want {
				t.Fatalf("CountRunnableWorkflows = %d, want %d -- its registered-queue liveness arm judged the holder by workflow_instances.status", n, want)
			}

			wf, err := store.ClaimWorkflow(context.Background(), "b3c-liveness-worker")
			if err != nil {
				t.Fatalf("ClaimWorkflow: %v", err)
			}
			switch {
			case c.keyFree && (wf == nil || wf.ID != waiter):
				t.Fatalf("ClaimWorkflow = %v, want the waiter %s -- the only slot's holder is finished by workflow_leases; a liveness arm on workflow_instances.status still counts it", wf, waiter)
			case !c.keyFree && wf != nil:
				t.Fatalf("ClaimWorkflow = %s, want nothing -- the only slot's holder is live by workflow_leases; a liveness arm on workflow_instances.status frees its slot", wf.ID)
			}
		})
	}
}

// TestTheWorkerCapLivenessArmReadsStatusFromWorkflowLeases covers the per-worker
// cap (queues.worker_concurrency): the queue admits plenty of holders overall,
// but this worker may hold one slot, and it already holds one for the holder.
// Only the worker-cap count can refuse the claim, so it alone decides.
func TestTheWorkerCapLivenessArmReadsStatusFromWorkflowLeases(t *testing.T) {
	const worker = "b3c-liveness-worker"
	// One store per test, opened before the cases, so a missing PostgreSQL DSN
	// skips the test itself rather than each case; seedKeyHolderAndWaiter
	// resets the tables every case uses.
	store, db := newRunTestStore(t)
	for _, c := range livenessCases {
		t.Run(c.name, func(t *testing.T) {
			const queue = "b3c-capped-queue"
			_, waiter := seedKeyHolderAndWaiter(t, store, db, queue, c.holderLease, c.holderInstance, func(holder string) {
				if _, err := db.Exec(`INSERT INTO queues (tenant_id, name, concurrency_limit, worker_concurrency) VALUES ($1, $2, 10, 1)`,
					DefaultTenantUUID, queue); err != nil {
					t.Fatalf("seed queue: %v", err)
				}
				if _, err := db.Exec(`
					INSERT INTO queue_holders (tenant_id, queue_name, workflow_id, expires_at, worker_id)
					VALUES ($1, $2, $3, $4, $5)`,
					DefaultTenantUUID, queue, holder, time.Now().UTC().Add(time.Hour), worker); err != nil {
					t.Fatalf("seed the holder's slot: %v", err)
				}
			})

			wf, err := store.ClaimWorkflow(context.Background(), worker)
			if err != nil {
				t.Fatalf("ClaimWorkflow: %v", err)
			}
			switch {
			case c.keyFree && (wf == nil || wf.ID != waiter):
				t.Fatalf("ClaimWorkflow = %v, want the waiter %s -- the worker's one held slot belongs to a run finished by workflow_leases; a worker-cap arm on workflow_instances.status still counts it", wf, waiter)
			case !c.keyFree && wf != nil:
				t.Fatalf("ClaimWorkflow = %s, want nothing -- the worker's one held slot belongs to a run live by workflow_leases; a worker-cap arm on workflow_instances.status frees it", wf.ID)
			}
		})
	}
}
