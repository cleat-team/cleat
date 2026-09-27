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

// TestWorkerShutdownAbortsAFireAndForgetSchedule is cleat#2020's acceptance test for the
// DurableScheduleInvoke arm.
//
// WHY THIS FILE EXISTS AT ALL, given the fix already shipped. #2057 replaced three dead
// ctx.Done()/ctx.Err() branches in engine/durablecalls.go -- the retry loop's backoff wait and the
// two fire-and-forget sites (DurableSend, DurableScheduleInvoke) -- with a check against
// engine.shutdownRequested, and it shipped an acceptance test for ONE of the three. Its title says
// only "host backoff wait", and the test (shutdown_aborts_durable_call_test.go, this package) is
// entirely about the backoff select.
//
// So two of the three arms are on develop, unreferenced by any test, and the interesting property
// is not whether they are PRESENT -- reading durablecalls.go settles that -- but whether they FIRE.
// That distinction is CLAUDE.md's "a mechanism that exists and is wired to nothing reads as done":
// an arm nothing exercises answers "does this codebase handle shutdown here?" with yes, to a grep,
// a reviewer, and its own author six weeks later.
//
// The two remaining arms are not the same shape, and only one of them can carry a deterministic
// test:
//
//   - DurableScheduleInvoke GENUINELY WAITS. Its goroutine sits in a select over ctx.Done() /
//     shutdownRequested / time.After(delayMs). A delay long enough to cancel inside, plus a
//     shutdown, is an observation with margin on both sides. This file tests that arm.
//
//   - DurableSend is a ONE-SHOT PRE-DISPATCH CHECK with nothing to wait on: the goroutine's first
//     act is `select { case <-s.engine.shutdownRequested: return; default: }`. For that arm to be
//     the one that returns, the shutdown must land in the microseconds between the guest's
//     DurableSend and the goroutine's first instruction -- a window this test cannot widen without
//     editing the code it is testing. There is deliberately no test for it here, rather than a
//     flaky one that would report a green most runs and prove nothing. Its own comment in
//     durablecalls.go already calls it "a pre-dispatch guard, not a wait", which is the same
//     admission.
//
// THE CONTROL IS THE HALF THAT MATTERS. "The service never saw the deferred call" is equally true
// of an abort, of a schedule that never registered, and of a goroutine that died for an unrelated
// reason. So the same fixture runs twice: once with a shutdown during the delay (the deferred call
// must NOT land) and once without (it MUST). Only the pair says the shutdown is what stopped it.
// That is the same reason the backoff test asserts `len(calls) == 1` rather than merely that the
// wait returned.
func TestWorkerShutdownAbortsAFireAndForgetSchedule(t *testing.T) {
	requireShutdownTestDB(t)

	cases := []struct {
		name     string
		shutdown bool
		want     bool
	}{
		// The subject. Before #2057 this case failed by DISPATCHING: the delay elapsed, the call
		// landed 3s after the worker was already going away, and nothing shortened it.
		{"a shutdown during the delay never dispatches", true, false},
		// The control. Without this row the row above passes on a tree where ScheduleInvoke is
		// simply broken -- which is exactly the tree cleat#2020 was filed against.
		{"control: without a shutdown the deferred call still lands", false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := testutil.SuiteTestDB(t, "cleat_worker")
			store := engine.NewPostgresStore(db)

			wasmBytes := buildDeferFixture(t)
			meta, err := wasm.ReadMetadata(wasmBytes)
			if err != nil {
				t.Fatalf("ReadMetadata: %v", err)
			}

			// Named per case: both cases deploy the same fixture, and the two subtests can overlap
			// with another package's run against this database.
			defName := fmt.Sprintf("shutdown-schedule-%t-%d", tc.shutdown, time.Now().UnixNano())
			if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
				Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
				ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			wfID := fmt.Sprintf("shutdown-schedule-%t-%d", tc.shutdown, time.Now().UnixNano())
			// The fixture makes a marker call, then schedules a 3s delayed call. The marker is what
			// makes cancelling "during the delay" an observation rather than a guess -- see its
			// comment in testdata/deferfunc/workflow.go.
			if _, _, err := store.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion,
				json.RawMessage(`{"__entry_point":"ScheduleInvokeAfterAMarker"}`), "", engine.DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			// 200, unlike the backoff fixture's always-503: the marker call has to SUCCEED for the
			// guest to reach ScheduleInvoke at all.
			calls := &recordedCalls{}
			svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.add(strings.TrimPrefix(r.URL.Path, "/call/"))
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, `{"ok":true}`)
			}))
			defer svc.Close()
			oldSvcURL := *benchSvcURL
			*benchSvcURL = svc.URL
			defer func() { *benchSvcURL = oldSvcURL }()

			logs := &syncBuffer{}
			worker := newRealDeferPhaseWorker(t, db, store, logs)
			worker.id = "shutdown-schedule-worker"
			worker.egress = &engine.EgressGuard{
				AllowLoopback:  true,
				TenantOptional: func(context.Context) bool { return true },
			}

			claimed := claimOne(t, ctx, store, worker.id, wfID)
			worker.inflight.Store(wfID, claimed)

			done := make(chan struct{})
			worker.wg.Add(1)
			go func() {
				worker.executeWorkflow(claimed)
				close(done)
			}()

			// The marker reaching the service is the precondition for everything below: it proves
			// the guest ran, and it places DurableScheduleInvoke microseconds behind it.
			markerDeadline := time.Now().Add(5 * time.Second)
			for !calls.has("marker/marker") {
				if time.Now().After(markerDeadline) {
					t.Fatalf("the marker call never reached the service within 5s, so the guest never "+
						"got as far as scheduling (logs:\n%s)", logs.String())
				}
				time.Sleep(10 * time.Millisecond)
			}

			// The guest returns as soon as the schedule is registered -- the delay lives in a host
			// goroutine, not in the guest. Waiting for that return is what makes the window below
			// unambiguous: the only thing still outstanding is the scheduled dispatch itself.
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("executeWorkflow did not return within 5s of the schedule being registered; "+
					"a ScheduleInvoke must not hold the guest (logs:\n%s)", logs.String())
			}

			// Into the select, rather than racing it.
			time.Sleep(100 * time.Millisecond)

			if tc.shutdown {
				worker.cancel()
			}

			// Settle well past the fixture's 3s delay. Both directions use the same window so the
			// two cases differ only in whether the shutdown happened.
			const settle = 5 * time.Second
			if tc.want {
				landedDeadline := time.Now().Add(settle)
				for !calls.has("marker/deferred") {
					if time.Now().After(landedDeadline) {
						t.Fatalf("the deferred call never arrived within %s of being scheduled, though "+
							"nothing shut this worker down -- DurableScheduleInvoke did not dispatch at "+
							"all, so the case above would pass vacuously. service saw %v (logs:\n%s)",
							settle, calls.all(), logs.String())
					}
					time.Sleep(20 * time.Millisecond)
				}
				return
			}

			time.Sleep(settle)
			if calls.has("marker/deferred") {
				t.Errorf("the worker was cancelled during the 3s schedule delay, and the deferred call "+
					"was dispatched anyway (observed within the %s settle window). This is cleat#2020: "+
					"the goroutine's select must return on engine.shutdownRequested instead of waiting "+
					"out time.After(delayMs). The arm is present on develop; this is what fires it. "+
					"service saw %v (logs:\n%s)", settle, calls.all(), logs.String())
			}
		})
	}
}
