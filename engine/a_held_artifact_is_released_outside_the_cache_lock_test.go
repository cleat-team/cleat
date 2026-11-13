//go:build cgo

package engine

import (
	"sync/atomic"
	"testing"
	"time"
)

// cleat#3748: the LRU gained entries it owns a reference to (a compiled
// component) beside the modules the collector tracks. These pin the ownership
// rules with fake artifacts, so they run without compiling anything and a failure
// names the rule.

type fakeHeld struct {
	id       int
	released atomic.Int32
}

func (f *fakeHeld) release() { f.released.Add(1) }

// finishesWithin runs fn in its own goroutine and reports whether it returned in
// time. The calls under test run their releases SYNCHRONOUSLY, so a release that
// ran under the cache's lock would deadlock the calling goroutine itself; run on
// the test's goroutine that is a runtime "all goroutines are asleep" crash with
// no test name on it, instead of a failure that names the rule.
func finishesWithin(d time.Duration, fn func()) bool {
	ok := make(chan struct{})
	go func() {
		fn()
		close(ok)
	}()
	select {
	case <-ok:
		return true
	case <-time.After(d):
		return false
	}
}

// A release that ran UNDER the cache's lock would deadlock any release that
// touches the cache, and would stall every lookup behind a free of tens of
// megabytes. This release calls back into the cache, so it can only finish if the
// lock was already dropped.
func TestAReleaseRunsAfterTheCacheLockIsDropped(t *testing.T) {
	c := newModuleLRU(1, 1<<30)
	done := make(chan struct{})
	first := &fakeHeld{id: 1}
	c.storeHeld("a", first, func() {
		_ = c.len() // takes c.mu: deadlocks if the lock is still held
		first.release()
		close(done)
	}, 100)

	if !finishesWithin(5*time.Second, func() {
		c.storeHeld("b", &fakeHeld{id: 2}, func() {}, 100) // evicts "a"
	}) {
		t.Fatal("storeHeld did not return: the release of an evicted entry is being run under the cache's lock")
	}
	<-done
	if got := first.released.Load(); got != 1 {
		t.Fatalf("evicted entry released %d time(s), want exactly 1", got)
	}
}

func TestLoadHeldTakesTheCallersReferenceAndMarksTheEntryRecentlyUsed(t *testing.T) {
	c := newModuleLRU(2, 1<<30)
	a, b := &fakeHeld{id: 1}, &fakeHeld{id: 2}
	c.storeHeld("a", a, a.release, 100)
	c.storeHeld("b", b, b.release, 100)

	var acquired atomic.Int32
	got, ok := c.loadHeld("a", func(h any) any { acquired.Add(1); return h })
	if !ok || got.(*fakeHeld) != a {
		t.Fatalf("loadHeld(a) = %v, %v", got, ok)
	}
	if acquired.Load() != 1 {
		t.Fatalf("acquire ran %d time(s), want 1", acquired.Load())
	}

	// "a" was just used, so inserting a third evicts "b", not "a".
	c.storeHeld("c", &fakeHeld{id: 3}, func() {}, 100)
	if a.released.Load() != 0 {
		t.Fatal("the entry that was just loaded was evicted instead of the least recently used one")
	}
	if b.released.Load() != 1 {
		t.Fatalf("the least recently used entry was released %d time(s), want 1", b.released.Load())
	}
	if _, ok := c.loadHeld("b", func(h any) any { return h }); ok {
		t.Fatal("an evicted entry is still loadable")
	}
}

func TestReplacingAHeldEntryReleasesTheOldReference(t *testing.T) {
	c := newModuleLRU(10, 1<<30)
	old, fresh := &fakeHeld{id: 1}, &fakeHeld{id: 2}
	c.storeHeld("k", old, old.release, 100)
	c.storeHeld("k", fresh, fresh.release, 100)

	if old.released.Load() != 1 {
		t.Fatalf("the replaced reference was released %d time(s), want 1; otherwise it leaks", old.released.Load())
	}
	if fresh.released.Load() != 0 {
		t.Fatal("the new reference was released on insert")
	}
	if c.len() != 1 {
		t.Fatalf("replacing a key left %d entries, want 1", c.len())
	}
	if got := c.estimatedBytes(); got != CompiledSizeEstimate(100) {
		t.Fatalf("estimated bytes %d after a replace, want one entry's %d", got, CompiledSizeEstimate(100))
	}
}

