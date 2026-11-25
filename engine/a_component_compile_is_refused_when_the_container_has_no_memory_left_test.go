//go:build cgo

package engine

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A compile in THIS process keeps for good 198 to 308 MiB for each of the first four further distinct Python components and 44 to 76 for each of the next three (mean 175), so serialising compiles is not enough: at a 2 GiB limit a worker without headroom checks survived
// six distinct components and was killed on the seventh (measured 2026-11-23, arms 2 and 4). compileHeadroom refuses instead, and the refusal must (1) not compile, (2) release the run the way a
// killed child does, (3) not read the memory again for the same component inside the window, and (4) not refuse when the memory cannot be read.

// A component of Python's size (the measured ones are 17.6 to 28.7 MiB of wasm) and of a small C# workflow's (1.0 to 2.8 MiB). Only the LENGTH matters to the headroom check.
var (
	pythonSizedWasm = make([]byte, 20<<20)
	csharpSizedWasm = make([]byte, 1<<20)
)

type fakeHeadroom struct {
	avail atomic.Uint64
	ok    atomic.Bool
	reads atomic.Int64
	now   atomic.Int64 // unix nanos
}

func newFakeHeadroom(avail uint64) (*fakeHeadroom, *compileHeadroom) {
	f := &fakeHeadroom{}
	f.avail.Store(avail)
	f.ok.Store(true)
	f.now.Store(time.Unix(1_700_000_000, 0).UnixNano())
	h := &compileHeadroom{
		read:   func() (uint64, bool) { f.reads.Add(1); return f.avail.Load(), f.ok.Load() },
		need:   800 << 20,
		window: 5 * time.Minute,
		limit:  2 << 30,
		now:    func() time.Time { return time.Unix(0, f.now.Load()) },
		until:  map[string]time.Time{},
	}
	return f, h
}

func TestAnInProcessComponentCompileIsRefusedWhenTheContainerHasNoMemoryLeft(t *testing.T) {
	restore, _, calls := countingCompile(t)
	defer restore()
	f, h := newFakeHeadroom(300 << 20)
	b := &wasmtimeBackend{componentCompileSem: make(chan struct{}, 1), compileHeadroom: h}

	_, inProc, err := b.compileComponentMiss("py-a", pythonSizedWasm)
	var ref *ComponentCompileRefusedError
	if !errors.As(err, &ref) || !errors.Is(err, ErrComponentCompileRefused) {
		t.Fatalf("want a ComponentCompileRefusedError, got %v", err)
	}
	if ref.Reason != ComponentCompileRefusedLowHeadroom || ref.ReasonLabel() != "low_headroom" {
		t.Errorf("reason %q, label %q; want low_headroom", ref.Reason, ref.ReasonLabel())
	}
	if ref.HeadroomBytes != 300<<20 || ref.NeededBytes != 800<<20 || ref.RetryAfter != 5*time.Minute {
		t.Errorf("headroom %d MiB, needed %d MiB, retry %s; want 300, 800, 5m", ref.HeadroomBytes>>20, ref.NeededBytes>>20, ref.RetryAfter)
	}
	if inProc || calls.Load() != 0 {
		t.Fatalf("the compile ran (%d calls) although the container had 300 MiB left of the 800 it needs", calls.Load())
	}

	// The same component inside the window is answered without reading the memory again and without compiling.
	reads := f.reads.Load()
	_, _, err = b.compileComponentMiss("py-a", pythonSizedWasm)
	if !errors.As(err, &ref) || !ref.Repeat {
		t.Fatalf("the second answer was %v; want a repeat refusal", err)
	}
	if f.reads.Load() != reads || calls.Load() != 0 {
		t.Errorf("the repeat read the memory %d more times and compiled %d times; want 0 and 0", f.reads.Load()-reads, calls.Load())
	}

	// A DIFFERENT component is judged on its own reading: room has come back, so it compiles.
	f.avail.Store(1500 << 20)
	if _, inProc, _ = b.compileComponentMiss("py-b", pythonSizedWasm); !inProc || calls.Load() != 1 {
		t.Errorf("a different component with 1500 MiB left did not compile (in-process %v, calls %d)", inProc, calls.Load())
	}

	// And the first one is tried again once its window has passed.
	f.now.Add(int64(6 * time.Minute))
	if _, _, _ = b.compileComponentMiss("py-a", pythonSizedWasm); calls.Load() != 2 {
		t.Errorf("py-a was not tried again after its window (calls %d)", calls.Load())
	}
}

