package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Group 6 — Signals
// ---------------------------------------------------------------------------

func TestDeliverSignal(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "deliver-signal-test", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if runID == "" {
				t.Fatal("StartNewRun returned empty runID")
			}

			if err := store.DeliverSignal(ctx, runID, "my-signal", `{"data":"hello"}`); err != nil {
				t.Fatalf("DeliverSignal: %v", err)
			}

			d, found, err := store.PollSignal(ctx, runID, "my-signal")
			if err != nil {
				t.Fatalf("PollSignal: %v", err)
			}
			if !found {
				t.Fatal("PollSignal: expected found=true")
			}
			if d.Payload != `{"data":"hello"}` {
				t.Fatalf("PollSignal: expected payload %q, got %q", `{"data":"hello"}`, d.Payload)
			}
		})
	}
}

func TestDeliverSignalWakesWorkflow(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			// --- Case 1: status='ready' ---
			// Create a ready workflow and set next_wake_at far in the future.
			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), "wakeup-ready", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun (ready): %v", err)
			}

			futureTime := time.Now().Add(1 * time.Hour)
			updateWorkflowNextWakeAt(t, store, runID, futureTime)

			// Deliver a signal — expect next_wake_at to be updated to now().
			//
			// Bracketed against the DATABASE's clock, not the Go process's.
			// DeliverSignal sets next_wake_at from the server's own now(), so the
			// stored value must lie between two readings of that same clock taken
			// either side of the call. That is an exact statement of the
			// behaviour and needs no tolerance for skew.
			//
			// This used to read `beforeDeliver := time.Now()` and allow ±1s. It
			// failed once on a loaded run (2026-09-01, engine suite at 93% CPU
			// and 1:46 wall against a usual 1:12).
			//
			// Two things were wrong with it, and measuring separated them.
			// Go-to-database clock skew on this machine is only 42ms
			// (postgres, mysql) to 74ms (mssql), so skew alone does not reach
			// the 1s window -- the first explanation was wrong. The dominant
			// term is CALL LATENCY: next_wake_at is written at the end of
			// DeliverSignal, so the gap from beforeDeliver to the stored value
			// is the whole round trip. Under load that exceeds a second and the
			// `diff > time.Second` arm fires.
			//
			// Bracketing fixes both at once: dbAfter is sampled after the call
			// returns, so however long the call takes, nw <= dbAfter. Widening
			// the window would only have moved the threshold.
			dbBefore := queryDatabaseNow(t, store)
			if err := store.DeliverSignal(ctx, runID, "wake-signal", "{}"); err != nil {
				t.Fatalf("DeliverSignal (ready): %v", err)
			}
			dbAfter := queryDatabaseNow(t, store)

			nw := queryWorkflowNextWakeAt(t, store, runID)
			// The slack absorbs stored-column precision only (MySQL truncates
			// DATETIME to the column's fractional seconds), never clock skew:
			// every value here comes from the same server.
			const precisionSlack = time.Second
			if nw.Before(dbBefore.Add(-precisionSlack)) || nw.After(dbAfter.Add(precisionSlack)) {
				t.Errorf("ready case: next_wake_at %v is outside [%v, %v], the database's "+
					"own clock either side of DeliverSignal — so it was not reset to now()",
					nw, dbBefore, dbAfter)
			}
			// And the point of the case: it moved off the far-future value.
			if !nw.Before(futureTime.Add(-time.Minute)) {
				t.Errorf("ready case: next_wake_at %v is still at or near the future value %v; "+
					"the signal did not wake the workflow", nw, futureTime)
			}

			// --- Case 2: status='running' (guard) ---
			runID2, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), "wakeup-running", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun (running): %v", err)
			}

			setWorkflowStatus(t, store, runID2, "running")
			futureTime2 := time.Now().Add(1 * time.Hour)
			updateWorkflowNextWakeAt(t, store, runID2, futureTime2)

			// Deliver a signal — next_wake_at should NOT change (status is 'running', not 'ready').
			if err := store.DeliverSignal(ctx, runID2, "wake-signal-2", "{}"); err != nil {
				t.Fatalf("DeliverSignal (running): %v", err)
			}

			// No database-clock bracket here, and it is not an oversight: this
			// case asserts next_wake_at did NOT change, comparing the value read
			// back against the one this test wrote. Both sides are the same Go
			// value round-tripped through the column, so only storage precision
			// is in play -- there is no second clock to disagree with.
			nw2 := queryWorkflowNextWakeAt(t, store, runID2)
			diff2 := nw2.Sub(futureTime2)
			if diff2 < -time.Second || diff2 > time.Second {
				t.Errorf("running case: next_wake_at %v should be close to %v (diff=%v) — was changed despite status='running'",
					nw2, futureTime2, diff2)
			}
		})
	}
}

func TestPollAndClaimSignal(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "poll-claim-signal-test", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if runID == "" {
				t.Fatal("StartNewRun returned empty runID")
			}

			if err := store.DeliverSignal(ctx, runID, "sig-1", "payload-1"); err != nil {
				t.Fatalf("DeliverSignal: %v", err)
			}

			// Poll, then consume by id -- what the await path does.
			d, found, err := store.PollSignal(ctx, runID, "sig-1")
			if err != nil {
				t.Fatalf("PollSignal (first): %v", err)
			}
			if !found {
				t.Fatal("PollSignal (first): expected found=true")
			}
			if d.Payload != "payload-1" {
				t.Fatalf("PollSignal (first): expected payload %q, got %q", "payload-1", d.Payload)
			}
			if err := store.ConsumeSignal(ctx, runID, d.ID); err != nil {
				t.Fatalf("ConsumeSignal: %v", err)
			}

			// Second call should return found=false — the delivery is gone.
			_, found, err = store.PollSignal(ctx, runID, "sig-1")
			if err != nil {
				t.Fatalf("PollSignal (second): %v", err)
			}
			if found {
				t.Fatal("PollSignal (second): expected found=false (the delivery was consumed)")
			}
		})
	}
}

