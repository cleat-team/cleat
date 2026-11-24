package engine

// cleat#4314. A compaction limit of 1 compacts the whole history, leaving an EMPTY tail, and the executor used to apply no compaction state to an empty tail
// (`len(history) > 0`), so the workflow started over and repeated every call it had made. Measured on the real store with a 10-call run that got as far as its 4th call:
// a limit of 2 (tail of 1) made the service see 6 calls, which is correct; a limit of 1 made it see 10.
//
// The shapes here are the ones the compaction can produce: for each limit and each number of recorded events above it, compact for real, take the state and the tail the
// store hands back, and replay the way the worker resumes. The service must see exactly the calls not yet made, whatever the tail's size, and the tail's size is part of the
// measurement: the question "can the tail be empty at a limit of 2 or more?" is answered by the rows below, not by reading `len(events) - effective/2`.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestACompactedRunResumesWithoutRepeatingACallWhateverTheLimit(t *testing.T) {
	ctx := context.Background()
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "minimal-wf"))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wt.Close(ctx) })
	count := func(c *mockCaller) (n int) {
		for _, call := range c.calls {
			if call.Service == "increment" && call.Op == "counter" {
				n++
			}
		}
		return n
	}
	// the full, real history of the 10-call run: the first k records are what a run that got k calls far wrote.
	const total = 10
	_, full, _, _, _, err := NewEngine(rt, &mockCaller{}, WithBackends(WasmtimeLanguages, wt), WithWorkflowID("wf-full")).Execute(ctx, wasmBytes, "SimpleWorkflowShort", json.RawMessage(`{}`))
	if err != nil || len(full) != total {
		t.Fatalf("setup: %d records, err=%v", len(full), err)
	}

	type shape struct{ limit, recorded int }
	var shapes []shape
	for limit := 1; limit <= 6; limit++ {
		for recorded := limit + 1; recorded <= limit+3 && recorded < total; recorded++ {
			shapes = append(shapes, shape{limit, recorded})
		}
	}
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			emptyTails := 0
			for _, sh := range shapes {
				id := fmt.Sprintf("compact-limit-%d-rec-%d-%d", sh.limit, sh.recorded, time.Now().UnixNano())
				if _, _, err := store.StartNewRun(ctx, id, "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
					t.Fatalf("limit %d / %d recorded: StartNewRun: %v", sh.limit, sh.recorded, err)
				}
				for _, ev := range full[:sh.recorded] {
					if err := store.AppendEventHistory(ctx, id, ev); err != nil {
						t.Fatalf("limit %d / %d recorded: append: %v", sh.limit, sh.recorded, err)
					}
				}
				if err := CompactWorkflowHistory(ctx, store, id, sh.limit, nil); err != nil {
					t.Fatalf("limit %d / %d recorded: compact: %v", sh.limit, sh.recorded, err)
				}
				cs, err := store.LoadCompactionState(ctx, id)
				if err != nil || cs == nil || len(cs.Events) == 0 {
					t.Fatalf("PRECONDITION FAILED: limit %d with %d recorded events did not compact (state=%v, err=%v): the row would measure nothing", sh.limit, sh.recorded, cs, err)
				}
				tail, err := store.LoadEventHistory(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if len(tail) == 0 {
					emptyTails++
				}
				caller := &mockCaller{}
				eng := NewEngine(rt, caller, WithBackends(WasmtimeLanguages, wt), WithWorkflowID(id), WithCompactionState(cs))
				if _, _, _, _, _, err := eng.Replay(ctx, wasmBytes, "SimpleWorkflowShort", json.RawMessage(`{}`), tail); err != nil {
					t.Errorf("limit %d / %d recorded (compacted %d, tail %d): replay: %v", sh.limit, sh.recorded, len(cs.Events), len(tail), err)
					continue
				}
				if got, want := count(caller), total-sh.recorded; got != want {
					t.Errorf("limit %d with %d recorded (compacted %d, tail %d): the service saw %d calls, want %d (the ones not yet made): %d call(s) repeated",
						sh.limit, sh.recorded, len(cs.Events), len(tail), got, want, got-want)
				}
			}
			// REACHABILITY, MEASURED: an empty tail happens at a limit of 1 and at no limit above it.
			wantEmpty := 0
			for _, sh := range shapes {
				if sh.limit == 1 {
					wantEmpty++
				}
			}
			if emptyTails != wantEmpty {
				t.Errorf("%d of %d shapes left an empty tail, want exactly the %d at a limit of 1: an empty tail is reachable at a larger limit than the compaction arithmetic says", emptyTails, len(shapes), wantEmpty)
			}
		})
	}
}
