//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v48"
)

// cleat#3748. ExecuteComponentCGo compiled the component on every Execute and
// every Replay and deleted it on return, so every resume of a Python workflow
// paid a full compile of its guest (1175 to 1334 ms for the 19.3 MB component on
// an M4 that was not idle).
//
// These assert on a COUNT of compilations, not on a duration. "The second call is
// faster" is true of a warm file cache and a quiet machine and says nothing about
// whether anything was kept; "the second call did not compile" cannot be
// satisfied by luck.
//
// Every artifact here is a few hundred bytes. That is deliberate and it makes the
// bounds testable: a component's cost against the byte bound is
// CompiledSizeEstimate over its wasm, so a bound of a few kilobytes holds a
// known number of them, which a 19 MB guest could not.

// distinctComponent returns the same probe component as componentWithNoImports
// with one constant changed, so each n is a different artifact -- a different
// SHA-256, a different cache key -- that still instantiates and runs.
func distinctComponent(t *testing.T, n int) []byte {
	t.Helper()
	const marker = "(i32.const 4096)"
	if strings.Count(componentWithNoImports, marker) != 1 {
		t.Fatalf("componentWithNoImports no longer contains %q exactly once, so distinctComponent "+
			"cannot vary it; the probe changed under this test", marker)
	}
	wat := strings.Replace(componentWithNoImports, marker, fmt.Sprintf("(i32.const %d)", 4096+8*n), 1)
	b, err := wasmtime.Wat2Wasm(wat)
	if err != nil {
		t.Fatalf("Wat2Wasm(variant %d): %v", n, err)
	}
	if !isComponentWasm(b) {
		t.Fatalf("variant %d is not a Component Model binary, so it would never reach the component path", n)
	}
	return b
}

func newComponentBackend(t *testing.T, opts ...WasmtimeOption) *wasmtimeBackend {
	t.Helper()
	b, err := NewWasmtimeBackend(context.Background(), opts...)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	return b
}

// runComponent executes wasm the way production does: through PerExecution, which
// is the copy that actually runs a workflow and the one that has to carry the
// cache.
func runComponent(t *testing.T, b *wasmtimeBackend, wasm []byte) {
	t.Helper()
	res, err := b.PerExecution(0).Execute(context.Background(), wasm, "run", json.RawMessage(`"x"`), &mockHostHandler{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res == nil {
		t.Fatal("Execute returned no result and no error")
	}
}

// compilesOf reads the cache's counter. A backend with no cache reports -1, so a
// test that expected a cache cannot pass by finding none.
func compilesOf(b *wasmtimeBackend) int64 {
	if b.components == nil {
		return -1
	}
	return b.components.compiles.Load()
}

func TestAComponentIsCompiledOnceAcrossExecutions(t *testing.T) {
	b := newComponentBackend(t)
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	wasm := distinctComponent(t, 0)

	for i := 0; i < 5; i++ {
		runComponent(t, b, wasm)
	}
	// Five executions, each through its own PerExecution copy. A copy that dropped
	// the cache field would leave this at -1 or 0; a cache that kept nothing would
	// leave it at 5.
	if got := compilesOf(b); got != 1 {
		t.Fatalf("5 executions of one component compiled it %d time(s), want 1 "+
			"(-1 means the backend has no component cache at all)", got)
	}
}

// The control for the test above: it must be able to say "compiled again". Without
// it, "1 compile" could be a counter that never counts, and every assertion in this
// file would be satisfied by a counter that is stuck at one.
func TestADifferentComponentIsCompiledSeparately(t *testing.T) {
	b := newComponentBackend(t)
	t.Cleanup(func() { _ = b.Close(context.Background()) })

	runComponent(t, b, distinctComponent(t, 0))
	runComponent(t, b, distinctComponent(t, 1))
	runComponent(t, b, distinctComponent(t, 0)) // a hit
	runComponent(t, b, distinctComponent(t, 1)) // a hit

	if got := compilesOf(b); got != 2 {
		t.Fatalf("two distinct components compiled %d time(s), want 2", got)
	}
}

// The compile is serialised per key. Without that, N workflows resuming at the same
// moment on a cold cache would each pay the full compile.
func TestConcurrentFirstCallsOfOneComponentCompileItOnce(t *testing.T) {
	b := newComponentBackend(t)
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	wasm := distinctComponent(t, 0)

	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := b.PerExecution(0).Execute(context.Background(), wasm, "run", json.RawMessage(`"x"`), &mockHostHandler{})
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Execute: %v", err)
		}
	}
	if got := compilesOf(b); got != 1 {
		t.Fatalf("%d concurrent first calls compiled the component %d time(s), want 1", n, got)
	}
}