func TestPollSignal_NotDelivered(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "poll-notfound-test", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if runID == "" {
				t.Fatal("StartNewRun returned empty runID")
			}

			_, found, err := store.PollSignal(ctx, runID, "never-delivered")
			if err != nil {
				t.Fatalf("PollSignal: %v", err)
			}
			if found {
				t.Fatal("PollSignal for undelivered signal: expected found=false")
			}
		})
	}
}

// TestSignalsOfTheSameNameQueueOldestFirst is the regression test for
// IMPROVEMENT-PLAN 3.215(b): two signals of the same name, delivered before
// either is consumed, must both arrive, oldest first.
//
// It ran red on all three dialects before the fix and for three different
// reasons, which is the point of putting it here rather than in a
// dialect-specific file -- the overwrite was not one bug reachable from three
// places, it was one SCHEMA decision (PRIMARY KEY (workflow_id, signal_name))
// that each dialect then honoured in its own syntax:
//
//	postgres  ON CONFLICT (workflow_id, signal_name) DO UPDATE SET payload = $3
//	mysql     ON DUPLICATE KEY UPDATE payload = VALUES(payload)
//	mssql     MERGE ... WHEN MATCHED THEN UPDATE SET payload = source.payload
//
// A fix that changed the Go and not the key would go green on none of them,
// and a fix that changed one dialect's clause would go green on one.
//
// The third delivery is not padding. With two, a store that returns the LAST
// delivery rather than the FIRST still fails the payload assertion, but a
// store that returns them in arbitrary order can pass by luck half the time.
// Three makes an ordering claim that a coin flip does not satisfy.
func TestSignalsOfTheSameNameQueueOldestFirst(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "signal-queue-test", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			want := []string{"first", "second", "third"}
			for _, p := range want {
				if err := store.DeliverSignal(ctx, runID, "approve", p); err != nil {
					t.Fatalf("DeliverSignal(%q): %v", p, err)
				}
			}

			var lastID int64
			for i, expected := range want {
				d, found, err := store.PollSignal(ctx, runID, "approve")
				if err != nil {
					t.Fatalf("PollSignal %d: %v", i, err)
				}
				if !found {
					t.Fatalf("delivery %d of %d is missing: a second signal of the same "+
						"name overwrote an earlier one", i+1, len(want))
				}
				if d.Payload != expected {
					t.Fatalf("delivery %d: got payload %q, want %q -- deliveries are "+
						"not being returned oldest-first", i+1, d.Payload, expected)
				}
				if d.ID <= lastID {
					t.Fatalf("delivery %d: id %d does not advance past %d, so ORDER BY id "+
						"cannot express FIFO", i+1, d.ID, lastID)
				}
				lastID = d.ID

				if err := store.ConsumeSignal(ctx, runID, d.ID); err != nil {
					t.Fatalf("ConsumeSignal %d: %v", i, err)
				}
			}

			if _, found, err := store.PollSignal(ctx, runID, "approve"); err != nil {
				t.Fatalf("final PollSignal: %v", err)
			} else if found {
				t.Fatal("expected the queue to be empty after all three were consumed")
			}
		})
	}
}

// TestAPromiseIsSettledByAnyHolderOfItsID covers both halves of how settling
// ended up: #818 made a settle that matched nothing report it, and #813 made a
// settle by a workflow other than the creator match in the first place.
//
// This replaces TestSettlingAPromiseThatIsNotYoursIsAnError, which asserted
// that a different workflow settling a promise is an error. That was a correct
// reading of the code at the time and the wrong requirement: a promise exists
// SO THAT something other than the waiter can complete it, and the settler is
// handed an opaque ID and nothing else. Under the old rule the only caller who
// could settle a promise was the one workflow with no reason to -- a workflow
// awaiting its own promise deadlocks.
//
// What survives from #818 is the half that was always right and is the harder
// one to keep: a settle that matches no row must say so. Now that any holder of
// the ID may settle, ErrPromiseNotFound means the promise genuinely does not
// exist, which is a more useful thing for it to mean than "exists, but not
// yours".
//
// Cross-backend because the behaviour lives in three separate UPDATE
// statements, not one shared helper: postgres, mysql and mssql each write their
// own.
func TestAPromiseIsSettledByAnyHolderOfItsID(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			owner, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "promise-owner", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun(owner): %v", err)
			}

			if err := store.CreatePromise(ctx, owner, "approval", "prom-1"); err != nil {
				t.Fatalf("CreatePromise: %v", err)
			}
			if err := store.CreatePromise(ctx, owner, "approval-2", "prom-2"); err != nil {
				t.Fatalf("CreatePromise: %v", err)
			}

			// Settling reaches the row without being told who owns it. This is
			// the case a settler is actually in: it has the ID and nothing
			// else. It used to update zero rows.
			if err := store.ResolvePromise(ctx, "prom-1", `{"ok":true}`); err != nil {
				t.Fatalf("resolving a promise by ID alone: %v", err)
			}
			status, result, _, err := store.GetPromise(ctx, owner, "prom-1")
			if err != nil {
				t.Fatalf("GetPromise: %v", err)
			}
			if status != "resolved" {
				t.Errorf("status is %q, want resolved -- the settle reported success without "+
					"changing the row, which is the shape of the defect it replaced", status)
			}
			if result == "" {
				t.Error("the resolved promise carries no result; a settle that reaches the row " +
					"but drops the value is only half the mechanism")
			}

			if err := store.RejectPromise(ctx, "prom-2", "nope"); err != nil {
				t.Fatalf("rejecting a promise by ID alone: %v", err)
			}
			if status, _, _, err := store.GetPromise(ctx, owner, "prom-2"); err != nil {
				t.Fatalf("GetPromise: %v", err)
			} else if status != "rejected" {
				t.Errorf("status is %q, want rejected", status)
			}

			// The half kept from #818: a settle that matches nothing is
			// reported. Without it the call above cannot be trusted either --
			// a store that returns nil unconditionally passes every assertion
			// up to here.
			if err := store.ResolvePromise(ctx, "no-such-promise", `{}`); err == nil {
				t.Error("resolving a promise that does not exist returned nil; the caller " +
					"believes it succeeded")
			}
			if err := store.RejectPromise(ctx, "no-such-promise", "nope"); err == nil {
				t.Error("rejecting a promise that does not exist returned nil")
			}
		})
	}
}

