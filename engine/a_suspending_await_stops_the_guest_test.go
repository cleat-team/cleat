//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"
)

// cleat#3378: a FRESH AwaitSignals that had to wait returned
// packAwaitSignalsResult(0, 0, true, 0) -- byte-identical to a genuine
// timeout -- and relied on suspendErr to end the segment before the guest saw
// it. The guest saw it. It took its timeout branch, and a PluginCall there
// executed before the run stopped: the clew-service port measured a second
// webhook delivered 5ms into a 120s wait, with the plugin_call record then
// committing every replay to the same false timeout.
//
// This drives a compiled Go SDK guest (testdata/awaittimeoutbranch) through
// the wasmtime backend, segment by segment, with the clock stepped explicitly
// and the history persisted through each dialect's store between segments --
// the path a worker takes. The fixture is AwaitSignals -> on timeout,
// PluginCall -> AwaitSignals, and the property is the issue's acceptance:
// EXACTLY ONE plugin call, made only once the first deadline has really
// passed, and not repeated on replay.

// escalateInput binds by parameter name, as every generated Go entry point
// does, and is an object so SQL Server's input CHECK accepts it.
var escalateInput = json.RawMessage(`{"input":{"attempt":1}}`)

func TestASuspendingAwaitStopsTheGuestBeforeItsTimeoutBranch(t *testing.T) {
	wasmPath := buildFixtureWasm(t, "awaittimeoutbranch")
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
			Name: "await-timeout-branch", Version: 1, WASMBytes: wasmBytes, ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("DeployWorkflowDef: %v", err)
		}
		id, _, err := store.StartNewRun(ctx, "", "await-timeout-branch", 1, escalateInput, "", DefaultTenantUUID, 0)
		if err != nil {
			t.Fatalf("StartNewRun: %v", err)
		}

		// The side effect being counted. Registered with neither policy flag,
		// like a webhook send: replay must serve the record, never re-invoke.
		var redispatches atomic.Int32
		pr := NewPluginRegistry()
		if err := pr.Register("notify", "redispatch", func(ctx context.Context, input string) (string, error) {
			redispatches.Add(1)
			return `{"sent":true}`, nil
		}); err != nil {
			t.Fatalf("register plugin: %v", err)
		}

		rt, err := NewRuntime(ctx, 0, 0)
		if err != nil {
			t.Fatalf("NewRuntime: %v", err)
		}
		defer rt.Close(ctx)

		var clockMs int64
		engine := func() *Engine {
			return NewEngine(rt, &mockCaller{}, withWasmtimeBackend(t), WithPluginRegistry(pr),
				WithWorkflowID(id), WithClock(func() int64 { return clockMs }))
		}
		// persist appends what a segment recorded beyond `have` and returns
		// the history as the store hands it back -- which is what the next
		// segment must replay, codec and created_at timestamps included.
		persist := func(segment string, have int, hist []EventRecord) []EventRecord {
			t.Helper()
			for _, rec := range hist[have:] {
				if err := store.AppendEventHistory(ctx, id, rec); err != nil {
					t.Fatalf("%s: AppendEventHistory step %d: %v", segment, rec.Step, err)
				}
			}
			loaded, err := store.LoadEventHistory(ctx, id)
			if err != nil {
				t.Fatalf("%s: LoadEventHistory: %v", segment, err)
			}
			return loaded
		}
		kinds := func(h []EventRecord) []EventType {
			out := make([]EventType, len(h))
			for i, r := range h {
				out[i] = r.EventType
			}
			return out
		}

		// ---- segment 1: first execution. The await must suspend the run
		// before the guest reaches its timeout branch.
		_, h1, susp, _, _, err := engine().Execute(ctx, wasmBytes, "escalate", escalateInput)
		if err != nil {
			t.Fatalf("segment 1: %v", err)
		}
		if n := redispatches.Load(); n != 0 {
			t.Fatalf("segment 1 made %d plugin call(s), want 0. The first await had only just "+
				"begun its 60s wait; a call here is the guest acting on a timeout that did not "+
				"happen (cleat#3378). History: %v", n, kinds(h1))
		}
		if susp == nil {
			t.Fatal("segment 1 did not suspend at the first await")
		}
		if len(h1) != 1 || h1[0].EventType != EventTypeAwaitSignals {
			t.Fatalf("segment 1 recorded %v, want exactly [await_signals]", kinds(h1))
		}
		hist := persist("segment 1", 0, h1)

		// ---- segment 2: woken early (half the deadline). Replay reaches the
		// recorded await unexpired; it must suspend again, not time out.
		clockMs = hist[0].TimestampMs + 30_000
		_, h2, susp, _, _, err := engine().Replay(ctx, wasmBytes, "escalate", escalateInput, hist)
		if err != nil {
			t.Fatalf("segment 2: %v", err)
		}
		if n := redispatches.Load(); n != 0 {
			t.Fatalf("segment 2 (30s into a 60s wait) made %d plugin call(s), want 0", n)
		}
		if susp == nil || len(h2) != len(hist) {
			t.Fatalf("segment 2: suspended=%v history=%v; want a re-suspend that records nothing", susp != nil, kinds(h2))
		}

		// ---- segment 3: past the first deadline. Now the timeout is real:
		// the plugin call runs once, and the second await suspends.
		clockMs = hist[0].TimestampMs + 60_001
		_, h3, susp, _, _, err := engine().Replay(ctx, wasmBytes, "escalate", escalateInput, hist)
		if err != nil {
			t.Fatalf("segment 3: %v", err)
		}
		if n := redispatches.Load(); n != 1 {
			t.Fatalf("segment 3 made %d plugin call(s) in total, want exactly 1", n)
		}
		if susp == nil {
			t.Fatal("segment 3 did not suspend at the second await")
		}
		want := []EventType{EventTypeAwaitSignals, EventTypePluginCall, EventTypeAwaitSignals}
		if got := kinds(h3); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Fatalf("segment 3 history %v, want %v", got, want)
		}
		hist = persist("segment 3", len(hist), h3)

		// ---- segment 4: past the second deadline. Replay serves the
		// recorded plugin call -- no second side effect -- and finishes.
		clockMs = hist[2].TimestampMs + 60_001
		result, _, susp, _, _, err := engine().Replay(ctx, wasmBytes, "escalate", escalateInput, hist)
		if err != nil {
			t.Fatalf("segment 4: %v", err)
		}
		if susp != nil {
			t.Fatalf("segment 4 suspended again: %v", susp.Reason)
		}
		if n := redispatches.Load(); n != 1 {
			t.Fatalf("after replay the plugin had been called %d times, want exactly 1 -- replay re-invoked it", n)
		}
		if result != `"timed_out_twice"` && result != "timed_out_twice" {
			t.Fatalf("result = %q, want timed_out_twice", result)
		}
	})
}
