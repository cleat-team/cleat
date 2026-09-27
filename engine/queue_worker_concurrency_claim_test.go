package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// A registered queue's worker_concurrency is enforced by the claim, not
// merely declared -- the behavioural half of cleat#1917, the way
// queue_limit_claim_test.go and queue_rate_limit_claim_test.go are the
// behavioural halves of cleat#1116 and cleat#1918. There is no shared
// predicate text for this arm to check against
// TestTheClaimableConcurrencyKeyPredicateIsIdenticalAtEverySite: the cap is
// read and enforced at exactly one site per dialect (acquireCandidateConcurrencyKey),
// not duplicated across a snapshot predicate and an acquire step the way the
// concurrency and rate arms are -- see that test's own header for why
// duplication is what earns a guard. These tests hold the semantics directly.

// countQueueHoldersForWorker counts live queue_holders rows for a queue,
// scoped to one worker_id -- what acquireCandidateConcurrencyKey's own
// worker-cap check reads.
func countQueueHoldersForWorker(ctx context.Context, db *sql.DB, dialect, name, workerID string) (int, error) {
	var q string
	switch dialect {
	case "postgres":
		q = `SELECT count(*) FROM queue_holders WHERE queue_name = $1 AND worker_id = $2 AND expires_at > now()`
	case "mysql":
		q = `SELECT count(*) FROM queue_holders WHERE queue_name = ? AND worker_id = ? AND expires_at > NOW(6)`
	case "mssql":
		q = `SELECT count(*) FROM queue_holders WHERE queue_name = @p1 AND worker_id = @p2 AND expires_at > SYSUTCDATETIME()`
	default:
		return 0, fmt.Errorf("unknown dialect %q", dialect)
	}
	var n int
	if err := db.QueryRowContext(ctx, q, name, workerID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func mustCountQueueHoldersForWorker(t *testing.T, ctx context.Context, db *sql.DB, dialect, name, workerID string) int {
	t.Helper()
	n, err := countQueueHoldersForWorker(ctx, db, dialect, name, workerID)
	if err != nil {
		t.Fatalf("count queue_holders for worker %q: %v", workerID, err)
	}
	return n
}

// TestARegisteredQueueWorkerConcurrencyAdmitsAtMostItsLimit is the
// failing-twin shape cleat#1917's design asks for, mirroring
// TestARegisteredQueueRateLimitAdmitsAtMostItsLimit: a worker-capped queue
// and an otherwise identical UNCAPPED queue are seeded with the same
// population and claimed by the same worker in the same run. The uncapped
// arm is what rules out a limiter that silently does nothing: it must admit
// MORE than the cap in the same claim that the capped arm does not exceed
// it, so both arms can only pass if the cap is actually doing the refusing.
//
// It also proves the cap is PER-WORKER, not a second queue-wide limit: a
// different worker's later claim admits MORE from the already-capped queue,
// which a queue-global limit (concurrency_limit) would never do.
func TestARegisteredQueueWorkerConcurrencyAdmitsAtMostItsLimit(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			const (
				workerConcurrency = 3
				concurrencyLimit  = 100 // high enough that the global cap never gates first
				perQueue          = 10
			)
			db := queueClaimTestDB(t, store)
			qs := NewQueueStore(db, backend.Name())

			cappedName := queueTestName("worker-capped")
			wc := workerConcurrency
			if err := qs.CreateQueue(ctx, DefaultTenantUUID, cappedName, concurrencyLimit, nil, nil, &wc); err != nil {
				t.Fatalf("CreateQueue(capped): %v", err)
			}

			uncappedName := queueTestName("worker-uncapped")
			if err := qs.CreateQueue(ctx, DefaultTenantUUID, uncappedName, concurrencyLimit, nil, nil, nil); err != nil {
				t.Fatalf("CreateQueue(uncapped): %v", err)
			}

			starter, ok := store.(claimQueueConcurrencyKeyStarter)
			if !ok {
				t.Fatalf("%T cannot record a concurrency key", store)
			}
			for i := 0; i < perQueue; i++ {
				if _, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
					fmt.Sprintf("worker-capped-%d", i), "test-workflow", 1, json.RawMessage(`{}`),
					"", DefaultTenantUUID, 0, cappedName); err != nil {
					t.Fatalf("start capped run %d: %v", i, err)
				}
				if _, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
					fmt.Sprintf("worker-uncapped-%d", i), "test-workflow", 1, json.RawMessage(`{}`),
					"", DefaultTenantUUID, 0, uncappedName); err != nil {
					t.Fatalf("start uncapped run %d: %v", i, err)
				}
			}

			claimed, err := store.ClaimWorkflows(ctx, "worker-1", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}

			gotCapped := mustCountQueueHoldersForWorker(t, ctx, db, backend.Name(), cappedName, "worker-1")
			if gotCapped != workerConcurrency {
				t.Fatalf("worker-1 holds %d of the capped queue's slots, want exactly %d (the declared worker concurrency)",
					gotCapped, workerConcurrency)
			}

			// The failing twin.
			gotUncapped := mustCountQueueHoldersForWorker(t, ctx, db, backend.Name(), uncappedName, "worker-1")
			if gotUncapped <= workerConcurrency {
				t.Fatalf("worker-1 holds %d of the uncapped queue's slots, want more than %d -- "+
					"if this arm does not exceed the cap too, the fixture cannot tell a real "+
					"per-worker cap from one that does nothing", gotUncapped, workerConcurrency)
			}
			if gotUncapped != perQueue {
				t.Fatalf("worker-1 holds %d of the uncapped queue's slots, want all %d (no worker cap, no concurrency pressure at %d)",
					gotUncapped, perQueue, concurrencyLimit)
			}

			if len(claimed) != workerConcurrency+perQueue {
				t.Fatalf("claimed %d total, want %d (capped queue's %d plus uncapped queue's %d)",
					len(claimed), workerConcurrency+perQueue, workerConcurrency, perQueue)
			}

			// DBOS parity, not machine protection: a DIFFERENT worker's claim
			// against the SAME capped queue admits more, up to its own cap --
			// exactly what a queue-global limit would never allow. See the
			// queue store's own Purpose comment.
			next, err := store.ClaimWorkflows(ctx, "worker-2", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows(worker-2): %v", err)
			}
			gotCappedWorker2 := mustCountQueueHoldersForWorker(t, ctx, db, backend.Name(), cappedName, "worker-2")
			if gotCappedWorker2 != workerConcurrency {
				t.Fatalf("worker-2 holds %d of the capped queue's slots, want exactly %d -- "+
					"the cap is per-worker, so a second worker should reach its own cap independently of worker-1's",
					gotCappedWorker2, workerConcurrency)
			}
			if len(next) != workerConcurrency {
				t.Fatalf("worker-2's claim took %d, want %d (its own share of the capped queue; "+
					"the uncapped queue's remaining rows were already exhausted by worker-1)",
					len(next), workerConcurrency)
			}
		})
	}
}