func TestPollSignal_NonDestructive(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "poll-nondestructive-test", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if runID == "" {
				t.Fatal("StartNewRun returned empty runID")
			}

			if err := store.DeliverSignal(ctx, runID, "nd-sig", "nd-payload"); err != nil {
				t.Fatalf("DeliverSignal: %v", err)
			}

			// First PollSignal should find the signal.
			p1, found1, err := store.PollSignal(ctx, runID, "nd-sig")
			if err != nil {
				t.Fatalf("PollSignal (first): %v", err)
			}
			if !found1 {
				t.Fatal("PollSignal (first): expected found=true")
			}
			if p1.Payload != "nd-payload" {
				t.Fatalf("PollSignal (first): expected payload %q, got %q", "nd-payload", p1.Payload)
			}

			// Second PollSignal must also find the signal — PollSignal is non-destructive.
			p2, found2, err := store.PollSignal(ctx, runID, "nd-sig")
			if err != nil {
				t.Fatalf("PollSignal (second): %v", err)
			}
			if !found2 {
				t.Fatal("PollSignal (second): expected found=true (non-destructive)")
			}
			if p2.Payload != "nd-payload" {
				t.Fatalf("PollSignal (second): expected payload %q, got %q", "nd-payload", p2.Payload)
			}
			if p2.ID != p1.ID {
				t.Fatalf("PollSignal returned a different delivery on the second read: %d then %d", p1.ID, p2.ID)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Group 7 — Child Workflows
// ---------------------------------------------------------------------------

func TestStartChildWorkflow(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			parentID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{"parent":true}`), "child-parent-test", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if parentID == "" {
				t.Fatal("StartNewRun returned empty parentID")
			}

			childID, err := store.StartChildWorkflow(ctx, parentID, "test-workflow", `{"from":"parent"}`, 1, "abandon", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow: %v", err)
			}
			if childID == "" {
				t.Fatal("StartChildWorkflow returned empty runID")
			}
			if childID == parentID {
				t.Fatal("child runID must not equal parent runID")
			}

			childWF, err := store.GetWorkflowByID(ctx, childID)
			if err != nil {
				t.Fatalf("GetWorkflowByID for child: %v", err)
			}
			if childWF == nil {
				t.Fatal("GetWorkflowByID for child returned nil")
			}
			if childWF.DefName != "test-workflow" {
				t.Fatalf("child def name: expected %q, got %q", "test-workflow", childWF.DefName)
			}
		})
	}
}

func TestGetChildResult_Completed(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			parentID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{"parent":true}`), "child-result-parent-test", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if parentID == "" {
				t.Fatal("StartNewRun returned empty parentID")
			}

			childID, err := store.StartChildWorkflow(ctx, parentID, "test-workflow", `{"from":"parent"}`, 1, "abandon", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow: %v", err)
			}
			if childID == "" {
				t.Fatal("StartChildWorkflow returned empty childID")
			}

			// Claim the child so it transitions from "ready" to "running".
			claimed, err := store.ClaimWorkflow(ctx, "test-worker")
			if err != nil {
				t.Fatalf("ClaimWorkflow: %v", err)
			}
			if claimed == nil {
				t.Fatal("ClaimWorkflow returned nil")
			}

			// Complete the claimed workflow.
			if err := store.CompleteWorkflow(ctx, claimed.ID, "test-worker", claimed.Generation, `{"child":"done"}`, nil); err != nil {
				t.Fatalf("CompleteWorkflow: %v", err)
			}

			// GetChildResult on the completed workflow ID.
			_outcome, err := store.GetChildResult(ctx, claimed.ID)
			resultJSON := _outcome.Result
			completed := _outcome.Completed
			if err != nil {
				t.Fatalf("GetChildResult: %v", err)
			}
			if !completed {
				t.Fatal("GetChildResult: expected completed=true")
			}
			if resultJSON != `{"child":"done"}` {
				t.Fatalf("GetChildResult: expected result %q, got %q", `{"child":"done"}`, resultJSON)
			}
		})
	}
}

