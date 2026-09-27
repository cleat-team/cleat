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
	c := newModuleLRU(max, 1<<40)
	for i := 0; i < max*5; i++ {
		c.store(fmt.Sprintf("key-%02d", i), &wasmtime.Module{}, 0)
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
	c := newModuleLRU(3, 1<<40)
	for _, k := range []string{"a", "b", "c"} {
		c.store(k, &wasmtime.Module{}, 0)
	}

	// Touch "a" so it is no longer the coldest, then overflow by one.
	if _, ok := c.load("a"); !ok {
		t.Fatal("a should be cached")
	}
	c.store("d", &wasmtime.Module{}, 0)

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

// The cache is bounded by ESTIMATED BYTES as well as by entry count.
//
// This is the bound cleat#1907 exists for. The entry count could not say what
// a hundred modules cost -- measured on this repo's artifacts, a hundred is
// 8.6 MB of AssemblyScript or 4.6 GB of Python -- so a deployment had no
// number to size against.
func TestCompiledModuleCacheIsBoundedInBytes(t *testing.T) {
	// Entry bound deliberately generous, so only the byte bound can bite.
	// Sizes derived from the multiplier rather than written as literals: a
	// change to it should move this test's arithmetic, not break it.
	const wasmLen = 1 << 20
	perEntry := CompiledSizeEstimate(wasmLen)
	const wantEntries = 3
	maxBytes := perEntry*wantEntries + perEntry/2 // room for 3, not 4
	c := newModuleLRU(1000, maxBytes)

	for i := 0; i < 20; i++ {
		c.store(fmt.Sprintf("key-%02d", i), &wasmtime.Module{}, wasmLen)
	}

	if got := c.estimatedBytes(); got > maxBytes {
		t.Errorf("cache holds an estimated %d bytes against a bound of %d", got, maxBytes)
	}
	if got := c.len(); got != wantEntries {
		t.Errorf("cache holds %d entries; want %d (a %d-byte bound at %d each). "+
			"The entry bound is 1000 here, so only the byte bound can be doing this.",
			got, wantEntries, maxBytes, perEntry)
	}
}

// One entry larger than the whole bound is KEPT, not evicted on insert.
//
// The alternative is a cache that compiles a large artifact, immediately drops
// it, and recompiles it on every call forever -- which is worse than holding
// it, and is invisible except as latency. The bound is a target for the SET.
func TestAnOversizedModuleIsHeldRatherThanThrashed(t *testing.T) {
	c := newModuleLRU(1000, 1<<20) // 1 MB bound
	c.store("huge", &wasmtime.Module{}, 10<<20)

	if got := c.len(); got != 1 {
		t.Fatalf("cache holds %d entries after storing one oversized module; want 1. "+
			"Evicting it on insert means recompiling it on every call.", got)
	}
	if _, ok := c.load("huge"); !ok {
		t.Error("the oversized module was not retrievable, so every call would recompile it")
	}
}

// A store with no source length contributes nothing to the byte bound, and is
// still bounded by the entry count.
//
// That is the honest answer for a caller that cannot supply a length: an
// invented figure would make the byte gauge a guess about a guess.
func TestAModuleWithNoSourceLengthIsStillBounded(t *testing.T) {
	c := newModuleLRU(3, 1<<40)
	for i := 0; i < 10; i++ {
		c.store(fmt.Sprintf("key-%02d", i), &wasmtime.Module{}, 0)
	}
	if got := c.estimatedBytes(); got != 0 {
		t.Errorf("estimated bytes = %d for entries with no source length; want 0", got)
	}
	if got := c.len(); got != 3 {
		t.Errorf("cache holds %d entries against an entry bound of 3", got)
	}
}
