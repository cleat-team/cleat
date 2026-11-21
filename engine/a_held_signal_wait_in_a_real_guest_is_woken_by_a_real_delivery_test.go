//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// THE WHOLE CHAIN, on the real backend: a real guest awaits a signal, the call parks it behind the resident set, a real store's DeliverSignal arrives from outside, the hold's recheck finds it, the
// call's own read consumes it, and the guest carries on IN THE SAME EXECUTION (observable: it makes a host call after the wait, which a suspended run never reaches). #4286 showed what unit rows
// can agree with while the real case is wrong, so this is the row that has to hold. It runs on every configured dialect.

type signalGuestRun struct {
	result    string
	suspended bool
	ops       []string
	holds     int
	elapsed   time.Duration
}

// runSignalGuest executes await_go_then_call against store/wf. whileRunning is called once the run is held (resident) or after a short moment (not), from another goroutine's point of view: the
// run is still executing when it is called.
func runSignalGuest(t *testing.T, rt *Runtime, wt *wasmtimeBackend, wasm []byte, store WorkflowStore, wf string, resident bool, input string, whileHeld func(set *ResidentSet)) signalGuestRun {
	t.Helper()
	m := newReplayMetrics(t)
	mc := &mockCaller{}
	opts := []EngineOption{WithBackends(WasmtimeLanguages, wt), WithWorkflowID(wf), WithSignalStore(store)}
	var set *ResidentSet
	if resident {
		cfg := DefaultResidencyConfig()
		cfg.MaxRuns = 2
		cfg.RecheckInterval = 50 * time.Millisecond
		cfg.Metrics = m
		var err error
		if set, err = NewResidentSet(cfg); err != nil {
			t.Fatalf("NewResidentSet: %v", err)
		}
		if !set.AttachToCodeCache(wt) { // the worker's own wiring, so this also exercises the code-cache check
			t.Fatal("PRECONDITION FAILED: the backend has no code cache to attach to")
		}
		opts = append(opts, WithResidency(set.ForRun(ResidentRunInfo{WorkflowID: wf, Language: "go", CodeKey: CodeID(wasm), ClaimCheck: claimStillHeld})))
	}
	eng := NewEngine(rt, mc, opts...)
	eng.Metrics = m
	type result struct {
		res  string
		susp bool
		err  error
	}
	done := make(chan result, 1)
	t0 := time.Now()
	go func() {
		res, _, susp, _, _, err := eng.Execute(context.Background(), wasm, "await_go_then_call", json.RawMessage(input))
		done <- result{res, susp != nil, err}
	}()
	if whileHeld != nil {
		if resident {
			deadline := time.Now().Add(15 * time.Second)
			for set.Len() == 0 && time.Now().Before(deadline) {
				time.Sleep(2 * time.Millisecond)
			}
			if set.Len() == 0 {
				t.Fatal("PRECONDITION FAILED: the run was never held, so whatever follows measured nothing")
			}
		}
		whileHeld(set)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("execute: %v", r.err)
		}
		return signalGuestRun{result: r.res, suspended: r.susp, ops: operationsCalled(mc), holds: residentCounter(t, m, "cleat_resident_parks_total"), elapsed: time.Since(t0)}
	case <-time.After(60 * time.Second):
		t.Fatal("the execution never ended")
		return signalGuestRun{}
	}
}

func TestAHeldSignalWaitInARealGuestIsWokenByARealDeliveryAndTheGuestContinuesInPlace(t *testing.T) {
	if testing.Short() {
		t.Skip("no database in -short mode; every per-dialect subtest would skip")
	}
	wasm, err := os.ReadFile(buildFixtureWasm(t, "residentsleep"))
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	rt, wt := newResidentBackend(t, 30*time.Second)
	var ran []string
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			// The first run on a backend compiles the module, which takes longer than the waits below; it must not be what a wait measures.
			warm := startRealRun(t, store, "sig-guest-warm-"+backend.Name())
			runSignalGuest(t, rt, wt, wasm, store, warm, false, `{"ms":50}`, nil)

			// THE CASE: held, an unrelated signal does not wake it, then the real delivery does, and the guest continues in place with the payload.
			wf := startRealRun(t, store, "sig-guest-held-"+backend.Name())
			got := runSignalGuest(t, rt, wt, wasm, store, wf, true, `{"ms":6000}`, func(set *ResidentSet) {
				if err := store.DeliverSignal(ctx, wf, "other", `{"not":"for you"}`); err != nil {
					t.Fatalf("DeliverSignal(other): %v", err)
				}
				time.Sleep(300 * time.Millisecond) // six recheck intervals
				if n := set.Len(); n != 1 {
					t.Fatalf("an unrelated signal ended the hold: %d runs held, want 1", n)
				}
				if err := store.DeliverSignal(ctx, wf, "go", `{"approved":true}`); err != nil {
					t.Fatalf("DeliverSignal(go): %v", err)
				}
			})
			if got.suspended {
				t.Fatalf("the run suspended: it was not held through the wait (ops %v, result %q)", got.ops, got.result)
			}
			if len(got.ops) != 1 || got.ops[0] != "after_await" {
				t.Errorf("the guest made calls %v, want exactly [after_await]: it did not carry on in the same execution", got.ops)
			}
			if !strings.Contains(got.result, `"status":"signalled"`) || !strings.Contains(got.result, `"approved":true`) {
				t.Errorf("the guest returned %q, want the signal and its payload", got.result)
			}
			if got.holds != 1 {
				t.Errorf("%d hold(s) counted, want 1", got.holds)
			}
			if got.elapsed > 5*time.Second {
				t.Errorf("the run took %s against a 6 s wait: it waited for the deadline and not for the signal", got.elapsed)
			}
			// The CALL consumed the delivery (the recheck must not have): once, through the one path replay uses.
			if _, found, err := store.PollSignal(ctx, wf, "go"); err != nil || found {
				t.Errorf("after the run completed the delivery is still pending (found=%v err=%v): the call's own read did not consume it", found, err)
			}

			// CONTROL: the same guest with residency OFF suspends at the wait and never reaches the call, so "held" is what made the case above continue.
			off := startRealRun(t, store, "sig-guest-off-"+backend.Name())
			ctl := runSignalGuest(t, rt, wt, wasm, store, off, false, `{"ms":6000}`, nil)
			if !ctl.suspended || len(ctl.ops) != 0 {
				t.Errorf("CONTROL FAILED: with residency off the guest suspended=%v and made calls %v, want suspended with none: the held case proves nothing", ctl.suspended, ctl.ops)
			}

			// DEADLINE, in place: held with no signal, the wait ends at its OWN deadline (600 ms) and the guest takes its timed-out branch in the same execution.
			dl := startRealRun(t, store, "sig-guest-deadline-"+backend.Name())
			tm := runSignalGuest(t, rt, wt, wasm, store, dl, true, `{"ms":600}`, func(*ResidentSet) {})
			if tm.suspended || !strings.Contains(tm.result, `"status":"timed_out"`) || len(tm.ops) != 1 {
				t.Errorf("a held wait with no signal: suspended=%v result %q ops %v, want it to time out in place and then make its call", tm.suspended, tm.result, tm.ops)
			}
			if tm.elapsed < 500*time.Millisecond {
				t.Errorf("the wait ended after %s, before its 600 ms deadline", tm.elapsed)
			}
			ran = append(ran, backend.Name())
		})
	}
	if len(ran) == 0 {
		t.Fatal("no backend ran, so this test measured nothing")
	}
}