func TestAnUnreadableMemoryReadingDoesNotRefuseACompile(t *testing.T) {
	restore, _, calls := countingCompile(t)
	defer restore()
	f, h := newFakeHeadroom(0)
	f.ok.Store(false)
	b := &wasmtimeBackend{componentCompileSem: make(chan struct{}, 1), compileHeadroom: h}
	if _, inProc, _ := b.compileComponentMiss("py-a", []byte{0}); !inProc || calls.Load() != 1 {
		t.Fatalf("an unreadable reading refused or skipped the compile (in-process %v, calls %d): with no basis to refuse the worker must do what it did before", inProc, calls.Load())
	}
	if !h.unreadable.Load() {
		t.Error("the warning for an unreadable reading was never given")
	}
}

// The fallback after a child that failed for an ordinary reason is the other in-process path: it must refuse too, not compile. /usr/bin/false exits 1, which is a failure and not a SIGKILL.
func TestTheFallbackAfterAFailedChildIsRefusedForWantOfMemoryNotCompiled(t *testing.T) {
	restore, _, calls := countingCompile(t)
	defer restore()
	p, err := newComponentPrecompiler(ComponentPrecompile{Executable: "/usr/bin/false"}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	var hooked atomic.Int64
	p.onFallback = func(string) { hooked.Add(1) }
	_, h := newFakeHeadroom(100 << 20)
	b := &wasmtimeBackend{precompiler: p, componentCompileSem: make(chan struct{}, 1), compileHeadroom: h}

	_, _, err = b.compileComponentMiss("py-a", []byte{0})
	var ref *ComponentCompileRefusedError
	if !errors.As(err, &ref) || ref.Reason != ComponentCompileRefusedLowHeadroom {
		t.Fatalf("the fallback with 100 MiB left returned %v; want a low_headroom refusal", err)
	}
	if calls.Load() != 0 {
		t.Errorf("the worker compiled in-process %d times with 100 MiB left", calls.Load())
	}
	if p.fallbacks.Load() != 1 || hooked.Load() != 1 {
		t.Errorf("the fallback was counted %d times and hooked %d times; want 1 and 1, since the child did fail", p.fallbacks.Load(), hooked.Load())
	}
}

// A refusal must not be sticky when the memory returns: after the window the component is judged again on a fresh reading, and compiles if the reading is above what it needs; and while the memory is
// STILL short it is refused again, on a new reading and not on the old answer (cleat-review3, condition C). Says what an operator is told: the refusal usually lasts until a restart or a larger limit.
func TestARefusalClearsAfterItsWindowWhenTheMemoryReturnsAndIsRepeatedWhenItDoesNot(t *testing.T) {
	restore, _, calls := countingCompile(t)
	defer restore()
	f, h := newFakeHeadroom(200 << 20)
	b := &wasmtimeBackend{componentCompileSem: make(chan struct{}, 1), compileHeadroom: h}

	if _, _, err := b.compileComponentMiss("py-a", pythonSizedWasm); !errors.Is(err, ErrComponentCompileRefused) {
		t.Fatalf("with 200 MiB left the first compile returned %v; want a refusal", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("compiled %d times while refused", calls.Load())
	}

	// The window passes and the memory is STILL short: refused again, on a NEW reading (the old one would have said 200).
	f.now.Add(int64(6 * time.Minute))
	f.avail.Store(500 << 20)
	reads := f.reads.Load()
	_, _, err := b.compileComponentMiss("py-a", pythonSizedWasm)
	var ref *ComponentCompileRefusedError
	if !errors.As(err, &ref) || ref.Repeat || ref.HeadroomBytes != 500<<20 {
		t.Fatalf("after the window with 500 MiB left got %v; want a fresh refusal naming 500 MiB", err)
	}
	if f.reads.Load() != reads+1 || calls.Load() != 0 {
		t.Errorf("reads +%d, compiles %d; want exactly one new reading and no compile", f.reads.Load()-reads, calls.Load())
	}
	if msg := err.Error(); !strings.Contains(msg, "RESTARTED") || !strings.Contains(msg, "RAISED") {
		t.Errorf("the refusal does not say that it lasts until a restart or a larger limit, and what to do about it: %q", msg)
	}

	// The window passes again and the memory has returned (a restart, a larger limit): it compiles.
	f.now.Add(int64(6 * time.Minute))
	f.avail.Store(1500 << 20)
	if _, inProc, err := b.compileComponentMiss("py-a", pythonSizedWasm); err != nil && !strings.Contains(err.Error(), "fake compile") || !inProc || calls.Load() != 1 {
		t.Errorf("after the window with 1500 MiB left: err %v, in-process %v, compiles %d; want it to compile once", err, inProc, calls.Load())
	}
}

// The need is PER COMPONENT, not one figure for every language: a small C# workflow was measured at +11 to +118 MiB to compile in the worker and a Python component at about 500 MiB of scratch
// plus up to about 300 kept (198 to 308 for each of the first four further components, then 44 to 76) (cleat-review3 condition B, cleat-coordinator ruling). At 300 MiB left a C# component compiles where a Python one is refused; at 200 MiB the C# one is refused too,
// by the floor, so the floor is a limit and not the absence of one.
func TestWhatACompileNeedsDependsOnTheSizeOfTheComponent(t *testing.T) {
	restore, _, calls := countingCompile(t)
	defer restore()
	f, h := newFakeHeadroom(300 << 20)
	b := &wasmtimeBackend{componentCompileSem: make(chan struct{}, 1), compileHeadroom: h}

	if got := h.needFor(len(pythonSizedWasm)); got != 800<<20 {
		t.Errorf("a 20 MiB component needs %d MiB; want the maximum, 800", got>>20)
	}
	if got := h.needFor(len(csharpSizedWasm)); got != compileHeadroomFloor {
		t.Errorf("a 1 MiB component needs %d MiB; want the floor, %d", got>>20, compileHeadroomFloor>>20)
	}

	if _, _, err := b.compileComponentMiss("py", pythonSizedWasm); !errors.Is(err, ErrComponentCompileRefused) {
		t.Fatalf("a Python-sized component with 300 MiB left returned %v; want a refusal", err)
	}
	if _, inProc, _ := b.compileComponentMiss("cs", csharpSizedWasm); !inProc || calls.Load() != 1 {
		t.Fatalf("a C#-sized component with 300 MiB left did not compile (in-process %v, calls %d): one 800 MiB figure for every language refuses what develop compiled", inProc, calls.Load())
	}

	f.avail.Store(200 << 20)
	_, _, err := b.compileComponentMiss("cs-2", csharpSizedWasm)
	var ref *ComponentCompileRefusedError
	if !errors.As(err, &ref) || ref.NeededBytes != compileHeadroomFloor || ref.HeadroomBytes != 200<<20 {
		t.Fatalf("a C#-sized component with 200 MiB left returned %v; want a refusal naming the %d MiB floor", err, compileHeadroomFloor>>20)
	}

	// A maximum below the floor is the floor: an operator who sets the flag to 100 MiB gets a 100 MiB need for everything, not the 256 MiB floor.
	h2 := &compileHeadroom{need: 100 << 20}
	if got := h2.needFor(len(csharpSizedWasm)); got != 100<<20 {
		t.Errorf("with a 100 MiB maximum a 1 MiB component needs %d MiB; want 100", got>>20)
	}
}
