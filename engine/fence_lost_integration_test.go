package engine

// Real-database reproduction of the IMPROVEMENT-PLAN.md 1.1/1.2
// "zombie writer" bug: a worker that stalls long enough to be reaped and
// have its workflow reclaimed by another worker must not be able to
// corrupt the new owner's event history or the parent's await_child event
// when it finally wakes up and calls back into the store with its now-stale
// (worker_id, generation).
//
// This exercises the real finalize_workflow_status stored procedure/
// function (installed via applyPostgresProcedures / applyMySQLProcedures /
// applyMSSQLProcedures in store_backends_procedures_test.go, reading the
// actual migrations/<dialect>/003_procedures.sql + 004_*.sql files from
// disk) rather than a hand-rolled substitute, so it needs a real database
// and is skipped when none is configured -- see registeredBackends /
// StoreBackend.Setup in store_backends_test.go:
//   - Postgres: CLEAT_TEST_POSTGRES or CLEAT_TEST_DB (falls back to
//     postgres://localhost:5432/cleat?sslmode=disable); also skipped
//     outright in `go test -short`.
//   - MySQL: CLEAT_TEST_MYSQL
//   - MSSQL: CLEAT_TEST_MSSQL

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFinalizeWorkflowSegment_ZombieWriterFence(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			// ---- Set up a parent workflow suspended awaiting a child ----

			parentID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "zombie-parent", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun (parent): %v", err)
			}
			parentWF, err := store.ClaimWorkflow(ctx, "worker-parent")
			if err != nil || parentWF == nil {
				t.Fatalf("ClaimWorkflow (parent): wf=%v err=%v", parentWF, err)
			}

			childID, err := store.StartChildWorkflow(ctx, parentID, "test-workflow", `{}`, 1, "abandon", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow: %v", err)
			}

			// Suspend the parent with an await_child event referencing the
			// child's run ID and an empty response -- this is exactly what
			// finalize_workflow_status looks for when injecting a child's
			// result into its parent.
			awaitEvent := EventRecord{Step: 0, EventType: "await_child", ChildName: "test-workflow", RunID: childID}
			farFuture := time.Now().Add(1 * time.Hour)
			if err := store.FinalizeWorkflowSegment(ctx, parentID, "worker-parent", parentWF.Generation,
				[]EventRecord{awaitEvent}, "ready", "", "", "", nil, farFuture); err != nil {
				t.Fatalf("FinalizeWorkflowSegment (parent suspend): %v", err)
			}

			// ---- Worker A claims the child and then stalls ----

			wfA, err := store.ClaimWorkflow(ctx, "worker-A")
			if err != nil || wfA == nil || wfA.ID != childID {
				t.Fatalf("ClaimWorkflow (A): wf=%v err=%v", wfA, err)
			}
			staleGeneration := wfA.Generation // captured before A stalls; never refreshed

			// A never calls back. The reaper reclaims the child (status ->
			// ready, assigned_to -> NULL, generation bumped per CLEAT-1.4)
			// -- a 1ms timeout is enough since A's heartbeat was just set.
			time.Sleep(2 * time.Millisecond)
			reaped, err := store.ReapStaleInstances(ctx, 1*time.Millisecond)
			if err != nil {
				t.Fatalf("ReapStaleInstances: %v", err)
			}
			if reaped < 1 {
				t.Fatalf("ReapStaleInstances reclaimed %d instances, want >= 1 (the stalled child)", reaped)
			}

			// ---- Worker B claims the reclaimed child and makes real progress ----

			wfB, err := store.ClaimWorkflow(ctx, "worker-B")
			if err != nil || wfB == nil || wfB.ID != childID {
				t.Fatalf("ClaimWorkflow (B): wf=%v err=%v", wfB, err)
			}
			if wfB.Generation == staleGeneration {
				t.Fatalf("worker B's generation (%d) equals A's stale generation (%d) -- test setup did not reproduce the scenario", wfB.Generation, staleGeneration)
			}

			liveEvent := EventRecord{Step: 0, EventType: EventTypeCall, Service: "real", Op: "b-in-progress", Response: "live-response-from-B"}
			bWake := time.Now().Add(1 * time.Hour)
			if err := store.FinalizeWorkflowSegment(ctx, childID, "worker-B", wfB.Generation,
				[]EventRecord{liveEvent}, "ready", "", "", "", nil, bWake); err != nil {
				t.Fatalf("FinalizeWorkflowSegment (B live progress): %v", err)
			}

			before, err := store.LoadEventHistory(ctx, childID)
			if err != nil {
				t.Fatalf("LoadEventHistory (before A's stale call): %v", err)
			}
			if len(before) != 1 || before[0].Response != "live-response-from-B" {
				t.Fatalf("unexpected event history before A's stale call: %+v", before)
			}

			// ---- A wakes up and tries to finalize the child as done, using its stale fence ----

			err = store.FinalizeWorkflowSegment(ctx, childID, "worker-A", staleGeneration,
				nil, "done", `{"stale":"result-from-A"}`, "", "", nil, time.Time{})
			if !errors.Is(err, ErrFenceLost) {
				t.Fatalf("A's stale FinalizeWorkflowSegment: err = %v, want ErrFenceLost", err)
			}

			// ---- The whole point: B's event history must survive untouched ----

			after, err := store.LoadEventHistory(ctx, childID)
			if err != nil {
				t.Fatalf("LoadEventHistory (after A's stale call): %v", err)
			}
			if len(after) != 1 || after[0].Response != "live-response-from-B" {
				t.Fatalf("event history was corrupted by the stale writer: got %+v, want B's single live event untouched", after)
			}

			// The child's ownership/status must still reflect B, not have
			// been reset (or worse, finalized) by A.
			childAfter, err := store.GetWorkflowByID(ctx, childID)
			if err != nil {
				t.Fatalf("GetWorkflowByID (child): %v", err)
			}
			if childAfter.Status != "ready" {
				t.Errorf("child status = %q, want %q (untouched by A's stale call)", childAfter.Status, "ready")
			}
			if childAfter.Generation != wfB.Generation {
				t.Errorf("child generation = %d, want %d (B's, untouched by A)", childAfter.Generation, wfB.Generation)
			}

			// The parent's await_child event must not have been injected
			// with A's stale result.
			parentEvents, err := store.LoadEventHistory(ctx, parentID)
			if err != nil {
				t.Fatalf("LoadEventHistory (parent): %v", err)
			}
			for _, ev := range parentEvents {
				if ev.EventType == "await_child" && ev.RunID == childID && strings.Contains(ev.Response, "stale") {
					t.Fatalf("parent's await_child event was corrupted with A's stale result: %+v", ev)
				}
			}
		})
	}
}
