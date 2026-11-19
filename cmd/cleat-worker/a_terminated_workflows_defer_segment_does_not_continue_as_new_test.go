package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#4143, the ContinueAsNew reading, on a real worker. A terminated workflow's defer segment replays the history and the body's next
// call arrives at the replay frontier. When that call is ContinueAsNew, three things could go wrong, and each is a separate observable:
//
//	a new run    the continuation starts, so a terminated workflow keeps running as a fresh one
//	an event     a continue_as_new event lands in the history of a workflow that is being terminated
//	the status   the workflow does not end terminated
//
// The run: it registers a defer and then continues as new, with nothing that waits in between (DeferThenContinueAsNew). It was killed after
// the registration, so its history is [defer].
//
//	resumed     an ordinary claim replays [defer] and continues as new: a run follows it. This is the CONTROL, so the probe can see a
//	            continuation at all.
//	terminated  the terminate's segment must run the defer and neither continue nor record that it tried.
//
// A defer segment is also finished before the worker's continue_as_new branch, which is why a SUSPENSION with that reason cannot start a run
// from a segment (finishDeferPhase runs first). That is a reading of the code; this test is the measurement of it, and of the part the code
// does not say: whether the event is recorded.
func TestATerminatedWorkflowsDeferSegmentDoesNotContinueAsNew(t *testing.T) {
	ctx := context.Background()

	db := testutil.SuiteTestDB(t, "cleat_worker")
	store := engine.NewPostgresStore(db)

	calls := &frontierCalls{}
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.add(strings.TrimPrefix(r.URL.Path, "/call/"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer svc.Close()
	oldSvcURL := *benchSvcURL
	*benchSvcURL = svc.URL
	defer func() { *benchSvcURL = oldSvcURL }()

	wasmBytes := buildDeferFixture(t)
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	const defName = "defer-segment-continue-as-new-worker"
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
		ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	prefix := killedAfterTheDeferThatContinues(t, ctx, wasmBytes, "defer_then_continue_as_new")

	// continuations counts the runs that name wfID as the run they continued from.
	continuations := func(t *testing.T, wfID string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM workflow_leases WHERE continued_from = $1`, wfID).Scan(&n); err != nil {
			t.Fatalf("counting continuations of %s: %v", wfID, err)
		}
		return n
	}

	type outcome struct {
		ops           []string
		runs          int
		status        string
		continueEvent bool
		logs          string
	}
	scenario := func(t *testing.T, how string) outcome {
		t.Helper()
		calls.reset()
		logs := &syncBuffer{}
		w := newRealDeferPhaseWorker(t, db, store, logs)
		w.egress = &engine.EgressGuard{AllowLoopback: true, TenantOptional: func(context.Context) bool { return true }}
		wfID := fmt.Sprintf("defer-segment-can-%s-%d", how, time.Now().UnixNano())
		input := json.RawMessage(`{"__entry_point":"defer_then_continue_as_new"}`)
		if _, _, err := store.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion, input, "", engine.DefaultTenantUUID, 0); err != nil {
			t.Fatalf("StartNewRun: %v", err)
		}
		if err := store.AppendEventHistoryBatch(ctx, wfID, prefix); err != nil {
			t.Fatalf("AppendEventHistoryBatch: %v", err)
		}
		want := ""
		switch how {
		case "terminate":
			want = "terminated"
			if err := store.TerminateWorkflow(ctx, wfID, "terminated by test"); err != nil {
				t.Fatalf("TerminateWorkflow: %v", err)
			}
		case "cancel":
			want = "cancelled"
			if err := store.CancelWorkflow(ctx, wfID, "cancelled by test"); err != nil {
				t.Fatalf("CancelWorkflow: %v", err)
			}
		}
		claimed := claimOne(t, ctx, store, w.id, wfID)
		if claimed.PendingTerminalStatus != want {
			t.Fatalf("the claim carried PendingTerminalStatus %q, want %q (%s)", claimed.PendingTerminalStatus, want, how)
		}
		runExecuteWorkflow(w, claimed)

		history, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("LoadEventHistory: %v", err)
		}
		o := outcome{ops: calls.all(), runs: continuations(t, wfID), status: statusOf(t, ctx, store, wfID), logs: logs.String()}
		// The control's continuation is a live run in a database that outlives the test, and the suite refuses to finish with runs left
		// ready. End the ones this scenario started.
		t.Cleanup(func() {
			rows, err := db.QueryContext(ctx, `SELECT id FROM workflow_leases WHERE continued_from = $1`, wfID)
			if err != nil {
				t.Errorf("listing the continuations of %s to end them: %v", wfID, err)
				return
			}
			var ids []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
			for _, id := range ids {
				if err := store.TerminateWorkflow(ctx, id, "ended by the test that started it"); err != nil {
					t.Errorf("ending continuation %s: %v", id, err)
				}
			}
		})
		for _, e := range history {
			if e.EventType == engine.EventTypeContinueAsNew {
				o.continueEvent = true
			}
		}
		return o
	}

	t.Run("control: an ordinary resume continues as new", func(t *testing.T) {
		o := scenario(t, "resume")
		if o.runs != 1 || !o.continueEvent {
			t.Fatalf("the control produced %d continuation run(s) and continue_as_new event=%v (status %q); want 1 and true, so this probe cannot see a continuation (logs:\n%s)", o.runs, o.continueEvent, o.status, o.logs)
		}
	})

	for _, how := range []string{"terminate", "cancel"} {
		how := how
		want := map[string]string{"terminate": "terminated", "cancel": "cancelled"}[how]
		t.Run("a "+how+"'s defer segment runs the cleanup and does not continue as new", func(t *testing.T) {
			o := scenario(t, how)
			if countOp(o.ops, "cleanup/after_defer_then_continue") != 1 {
				t.Errorf("the registered cleanup ran %d times, want exactly once: %v (logs:\n%s)", countOp(o.ops, "cleanup/after_defer_then_continue"), o.ops, o.logs)
			}
			if o.runs != 0 {
				t.Errorf("a %s workflow's defer segment STARTED %d continuation run(s)", how, o.runs)
			}
			if o.continueEvent {
				t.Errorf("a %s workflow's history gained a continue_as_new event: the segment recorded the body's attempt to continue", how)
			}
			if o.status != want {
				t.Errorf("the workflow ended %q, want %q", o.status, want)
			}
		})
	}
}

// killedAfterTheDeferThatContinues is killedAfterTheDefer for an entry whose in-process run ends in a continue_as_new suspension rather than
// completing. The history it returns is cut right after the defer registration.
func killedAfterTheDeferThatContinues(t *testing.T, ctx context.Context, wasmBytes []byte, entry string) []engine.EventRecord {
	t.Helper()
	rt, err := engine.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := engine.NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })
	eng := engine.NewEngine(rt, stubCaller{}, engine.WithBackends(engine.WasmtimeLanguages, wt))
	_, history, susp, _, _, err := eng.Execute(ctx, wasmBytes, entry, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("in-process run of %s: %v", entry, err)
	}
	if susp == nil || susp.Reason != "continue_as_new" {
		t.Fatalf("PRECONDITION FAILED: %s did not end in a continue_as_new suspension (suspension %+v)", entry, susp)
	}
	for i, ev := range history {
		if ev.EventType == engine.EventTypeDefer {
			if i+1 >= len(history) {
				t.Fatalf("PRECONDITION FAILED: the defer registration is the run's last event, so a run killed there has nothing left to do")
			}
			return history[:i+1]
		}
	}
	t.Fatalf("PRECONDITION FAILED: no defer registration in the history of %s: %v", entry, history)
	return nil
}
