//go:build cgo

package engine

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v48"
)

// The cache half of "a hold never outlives its code's cache entry": the module cache reports every entry it EVICTS, once, with no lock held, and not an entry it replaces.

func TestTheModuleCacheReportsEachEvictedKeyOnceWithNoLockHeld(t *testing.T) {
	c := newModuleLRU(2, 1<<40)
	var mu sync.Mutex
	var got []string
	reentered := make(chan struct{}, 8)
	c.setOnEvict(func(key string) {
		mu.Lock()
		got = append(got, key)
		mu.Unlock()
		// The hook calls back into the cache. If it were called under the cache's lock this would deadlock, and the bounded wait below turns that into a failure and not
		// a hang: this is the known-positive for "no lock held".
		c.load("k1")
		reentered <- struct{}{}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.store("k0", &wasmtime.Module{}, 0)
		c.store("k1", &wasmtime.Module{}, 0)
		c.store("k2", &wasmtime.Module{}, 0) // evicts k0, the least recently used
		c.store("k1", &wasmtime.Module{}, 0) // REPLACES k1: not an eviction
		c.drainHeld()                        // drains held artifacts: not an eviction
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the cache deadlocked: the eviction hook ran under the cache's lock")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "k0" {
		t.Fatalf("the hook was called with %v, want exactly [k0]: once for the one eviction, and not for a replacement or a drain", got)
	}
}

func TestAModuleCacheWithNoHookEvictsAsItAlwaysDid(t *testing.T) {
	c := newModuleLRU(1, 1<<40)
	c.store("k0", &wasmtime.Module{}, 0)
	c.store("k1", &wasmtime.Module{}, 0) // must not panic with no hook installed
	if c.len() != 1 {
		t.Fatalf("cache holds %d entries, want 1", c.len())
	}
}

// THE END-TO-END CASE the coordinator asked for: a real C# component is held through its sleep, the cache evicts that component while the run is held, and the run
// suspends. Without the eviction the same run completes in place (a_real_csharp_component_is_held_through_a_sleep_and_fenced_test.go).
func TestAHeldRunSuspendsWhenItsComponentIsEvictedFromTheCache(t *testing.T) {
	b := csharpFixtureStamped(t)
	langs := append(append([]string{}, WasmtimeLanguages...), "csharp")
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	// One entry, so storing anything else evicts the component.
	wt, err := NewWasmtimeBackend(ctx, WithWasmtimeModuleCacheMaxEntries(1))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	cfg := DefaultResidencyConfig()
	cfg.MaxRuns = 2
	set, err := NewResidentSet(cfg)
	if err != nil {
		t.Fatalf("NewResidentSet: %v", err)
	}
	// The worker's own wiring, not a hand-written equivalent. A recording hook is stacked in front of it so the row can also check WHICH key the cache reported.
	var evictedMu sync.Mutex
	var evicted []string
	if !set.AttachToCodeCache(wt) {
		t.Fatal("PRECONDITION FAILED: the wasmtime backend has no code cache to attach to")
	}
	inner := wt.moduleCache.onEvict
	wt.moduleCache.setOnEvict(func(key string) {
		evictedMu.Lock()
		evicted = append(evicted, codeIDOfCacheKey(key))
		evictedMu.Unlock()
		inner(key)
	})

	run := func(resident bool, evict bool) (heldRun, int) {
		caller := &timedCaller{mockCaller: &mockCaller{}}
		var checks int
		opts := []EngineOption{WithBackends(langs, wt), WithWorkflowID("wf-evict-held-component")}
		if resident {
			opts = append(opts, WithResidency(set.ForRun(ResidentRunInfo{
				WorkflowID: "wf-evict-held-component",
				Language:   "csharp",
				CodeKey:    CodeID(b),
				ClaimCheck: func(context.Context) error { checks++; return nil },
			})))
		}
		eng := NewEngine(rt, caller, opts...)
		finished := make(chan struct{})
		var res string
		var suspended bool
		var execErr error
		go func() {
			defer close(finished)
			r, _, susp, _, _, e := eng.Execute(ctx, b, "run", []byte(`{"mode":"sleep"}`))
			res, suspended, execErr = r, susp != nil, e
		}()
		if evict {
			// Wait until the run is held, then push the component out of the one-entry cache.
			deadline := time.Now().Add(10 * time.Second)
			for set.Len() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if set.Len() == 0 {
				t.Fatal("PRECONDITION FAILED: the run was never held, so evicting its component measured nothing")
			}
			wt.moduleCache.store("some-other-code", &wasmtime.Module{}, 0)
		}
		select {
		case <-finished:
		case <-time.After(60 * time.Second):
			t.Fatal("the execution never ended")
		}
		if execErr != nil {
			t.Fatalf("execute: %v", execErr)
		}
		return heldRun{result: res, suspended: suspended, ops: operationsCalled(caller.mockCaller)}, checks
	}

	run(false, false) // compiles and caches the component first, so there is an entry to evict
	evictedMu.Lock()
	evicted = nil // the warm-up's own insertions are not what is under test
	evictedMu.Unlock()

	// CONTROL: held, nothing evicts it: it finishes in place, both calls in one execution.
	ctl, _ := run(true, false)
	if ctl.suspended || len(ctl.ops) != 2 {
		t.Fatalf("PRECONDITION FAILED: the control (held, not evicted) did not complete in place: suspended=%v ops=%v, so the eviction row below could not be told from a run that was never held", ctl.suspended, ctl.ops)
	}

	// Seed the cache entry again for the evicted run (the control used it), then hold and evict.
	got, claimChecks := run(true, true)
	if !got.suspended || len(got.ops) != 1 {
		t.Fatalf("a held run whose component was evicted from the cache did not suspend: suspended=%v ops=%v result=%q. It kept running on code the cache had decided to free.", got.suspended, got.ops, got.result)
	}
	if claimChecks != 0 {
		t.Errorf("the claim was checked %d time(s): a run sent back by an eviction is a refusal, not a wake", claimChecks)
	}
	evictedMu.Lock()
	defer evictedMu.Unlock()
	want := CodeID(b)
	found := false
	for _, k := range evicted {
		if k == want {
			found = true
		}
	}
	if !found {
		t.Errorf("the cache reported %v as evicted, want the component's key %q among them", evicted, want)
	}
	if n := chargedBytes(set); n != 0 {
		t.Errorf("the set is still charged %d bytes after the run was sent back", n)
	}
}

// THE TEST THAT WOULD HAVE CAUGHT THE FIRST VERSION: both key spaces the cache uses map to the CodeID a held run carries. A core module is cached under its xxhash, a component
// under "component:<sha256>:<engine id>"; the engine id differs per backend and must not matter.
func TestEveryKeyTheCacheUsesMapsToTheCodeIDOfItsBytes(t *testing.T) {
	// A core module: any bytes that are not a component header.
	core := []byte("\x00asm\x01\x00\x00\x00 a core module, as far as the key is concerned")
	if isComponentWasm(core) {
		t.Fatal("PRECONDITION FAILED: the core fixture reads as a component")
	}
	if got, want := codeIDOfCacheKey(moduleCacheKey(core)), CodeID(core); got != want {
		t.Errorf("a core module cached under %q maps to %q, want its CodeID %q", moduleCacheKey(core), got, want)
	}
	// A component: the version bytes isComponentWasm looks for.
	comp := []byte("\x00asm\x0d\x00\x01\x00 a component, as far as the key is concerned")
	if !isComponentWasm(comp) {
		t.Fatal("PRECONDITION FAILED: the component fixture does not read as a component")
	}
	for _, engineID := range []uint64{1, 2, 9999} {
		cc := &componentCache{engineID: engineID}
		if got, want := codeIDOfCacheKey(cc.key(comp)), CodeID(comp); got != want {
			t.Errorf("a component cached by engine %d under %q maps to %q, want its CodeID %q", engineID, cc.key(comp), got, want)
		}
	}
	// And the two spaces never collide: different bytes, different identity, and a module is never mistaken for a component.
	if CodeID(core) == CodeID(comp) || CodeID(core)[:7] != "module:" || CodeID(comp)[:10] != "component:" {
		t.Errorf("the identities are not distinct: core %q, component %q", CodeID(core), CodeID(comp))
	}
}

// ---- THE RACE EvictCode ALONE LEAVES OPEN, on the real backend (cleat-review3's finding on 5c364e9d) ----
//
// EvictCode reaches only runs that are already held. A run whose code is evicted while it is still EXECUTING (another run's insert into a full cache) reaches its sleep
// afterwards, is admitted, and is held against code that is gone. So admission also asks the cache (ResidentSet.codeCached, backend CodeCached). These rows evict from inside
// a host call, which is the guest being mid-execution, for BOTH key spaces, because the last time the key spaces were taken on trust every unit row agreed and the real case
// was wrong. Each has a control proving the check does not refuse a hold that is legitimate, since a check that refuses too much is an outage and not a safety measure.

// evictingCaller calls evict once, on its first host call, then behaves as inner.
type evictingCaller struct {
	inner ServiceCaller
	once  sync.Once
	evict func()
}

func (c *evictingCaller) Call(ctx context.Context, service, op, req string) (string, error) {
	c.once.Do(c.evict)
	return c.inner.Call(ctx, service, op, req)
}

func TestCodeCachedAnswersForBothKeySpacesOfTheRealBackend(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx, WithWasmtimeModuleCacheMaxEntries(1))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	core, err := os.ReadFile(buildFixtureWasm(t, "residentsleep"))
	if err != nil {
		t.Fatalf("reading the core fixture: %v", err)
	}
	comp := csharpFixtureStamped(t)
	langs := append(append([]string{}, WasmtimeLanguages...), "csharp")

	for _, tc := range []struct {
		name  string
		bytes []byte
		entry string
		input string
	}{
		{"a core module", core, "work_sleep_work", `{}`},
		{"a component", comp, "run", `{"mode":"hello"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := CodeID(tc.bytes)
			if wt.CodeCached(id) {
				t.Fatalf("PRECONDITION FAILED: %s reads as cached before anything ran, so a true answer below proves nothing", tc.name)
			}
			eng := NewEngine(rt, &mockCaller{}, WithBackends(langs, wt), WithWorkflowID("wf-codecached"))
			if _, _, _, _, _, err := eng.Execute(ctx, tc.bytes, tc.entry, []byte(tc.input)); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if !wt.CodeCached(id) {
				t.Errorf("%s ran and was cached, and CodeCached says it is not: every hold of it would be refused", tc.name)
			}
			wt.moduleCache.store("some-other-code", &wasmtime.Module{}, 0) // a one-entry cache: this evicts it
			if wt.CodeCached(id) {
				t.Errorf("%s was evicted, and CodeCached still says it is cached", tc.name)
			}
		})
	}
	if !wt.CodeCached("not-an-identity-it-knows") {
		t.Error("an identity the backend cannot judge must read as cached (fail open)")
	}
}

func TestARunWhoseCoreModuleIsEvictedWhileItExecutesIsNotHeldAtItsSleep(t *testing.T) {
	ctx := context.Background()
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "residentsleep"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx, WithWasmtimeModuleCacheMaxEntries(1))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	run := func(evictMidRun bool) (suspended bool, parks, codeRefusals int) {
		m := newReplayMetrics(t)
		cfg := DefaultResidencyConfig()
		cfg.MaxRuns = 2
		cfg.Metrics = m
		set, err := NewResidentSet(cfg)
		if err != nil {
			t.Fatalf("NewResidentSet: %v", err)
		}
		if !set.AttachToCodeCache(wt) { // the worker's own wiring
			t.Fatal("PRECONDITION FAILED: the wasmtime backend has no code cache to attach to")
		}
		var caller ServiceCaller = &phasedWorkCaller{}
		if evictMidRun {
			caller = &evictingCaller{inner: caller, evict: func() { wt.moduleCache.store("some-other-code", &wasmtime.Module{}, 0) }}
		}
		eng := NewEngine(rt, caller, WithBackends(WasmtimeLanguages, wt), WithWorkflowID("wf-core-evicted-midrun"),
			WithResidency(set.ForRun(ResidentRunInfo{WorkflowID: "wf-core-evicted-midrun", Language: "go", CodeKey: CodeID(wasmBytes), ClaimCheck: claimStillHeld})))
		eng.Metrics = m
		res, _, susp, _, _, execErr := eng.Execute(ctx, wasmBytes, "work_sleep_work", []byte(`{}`))
		if execErr != nil {
			t.Fatalf("execute: %v (res %q)", execErr, res)
		}
		return susp != nil, residentCounter(t, m, "cleat_resident_parks_total"), residentCounter(t, m, "cleat_resident_refusals_total", `reason="code"`)
	}

	run(false) // compiles and caches the module first
	// CONTROL: the check is active (attached) and nothing evicts the code: the hold must still happen, or the check refuses legitimate holds.
	if susp, parks, refusals := run(false); susp || parks != 1 || refusals != 0 {
		t.Fatalf("CONTROL FAILED: a core run with its code cached, with the check attached, was suspended=%v parks=%d codeRefusals=%d, want completed in place with 1 hold and 0 refusals", susp, parks, refusals)
	}
	// THE RACE: the code is evicted from inside the first host call, while the guest is executing, before it reaches its sleep.
	susp, parks, refusals := run(true)
	if !susp || parks != 0 || refusals != 1 {
		t.Errorf("a run whose code was evicted while it executed: suspended=%v parks=%d codeRefusals=%d, want suspended with 0 holds and 1 refusal (reason code): it was held against code that is gone", susp, parks, refusals)
	}
}

func TestARunWhoseComponentIsEvictedWhileItExecutesIsNotHeldAtItsSleep(t *testing.T) {
	ctx := context.Background()
	b := csharpFixtureStamped(t)
	langs := append(append([]string{}, WasmtimeLanguages...), "csharp")
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx, WithWasmtimeModuleCacheMaxEntries(1))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	run := func(evictMidRun bool) (suspended bool, ops []string, codeRefusals int) {
		m := newReplayMetrics(t)
		cfg := DefaultResidencyConfig()
		cfg.MaxRuns = 2
		cfg.Metrics = m
		set, err := NewResidentSet(cfg)
		if err != nil {
			t.Fatalf("NewResidentSet: %v", err)
		}
		if !set.AttachToCodeCache(wt) {
			t.Fatal("PRECONDITION FAILED: the wasmtime backend has no code cache to attach to")
		}
		mc := &mockCaller{}
		var caller ServiceCaller = mc
		if evictMidRun {
			caller = &evictingCaller{inner: mc, evict: func() { wt.moduleCache.store("some-other-code", &wasmtime.Module{}, 0) }}
		}
		eng := NewEngine(rt, caller, WithBackends(langs, wt), WithWorkflowID("wf-component-evicted-midrun"),
			WithResidency(set.ForRun(ResidentRunInfo{WorkflowID: "wf-component-evicted-midrun", Language: "csharp", CodeKey: CodeID(b), ClaimCheck: claimStillHeld})))
		eng.Metrics = m
		_, _, susp, _, _, execErr := eng.Execute(ctx, b, "run", []byte(`{"mode":"sleep"}`))
		if execErr != nil {
			t.Fatalf("execute: %v", execErr)
		}
		return susp != nil, operationsCalled(mc), residentCounter(t, m, "cleat_resident_refusals_total", `reason="code"`)
	}

	run(false) // compiles and caches the component first
	if susp, ops, refusals := run(false); susp || len(ops) != 2 || refusals != 0 {
		t.Fatalf("CONTROL FAILED: a component with its code cached, with the check attached, was suspended=%v ops=%v codeRefusals=%d, want completed in place (2 calls) and 0 refusals", susp, ops, refusals)
	}
	susp, ops, refusals := run(true)
	if !susp || len(ops) != 1 || refusals != 1 {
		t.Errorf("a component evicted while it executed: suspended=%v ops=%v codeRefusals=%d, want suspended after 1 call with 1 refusal (reason code): it was held against code that is gone", susp, ops, refusals)
	}
}
