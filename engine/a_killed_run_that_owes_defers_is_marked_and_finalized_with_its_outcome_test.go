//go:build cgo

package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// cleat#4079: MarkFailureForDeferPhase, and the idempotency-key error that FinalizeDeferPhase now writes.

func deferPhaseTestDB(t *testing.T, store WorkflowStore) (*sql.DB, func(i int) string) {
	t.Helper()
	switch st := store.(type) {
	case *PostgresStore:
		return st.db, func(i int) string { return fmt.Sprintf("$%d", i) }
	case *MySQLStore:
		return st.db, func(int) string { return "?" }
	case *MSSQLStore:
		return st.db, func(i int) string { return fmt.Sprintf("@p%d", i) }
	}
	t.Fatalf("no *sql.DB for %T", store)
	return nil, nil
}

// idempotencyKeyError reads the error recorded on a run's idempotency key row, and whether there is one.
func idempotencyKeyError(t *testing.T, store WorkflowStore, wfID string) (string, bool) {
	t.Helper()
	db, ph := deferPhaseTestDB(t, store)
	var msg sql.NullString
	err := db.QueryRow("SELECT error_msg FROM idempotency_keys WHERE workflow_id = "+ph(1), wfID).Scan(&msg)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read idempotency key: %v", err)
	}
	return msg.String, msg.Valid
}

