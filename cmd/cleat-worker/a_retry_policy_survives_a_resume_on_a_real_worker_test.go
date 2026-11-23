package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#4152, on a real worker: a call made under a retry policy AFTER the workflow's first suspension.
//
// The service fails its first two attempts and then succeeds; the policy is five attempts at 1ms. Two rows:
//
//	fresh      retry_call_only makes the call from a session with nothing to replay. This is the CONTROL: it shows the probe sees the whole
//	           policy retry (3 attempts, success).
//	resumed    sleep_then_retry_call suspends on a sleep, and the claim that resumes it replays [sleep] and meets the same call at the
//	           replay frontier. The policy must apply there too: 3 attempts, success.
func TestARetryPolicySurvivesAResumeOnARealWorker(t *testing.T) {
	ctx := context.Background()

	db := testutil.SuiteTestDB(t, "cleat_worker")
	store := engine.NewPostgresStore(db)

	svc := &flakyService{failFirst: 2}
	srv := httptest.NewServer(svc)
	defer srv.Close()
	oldSvcURL := *benchSvcURL
	*benchSvcURL = srv.URL
	defer func() { *benchSvcURL = oldSvcURL }()

	wasmBytes := buildDeferFixture(t)
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	const defName = "retry-policy-resume-worker"
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
		ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	start := func(t *testing.T, tag, entry string) string {
		t.Helper()
		wfID := fmt.Sprintf("retry-policy-resume-%s-%d", tag, time.Now().UnixNano())
		input := json.RawMessage(fmt.Sprintf(`{"__entry_point":%q}`, entry))
		if _, _, err := store.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion, input, "", engine.DefaultTenantUUID, 0); err != nil {
			t.Fatalf("StartNewRun: %v", err)
		}
		return wfID
	}

	// claimWhenDue claims the workflow once its wake time has passed, releasing anything else the claim picked up (see claimOne).
	claimWhenDue := func(t *testing.T, w *Worker, wfID string) *engine.WorkflowInstance {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			claimed, err := store.ClaimWorkflows(ctx, w.id, 50)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}
			var mine *engine.WorkflowInstance
			for _, wf := range claimed {
				if wf.ID == wfID {
					mine = wf
					continue
				}
				if err := store.ReleaseWorkflow(ctx, wf.ID, w.id, wf.Generation, wf.NextWakeAt); err != nil {
					t.Logf("releasing %s, which this test claimed but does not own: %v", wf.ID, err)
				}
			}
			if mine != nil {
				return mine
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("workflow %s was never claimed", wfID)
		return nil
	}

	t.Run("control: a fresh session's retried call gets its whole policy", func(t *testing.T) {
		svc.reset()
		logs := &syncBuffer{}
		w := newRealDeferPhaseWorker(t, db, store, logs)
		w.egress = &engine.EgressGuard{AllowLoopback: true, TenantOptional: func(context.Context) bool { return true }}
		wfID := start(t, "fresh", "retry_call_only")
		runExecuteWorkflow(w, claimWhenDue(t, w, wfID))
		done, err := store.GetWorkflowByID(ctx, wfID)
		if err != nil {
			t.Fatalf("GetWorkflowByID: %v", err)
		}
		if got := svc.attempts(); got != 3 || done.Status != "done" {
			t.Fatalf("the control did not retry to success: %d attempt(s) (want 3), run %q (want done), error %q; this probe cannot see the policy (logs:\n%s)",
				got, done.Status, done.Error, logs.String())
		}
	})

	t.Run("a run lost after its first call: the retried call at the replay frontier gets its whole policy", func(t *testing.T) {
		svc.reset()
		logs := &syncBuffer{}
		w := newRealDeferPhaseWorker(t, db, store, logs)
		w.egress = &engine.EgressGuard{AllowLoopback: true, TenantOptional: func(context.Context) bool { return true }}
		prefix := historyAfterTheFirstCall(t, ctx, wasmBytes, "call_then_retry_call")
		wfID := start(t, "lost", "call_then_retry_call")
		if err := store.AppendEventHistoryBatch(ctx, wfID, prefix); err != nil {
			t.Fatalf("AppendEventHistoryBatch: %v", err)
		}
		runExecuteWorkflow(w, claimWhenDue(t, w, wfID))
		done, err := store.GetWorkflowByID(ctx, wfID)
		if err != nil {
			t.Fatalf("GetWorkflowByID: %v", err)
		}
		if got := svc.attempts(); got != 3 || done.Status != "done" {
			t.Errorf("the retry policy was DROPPED at the replay frontier: the service saw %d attempt(s) (want 3, two failures then the success) and the run "+
				"ended %q (want done), error %q.\n\nThe run's history was [work/before] and the retried call arrived on a replaying session with "+
				"nothing left to replay, where it got one plain attempt instead of the five its policy asked for (logs:\n%s)", got, done.Status, done.Error, logs.String())
		}
	})

	t.Run("a resumed run's retried call gets its whole policy", func(t *testing.T) {
		svc.reset()
		logs := &syncBuffer{}
		w := newRealDeferPhaseWorker(t, db, store, logs)
		w.egress = &engine.EgressGuard{AllowLoopback: true, TenantOptional: func(context.Context) bool { return true }}
		wfID := start(t, "resumed", "sleep_then_retry_call")

		// First claim: the run reaches its sleep and suspends. No call has been made.
		runExecuteWorkflow(w, claimWhenDue(t, w, wfID))
		if n := svc.attempts(); n != 0 {
			t.Fatalf("PRECONDITION FAILED: the service saw %d attempt(s) before the run's sleep ended; the run did not suspend on the sleep (logs:\n%s)", n, logs.String())
		}
		suspended, err := store.GetWorkflowByID(ctx, wfID)
		if err != nil {
			t.Fatalf("GetWorkflowByID: %v", err)
		}
		if suspended.Status == "done" || suspended.Status == "failed" {
			t.Fatalf("PRECONDITION FAILED: after its first claim the run is %q, so there is no resume to measure (logs:\n%s)", suspended.Status, logs.String())
		}

		// Second claim: replays [sleep] and meets the retried call at the frontier.
		runExecuteWorkflow(w, claimWhenDue(t, w, wfID))
		done, err := store.GetWorkflowByID(ctx, wfID)
		if err != nil {
			t.Fatalf("GetWorkflowByID: %v", err)
		}
		if got := svc.attempts(); got != 3 || done.Status != "done" {
			t.Errorf("the retry policy was DROPPED across a resume: the service saw %d attempt(s) (want 3, two failures then the success) and the run "+
				"ended %q (want done), error %q.\n\nThe call was made on a replaying session at its frontier, where it got one plain attempt "+
				"instead of the five its policy asked for (logs:\n%s)", got, done.Status, done.Error, logs.String())
		}
	})
}

