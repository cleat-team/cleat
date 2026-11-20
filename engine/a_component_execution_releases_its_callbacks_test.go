//go:build cgo

package engine

// cleat#4257. cbRegistry never removed an entry, so every component execution (Python, C#) kept its per-execution backend reachable from a
// package-level map for the life of the process. A backend holds its execution session (handler), its budget and its store, so the leak was
// not a handful of integers: each execution pinned a whole session.
//
// THE COUNT IS OF ENTRIES WHOSE BACKEND IS THE TEST'S OWN, not of the registry's length: other tests in this package register callbacks too,
// and a length would make this test depend on whichever of them ran first.
//
// THE PRECONDITION IS THE POINT. "Zero entries remain" is true of an execution that registered nothing, so the test also watches the registry
// WHILE the guest runs and requires it to have seen entries then: without that, a component that failed before registering would pass.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// callbacksOf counts the registry entries that belong to b.
func callbacksOf(b *wasmtimeBackend) int {
	cbRegistry.Lock()
	defer cbRegistry.Unlock()
	n := 0
	for _, e := range cbRegistry.entries {
		if e.backend == b {
			n++
		}
	}
	return n
}

// runSpinComponent executes the 157-byte spinning component for d, which the epoch fence then stops, and reports the largest number of b's
// entries the registry held WHILE it ran.
func runSpinComponent(t *testing.T, b *wasmtimeBackend, d time.Duration) (during int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var peak atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if n := int64(callbacksOf(b)); n > peak.Load() {
				peak.Store(n)
			}
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	_, err := b.ExecuteComponentCGo(ctx, spinComponent(t), "run", []byte(`{}`), OutBufSize)
	close(stop)
	wg.Wait()
	if err == nil {
		t.Fatal("the spinning component completed; the fence was expected to stop it")
	}
	return int(peak.Load())
}

func TestAComponentExecutionDoesNotLeaveItsCallbacksRegistered(t *testing.T) {
	b := newDeadlineTestBackend(t)
	const executions = 4
	var perExecution int
	for i := 1; i <= executions; i++ {
		during := runSpinComponent(t, b, 250*time.Millisecond)
		after := callbacksOf(b)
		t.Logf("execution %d: %d entries held while it ran, %d left after it ended", i, during, after)
		if during == 0 {
			t.Fatalf("execution %d: the registry held none of this backend's entries while the guest ran, so \"none left\" would "+
				"be true of an execution that registered nothing and this test would pass vacuously", i)
		}
		if perExecution == 0 {
			perExecution = during
		}
		if after != 0 {
			t.Errorf("after execution %d, %d of this backend's callbacks are still registered (%d per execution): the registry keeps the "+
				"backend, and with it the whole execution session, reachable for the life of the process", i, after, perExecution)
		}
	}
}

// TestAScopeRemovesItsOwnCallbacksAndNeverReusesAnID is the hazard that removing entries creates. Ids used to be len(entries)+1, which was
// unique only because nothing was removed: with live ids {1,2,3} and 1 removed, the next id is 3 and OVERWRITES a live entry, so a callback
// belonging to one execution is served by another's backend. The sequence below is exactly that one, across two scopes.
func TestAScopeRemovesItsOwnCallbacksAndNeverReusesAnID(t *testing.T) {
	a, b, c := &wasmtimeBackend{}, &wasmtimeBackend{}, &wasmtimeBackend{}
	var first, second, third cbScope
	idsA := []uintptr{first.register(a, cbTypeNow), first.register(a, cbTypeNow), first.register(a, cbTypeNow)}
	idB := second.register(b, cbTypeDurableLog)

	first.release() // A's three entries go; B's one stays, and the registry is now SMALLER than the highest id

	for _, id := range idsA {
		if e := lookupCB(id); e.backend != nil {
			t.Errorf("id %d belonged to a released scope and still resolves to a backend", id)
		}
	}
	if e := lookupCB(idB); e.backend != b || e.typ != cbTypeDurableLog {
		t.Fatalf("releasing another scope removed or changed this scope's entry: %+v", e)
	}

	idC1, idC2 := third.register(c, cbTypeNow), third.register(c, cbTypeNow)
	for _, id := range []uintptr{idC1, idC2} {
		if id == idB {
			t.Errorf("a new registration was given id %d, which is a LIVE entry of another scope", id)
		}
		for _, old := range idsA {
			if id == old {
				t.Errorf("a new registration reused id %d, which a released scope had: a late callback of that scope would be served by a stranger", id)
			}
		}
	}
	if e := lookupCB(idB); e.backend != b {
		t.Errorf("a later registration overwrote another scope's live entry (id %d now serves %p, want %p)", idB, e.backend, b)
	}
	second.release()
	third.release()
	for _, id := range []uintptr{idB, idC1, idC2} {
		if e := lookupCB(id); e.backend != nil {
			t.Errorf("id %d still resolves after its scope was released", id)
		}
	}
	first.release() // releasing twice is a no-op, not a double delete of something re-registered
}
