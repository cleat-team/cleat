package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#4174. --wasm-instance-timeout bounds GUEST EXECUTION: "time the guest spends blocked in a host call ... is NOT charged against it". The core-module
// path brackets host time (hostBudget); the component path (Python, C#) did not, so a durable call whose service took longer than the instance timeout
// killed the run with "execution time limit exceeded". Measured on a stock worker (30 s fence, one call to a service that sleeps, nothing else): a 25 s
// service completed in 31 s, a 40 s service FAILED after 44 s.
//
// The fence here is shortened (10 s) so the test is CI-sized; the service delays are scaled to match (6 s under it, 16 s over it). EVERY row runs through
// a real worker: the Python or Go component, wasmtime, the engine, PostgreSQL.
func TestASlowHostCallIsNotChargedToTheInstanceFence(t *testing.T) {
	const fence = 10 * time.Second
	ctx := context.Background()

	svcDelay := atomic.Int64{}
	var served atomic.Int64
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		time.Sleep(time.Duration(svcDelay.Load()) * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer svc.Close()
	oldSvcURL := *benchSvcURL
	*benchSvcURL = svc.URL
	defer func() { *benchSvcURL = oldSvcURL }()

	db := testutil.SuiteTestDB(t, "cleat_worker")
	settleRunsLeftBehind(t, db)
	store := engine.NewPostgresStore(db)

	py := func(entry string) func(*testing.T) ([]byte, json.RawMessage) {
		return func(t *testing.T) ([]byte, json.RawMessage) {
			return buildPythonExample(t, "python-sdk/examples/slow_call_workflow.py", entry), json.RawMessage(`{"request":{}}`)
		}
	}
	for _, tc := range []struct {
		name       string
		build      func(*testing.T) ([]byte, json.RawMessage)
		serviceMs  int64
		wantStatus string
		// wantAtLeast / wantAtMost bound the run's wall time: a kill that fires at the fence is a different number from one that fires late.
		wantAtLeast, wantAtMost time.Duration
		wantErrContains         string // a kill names the budget that was enforced: the fence under test, not the stock one
	}{
		{"python, service 6 s, under the fence (control)", py("slow_call"), 6000, "done", 6 * time.Second, 0, ""},
		{"python, service 16 s, OVER the fence: the host call is not charged", py("slow_call"), 16000, "done", 16 * time.Second, 0, ""},
		{"python, a PLUGIN call of 16 s, over the fence", py("slow_plugin_call"), 16000, "done", 16 * time.Second, 0, ""},
		{"python, a STREAMING plugin call of 16 s, over the fence", py("slow_plugin_stream"), 16000, "done", 16 * time.Second, 0, ""},
		{"go core module, service 16 s, over the fence (core control)", func(t *testing.T) ([]byte, json.RawMessage) {
			return buildSDKHelperFixture(t, "slowcall"), json.RawMessage(`"go"`)
		}, 16000, "done", 16 * time.Second, 0, ""},
		// The fix must not switch the fence off. A host call that returns and then a guest-side tight loop is killed at the fence, counted from the
		// call's return; a tight loop with no host call at all is killed at the fence.
		{"python, a short call and then a tight loop: the fence still kills it", py("slow_call_then_spin"), 2000, "failed", 2*time.Second + fence, 2*time.Second + fence + 15*time.Second, "(10s wall-clock budget"},
		// A guest that never re-enters the host: its budget is armed once and never re-armed. This shows the fence still fires for such a guest; it does NOT isolate
		// the arm() before the guest runs, because configureStore's own deadline would kill it too.
		// An earlier version of this row used killed_with_defers_workflow.py's spin_no_defer, which makes a durable call before it spins (the service
		// log said requests served=1), so it was a second "call, then loop" row. This one asserts that no service request was made (a plugin call would not
		// show there, but the entry makes no call of any kind).
		{"python, a tight loop and no host call at all: the fence kills it", py("spin_without_a_host_call"), 0, "failed", fence, fence + 15*time.Second, "(10s wall-clock budget"},
	} {
		t.Run(strings.ReplaceAll(tc.name, " ", "_"), func(t *testing.T) {
			wasmBytes, input := tc.build(t)
			meta, err := wasm.ReadMetadata(wasmBytes)
			if err != nil {
				t.Fatalf("ReadMetadata: %v", err)
			}
			defName := fmt.Sprintf("slowhost-%d", time.Now().UnixNano())
			if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
				ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion}); err != nil {
				t.Fatalf("deploy: %v", err)
			}
			wfID := defName + "-run"
			if _, _, err := store.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion, input, "", engine.DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			logs := &syncBuffer{}
			w := newRealDeferPhaseWorker(t, db, store, logs)
			w.egress = &engine.EgressGuard{AllowLoopback: true, TenantOptional: func(context.Context) bool { return true }}
			// The fence is a property of the BACKEND (newRealDeferPhaseWorker builds one with the stock 30 s): the worker's own wasmInstanceTimeout field
			// does not set it, so a first version of this test ran every row against 30 s and its "over the fence" rows were under it.
			backend, err := engine.NewWasmtimeBackend(ctx, engine.WithWasmtimeExecutionTimeout(fence))
			if err != nil {
				t.Fatalf("NewWasmtimeBackend: %v", err)
			}
			t.Cleanup(func() { backend.Close(ctx) })
			w.wasmtimeBackend = backend
			w.wasmInstanceTimeout = fence
			// A plugin call and a STREAMING plugin call that block: the AI-call shapes. Both sleep for the row's service delay.
			w.pluginRegistry = engine.NewPluginRegistry()
			if err := w.pluginRegistry.Register("slow", "wait", func(ctx context.Context, _ string) (string, error) {
				time.Sleep(time.Duration(svcDelay.Load()) * time.Millisecond)
				return `{"ok":true}`, nil
			}); err != nil {
				t.Fatalf("registering the slow plugin: %v", err)
			}
			w.pluginStreamRegistry = engine.NewPluginStreamRegistry()
			if err := w.pluginStreamRegistry.Register("slowstream", "run", func(ctx context.Context, _ string) (<-chan plugin.StreamEvent, error) {
				ch := make(chan plugin.StreamEvent, 4)
				go func() {
					defer close(ch)
					// Nothing for the first half of the delay, then two chunks with the rest of it between them: the guest is blocked reading the stream.
					half := time.Duration(svcDelay.Load()) * time.Millisecond / 2
					time.Sleep(half)
					ch <- plugin.StreamEvent{Index: 0, Content: "a"}
					time.Sleep(half)
					ch <- plugin.StreamEvent{Index: 1, Content: "b", Finish: true}
				}()
				return ch, nil
			}); err != nil {
				t.Fatalf("registering the slow stream: %v", err)
			}
			svcDelay.Store(tc.serviceMs)
			served.Store(0)
			wf := claimOne(t, ctx, store, w.id, wfID)
			began := time.Now()
			runExecuteWorkflow(w, wf)
			elapsed := time.Since(began)
			got, _ := store.GetWorkflowByID(ctx, wfID)
			t.Logf("fence %v, service %d ms -> status=%q after %.1fs, requests served=%d, error=%q", fence, tc.serviceMs, got.Status, elapsed.Seconds(), served.Load(), got.Error)
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (error %q)", got.Status, tc.wantStatus, got.Error)
			}
			if strings.Contains(tc.name, "no host call at all") && served.Load() != 0 {
				t.Fatalf("%d request(s) reached the service: this row is meant to be a guest that makes no service call", served.Load())
			}
			if tc.wantErrContains != "" && !strings.Contains(got.Error, tc.wantErrContains) {
				t.Fatalf("error = %q, want it to contain %q", got.Error, tc.wantErrContains)
			}
			if tc.wantAtLeast > 0 && elapsed < tc.wantAtLeast {
				t.Fatalf("finished after %v, want at least %v", elapsed, tc.wantAtLeast)
			}
			if tc.wantAtMost > 0 && elapsed > tc.wantAtMost {
				t.Fatalf("took %v, want at most %v: the fence did not fire when it should", elapsed, tc.wantAtMost)
			}
		})
	}
}