// historyAfterTheFirstCall runs the entry to completion in-process and returns its history cut after the first event, which is the history
// of a run lost right after its first call. It fails if the cut would be the whole history, since then nothing was left to do.
func historyAfterTheFirstCall(t *testing.T, ctx context.Context, wasmBytes []byte, entry string) []engine.EventRecord {
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
	eng := engine.NewEngine(rt, okCaller{}, engine.WithBackends(engine.WasmtimeLanguages, wt))
	_, history, susp, _, _, err := eng.Execute(ctx, wasmBytes, entry, json.RawMessage(`{}`))
	if err != nil || susp != nil {
		t.Fatalf("in-process run of %s: err=%v suspended=%v", entry, err, susp != nil)
	}
	if len(history) < 2 {
		t.Fatalf("PRECONDITION FAILED: %s recorded %d event(s); a cut after the first leaves nothing for the retried call to do", entry, len(history))
	}
	return history[:1]
}

// okCaller answers every service call, so an in-process run completes without touching the network.
type okCaller struct{}

func (okCaller) Call(context.Context, string, string, string) (string, error) {
	return `{"ok":true}`, nil
}

// flakyService fails its first failFirst attempts at flaky/op with a retryable 503 and answers every later one with success. It counts attempts.
type flakyService struct {
	mu        sync.Mutex
	failFirst int
	n         int
}

func (f *flakyService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path != "/call/flaky/op" {
		// Any other call (the fixture's work/before) always succeeds and is not an attempt of the retried call.
		fmt.Fprint(w, `{"ok":true}`)
		return
	}
	f.mu.Lock()
	f.n++
	n := f.n
	f.mu.Unlock()
	if n <= f.failFirst {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":"try again"}`)
		return
	}
	fmt.Fprint(w, `{"ok":true}`)
}

func (f *flakyService) attempts() int { f.mu.Lock(); defer f.mu.Unlock(); return f.n }
func (f *flakyService) reset()        { f.mu.Lock(); f.n = 0; f.mu.Unlock() }