// The byte bound, and a run after eviction. A holds are sized so exactly two fit:
// running A, B, C evicts A, and running A again must work and must compile again.
func TestComponentsAreEvictedByTheByteBoundAndStillRunAfterwards(t *testing.T) {
	a, bb, c := distinctComponent(t, 0), distinctComponent(t, 1), distinctComponent(t, 2)
	maxLen := len(a)
	for _, w := range [][]byte{bb, c} {
		if len(w) > maxLen {
			maxLen = len(w)
		}
	}
	// Room for two estimates, not three.
	bound := CompiledSizeEstimate(maxLen)*2 + CompiledSizeEstimate(maxLen)/2
	b := newComponentBackend(t, WithWasmtimeModuleCacheMaxBytes(bound))
	t.Cleanup(func() { _ = b.Close(context.Background()) })

	runComponent(t, b, a)
	runComponent(t, b, bb)
	runComponent(t, b, c)

	if got := b.moduleCache.len(); got != 2 {
		t.Fatalf("cache holds %d entries after three components against a bound sized for two, want 2", got)
	}
	if got := b.moduleCache.estimatedBytes(); got > bound {
		t.Fatalf("cache estimates %d bytes, over its %d byte bound", got, bound)
	}
	if got := compilesOf(b); got != 3 {
		t.Fatalf("three distinct components compiled %d time(s), want 3", got)
	}

	// A was least recently used, so it is the one that went.
	runComponent(t, b, a)
	if got := compilesOf(b); got != 4 {
		t.Fatalf("after A was evicted, running it again left %d compiles, want 4: it must be "+
			"recompiled, and it must still run", got)
	}
	// C survived the whole sequence and is still a hit.
	runComponent(t, b, c)
	if got := compilesOf(b); got != 4 {
		t.Fatalf("C was resident and was recompiled anyway: %d compiles, want 4", got)
	}
}

func TestComponentsAreEvictedByTheEntryBound(t *testing.T) {
	b := newComponentBackend(t, WithWasmtimeModuleCacheMaxEntries(2))
	t.Cleanup(func() { _ = b.Close(context.Background()) })

	for i := 0; i < 5; i++ {
		runComponent(t, b, distinctComponent(t, i))
	}
	if got := b.moduleCache.len(); got != 2 {
		t.Fatalf("cache holds %d entries after five components against a bound of 2", got)
	}
}

// Components and modules share one budget, so a deployment sets one limit and gets
// what it set.
func TestComponentsAndModulesShareOneBudget(t *testing.T) {
	b := newComponentBackend(t, WithWasmtimeModuleCacheMaxEntries(2))
	t.Cleanup(func() { _ = b.Close(context.Background()) })

	mod, err := wasmtime.NewModule(b.engine, []byte("\x00asm\x01\x00\x00\x00"))
	if err != nil {
		t.Fatalf("NewModule: %v", err)
	}
	b.moduleCache.store("m:one", mod, 8)
	runComponent(t, b, distinctComponent(t, 0))
	runComponent(t, b, distinctComponent(t, 1))

	if got := b.moduleCache.len(); got != 2 {
		t.Fatalf("one module and two components against an entry bound of 2 left %d entries: "+
			"the two kinds are not counted together", got)
	}
}