func TestGetChildResult_NotCompleted(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			parentID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{"parent":true}`), "child-notcomplete-parent-test", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if parentID == "" {
				t.Fatal("StartNewRun returned empty parentID")
			}

			childID, err := store.StartChildWorkflow(ctx, parentID, "test-workflow", `{"from":"parent"}`, 1, "abandon", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow: %v", err)
			}
			if childID == "" {
				t.Fatal("StartChildWorkflow returned empty childID")
			}

			// Do NOT claim or complete the child — it should still be pending.
			_outcome, err := store.GetChildResult(ctx, childID)
			completed := _outcome.Completed
			if err != nil {
				t.Fatalf("GetChildResult: %v", err)
			}
			if completed {
				t.Fatal("GetChildResult: expected completed=false (child was not completed)")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Group 8 — Reaping
// ---------------------------------------------------------------------------

func TestReapStaleInstances(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "reap-test", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if runID == "" {
				t.Fatal("StartNewRun returned empty runID")
			}

			// Claim THIS run to transition it to "running" -- draining claims by
			// distinct worker id until it appears, for the reason the sibling
			// below records: setupTestData leaves its own "ready" row
			// (setup-ready-1) unclaimed and ClaimWorkflow takes no task-queue
			// filter, so a single claim is not guaranteed to return this run.
			// A single claim used to stand here; it could claim setupTestData's
			// row instead, and the outcome assertions below would then be about
			// a row this test never started.
			var claimed *WorkflowInstance
			workerNames := []string{"reap-worker-a", "reap-worker-b", "reap-worker-c"}
			claims := map[string]*WorkflowInstance{}
			for _, w := range workerNames {
				c, err := store.ClaimWorkflow(ctx, w)
				if err != nil {
					t.Fatalf("ClaimWorkflow(%s): %v", w, err)
				}
				if c == nil {
					t.Fatalf("ClaimWorkflow(%s) returned nil -- expected a ready row for each of %v", w, workerNames)
				}
				claims[c.ID] = c
				if c.ID == runID {
					claimed = c
					break
				}
			}
			if claimed == nil {
				t.Fatalf("this test's run %s was not claimed by any of %v; claimed IDs: %v",
					runID, workerNames, claimedIDsFromMap(claims))
			}

			// PRECONDITION: the run is in the state the reaper acts on, asserted
			// rather than assumed. Without it the outcome assertions below could
			// hold for a store that never moved the row.
			before, err := store.GetWorkflowByID(ctx, runID)
			if err != nil {
				t.Fatalf("GetWorkflowByID(before): %v", err)
			}
			if before.Status != "running" {
				t.Fatalf("run %s is %q before the reap, want \"running\" -- claiming it is what puts it there",
					runID, before.Status)
			}
			if before.AssignedTo != claimed.AssignedTo {
				t.Fatalf("run %s is assigned to %q before the reap, want %q (from ClaimWorkflow)",
					runID, before.AssignedTo, claimed.AssignedTo)
			}

			// Reap with a NEGATIVE window, so the row is stale by construction
			// rather than by luck.
			//
			// This passed time.Nanosecond until cleat#3032, and on MSSQL that is
			// a ZERO: Duration.Milliseconds() truncates, so -1ns became 0 and
			// the threshold became exactly SYSUTCDATETIME(). ClaimWorkflow
			// stamps heartbeat_at = SYSUTCDATETIME() from the same server clock,
			// so the row counted as stale only if that clock had ticked between
			// the two statements -- and it advances in ~4ms steps. That is the
			// ~2-in-13 failure observed on CI, and it is the identical value
			// cleat#1448 had already taken out of the sibling in this package;
			// reapEverythingNow's comment has the measurements.
			count, err := store.ReapStaleInstances(ctx, reapEverythingNow, 0)
			if err != nil {
				t.Fatalf("ReapStaleInstances(%v): %v", reapEverythingNow, err)
			}
			// count < 1 is what a reaper that reclaims NOTHING fails: this test's
			// row is stale by construction, so a working reap takes at least it.
			// The assertion that stood here was `count < 0`, which no implementation
			// can fail -- rows-affected is never negative -- so the test named for
			// reaping asserted nothing about it (cleat#3036).
			if count < 1 {
				t.Fatalf("ReapStaleInstances(%v) reclaimed %d rows; this test's run %s is stale by construction, so a working reap reclaims at least it",
					reapEverythingNow, count, runID)
			}

			// THE OUTCOME, asserted on the ROW rather than the count: count >= 1 is
			// also satisfied by a reaper that reclaimed some other row.
			after, err := store.GetWorkflowByID(ctx, runID)
			if err != nil {
				t.Fatalf("GetWorkflowByID(after the reap): %v", err)
			}
			if after.Status != "ready" {
				t.Fatalf("run %s is %q after the reap, want \"ready\" -- a reclaimed row goes back to the ready queue (its terminal status is not decided, so not \"terminating\")",
					runID, after.Status)
			}
			if after.AssignedTo != "" {
				t.Fatalf("run %s still reports assigned_to=%q after the reap; reclaiming it releases the lease",
					runID, after.AssignedTo)
			}
			if after.Generation != before.Generation+1 {
				t.Fatalf("run %s generation = %d after the reap, want %d; the reap fences the previous claim by incrementing it",
					runID, after.Generation, before.Generation+1)
			}
			if after.ReclaimCount != before.ReclaimCount+1 {
				t.Fatalf("run %s reclaim_count = %d after the reap, want %d",
					runID, after.ReclaimCount, before.ReclaimCount+1)
			}

			// THE ZERO WINDOW, asserted as its own subject rather than as a
			// second helping of the first call's.
			//
			// This call used to follow the reap directly and assert the row was
			// UNTOUCHED, on the reading that the window alone must not re-take a
			// row the first call had released. That assertion cannot fail first:
			// the reclaim leaves the row excluded by two independent predicates
			// -- status is "ready", and heartbeat_at is NULL (and NULL fails
			// `heartbeat_at < now() - interval` on every dialect) -- so removing
			// either one alone still passes, and a mutation that removes both
			// fails the status assertion above first. Measured: dropping
			// `heartbeat_at = NULL` from the UPDATE left this test green.
			//
			// What the second call tests is that a row claimed SINCE the last
			// reap is reclaimable too -- the reap is not one-shot. It must use
			// the same negative window as the first call, and NOT a zero one:
			// on MSSQL a zero window is the coin flip cleat#3032 fixed, because
			// Duration.Milliseconds() truncates and `now() - 0` is
			// SYSUTCDATETIME(), the same clock the claim stamped -- so a row
			// claimed milliseconds ago is reclaimed only if that clock ticked.
			// A first version of this call asserted exactly that and failed on
			// the SQL Server CI job at ~2-in-13, which is the identical value
			// the sibling above records for the same mistake.
			reclaimedAgain := false
			for _, w := range workerNames {
				c, err := store.ClaimWorkflow(ctx, w)
				if err != nil {
					t.Fatalf("ClaimWorkflow(%s) after the reap: %v", w, err)
				}
				if c == nil {
					break
				}
				if c.ID == runID {
					reclaimedAgain = true
					break
				}
			}
			if !reclaimedAgain {
				t.Fatalf("run %s could not be claimed again after being reclaimed; a reclaimed row must be runnable work", runID)
			}

			count, err = store.ReapStaleInstances(ctx, reapEverythingNow, 0)
			if err != nil {
				t.Fatalf("ReapStaleInstances(%v) after re-claiming: %v", reapEverythingNow, err)
			}
			if count < 1 {
				t.Fatalf("ReapStaleInstances(%v) reclaimed %d rows; this test's run was claimed again since the first reap and is stale by construction, so a second reap takes it",
					reapEverythingNow, count)
			}
			again, err := store.GetWorkflowByID(ctx, runID)
			if err != nil {
				t.Fatalf("GetWorkflowByID(after the second reap): %v", err)
			}
			if again.ReclaimCount != after.ReclaimCount+1 {
				t.Fatalf("run %s reclaim_count = %d after the second reap, want %d; the row was claimed again since the first reap, so the reap must take it a second time",
					runID, again.ReclaimCount, after.ReclaimCount+1)
			}
			if again.Status != "ready" {
				t.Fatalf("run %s is %q after the second reap, want \"ready\"", runID, again.Status)
			}
		})
	}
}

// claimedIDsFromMap is claimedIDs' sibling: a diagnostic helper for
// TestListStaleHoldersAndReapExcept's failure messages, over a map rather
// than the slice claimedIDs (cross_tenant_fixtures_test.go) already covers.
func claimedIDsFromMap(claims map[string]*WorkflowInstance) []string {
	ids := make([]string, 0, len(claims))
	for id := range claims {
		ids = append(ids, id)
	}
	return ids
}

// TestListStaleHoldersAndReapExcept is cleat#2196's veto-channel foundation:
// a holder has to be nameable before it can be asked, and excluding it has
// to actually protect its row. All three dialects must implement
// StaleHolderReaper -- t.Fatal, not t.Skip, if one doesn't, because a
// silently-missing dialect here is exactly the "a skip that hides a gap"
// shape this repo's CLAUDE.md warns about, not a genuine environmental
// precondition.
func TestListStaleHoldersAndReapExcept(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			reaper, ok := store.(StaleHolderReaper)
			if !ok {
				t.Fatalf("%s does not implement StaleHolderReaper", backend.Name())
			}
			ctx := context.Background()

			runA, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "stale-holders-a", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun(A): %v", err)
			}
			runB, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "stale-holders-b", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun(B): %v", err)
			}

			// setupTestData leaves its own "ready" row (setup-ready-1)
			// unclaimed, competing with A and B for whichever worker claims
			// next -- ClaimWorkflow takes no task-queue filter, so it is not
			// safe to assume the Nth claim returns the Nth StartNewRun. Drain
			// claims, by distinct worker id, until both A and B are found
			// rather than assuming an order; leave setupTestData's own row
			// claimed by whichever worker got it (ignored, not asserted on).
			claims := map[string]*WorkflowInstance{}
			workerNames := []string{"veto-worker-a", "veto-worker-b", "veto-worker-c"}
			for _, w := range workerNames {
				claimed, err := store.ClaimWorkflow(ctx, w)
				if err != nil {
					t.Fatalf("ClaimWorkflow(%s): %v", w, err)
				}
				if claimed == nil {
					t.Fatalf("ClaimWorkflow(%s) returned nil -- expected a ready row available for each of %v", w, workerNames)
				}
				claims[claimed.ID] = claimed
			}
			claimedA, ok := claims[runA]
			if !ok {
				t.Fatalf("run A (%s) was not claimed by any of %v; claimed IDs: %v", runA, workerNames, claimedIDsFromMap(claims))
			}
			claimedB, ok := claims[runB]
			if !ok {
				t.Fatalf("run B (%s) was not claimed by any of %v; claimed IDs: %v", runB, workerNames, claimedIDsFromMap(claims))
			}

			// Both are stale by construction: reapEverythingNow, the same
			// negative window TestReapStaleInstances uses above, which is what
			// makes this independent of whether the server's clock ticked
			// (cleat#3032). ListStaleHolders must name BOTH, with the holder and
			// generation ClaimWorkflow actually recorded -- not just a count.
			holders, err := reaper.ListStaleHolders(ctx, reapEverythingNow, 0)
			if err != nil {
				t.Fatalf("ListStaleHolders: %v", err)
			}
			byID := map[string]StaleHold{}
			for _, h := range holders {
				byID[h.Key.WorkflowID] = h
			}
			hA, ok := byID[runA]
			if !ok {
				t.Fatalf("ListStaleHolders did not report run A (%s); got %+v", runA, holders)
			}
			if hA.AssignedTo != claimedA.AssignedTo {
				t.Fatalf("run A's AssignedTo = %q, want %q (from ClaimWorkflow)", hA.AssignedTo, claimedA.AssignedTo)
			}
			if hA.Key.Generation != claimedA.Generation {
				t.Fatalf("run A's Generation = %d, want %d (from ClaimWorkflow)", hA.Key.Generation, claimedA.Generation)
			}
			hB, ok := byID[runB]
			if !ok {
				t.Fatalf("ListStaleHolders did not report run B (%s); got %+v", runB, holders)
			}
			if hB.AssignedTo != claimedB.AssignedTo {
				t.Fatalf("run B's AssignedTo = %q, want %q (from ClaimWorkflow)", hB.AssignedTo, claimedB.AssignedTo)
			}
			if hB.Key.Generation != claimedB.Generation {
				t.Fatalf("run B's Generation = %d, want %d (from ClaimWorkflow)", hB.Key.Generation, claimedB.Generation)
			}

			// Exclude A's real generation. setupTestData leaves its OWN
			// already-running row (setup-running-1, claimed by "test-worker"
			// before this test ever starts) stale too, and one of
			// workerNames above may have also drained setup-ready-1 into
			// running -- so the total reclaimed here is not pinned to a
			// specific number; what matters is which ONE row did not move,
			// checked below by status, not by count.
			excluded, err := reaper.ReapStaleInstancesExcept(ctx, reapEverythingNow, 0, []GenerationKey{
				{WorkflowID: runA, Generation: hA.Key.Generation},
			})
			if err != nil {
				t.Fatalf("ReapStaleInstancesExcept(exclude A): %v", err)
			}
			if excluded < 1 {
				t.Fatalf("ReapStaleInstancesExcept(exclude A) reclaimed %d, want at least 1 (B, and possibly other stale fixture rows)", excluded)
			}

			afterA, err := store.GetWorkflowByID(ctx, runA)
			if err != nil {
				t.Fatalf("GetWorkflowByID(A) after excluded reap: %v", err)
			}
			if afterA.Status != "running" {
				t.Fatalf("run A's status = %q after being excluded from reclaim, want %q", afterA.Status, "running")
			}
			afterB, err := store.GetWorkflowByID(ctx, runB)
			if err != nil {
				t.Fatalf("GetWorkflowByID(B) after excluded reap: %v", err)
			}
			if afterB.Status == "running" {
				t.Fatalf("run B's status is still %q; it was not in the exclude list and should have been reclaimed", afterB.Status)
			}

			// A stale-but-WRONG-generation exclude entry for A must not protect
			// it: the generation this run actually holds has not changed, so an
			// exclude naming a generation it never had is a no-op, and A is
			// reclaimed on this call exactly as an empty exclude would reclaim
			// it -- confirming empty-exclude parity with plain ReapStaleInstances
			// at the same time.
			stillExcluded, err := reaper.ReapStaleInstancesExcept(ctx, reapEverythingNow, 0, []GenerationKey{
				{WorkflowID: runA, Generation: hA.Key.Generation + 999},
			})
			if err != nil {
				t.Fatalf("ReapStaleInstancesExcept(wrong generation): %v", err)
			}
			if stillExcluded < 1 {
				t.Fatalf("ReapStaleInstancesExcept(wrong generation for A) reclaimed %d, want at least 1 (A, since the exclude entry's generation never matched)", stillExcluded)
			}
			afterA2, err := store.GetWorkflowByID(ctx, runA)
			if err != nil {
				t.Fatalf("GetWorkflowByID(A) after second reap: %v", err)
			}
			if afterA2.Status == "running" {
				t.Fatal("run A is still running after a reap whose exclude entry named a generation it never held")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Group 9 — List and Search
// ---------------------------------------------------------------------------

func TestListWorkflows_ByStatus(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			// Create two ready workflows.
			_, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{"seq":1}`), "list-by-status-1", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun 1: %v", err)
			}
			_, _, err = store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{"seq":2}`), "list-by-status-2", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun 2: %v", err)
			}

			// Claim one workflow to transition it to "running".
			claimed, err := store.ClaimWorkflow(ctx, "list-worker")
			if err != nil {
				t.Fatalf("ClaimWorkflow: %v", err)
			}
			if claimed == nil {
				t.Fatal("ClaimWorkflow returned nil")
			}

			// List ready workflows — should have at least 1.
			readyResults, err := store.ListWorkflows(ctx, WorkflowFilter{Status: "ready"})
			if err != nil {
				t.Fatalf("ListWorkflows(status=ready): %v", err)
			}
			if len(readyResults) < 1 {
				describeClaimState(t, store)
				t.Fatal("ListWorkflows(status=ready): expected at least 1 result")
			}

			// List running workflows — should have at least 1.
			runningResults, err := store.ListWorkflows(ctx, WorkflowFilter{Status: "running"})
			if err != nil {
				t.Fatalf("ListWorkflows(status=running): %v", err)
			}
			if len(runningResults) < 1 {
				t.Fatal("ListWorkflows(status=running): expected at least 1 result")
			}

			// List with a nonexistent status — should return 0 results.
			nonexistentResults, err := store.ListWorkflows(ctx, WorkflowFilter{Status: "nonexistent"})
			if err != nil {
				t.Fatalf("ListWorkflows(status=nonexistent): %v", err)
			}
			if len(nonexistentResults) != 0 {
				t.Fatalf("ListWorkflows(status=nonexistent): expected 0 results, got %d", len(nonexistentResults))
			}
		})
	}
}

func TestListWorkflows_Pagination(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			// Create 5 workflows with distinct idempotency keys.
			for i := 1; i <= 5; i++ {
				key := "paginate-key-" + itoa(i)
				_, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{"n":"`+itoa(i)+`"}`), key, DefaultTenantUUID, 0)
				if err != nil {
					t.Fatalf("StartNewRun %d: %v", i, err)
				}
			}

			// Get total count first (no limit filter → default 100).
			allResults, err := store.ListWorkflows(ctx, WorkflowFilter{})
			if err != nil {
				t.Fatalf("ListWorkflows(all): %v", err)
			}
			total := len(allResults)
			if total < 5 {
				t.Fatalf("ListWorkflows(all): expected at least 5 results, got %d", total)
			}

			// Page 1: offset=0, limit=2 → should return min(2, total).
			const pageSize = 2
			r1, err := store.ListWorkflows(ctx, WorkflowFilter{Offset: 0, Limit: pageSize})
			if err != nil {
				t.Fatalf("ListWorkflows(offset=0,limit=2): %v", err)
			}
			if len(r1) != pageSize {
				t.Fatalf("ListWorkflows(offset=0,limit=2): expected %d results, got %d", pageSize, len(r1))
			}

			// Page 2: offset=2 → should return min(2, total-2) results.
			r2, err := store.ListWorkflows(ctx, WorkflowFilter{Offset: 2, Limit: pageSize})
			if err != nil {
				t.Fatalf("ListWorkflows(offset=2,limit=2): %v", err)
			}
			exp2 := pageSize
			if total-2 < pageSize {
				exp2 = total - 2
			}
			if exp2 < 0 {
				exp2 = 0
			}
			if len(r2) != exp2 {
				t.Fatalf("ListWorkflows(offset=2,limit=2): expected %d results, got %d (total=%d)", exp2, len(r2), total)
			}

			// Verify no overlap between pages.
			page1IDs := make(map[string]bool)
			for _, wf := range r1 {
				page1IDs[wf.ID] = true
			}
			for _, wf := range r2 {
				if page1IDs[wf.ID] {
					t.Fatalf("ListWorkflows pagination: page 2 returned workflow %s already in page 1", wf.ID)
				}
			}

			// Page beyond end: offset=total, limit=5 → 0 results.
			r4, err := store.ListWorkflows(ctx, WorkflowFilter{Offset: total, Limit: 5})
			if err != nil {
				t.Fatalf("ListWorkflows(offset=%d,limit=5): %v", total, err)
			}
			if len(r4) != 0 {
				t.Fatalf("ListWorkflows(offset=%d,limit=5): expected 0 results, got %d", total, len(r4))
			}
		})
	}
}

