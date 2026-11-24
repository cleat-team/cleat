package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#4143, on a real worker: does the call a workflow's body was about to make reach the service when the workflow is TERMINATED or
// CANCELLED instead of resumed? Both end in the same kind of defer segment.
//
// The run: it registers a defer and would then call work/body, with nothing that waits in between. It was killed after the
// registration, so its history is [defer]. Two things can then happen to that history:
//
//	resumed     an ordinary claim replays [defer] and the body's next call arrives at the replay frontier. work/body reaches the
//	            service. This is the CONTROL: it shows the probe can see that call.
//	terminated  the terminate's defer segment replays [defer] and the body's next call arrives at the same frontier. A segment runs the
//	            defers and nothing else (IMPROVEMENT-PLAN 3.83), so work/body must not reach the service and the cleanup must.
//
// WHY THE FIXTURE HAS NOTHING THAT WAITS. A sleep or an await is also a dispatch point: the SDK polls for updates before it, and a defer
// segment refuses that poll, which stops the body there. A first version of this test registered a defer, slept 200ms and then called;
// it passed with the cancellation refusal in freshCall removed too, and a trace showed why: the segment stopped at the update poll before
// DurableSleep, so the body never reached the call at all. That scenario says nothing about the frontier.
func TestATerminatedWorkflowsDeferSegmentDoesNotMakeItsNextCall(t *testing.T) {
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
	const defName = "defer-segment-frontier-worker"
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
		ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	// The history of a run killed right after it registered its defer: a REAL execution's events, cut after the defer registration. The
	// execution is in-process with a stub caller, so nothing it does reaches the service above.
	prefix := killedAfterTheDefer(t, ctx, wasmBytes, "defer_then_call")

	// scenario seeds a run with that history and either terminates it or leaves it to be resumed, then runs its next claim on the worker.
	scenario := func(t *testing.T, how string) (ops []string, runLogs string) {
		t.Helper()
		calls.reset()
		logs := &syncBuffer{}
		w := newRealDeferPhaseWorker(t, db, store, logs)
		w.egress = &engine.EgressGuard{AllowLoopback: true, TenantOptional: func(context.Context) bool { return true }}
		terminate := how != "resume"
		wfID := fmt.Sprintf("defer-segment-frontier-%s-%d", how, time.Now().UnixNano())
		input := json.RawMessage(`{"__entry_point":"defer_then_call"}`)
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
		if terminate && claimed.PendingTerminalStatus != want {
			t.Fatalf("the claim carried PendingTerminalStatus %q, want %q; this is not a %s's defer segment", claimed.PendingTerminalStatus, want, how)
		}
		if !terminate && claimed.PendingTerminalStatus != "" {
			t.Fatalf("the control's claim carries PendingTerminalStatus %q; it must be an ordinary resume", claimed.PendingTerminalStatus)
		}
		runExecuteWorkflow(w, claimed)
		return calls.all(), logs.String()
	}

	t.Run("control: an ordinary resume makes the body's next call", func(t *testing.T) {
		ops, runLogs := scenario(t, "resume")
		if !hasOp(ops, "work/body") {
			t.Fatalf("the control did not reach the service with work/body (saw %v), so this probe cannot see the call (logs:\n%s)", ops, runLogs)
		}
	})

	for _, how := range []string{"terminate", "cancel"} {
		how := how
		t.Run("a "+how+"'s defer segment runs the cleanup and not the body's next call", func(t *testing.T) {
			ops, runLogs := scenario(t, how)
			if !hasOp(ops, "cleanup/after_defer_then_call") {
				t.Fatalf("the defer segment did not run the registered cleanup; the service saw %v (logs:\n%s)", ops, runLogs)
			}
			if hasOp(ops, "work/body") {
				t.Errorf("a workflow's %s defer segment MADE ITS BODY'S NEXT CALL: the service saw %v.\n\n"+
					"The segment replayed [defer] and work/body arrived at the replay frontier, where nothing refused it. A segment "+
					"must run the defers and nothing else.", how, ops)
			}
			if n := countOp(ops, "cleanup/after_defer_then_call"); n != 1 {
				t.Errorf("the registered cleanup ran %d times, want exactly once: %v", n, ops)
			}
		})
	}
}

// killedAfterTheDefer runs the entry to completion in-process and returns its history cut right after the defer registration, which is
// the history of a run killed there. It fails if the cut would be the whole history, since then nothing was left for the body to do.
func killedAfterTheDefer(t *testing.T, ctx context.Context, wasmBytes []byte, entry string) []engine.EventRecord {
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
	if err != nil || susp != nil {
		t.Fatalf("in-process run of %s: err=%v suspended=%v", entry, err, susp != nil)
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

// frontierCalls records what reached the service, resettable between scenarios.
type frontierCalls struct {
	mu  sync.Mutex
	ops []string
}

func (f *frontierCalls) add(op string) { f.mu.Lock(); f.ops = append(f.ops, op); f.mu.Unlock() }
func (f *frontierCalls) reset()        { f.mu.Lock(); f.ops = nil; f.mu.Unlock() }
func (f *frontierCalls) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

// stubCaller answers every service call, so an in-process run completes without touching the network.
type stubCaller struct{}

func (stubCaller) Call(context.Context, string, string, string) (string, error) {
	return `{"ok":true}`, nil
}

func countOp(ops []string, want string) int {
	n := 0
	for _, op := range ops {
		if op == want {
			n++
		}
	}
	return n
}

func hasOp(ops []string, want string) bool {
	for _, op := range ops {
		if op == want {
			return true
		}
	}
	return false
}