// What stops an in-flight execution being freed under itself. Executions of four
// components run concurrently while a bound of one entry evicts and recompiles
// constantly, so a component is routinely released while another goroutine is
// executing the very artifact it was released from. Under -race this is the
// refcount ownership being exercised, not asserted: a release that freed code in
// use would crash or trap here.
func TestAnEvictedComponentIsNotFreedUnderARunningExecution(t *testing.T) {
	b := newComponentBackend(t, WithWasmtimeModuleCacheMaxEntries(1))
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	comps := make([][]byte, 4)
	for i := range comps {
		comps[i] = distinctComponent(t, i)
	}

	const goroutines, iterations = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*iterations)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				w := comps[(g+i)%len(comps)]
				if _, err := b.PerExecution(0).Execute(context.Background(), w, "run", json.RawMessage(`"x"`), &mockHostHandler{}); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Execute under constant eviction: %v", err)
	}
	if got := b.moduleCache.len(); got > 1 {
		t.Fatalf("cache holds %d entries against a bound of 1", got)
	}
	// The churn is the point: with four artifacts and room for one, nearly every call
	// is a miss, so a count this low would mean the bound was not being applied.
	if got := compilesOf(b); got < int64(len(comps)) {
		t.Fatalf("only %d compiles for %d artifacts under a one-entry bound", got, len(comps))
	}
}

// The key is bound to the engine. Two backends share ONE LRU here, which production
// does not do, so the test can ask what the key is for: if the engine were not in
// it, the second engine would be handed the first one's compiled component, which
// is a different engine's code.
func TestTwoEnginesDoNotShareACompiledComponent(t *testing.T) {
	b1 := newComponentBackend(t)
	b2 := newComponentBackend(t)
	t.Cleanup(func() {
		_ = b2.Close(context.Background())
		_ = b1.Close(context.Background())
	})
	b2.moduleCache = b1.moduleCache
	b2.components = newComponentCache(b1.moduleCache)
	wasm := distinctComponent(t, 0)

	runComponent(t, b1, wasm)
	runComponent(t, b2, wasm)
	runComponent(t, b1, wasm)
	runComponent(t, b2, wasm)

	if got := compilesOf(b1); got != 1 {
		t.Fatalf("engine 1 compiled %d time(s), want 1", got)
	}
	if got := compilesOf(b2); got != 1 {
		t.Fatalf("engine 2 compiled %d time(s), want 1: it either reused engine 1's artifact (0) "+
			"or did not keep its own (2)", got)
	}
	if got := b1.moduleCache.len(); got != 2 {
		t.Fatalf("shared cache holds %d entries, want one per engine (2)", got)
	}
}

func TestTheComponentKeyCoversTheBytesAndTheEngine(t *testing.T) {
	lru := newModuleLRU(10, 1<<30)
	c1, c2 := newComponentCache(lru), newComponentCache(lru)
	a, b := []byte("aaaa"), []byte("aaab")

	// A copy with its own backing array: the key follows the CONTENT, not the slice.
	first, second := c1.key(a), c1.key(append([]byte(nil), a...))
	if first != second {
		t.Fatal("the key is not stable for the same bytes on the same engine")
	}
	if c1.key(a) == c1.key(b) {
		t.Fatal("two different byte strings share a key")
	}
	if c1.key(a) == c2.key(a) {
		t.Fatal("the same bytes on two engines share a key: a cache shared between them would " +
			"hand one engine the other's compiled code")
	}
	if !strings.HasPrefix(c1.key(a), "component:") {
		t.Fatalf("key %q has no component prefix, so it could collide with a module's key", c1.key(a))
	}
}

// Close must give back what the cache holds. A module is reclaimed by the garbage
// collector; a held component is a C allocation that nothing else frees.
func TestClosingTheBackendReleasesEveryCachedComponent(t *testing.T) {
	b := newComponentBackend(t)
	runComponent(t, b, distinctComponent(t, 0))
	runComponent(t, b, distinctComponent(t, 1))
	if got := b.moduleCache.len(); got != 2 {
		t.Fatalf("precondition: cache holds %d entries, want 2; nothing below was measured", got)
	}

	if err := b.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := b.moduleCache.len(); got != 0 {
		t.Fatalf("cache still holds %d entries after Close", got)
	}
	if got := b.moduleCache.estimatedBytes(); got != 0 {
		t.Fatalf("cache still estimates %d bytes after Close", got)
	}
}

// A backend assembled without the cache degrades to the old cost, not to an error.
func TestABackendWithNoComponentCacheStillRunsComponents(t *testing.T) {
	b := newComponentBackend(t)
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	b.components = nil
	runComponent(t, b, distinctComponent(t, 0))
	runComponent(t, b, distinctComponent(t, 0))
	if got := compilesOf(b); got != -1 {
		t.Fatalf("compilesOf reported %d for a backend with no cache, want -1", got)
	}
}