func TestListWorkflows_Search(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			_, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "list-search-test", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			results, err := store.ListWorkflows(ctx, WorkflowFilter{Search: "test-workflow"})
			if err != nil {
				t.Fatalf("ListWorkflows(search=test-workflow): %v", err)
			}
			if len(results) < 1 {
				t.Fatal("ListWorkflows(search=test-workflow): expected at least 1 result")
			}
		})
	}
}

func TestListWorkflows_InputContains(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			// Create workflows with distinctive JSON input.
			_, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{"needle":"find-me-12345"}`), "input-filter-1", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun 1: %v", err)
			}
			_, _, err = store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{"other":"value"}`), "input-filter-2", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun 2: %v", err)
			}

			// Filter by a substring unique to the first workflow.
			results, err := store.ListWorkflows(ctx, WorkflowFilter{
				InputContains: "find-me-12345",
			})
			if err != nil {
				t.Fatalf("ListWorkflows(InputContains): %v", err)
			}
			if len(results) != 1 {
				t.Fatalf("ListWorkflows(InputContains): expected 1 result, got %d", len(results))
			}

			// Filter by a substring that matches nothing.
			empty, err := store.ListWorkflows(ctx, WorkflowFilter{
				InputContains: "no-such-string-99999",
			})
			if err != nil {
				t.Fatalf("ListWorkflows(InputContains none): %v", err)
			}
			if len(empty) != 0 {
				t.Fatalf("ListWorkflows(InputContains none): expected 0, got %d", len(empty))
			}
		})
	}
}