// startKeyedRun starts a run under an idempotency key, with a defer registration already in its history.
func startKeyedRun(t *testing.T, ctx context.Context, store WorkflowStore, name string) string {
	t.Helper()
	const defName = "defer-phase-failure"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	id := fmt.Sprintf("%s-%d", name, time.Now().UnixNano())
	if _, _, err := store.StartNewRun(ctx, id, defName, 1, json.RawMessage(`{}`), "key-"+id, DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	if err := store.AppendEventHistoryBatch(ctx, id, []EventRecord{
		{Step: 0, EventType: EventTypeDefer, DeferID: "defer-0", DeferDescription: "cleanup"},
		{Step: 1, EventType: EventTypeCall, Service: "work", Op: "charge", Response: `{}`},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	return id
}

func TestMarkFailureForDeferPhase(t *testing.T) {
	ctx := context.Background()
	failure := func(status string) DeferPhaseFailure {
		return DeferPhaseFailure{Status: status, ErrorMsg: "execution time limit exceeded", ErrorCode: "timeout", ErrorOp: "run"}
	}
	held := []EventRecord{{Step: 2, EventType: EventTypeCall, Service: "work", Op: "ship", Response: `{}`}}

	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		dps, ok := store.(DeferPhaseStore)
		if !ok {
			t.Fatalf("%T is not a DeferPhaseStore", store)
		}
		marker, ok := store.(DeferPhaseFailureMarker)
		if !ok {
			t.Fatalf("%T is not a DeferPhaseFailureMarker", store)
		}

		for _, outcome := range []string{statusFailed, statusDeadLettered} {
			outcome := outcome
			t.Run("marks "+outcome+" with its events and error, then finalize applies it", func(t *testing.T) {
				id := startKeyedRun(t, ctx, store, "mark-"+outcome)
				claimed := claimByID(t, ctx, store, "worker-m1", id)
				if err := marker.MarkFailureForDeferPhase(ctx, id, "worker-m1", claimed.Generation, failure(outcome), held); err != nil {
					t.Fatalf("MarkFailureForDeferPhase: %v", err)
				}
				wf := mustGetWorkflow(t, ctx, store, id)
				if wf.Status != statusTerminating {
					t.Fatalf("status %q after the mark, want %q", wf.Status, statusTerminating)
				}
				if wf.Error != "execution time limit exceeded" || wf.ErrorCode != "timeout" || wf.ErrorOp != "run" {
					t.Fatalf("error payload after the mark = (%q, %q, %q): the run must already report its failure", wf.Error, wf.ErrorCode, wf.ErrorOp)
				}
				if wf.CompletedAt != nil {
					t.Fatalf("completed_at is set after the mark (%v): the run is not finished until its defers have run", wf.CompletedAt)
				}
				hist, err := store.LoadEventHistory(ctx, id)
				if err != nil {
					t.Fatalf("load (verifies the checksum chain): %v", err)
				}
				if len(hist) != 3 {
					t.Fatalf("%d events after the mark, want 3: the held-back event must land with it, chained from the persisted ones", len(hist))
				}
				if _, has := idempotencyKeyError(t, store, id); has {
					t.Fatal("the idempotency key already carries an error before the run is finished")
				}

				// Claimable again, carrying the pending outcome.
				again := claimByID(t, ctx, store, "worker-m2", id)
				if again.PendingTerminalStatus != outcome {
					t.Fatalf("the claim carries pending outcome %q, want %q", again.PendingTerminalStatus, outcome)
				}
				if err := dps.FinalizeDeferPhase(ctx, id, "worker-m2", again.Generation, nil); err != nil {
					t.Fatalf("FinalizeDeferPhase: %v", err)
				}
				done := mustGetWorkflow(t, ctx, store, id)
				if done.Status != outcome {
					t.Fatalf("status %q after finalize, want %q", done.Status, outcome)
				}
				if done.Error != "execution time limit exceeded" || done.ErrorCode != "timeout" {
					t.Fatalf("finalize changed the recorded failure to (%q, %q)", done.Error, done.ErrorCode)
				}
				if done.CompletedAt == nil {
					t.Fatal("completed_at is unset after finalize")
				}
				if msg, has := idempotencyKeyError(t, store, id); !has || msg != "execution time limit exceeded" {
					t.Fatalf("idempotency key error = (%q, present=%v), want the run's error: a keyed run must report its failure through the key", msg, has)
				}
			})
		}

		t.Run("a worker that lost its claim writes nothing", func(t *testing.T) {
			id := startKeyedRun(t, ctx, store, "mark-fence")
			claimed := claimByID(t, ctx, store, "worker-f1", id)
			err := marker.MarkFailureForDeferPhase(ctx, id, "worker-f1", claimed.Generation+7, failure(statusFailed), held)
			if !errors.Is(err, ErrFenceLost) {
				t.Fatalf("want ErrFenceLost for a stale generation, got %v", err)
			}
			err = marker.MarkFailureForDeferPhase(ctx, id, "someone-else", claimed.Generation, failure(statusFailed), held)
			if !errors.Is(err, ErrFenceLost) {
				t.Fatalf("want ErrFenceLost for another worker, got %v", err)
			}
			if wf := mustGetWorkflow(t, ctx, store, id); wf.Status != "running" || wf.Error != "" {
				t.Fatalf("a fenced-out mark changed the run: status %q error %q", wf.Status, wf.Error)
			}
			if hist, _ := store.LoadEventHistory(ctx, id); len(hist) != 2 {
				t.Fatalf("%d events after a fenced-out mark, want the original 2: it must not append after the new owner", len(hist))
			}
		})

		t.Run("an outcome it cannot record is refused and changes nothing", func(t *testing.T) {
			id := startKeyedRun(t, ctx, store, "mark-invalid")
			claimed := claimByID(t, ctx, store, "worker-i1", id)
			if err := marker.MarkFailureForDeferPhase(ctx, id, "worker-i1", claimed.Generation, failure("done"), held); err == nil {
				t.Fatal("recording 'done' as a failure was accepted")
			}
			if wf := mustGetWorkflow(t, ctx, store, id); wf.Status != "running" {
				t.Fatalf("a refused mark left status %q", wf.Status)
			}
		})
	})
}

// The gap the finalize-side update closes was not new with cleat#4079: a keyed run ended by terminate reported nothing through its key.
func TestFinalizeDeferPhaseReportsATerminatedRunsReasonThroughItsIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		id := startKeyedRun(t, ctx, store, "idem-terminate")
		if err := store.TerminateWorkflow(ctx, id, "terminated by operator"); err != nil {
			t.Fatalf("terminate: %v", err)
		}
		if wf := mustGetWorkflow(t, ctx, store, id); wf.Status != statusTerminating {
			t.Fatalf("PRECONDITION FAILED: status %q, want %q (a defer is registered, so terminate must take the two-phase path)", wf.Status, statusTerminating)
		}
		if _, has := idempotencyKeyError(t, store, id); has {
			t.Fatal("PRECONDITION FAILED: the key already carries an error before finalize")
		}
		claimed := claimByID(t, ctx, store, "worker-t1", id)
		if err := store.(DeferPhaseStore).FinalizeDeferPhase(ctx, id, "worker-t1", claimed.Generation, nil); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if msg, has := idempotencyKeyError(t, store, id); !has || msg != "terminated by operator" {
			t.Fatalf("idempotency key error = (%q, present=%v), want the terminate reason", msg, has)
		}
	})
}