// The same property on the REAL artifact: the checked-in 19,300,914-byte
// componentize-py build the issue is about, not a 200-byte probe. It needs no
// Python toolchain, because it is already built; what it does need is to reach the
// compile, which it does before the link failure its stale imports end in.
//
// The call is EXPECTED to fail. call_all_plugins.wasm is stale against the host's
// current imports (cleat#3748 measured 1.24 s then 1.05 s for a call that fails at
// link time), so this asserts on the compile count and on nothing the guest does.
// That is the point: the cost being removed is the compile, and it is paid before
// anything the guest could do.
//
// The two durations are logged, not asserted. They are a measurement on whatever
// machine runs this, not a property of the code.
func TestTheCheckedInPythonComponentIsCompiledOnce(t *testing.T) {
	wasm, err := os.ReadFile("../tests/plugin-harness/testdata/pythonworkflow/call_all_plugins.wasm")
	if err != nil {
		t.Fatalf("the checked-in Python component is missing: %v", err)
	}
	if !isComponentWasm(wasm) {
		t.Fatal("call_all_plugins.wasm is not a Component Model binary, so this would not reach the component path")
	}
	b := newComponentBackend(t)
	t.Cleanup(func() { _ = b.Close(context.Background()) })

	var took [3]time.Duration
	for i := range took {
		start := time.Now()
		_, _ = b.PerExecution(0).Execute(context.Background(), wasm, "run", json.RawMessage(`{}`), &mockHostHandler{})
		took[i] = time.Since(start)
	}
	t.Logf("three calls on the %d-byte Python component: %v, %v, %v", len(wasm), took[0], took[1], took[2])

	if got := compilesOf(b); got != 1 {
		t.Fatalf("three calls on the Python component compiled it %d time(s), want 1", got)
	}
	// It is resident, and costed as a Python component is: 4x the wasm, 77 MB, which
	// is inside the default 512 MB bound with room for others.
	if got, want := b.moduleCache.estimatedBytes(), CompiledSizeEstimate(len(wasm)); got != want {
		t.Fatalf("cache estimates %d bytes for the Python component, want %d", got, want)
	}
	if b.moduleCache.estimatedBytes() > DefaultModuleCacheMaxBytes {
		t.Fatalf("one Python component alone exceeds the default byte bound (%d > %d)",
			b.moduleCache.estimatedBytes(), DefaultModuleCacheMaxBytes)
	}
}

// A cache HIT must still reach configureStore. The compile used to run before
// configureStore and a hit skips the compile, so a cache that returned early, or
// that moved the lookup after the deadline check, would start a guest on a ctx
// that had already expired. TestComponentPathWithASpentDeadlineIsNotAOneMillisecondTrap
// does not cover this: it runs against an empty cache, so it only ever takes the
// compile path.
//
// The guest never returns, so a hit that skipped the check would not fail
// quickly: it would spin until the instance timeout. The call is therefore run
// in a goroutine and bounded, so a regression reads as a named failure and not a
// hang.
func TestACacheHitStillRefusesAnAlreadySpentDeadline(t *testing.T) {
	b := newDeadlineTestBackend(t)
	wasm := spinComponent(t)

	// Warm the cache: a live ctx, so the guest starts, is stopped by the fence, and
	// the compiled component is left behind.
	warm, cancelWarm := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancelWarm()
	if _, err := b.ExecuteComponentCGo(warm, wasm, "run", []byte(`{}`), OutBufSize); err == nil {
		t.Fatal("a guest that never returns completed while warming the cache")
	}
	if got := compilesOf(b); got != 1 {
		t.Fatalf("warming compiled %d time(s), want 1", got)
	}

	spent, cancelSpent := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelSpent()
	done := make(chan error, 1)
	go func() {
		_, err := b.ExecuteComponentCGo(spent, wasm, "run", []byte(`{}`), OutBufSize)
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a cache hit under a spent deadline did not return: it started the guest instead of refusing")
	}
	if !errors.Is(err, errDeadlineBeforeGuest) {
		t.Fatalf("a cache hit under a spent deadline returned %v, want errDeadlineBeforeGuest", err)
	}
	// It really was a hit, so the test is about the path it claims to be.
	if got := compilesOf(b); got != 1 {
		t.Fatalf("the spent-deadline call compiled again (%d compiles total): it was not a cache hit", got)
	}
}
