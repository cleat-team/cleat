//go:build cgo

package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// cleat#4079, part 2. With per-step flush OFF the engine writes no event as it goes and relies on the worker's finalization at
// segment end. A run that FAILS never reaches finalization, and the worker's terminal-failure write (FailWorkflow) carries no
// events, so a killed run lost its whole history: measured on all three dialects, 0 of the 2 events a killed guest had recorded
// were in the database with the flag off and 2 of 2 with it on. That is a defect for every failed run under the flag, core modules
// included, and it is independent of whether anyone ever runs a killed run's defers.
//
// The fix is a deferred tail on Execute that writes the unflushed events through the same fenced, chained, idempotent writer
// the flag-on path uses, before Execute returns. What is asserted is what the DATABASE holds afterwards.
//
// The flag-ON column is the known-positive and is not optional: this harness first read 0 persisted in BOTH columns, because the
// engine had not been given the store's *sql.DB and so could not write at all. A harness whose control reads 0 has measured nothing.

func zzStoreDB(t *testing.T, store WorkflowStore) *sql.DB {
	t.Helper()
	switch st := store.(type) {
	case *PostgresStore:
		return st.db
	case *MySQLStore:
		return st.db
	case *MSSQLStore:
		return st.db
	}
	t.Fatalf("no *sql.DB for %T", store)
	return nil
}

// runKilledGuest runs a fixture entry point to its terminal error against a real store, and returns the error, the number of events
// the engine held in memory, and the number the database holds afterwards.
func runKilledGuest(t *testing.T, store WorkflowStore, wasmBytes []byte, entry string, noFlush bool, opts ...EngineOption) (error, int, []EventRecord) {
	t.Helper()
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx, WithWasmtimeExecutionTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	wfID := newIntentWorkflow(t, ctx, store, fmt.Sprintf("kill-%s-%v", entry, noFlush))
	base := []EngineOption{
		WithBackends(WasmtimeLanguages, wt), WithWorkflowStore(store), WithDB(zzStoreDB(t, store)),
		WithTenantID(DefaultTenantUUID), WithWorkflowID(wfID), WithNoPerStepFlush(noFlush),
	}
	eng := NewEngine(rt, &mockCaller{}, append(base, opts...)...)
	_, hist, _, _, _, execErr := eng.Execute(ctx, wasmBytes, entry, json.RawMessage(`{}`))
	persisted, lerr := store.LoadEventHistory(ctx, wfID)
	if lerr != nil {
		t.Fatalf("LoadEventHistory: %v", lerr)
	}
	return execErr, len(hist), persisted
}

func TestAFailedRunsEventsAreWrittenBeforeExecuteReturns(t *testing.T) {
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "killafterwork"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// The fewest events the engine must hold for each entry point: the call, and for the spinning one also its defer.
	for entry, minEvents := range map[string]int{"call_then_spin": 2, "call_then_fail": 1} {
		entry, minEvents := entry, minEvents
		t.Run(entry, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, store WorkflowStore) {
				for _, noFlush := range []bool{false, true} {
					execErr, inMemory, persisted := runKilledGuest(t, store, wasmBytes, entry, noFlush)
					if execErr == nil {
						t.Fatalf("PRECONDITION FAILED: %s did not end in an error (flag-off=%v), so there is no failure path to test", entry, noFlush)
					}
					if inMemory < minEvents {
						t.Fatalf("PRECONDITION FAILED: the engine held only %d events for %s (flag-off=%v), want at least %d", inMemory, entry, noFlush, minEvents)
					}
					t.Logf("%s flag-off=%-5v engine held %d events, database holds %d", entry, noFlush, inMemory, len(persisted))
					// Everything the engine held must be in the database, flag on or off.
					if len(persisted) != inMemory {
						t.Errorf("%s with per-step flush %s: the engine recorded %d events and the database holds %d.\n\n"+
							"With the flag ON this is the known-positive: if it fails there the harness is not measuring persistence at all. "+
							"With it OFF a failed run's history is lost, because nothing writes it. cleat#4079.",
							entry, map[bool]string{true: "OFF", false: "ON"}[noFlush], inMemory, len(persisted))
					}
				}
			})
		})
	}
}

