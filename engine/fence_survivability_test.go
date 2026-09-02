//go:build cgo

package engine

import (
	"testing"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// What survives the execution fence?
//
// IMPROVEMENT-PLAN 3.35 phase 4 is about the workflows whose defers nothing
// runs: a guest killed by the fence, a trap, or a timeout never reaches its own
// defer runner (3.70), and the host cannot reach a closure registered in guest
// memory. Whether anything can be done about the fence case turns on a property
// of wasmtime nobody had measured: after epoch interruption stops a guest, is
// the instance still usable?
//
// It is. That is what this file pins -- as a test rather than a note, because
// it is a property of a pinned dependency and a wasmtime upgrade could change
// it silently. If it ever stops holding, phase 4's fence case needs a different
// design and this test is where that shows up.
//
// Read the boundary carefully. This is a hand-written module with no language
// runtime in it. A Go guest fenced mid-loop additionally has a Go runtime
// interrupted at an arbitrary point -- scheduler, GC and stack all in whatever
// state the interrupt found them -- and whether it is safe to re-enter that is
// a separate question this does NOT answer. See the note in 3.35.

// TestTheFenceLeavesTheInstanceUsable measures what an epoch interruption
// leaves behind.
//
// The module writes a marker into linear memory, then spins. The fence stops
// it. Then a second export reads the marker back: that it returns at all shows
// the instance is callable, and that it returns the marker shows the memory the
// guest wrote before it was stopped is still there -- which is where a
// registered defer's closure would live.
func TestTheFenceLeavesTheInstanceUsable(t *testing.T) {
	cfg := wasmtime.NewConfig()
	cfg.SetEpochInterruption(true)
	eng := wasmtime.NewEngineWithConfig(cfg)

	wasmBytes, err := wasmtime.Wat2Wasm(`(module
	  (memory (export "memory") 1)
	  (func (export "mark") (result i32)
	    (i32.store (i32.const 100) (i32.const 42))
	    (loop $forever (br $forever))
	    (i32.const 0))
	  (func (export "peek") (result i32)
	    (i32.load (i32.const 100))))`)
	if err != nil {
		t.Fatalf("Wat2Wasm: %v", err)
	}

	module, err := wasmtime.NewModule(eng, wasmBytes)
	if err != nil {
		t.Fatalf("NewModule: %v", err)
	}
	defer module.Close()

	store := wasmtime.NewStore(eng)
	defer store.Close()
	// Two ticks at the interval below, so the fence fires in ~100ms rather than
	// after the 30s production default.
	store.SetEpochDeadline(2)

	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(epochTickInterval)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				eng.IncrementEpoch()
			}
		}
	}()
	defer close(stop)

	instance, err := wasmtime.NewInstance(store, module, nil)
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}

	began := time.Now()
	if _, err := instance.GetFunc(store, "mark").Call(store); err == nil {
		t.Fatal("the spinning export returned instead of being interrupted, so " +
			"the fence did not fire and nothing below is a measurement of anything")
	}
	fencedAfter := time.Since(began)
	if fencedAfter > 5*time.Second {
		t.Errorf("the fence took %v to fire; the deadline was 2 ticks of %v",
			fencedAfter.Round(time.Millisecond), epochTickInterval)
	}

	// Grant a fresh budget -- SetEpochDeadline is relative to the current epoch,
	// so without this the next call is interrupted immediately.
	store.SetEpochDeadline(200)

	peek := instance.GetFunc(store, "peek")
	if peek == nil {
		t.Fatal("no peek export")
	}
	val, err := peek.Call(store)
	if err != nil {
		t.Fatalf("the instance is not callable after the fence stopped it: %v\n\n"+
			"This used to hold, and IMPROVEMENT-PLAN 3.35's phase 4 notes depend on "+
			"it: if a fenced instance cannot be re-entered, its defer closures are "+
			"unreachable and the fence case needs a different design. A wasmtime "+
			"upgrade changing this is the likeliest cause.", err)
	}
	if val != int32(42) {
		t.Fatalf("peek returned %v, want 42 -- the instance is callable but the "+
			"memory the guest wrote before the fence stopped it did not survive, "+
			"which is the half that matters for reaching a registered defer", val)
	}
}