func TestDrainReleasesHeldEntriesAndLeavesModulesAlone(t *testing.T) {
	c := newModuleLRU(10, 1<<30)
	h1, h2 := &fakeHeld{id: 1}, &fakeHeld{id: 2}
	c.storeHeld("h1", h1, h1.release, 100)
	c.storeHeld("h2", h2, h2.release, 100)
	// A module entry, stored the way the core path stores one: no held artifact.
	c.store("m", nil, 100)

	c.drainHeld()

	if h1.released.Load() != 1 || h2.released.Load() != 1 {
		t.Fatalf("drain released %d and %d, want 1 and 1", h1.released.Load(), h2.released.Load())
	}
	if c.len() != 1 {
		t.Fatalf("drain left %d entries, want the 1 module", c.len())
	}
	if got := c.estimatedBytes(); got != CompiledSizeEstimate(100) {
		t.Fatalf("estimated bytes %d after a drain, want the module's %d", got, CompiledSizeEstimate(100))
	}
	// Draining twice must not release twice.
	c.drainHeld()
	if h1.released.Load() != 1 {
		t.Fatalf("a second drain released an entry again (%d)", h1.released.Load())
	}
}

// A held entry and a module count against the same two bounds.
func TestHeldEntriesAndModulesAreBoundedTogether(t *testing.T) {
	c := newModuleLRU(100, CompiledSizeEstimate(100)*2+10)
	h := &fakeHeld{id: 1}
	c.storeHeld("h", h, h.release, 100)
	c.store("m1", nil, 100)
	c.store("m2", nil, 100) // three entries' estimates against room for two

	if c.len() != 2 {
		t.Fatalf("cache holds %d entries, want 2 under a byte bound sized for two", c.len())
	}
	if h.released.Load() != 1 {
		t.Fatalf("the oldest entry, a held one, was released %d time(s), want 1", h.released.Load())
	}
}

// The same rule on the other two paths that run releases. A release that touches
// the cache deadlocks if it runs under the lock, so each of these can only finish
// if its own path drops the lock first. The first test above covers storeHeld; a
// MODULE insert that evicts a held component, and a drain, are separate code and
// had no test of their own.
func TestAReleaseTriggeredByAModuleInsertRunsAfterTheCacheLockIsDropped(t *testing.T) {
	c := newModuleLRU(1, 1<<30)
	done := make(chan struct{})
	c.storeHeld("held", &fakeHeld{id: 1}, func() {
		_ = c.len()
		close(done)
	}, 100)

	if !finishesWithin(5*time.Second, func() {
		c.store("module", nil, 100) // evicts the held entry through store, not storeHeld
	}) {
		t.Fatal("store did not return: a release it triggered is being run under the cache's lock")
	}
	<-done
}

func TestAReleaseTriggeredByADrainRunsAfterTheCacheLockIsDropped(t *testing.T) {
	c := newModuleLRU(10, 1<<30)
	done := make(chan struct{})
	c.storeHeld("held", &fakeHeld{id: 1}, func() {
		_ = c.len()
		close(done)
	}, 100)

	if !finishesWithin(5*time.Second, c.drainHeld) {
		t.Fatal("drainHeld did not return: a release it triggered is being run under the cache's lock")
	}
	<-done
}

// Replacing a held key releases the old reference, and that release too must not
// run under the lock.
func TestAReleaseTriggeredByAReplaceRunsAfterTheCacheLockIsDropped(t *testing.T) {
	c := newModuleLRU(10, 1<<30)
	done := make(chan struct{})
	c.storeHeld("k", &fakeHeld{id: 1}, func() {
		_ = c.len()
		close(done)
	}, 100)

	if !finishesWithin(5*time.Second, func() {
		c.storeHeld("k", &fakeHeld{id: 2}, func() {}, 100)
	}) {
		t.Fatal("storeHeld did not return: the release of a replaced entry is being run under the cache's lock")
	}
	<-done
}
