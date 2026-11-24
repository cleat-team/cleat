//go:build cgo

package engine

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v48"
)

// Two DIFFERENT components must not compile at once in one worker. compileLocks serialises the same component only, so a cold start that claims runs of six distinct Python components
// compiled six at once: measured 2026-11-23 in a 2Gi container, 40 MiB to the limit in 17 seconds and OOM-killed (cleat-team/cleat#4397 follow-up).
//
// The compile itself is replaced by a function that counts how many are inside it at one instant, so this measures the serialisation and not wasmtime. THE KNOWN-POSITIVE is the same
// run on a backend with no semaphore, which must show the overlap: a test that cannot see overlap passes whatever it is given.

func countingCompile(t *testing.T) (restore func(), maxSeen *atomic.Int64, calls *atomic.Int64) {
	t.Helper()
	var inside, max, n atomic.Int64
	old := componentCompileFunc
	componentCompileFunc = func(*wasmtime.Engine, []byte) (componentHandle, error) {
		n.Add(1)
		cur := inside.Add(1)
		for {
			m := max.Load()
			if cur <= m || max.CompareAndSwap(m, cur) {
				break
			}
		}
		time.Sleep(60 * time.Millisecond)
		inside.Add(-1)
		return nil, fmt.Errorf("fake compile")
	}
	return func() { componentCompileFunc = old }, &max, &n
}

func compileSixDistinctAtOnce(b *wasmtimeBackend) {
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _ = b.compileComponentMiss(fmt.Sprintf("component-%d", i), []byte{0})
		}(i)
	}
	wg.Wait()
}

func TestSixDistinctComponentsCompileOneAtATimeInProcess(t *testing.T) {
	restore, max, calls := countingCompile(t)
	defer restore()

	// The known-positive: no semaphore, as a backend built before this change. The overlap must be visible, or the counter is blind.
	compileSixDistinctAtOnce(&wasmtimeBackend{})
	if max.Load() < 2 {
		t.Fatalf("PRECONDITION FAILED: with no semaphore only %d compile(s) overlapped, so this test cannot see the defect it guards", max.Load())
	}
	overlapWithout := max.Load()

	max.Store(0)
	calls.Store(0)
	compileSixDistinctAtOnce(&wasmtimeBackend{componentCompileSem: make(chan struct{}, 1)})
	if calls.Load() != 6 {
		t.Fatalf("%d of 6 compiles ran", calls.Load())
	}
	if max.Load() != 1 {
		t.Errorf("%d compiles ran at once with the semaphore (%d without it); want 1", max.Load(), overlapWithout)
	}
}

// The fallback after a child that failed for an ordinary reason (not killed) compiles in this process, and it must take the same turn: a child failing systematically otherwise turns
// precompile ON into the unserialised case above. /bin/false is an absolute executable that exits 1, which is a failure and not a SIGKILL.
func TestTheInProcessFallbackAfterAFailedChildAlsoTakesItsTurn(t *testing.T) {
	restore, max, calls := countingCompile(t)
	defer restore()

	p, err := newComponentPrecompiler(ComponentPrecompile{Executable: "/usr/bin/false"}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	b := &wasmtimeBackend{precompiler: p, componentCompileSem: make(chan struct{}, 1)}
	compileSixDistinctAtOnce(b)
	if calls.Load() != 6 {
		t.Fatalf("%d of 6 fallback compiles ran (fallbacks counted: %d)", calls.Load(), p.fallbacks.Load())
	}
	if got := p.fallbacks.Load(); got != 6 {
		t.Errorf("%d fallbacks counted, want 6: the child did not fail the way this test needs", got)
	}
	if max.Load() != 1 {
		t.Errorf("%d fallback compiles ran at once; want 1", max.Load())
	}
}

// The two tests above build their own backend, so they prove compileComponentMiss serialises when it is HANDED a semaphore, not that the worker's backend has one: deleting the line in
// NewWasmtimeBackend, or in PerExecution's copy, left every one of them green (cleat-review3, mutant X1). This is the worker's own path: a backend from NewWasmtimeBackend, copies of it
// the way each execution takes them, and the six distinct components through THOSE.
func TestTheBackendTheWorkerBuildsSharesOneCompileSemaphoreAndHeadroomWithEveryExecutionCopy(t *testing.T) {
	restore, max, calls := countingCompile(t)
	defer restore()

	b, err := NewWasmtimeBackend(t.Context(), WithWasmtimeCompileHeadroom(func() (uint64, bool) { return 8 << 30, true }, 800<<20, time.Minute, 16<<30))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	defer b.Close(t.Context())

	if b.componentCompileSem == nil || cap(b.componentCompileSem) != 1 {
		t.Fatalf("the backend NewWasmtimeBackend builds has no one-slot compile semaphore (%v): a worker would compile distinct components at once", b.componentCompileSem)
	}
	if b.compileHeadroom == nil {
		t.Fatal("the backend NewWasmtimeBackend builds dropped the compile headroom option")
	}
	copies := make([]*wasmtimeBackend, 6)
	for i := range copies {
		c, ok := b.PerExecution(time.Second).(*wasmtimeBackend)
		if !ok {
			t.Fatalf("PerExecution returned a %T", b.PerExecution(time.Second))
		}
		if c.componentCompileSem != b.componentCompileSem {
			t.Fatalf("copy %d has its own compile semaphore, not the worker's: two executions would compile at once", i)
		}
		if c.compileHeadroom != b.compileHeadroom {
			t.Fatalf("copy %d dropped the compile headroom check", i)
		}
		copies[i] = c
	}

	var wg sync.WaitGroup
	for i, c := range copies {
		wg.Add(1)
		go func(i int, c *wasmtimeBackend) {
			defer wg.Done()
			_, _, _ = c.compileComponentMiss(fmt.Sprintf("worker-component-%d", i), []byte{0})
		}(i, c)
	}
	wg.Wait()
	if calls.Load() != 6 {
		t.Fatalf("%d of 6 compiles ran", calls.Load())
	}
	if max.Load() != 1 {
		t.Errorf("%d compiles ran at once through the worker's own backend and its copies; want 1", max.Load())
	}
}