func TestGetWorkflowByID(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{"key":"val"}`), "get-by-id-test", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if runID == "" {
				t.Fatal("StartNewRun returned empty runID")
			}

			// Look up the existing workflow.
			wf, err := store.GetWorkflowByID(ctx, runID)
			if err != nil {
				t.Fatalf("GetWorkflowByID: %v", err)
			}
			if wf == nil {
				t.Fatal("GetWorkflowByID returned nil for an existing workflow")
			}
			if wf.ID != runID {
				t.Fatalf("GetWorkflowByID: expected ID %q, got %q", runID, wf.ID)
			}
			if wf.DefName != "test-workflow" {
				t.Fatalf("GetWorkflowByID: expected DefName %q, got %q", "test-workflow", wf.DefName)
			}

			// Look up a nonexistent ID.
			wf, err = store.GetWorkflowByID(ctx, "nonexistent-id-12345")
			if err == nil && wf != nil {
				t.Fatal("GetWorkflowByID for nonexistent ID: expected nil or error")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Group 10 — Schedules
// ---------------------------------------------------------------------------

func TestCreateSchedule(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			sch := Schedule{
				Name:           "test-create-schedule",
				DefName:        "test-workflow",
				EntryPoint:     "main",
				CronExpression: "* * * * *",
				Input:          json.RawMessage(`{}`),
				NextRunAt:      time.Now().Add(time.Hour),
			}

			if err := store.CreateSchedule(ctx, sch); err != nil {
				t.Fatalf("CreateSchedule: %v", err)
			}
		})
	}
}

func TestGetDueSchedules(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			// setupTestData should have created "test-schedule" with NextRunAt
			// in the past, so GetDueSchedules includes it.
			schedules, err := store.GetDueSchedules(ctx)
			if err != nil {
				t.Fatalf("GetDueSchedules: %v", err)
			}
			if len(schedules) < 1 {
				t.Fatal("GetDueSchedules: expected at least 1 schedule")
			}

			// Verify all returned schedules have NextRunAt <= now.
			now := time.Now()
			foundTestSchedule := false
			for _, s := range schedules {
				if s.NextRunAt.After(now) {
					t.Fatalf("GetDueSchedules: schedule %q has NextRunAt %v in the future", s.Name, s.NextRunAt)
				}
				if s.Name == "test-schedule" {
					foundTestSchedule = true
				}
			}
			if !foundTestSchedule {
				t.Fatal("GetDueSchedules: expected 'test-schedule' to be included")
			}
		})
	}
}

func TestSetScheduleEnabled(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			// Create a schedule with NextRunAt in the past so it would be due
			// if enabled.
			sch := Schedule{
				Name:           "test-disable-schedule",
				DefName:        "test-workflow",
				EntryPoint:     "main",
				CronExpression: "0 * * * *",
				Input:          json.RawMessage(`{}`),
				NextRunAt:      time.Now().Add(-time.Hour),
			}
			if err := store.CreateSchedule(ctx, sch); err != nil {
				t.Fatalf("CreateSchedule: %v", err)
			}

			// Disable the schedule.
			if err := store.SetScheduleEnabled(ctx, "test-disable-schedule", false); err != nil {
				t.Fatalf("SetScheduleEnabled: %v", err)
			}

			// GetDueSchedules should NOT include the disabled schedule.
			schedules, err := store.GetDueSchedules(ctx)
			if err != nil {
				t.Fatalf("GetDueSchedules: %v", err)
			}
			for _, s := range schedules {
				if s.Name == "test-disable-schedule" {
					t.Fatal("GetDueSchedules: disabled schedule should not appear")
				}
			}
		})
	}
}

func TestDeleteSchedule(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			sch := Schedule{
				Name:           "test-delete-schedule",
				DefName:        "test-workflow",
				EntryPoint:     "main",
				CronExpression: "0 * * * *",
				Input:          json.RawMessage(`{}`),
				NextRunAt:      time.Now().Add(time.Hour),
			}
			if err := store.CreateSchedule(ctx, sch); err != nil {
				t.Fatalf("CreateSchedule: %v", err)
			}

			if err := store.DeleteSchedule(ctx, "test-delete-schedule"); err != nil {
				t.Fatalf("DeleteSchedule: %v", err)
			}
		})
	}
}

// itoa is a minimal int-to-string helper used to build unique idempotency keys
// without importing strconv or fmt (standard library only via "testing").
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// updateWorkflowNextWakeAt sets the next_wake_at column via raw SQL.
// Uses a type switch to handle dialect-specific parameter placeholders.
func updateWorkflowNextWakeAt(t *testing.T, store WorkflowStore, workflowID string, tval time.Time) {
	t.Helper()
	switch s := store.(type) {
	case *PostgresStore:
		_, err := s.db.Exec(`UPDATE workflow_instances SET next_wake_at = $1 WHERE id = $2`, tval, workflowID)
		if err != nil {
			t.Fatalf("updateWorkflowNextWakeAt (postgres): %v", err)
		}
	case *MySQLStore:
		_, err := s.db.Exec(`UPDATE workflow_instances SET next_wake_at = ? WHERE id = ?`, tval, workflowID)
		if err != nil {
			t.Fatalf("updateWorkflowNextWakeAt (mysql): %v", err)
		}
	case *MSSQLStore:
		_, err := s.db.Exec(`UPDATE workflow_instances SET next_wake_at = @p1 WHERE id = @p2`, tval, workflowID)
		if err != nil {
			t.Fatalf("updateWorkflowNextWakeAt (mssql): %v", err)
		}
	default:
		t.Fatalf("updateWorkflowNextWakeAt: unknown store type %T", store)
	}
}

// queryDatabaseNow returns the database server's own clock, using the same
// expression the corresponding DeliverSignal uses to set next_wake_at:
// now() on PostgreSQL, NOW(6) on MySQL, SYSUTCDATETIME() on SQL Server.
//
// It exists so that a test comparing against next_wake_at compares two readings
// of ONE clock. The Go process and the database server do not share a clock --
// under colima the database runs in a VM with its own time -- so any assertion
// that puts time.Now() on one side and a database-generated timestamp on the
// other is measuring clock skew as much as behaviour.
func queryDatabaseNow(t *testing.T, store WorkflowStore) time.Time {
	t.Helper()
	var now time.Time
	switch s := store.(type) {
	case *PostgresStore:
		if err := s.db.QueryRow(`SELECT now()`).Scan(&now); err != nil {
			t.Fatalf("queryDatabaseNow (postgres): %v", err)
		}
	case *MySQLStore:
		if err := s.db.QueryRow(`SELECT NOW(6)`).Scan(&now); err != nil {
			t.Fatalf("queryDatabaseNow (mysql): %v", err)
		}
	case *MSSQLStore:
		if err := s.db.QueryRow(`SELECT SYSUTCDATETIME()`).Scan(&now); err != nil {
			t.Fatalf("queryDatabaseNow (mssql): %v", err)
		}
	default:
		t.Fatalf("queryDatabaseNow: unknown store type %T", store)
	}
	return now
}

// queryWorkflowNextWakeAt returns next_wake_at from the database.
func queryWorkflowNextWakeAt(t *testing.T, store WorkflowStore, workflowID string) time.Time {
	t.Helper()
	var nw time.Time
	switch s := store.(type) {
	case *PostgresStore:
		err := s.db.QueryRow(`SELECT next_wake_at FROM workflow_instances WHERE id = $1`, workflowID).Scan(&nw)
		if err != nil {
			t.Fatalf("queryWorkflowNextWakeAt (postgres): %v", err)
		}
	case *MySQLStore:
		err := s.db.QueryRow(`SELECT next_wake_at FROM workflow_instances WHERE id = ?`, workflowID).Scan(&nw)
		if err != nil {
			t.Fatalf("queryWorkflowNextWakeAt (mysql): %v", err)
		}
	case *MSSQLStore:
		err := s.db.QueryRow(`SELECT next_wake_at FROM workflow_instances WHERE id = @p1`, workflowID).Scan(&nw)
		if err != nil {
			t.Fatalf("queryWorkflowNextWakeAt (mssql): %v", err)
		}
	default:
		t.Fatalf("queryWorkflowNextWakeAt: unknown store type %T", store)
	}
	return nw
}

// setWorkflowStatus sets the status column via raw SQL.
func setWorkflowStatus(t *testing.T, store WorkflowStore, workflowID, status string) {
	t.Helper()
	switch s := store.(type) {
	case *PostgresStore:
		_, err := s.db.Exec(`UPDATE workflow_instances SET status = $1 WHERE id = $2`, status, workflowID)
		if err != nil {
			t.Fatalf("setWorkflowStatus (postgres): %v", err)
		}
	case *MySQLStore:
		_, err := s.db.Exec(`UPDATE workflow_instances SET status = ? WHERE id = ?`, status, workflowID)
		if err != nil {
			t.Fatalf("setWorkflowStatus (mysql): %v", err)
		}
	case *MSSQLStore:
		_, err := s.db.Exec(`UPDATE workflow_instances SET status = @p1 WHERE id = @p2`, status, workflowID)
		if err != nil {
			t.Fatalf("setWorkflowStatus (mssql): %v", err)
		}
	default:
		t.Fatalf("setWorkflowStatus: unknown store type %T", store)
	}
}