// The tail must not write for a run this worker no longer owns. WithWorkerID and WithGeneration turn fencing on; the generation the
// engine carries is not the one the store holds, which is exactly a run another worker has reclaimed.
func TestAFailedRunThatLostItsFenceWritesNothingAndSaysSo(t *testing.T) {
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "killafterwork"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		// worker "w-lost", generation 999: no lease row carries that pair, so every fenced insert reports a lost fence.
		execErr, inMemory, persisted := runKilledGuest(t, store, wasmBytes, "call_then_fail", true,
			WithWorkerID("w-lost"), WithGeneration(999))
		if execErr == nil || inMemory < 1 {
			t.Fatalf("PRECONDITION FAILED: no failure to flush (err=%v, in-memory=%d)", execErr, inMemory)
		}
		if len(persisted) != 0 {
			t.Errorf("a run that had lost its fence wrote %d events: another worker owns the history now", len(persisted))
		}
		if !errors.Is(execErr, ErrFenceLost) {
			t.Errorf("Execute returned %v for a run that lost its fence, want an error wrapping ErrFenceLost so the worker does not report a failure for a run it no longer owns", execErr)
		}
	})
}

// What if the process dies AFTER the tail has written the events and BEFORE the worker writes the terminal failure? The run is
// still claimed and not terminal, so its lease expires and another worker claims it and REPLAYS the persisted history. That is
// exactly what flag-on leaves behind at every step, so the recovery is the same one. Without the tail (flag-off before this
// change) the same death left no history at all, and the new owner re-executed the run from the top: the call below ran twice.
//
// Asserted: a second engine replaying what the database holds does not call the service again for the recorded call.
func TestAReplayOfAFailedRunsPersistedHistoryDoesNotRepeatItsCalls(t *testing.T) {
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "killafterwork"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		execErr, _, persisted := runKilledGuest(t, store, wasmBytes, "call_then_fail", true)
		if execErr == nil || len(persisted) == 0 {
			t.Fatalf("PRECONDITION FAILED: nothing to recover from (err=%v, persisted=%d)", execErr, len(persisted))
		}

		rt, _ := NewRuntime(ctx, 0, 0)
		t.Cleanup(func() { rt.Close(ctx) })
		wt, _ := NewWasmtimeBackend(ctx, WithWasmtimeExecutionTimeout(5*time.Second))
		t.Cleanup(func() { wt.Close(ctx) })
		caller := &mockCaller{}
		eng := NewEngine(rt, caller, WithBackends(WasmtimeLanguages, wt), WithWorkflowID("wf-recovering-worker"))
		_, _, _, _, _, replayErr := eng.Replay(ctx, wasmBytes, "call_then_fail", json.RawMessage(`{}`), persisted)

		if replayErr == nil {
			t.Fatal("the replay of a run that failed on purpose completed")
		}

		// CONTROL: the same replay from an EMPTY history, which is what a recovering worker finds when nothing was written. It must
		// reach the service, or "the service was not called" above proves nothing about the history.
		ctl := &mockCaller{}
		ctlEng := NewEngine(rt, ctl, WithBackends(WasmtimeLanguages, wt), WithWorkflowID("wf-recovering-worker-control"))
		_, _, _, _, _, _ = ctlEng.Execute(ctx, wasmBytes, "call_then_fail", json.RawMessage(`{}`))
		if ops := operationsCalled(ctl); len(ops) != 1 || ops[0] != "charge" {
			t.Fatalf("CONTROL FAILED: re-running from nothing called %v, want [charge]; the assertion below cannot see a re-execution", ops)
		}
		if ops := operationsCalled(caller); len(ops) != 0 {
			t.Errorf("the recovering worker called the service %v: it re-executed a call the persisted history already records", ops)
		}
	})
}
