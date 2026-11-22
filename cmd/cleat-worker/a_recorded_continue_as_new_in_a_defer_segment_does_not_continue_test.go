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

// cleat#4224. executeWorkflow ends a defer segment with finishDeferPhase BEFORE it reads a continue_as_new suspension (setup.go: `if deferPhase { ...; return }` precedes
// `if suspended != nil && suspended.Reason == "continue_as_new"`). The order is what keeps a terminated or cancelled workflow from continuing as a new run, and until this
// test nothing pinned it: TestATerminatedWorkflowsDeferSegmentDoesNotContinueAsNew drives a segment whose body reaches ContinueAsNew at the replay frontier, where the
// ENGINE refuses it, so the worker never sees a continue_as_new suspension from a segment and swapping the two branches changes nothing it can observe.
//
// This one hands the segment a suspension the engine does not refuse. continueAsNewStep's REPLAY half ends the run with a continue_as_new suspension when the history
// already holds the event (the refusal guards the fresh half only), so a segment that replays [defer, continue_as_new] comes back from the engine carrying exactly that
// suspension.
//
// THAT HISTORY IS BUILT BY HAND, and the test says so rather than pretend it is one a workflow can have. Measured while writing this: the engine's in-process run of the same
// entry ends in a continue_as_new suspension and puts NO continue_as_new event in the history; the worker appends the event in the same transaction that creates the next run
// and completes this one (ContinueAsNew), so a live, claimable workflow never holds it. The test pins the ORDER of the worker's two branches, which is what the issue asks
// for; it does not claim that a production history reaches them.
//
//	resume      an ordinary claim replays [defer, continue_as_new] and continues as new: a run follows it. The CONTROL, so the probe can see a continuation at all.
//	terminate   the segment must run the defer, end terminated, and start no run.
//	cancel      the same, ending cancelled.
func TestARecordedContinueAsNewInADeferSegmentDoesNotStartARun(t *testing.T) {
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
	const defName = "defer-segment-recorded-continue-as-new-worker"
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
		ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	prefix := aHistoryHandBuiltToEndInARecordedContinueAsNew(t, ctx, wasmBytes, "defer_then_continue_as_new")

	continuations := func(t *testing.T, wfID string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM workflow_leases WHERE continued_from = $1`, wfID).Scan(&n); err != nil {
			t.Fatalf("counting continuations of %s: %v", wfID, err)
		}
		return n
	}
	continueEvents := func(h []engine.EventRecord) int {
		n := 0
		for _, e := range h {
			if e.EventType == engine.EventTypeContinueAsNew {
				n++
			}
		}
		return n
	}
	seeded := continueEvents(prefix)

	type outcome struct {
		ops            []string
		runs, cnaEvent int
		status         string
		logs           string
	}
	scenario := func(t *testing.T, how string) outcome {
		t.Helper()
		calls.reset()
		logs := &syncBuffer{}
		w := newRealDeferPhaseWorker(t, db, store, logs)
		w.egress = &engine.EgressGuard{AllowLoopback: true, TenantOptional: func(context.Context) bool { return true }}
		wfID := fmt.Sprintf("defer-segment-recorded-can-%s-%d", how, time.Now().UnixNano())
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
			t.Fatalf("PRECONDITION FAILED: the claim carried PendingTerminalStatus %q, want %q (%s)", claimed.PendingTerminalStatus, want, how)
		}
		runExecuteWorkflow(w, claimed)

		history, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("LoadEventHistory: %v", err)
		}
		o := outcome{ops: calls.all(), runs: continuations(t, wfID), cnaEvent: continueEvents(history), status: statusOf(t, ctx, store, wfID), logs: logs.String()}
		// The control's continuation is a live run in a database that outlives the test; end the ones this scenario started.
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
		return o
	}

	t.Run("control: an ordinary resume of the same history continues as new", func(t *testing.T) {
		o := scenario(t, "resume")
		if o.runs != 1 {
			t.Fatalf("PRECONDITION FAILED: the control produced %d continuation run(s) (status %q), want 1, so this probe cannot see a continuation (logs:\n%s)", o.runs, o.status, o.logs)
		}
	})

	for _, how := range []string{"terminate", "cancel"} {
		how := how
		want := map[string]string{"terminate": "terminated", "cancel": "cancelled"}[how]
		t.Run("a "+how+"'s defer segment finishes the "+how+" and does not continue as new", func(t *testing.T) {
			o := scenario(t, how)
			if o.runs != 0 {
				t.Errorf("a %s workflow's defer segment STARTED %d continuation run(s): the continue_as_new suspension was read before the defer phase was finished (logs:\n%s)", how, o.runs, o.logs)
			}
			if o.cnaEvent != seeded {
				t.Errorf("a %s workflow's history holds %d continue_as_new event(s), want the %d it was seeded with", how, o.cnaEvent, seeded)
			}
			if o.status != want {
				t.Errorf("the workflow ended %q, want %q", o.status, want)
			}
			// NOT asserted: whether the registered cleanup ran. Measured on this hand-built history, it ran 0 times (the engine-refused variant in
			// TestATerminatedWorkflowsDeferSegmentDoesNotContinueAsNew runs it once). That is an observation about a history no live workflow has, not a property
			// this test is for, and its cause was not investigated here.
		})
	}
}

// aHistoryHandBuiltToEndInARecordedContinueAsNew is [.. defer, continue_as_new]: the history an in-process run of entry produces, cut right after the defer registration (as
// killedAfterTheDeferThatContinues does), plus ONE hand-written continue_as_new event. The event is not something the engine records (see the test's doc); continueAsNewStep
// has no identity to compare, so it is consumed by any record of its type. The helper fails, and says which precondition, unless the run really ends in the suspension and
// the cut really leaves the defer registration as its last event.
func aHistoryHandBuiltToEndInARecordedContinueAsNew(t *testing.T, ctx context.Context, wasmBytes []byte, entry string) []engine.EventRecord {
	t.Helper()
	cut := killedAfterTheDeferThatContinues(t, ctx, wasmBytes, entry)
	if len(cut) == 0 || cut[len(cut)-1].EventType != engine.EventTypeDefer {
		t.Fatalf("PRECONDITION FAILED: the cut history of %s does not end at the defer registration (%d events, last %v)", entry, len(cut), lastEventType(cut))
	}
	for _, e := range cut {
		if e.EventType == engine.EventTypeContinueAsNew {
			t.Fatalf("PRECONDITION FAILED: the engine DID record a continue_as_new event in %s's history, so the hand-built one would be a second; the test's premise (the worker appends it) is wrong", entry)
		}
	}
	return append(cut, engine.EventRecord{Step: len(cut), EventType: engine.EventTypeContinueAsNew, NewInput: `{"__entry_point":"defer_then_continue_as_new"}`})
}

func lastEventType(h []engine.EventRecord) engine.EventType {
	if len(h) == 0 {
		return ""
	}
	return h[len(h)-1].EventType
}