// TestAParkedRunStillCountsAgainstItsWorkersConcurrency is cleat#1917's other
// required test: "run 1 sleeps and run 2 is not admitted to the same
// worker." This is the case that separates decision 3 (a worker's count
// includes its PARKED holders) from the executing-only rule the design
// rejected -- an executing-only count would let run 2 in the moment run 1
// stops running, which is exactly what must not happen here.
func TestAParkedRunStillCountsAgainstItsWorkersConcurrency(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			const (
				workerConcurrency = 1
				concurrencyLimit  = 2 // room for both runs globally; only the worker cap should bind
			)
			db := queueClaimTestDB(t, store)
			qs := NewQueueStore(db, backend.Name())

			name := queueTestName("parked")
			wc := workerConcurrency
			if err := qs.CreateQueue(ctx, DefaultTenantUUID, name, concurrencyLimit, nil, nil, &wc); err != nil {
				t.Fatalf("CreateQueue: %v", err)
			}

			starter, ok := store.(claimQueueConcurrencyKeyStarter)
			if !ok {
				t.Fatalf("%T cannot record a concurrency key", store)
			}
			if _, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
				"parked-run-1", "test-workflow", 1, json.RawMessage(`{}`),
				"", DefaultTenantUUID, 0, name); err != nil {
				t.Fatalf("start run 1: %v", err)
			}
			if _, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
				"parked-run-2", "test-workflow", 1, json.RawMessage(`{}`),
				"", DefaultTenantUUID, 0, name); err != nil {
				t.Fatalf("start run 2: %v", err)
			}

			// worker-1 claims; only run 1 fits under its cap of 1. Which of the
			// two rows it takes is not the point of this test (either is a valid
			// admission of exactly one), so the test reads it back rather than
			// assuming an order.
			first, err := store.ClaimWorkflows(ctx, "worker-1", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows(worker-1) first: %v", err)
			}
			if len(first) != 1 {
				t.Fatalf("worker-1's first claim took %d, want exactly 1 (its worker concurrency cap)", len(first))
			}
			held := first[0]

			// Run 1 sleeps: released back to 'ready'/assigned_to=NULL with a
			// wake time in the past, so it is immediately claimable again --
			// ReleaseWorkflow, the exact mechanism a durable sleep uses
			// (cleat-ports' DurableSleepMs fixture, cited in the issue).
			if err := store.ReleaseWorkflow(ctx, held.ID, "worker-1", held.Generation, time.Now().Add(-time.Second)); err != nil {
				t.Fatalf("ReleaseWorkflow (park run 1): %v", err)
			}

			// The parked run still holds its slot: worker-1's own holder count
			// on this queue must be unchanged by parking.
			if got := mustCountQueueHoldersForWorker(t, ctx, db, backend.Name(), name, "worker-1"); got != workerConcurrency {
				t.Fatalf("worker-1 holds %d holder(s) on %q immediately after parking run 1, want %d -- "+
					"a parked run must still occupy its slot (decision 3)", got, name, workerConcurrency)
			}

			// worker-1 tries again: run 1 (parked, ready to wake) re-claims for
			// free under the unchanged re-claim shortcut, but run 2 must NOT be
			// admitted to worker-1 -- it is already at its cap of 1, occupied by
			// the sleeping run 1.
			second, err := store.ClaimWorkflows(ctx, "worker-1", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows(worker-1) second: %v", err)
			}
			for _, wf := range second {
				if wf.ID == "parked-run-2" {
					t.Fatalf("worker-1's second claim admitted run 2 while still holding run 1's slot; " +
						"an executing-only count would allow this, which is exactly what decision 3 refuses")
				}
			}
			if got := mustCountQueueHoldersForWorker(t, ctx, db, backend.Name(), name, "worker-1"); got != workerConcurrency {
				t.Fatalf("worker-1 holds %d holder(s) on %q after its second claim, want %d unchanged",
					got, name, workerConcurrency)
			}

			// A DIFFERENT worker has its own, independent cap and can take run 2
			// -- the cap is per-worker, not a second queue-wide limit blocking
			// everyone.
			third, err := store.ClaimWorkflows(ctx, "worker-2", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows(worker-2): %v", err)
			}
			foundRun2 := false
			for _, wf := range third {
				if wf.ID == "parked-run-2" {
					foundRun2 = true
				}
			}
			if !foundRun2 {
				t.Fatalf("worker-2's claim did not admit run 2, which no cap of worker-2's own should have refused")
			}
		})
	}
}
