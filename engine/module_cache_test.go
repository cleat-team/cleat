//go:build cgo

package engine

import (
	"fmt"
	"testing"

	"github.com/bytecodealliance/wasmtime-go/v48"
)

// The compiled-module cache is bounded.
//
// cleat#1563: it was a bare sync.Map with no Delete anywhere, so it retained
// one entry per distinct WASM artifact the process had ever executed, for the
// life of the process, across every tenant.
//
// Asserting the BOUND rather than the count after N inserts: a cache that
// dropped everything would also have "few entries", so the test inserts past
// the limit and requires both that the limit holds and that the cache is
// actually full.
func TestCompiledModuleCacheIsBounded(t *testing.T) {
	const max = 4
	c := newModuleLRU(max)
	for i := 0; i < max*5; i++ {
		c.store(fmt.Sprintf("key-%02d", i), &wasmtime.Module{})
	}
	if got := c.len(); got != max {
		t.Errorf("cache holds %d entries with a bound of %d.\n\n"+
			"Before cleat#1563 this was a sync.Map with no eviction and would "+
			"hold all %d.", got, max, max*5)
	}
}

// Eviction is least-recently-USED, not least-recently-inserted, and `load`
// counts as a use.
//
// Without this, an LRU that never refreshed on read would pass the bound test
// above while evicting the hottest entry -- which is worse than no cache for
// exactly the workload a cache exists for.
func TestCompiledModuleCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := newModuleLRU(3)
	for _, k := range []string{"a", "b", "c"} {
		c.store(k, &wasmtime.Module{})
	}

	// Touch "a" so it is no longer the coldest, then overflow by one.
	if _, ok := c.load("a"); !ok {
		t.Fatal("a should be cached")
	}
	c.store("d", &wasmtime.Module{})

	if _, ok := c.load("b"); ok {
		t.Error("b survived: it was the least recently USED and should have gone")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := c.load(k); !ok {
			t.Errorf("%q was evicted; only b should have been.\n\n"+
				"If a is missing, `load` is not refreshing recency and the cache "+
				"is evicting by insertion order.", k)
		}
	}
}

// THE PROPERTY THE WHOLE DESIGN RESTS ON: evicting a module cannot disturb an
// execution already instantiated from it.
//
// backend_wasmtime.go warns "Do NOT close the module -- it's cached and
// shared". That is correct about SHARING and silent about instance safety,
// which is the question an eviction policy needs answered. Reading the code
// cannot settle it either: wasmtime.NewInstance calls runtime.KeepAlive(module)
// for the duration of that call only, and the returned Instance keeps a C
// handle rather than a Go reference.
//
// This tests the STRONGEST form of eviction. Close() clears the finalizer and
// calls wasmtime_module_delete immediately; the cache's eviction merely drops a
// reference whose finalizer runs at some later GC. So if an instance survives
// Close, it survives eviction, and no GC timing has to be coaxed to prove it.
func TestAnInstanceSurvivesItsModuleBeingEvicted(t *testing.T) {
	wat := `(module (func (export "add") (param i32 i32) (result i32)
	          local.get 0 local.get 1 i32.add))`

	engine := wasmtime.NewEngine()
	store := wasmtime.NewStore(engine)
	wasmBytes, err := wasmtime.Wat2Wasm(wat)
	if err != nil {
		t.Fatalf("wat2wasm: %v", err)
	}
	module, err := wasmtime.NewModule(engine, wasmBytes)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	inst, err := wasmtime.NewInstance(store, module, nil)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}

	module.Close() // stronger than what eviction does

	fn := inst.GetFunc(store, "add")
	if fn == nil {
		t.Fatal("export 'add' vanished once the module was closed")
	}
	got, err := fn.Call(store, 2, 3)
	if err != nil {
		t.Fatalf("calling into an instance whose module was closed: %v.\n\n"+
			"If this fails, evicting from moduleLRU is NOT safe and the cache "+
			"must hold entries until their executions finish.", err)
	}
	if got.(int32) != 5 {
		t.Fatalf("got %v, want 5", got)
	}

	// NEGATIVE CONTROL, and the assertion above means nothing without it: a
	// Close() that silently did nothing produces an identical green, because
	// the instance would work either way. wasmtime-go panics once _ptr is nil,
	// so using the MODULE must fail while the INSTANCE keeps working.
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("using the module after Close() did not panic, so Close() " +
					"cannot be shown to have freed anything -- the survival above " +
					"is then consistent with Close() being a no-op and proves nothing")
			}
		}()
		_ = module.Exports()
	}()
}
