package engine

import (
	"sync"
	"testing"
	"time"
)

// A HOLD NEVER OUTLIVES ITS CODE'S CACHE ENTRY. A held instance refcounts the compiled code it runs, so when the module cache evicts that code nothing is freed while a held
// run still uses it: with many distinct definitions, residency could keep up to --resident-max-runs times a compiled component (about 500 MiB for Python) alive that the
// cache had decided to free, which no bound accounts for. So the cache reports each eviction and the set sends back every held run of that code (EvictCode). These rows pin
// the set's half with no guest and no clock; the cache's half and the real end-to-end case are in a_held_run_is_sent_back_when_its_code_leaves_the_cache_test.go.

func TestEvictCodeSendsBackOnlyTheRunsOfThatCodeAndGivesTheirChargeBack(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	m := newReplayMetrics(t)
	set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) { c.Metrics = m }))

	a1, held := holdOne(t, set, clock, ResidentRunInfo{Language: "python", WorkflowID: "a1", CodeKey: "code-a"})
	if !held {
		t.Fatal("run a1 was refused")
	}
	a2, held := holdOne(t, set, clock, ResidentRunInfo{Language: "python", WorkflowID: "a2", CodeKey: "code-a"})
	if !held {
		t.Fatal("run a2 was refused")
	}
	b, held := holdOne(t, set, clock, ResidentRunInfo{Language: "python", WorkflowID: "b", CodeKey: "code-b"})
	if !held {
		t.Fatal("run b was refused")
	}
	unknown, held := holdOne(t, set, clock, ResidentRunInfo{Language: "python", WorkflowID: "u"}) // no CodeKey
	if !held {
		t.Fatal("run u was refused")
	}

	// Keys that match nothing, and the empty key, send nothing back.
	set.EvictCode("code-zzz")
	set.EvictCode("")
	if n := set.Len(); n != 4 {
		t.Fatalf("EvictCode of a key no run has (or of the empty key) changed the set: %d held, want 4", n)
	}

	set.EvictCode("code-a")
	// Both runs of code-a are sent back as a refusal would have been: they suspend, nothing is lost, the next claim replays them.
	mustOutcome(t, a1, parkRefused, "run a1, whose code was evicted")
	mustOutcome(t, a2, parkRefused, "run a2, whose code was evicted")
	if n := set.Len(); n != 2 {
		t.Fatalf("%d runs still held, want 2 (the runs of another code and of no known code)", n)
	}
	if got, want := chargedBytes(set), uint64(2*17<<20); got != want {
		t.Errorf("the set is charged %d after two runs were sent back, want %d: their charges were not given back", got, want)
	}
	if n := residentCounter(t, m, "cleat_resident_evictions_total", `reason="code"`); n != 2 {
		t.Errorf("the eviction was counted %d time(s) under reason \"code\", want 2 (one per run sent back)", n)
	}

	// The survivors are undisturbed: they still wake normally.
	clock.fireAll()
	mustOutcome(t, b, parkDeadline, "run b, of another code")
	mustOutcome(t, unknown, parkDeadline, "run u, of no known code")
}

// A run is sent back once: a second eviction of the same code while it is already going does not close its channel twice (which would panic).
func TestEvictingTheSameCodeTwiceDoesNotSendBackARunTwice(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	set := newTestResidentSet(t, residentTestConfig(clock, nil))
	out, held := holdOne(t, set, clock, ResidentRunInfo{Language: "go", CodeKey: "code-a"})
	if !held {
		t.Fatal("the run was refused")
	}
	set.EvictCode("code-a")
	set.EvictCode("code-a") // must not panic on a closed channel
	mustOutcome(t, out, parkRefused, "the run sent back")
}

// A nil set is the residency-off worker's: the hook is installed whether or not residency is on, so it must be a no-op there.
func TestEvictCodeOnANilSetIsANoOp(t *testing.T) {
	var s *ResidentSet
	s.EvictCode("code-a")
}

// THE RACE THE FIRST VERSION MISSED (cleat-review3's probe). EvictCode reaches only runs that are ALREADY held, so a run whose code is evicted while it is still EXECUTING
// (another run's insert into a full cache) reaches its sleep afterwards, is admitted, and is held against code that is gone, with nothing left to send it back.
func TestARunWhoseCodeWasEvictedBeforeItReachedItsSleepIsNotHeld(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	m := newReplayMetrics(t)
	var cached = map[string]bool{"code-a": true}
	set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) { c.Metrics = m }))
	set.codeCached = func(key string) bool { return cached[key] }

	// The eviction fires while no run of code-a is held: there is nothing for EvictCode to reach.
	delete(cached, "code-a")
	set.EvictCode("code-a")

	_, held := holdOne(t, set, clock, ResidentRunInfo{Language: "go", CodeKey: "code-a"})
	if held {
		t.Fatal("a run was held against code the cache had already evicted: nothing will ever send it back")
	}
	if n := residentCounter(t, m, "cleat_resident_refusals_total", `reason="code"`); n != 1 {
		t.Errorf("the refusal was counted %d time(s) as reason \"code\", want 1", n)
	}
	if got := chargedBytes(set); got != 0 {
		t.Errorf("a refused run left %d bytes charged", got)
	}

	// CONTROLS. Code still cached is held; a run that names NO code is refused when a cache is attached (EvictCode could never reach it, so holding it would be a pin nothing sees);
	// and with no hook installed (a set not attached to a cache) nothing changes.
	if _, held := holdOne(t, set, clock, ResidentRunInfo{Language: "go", CodeKey: "code-b"}); held {
		t.Error("PRECONDITION: code-b is not in the cached set and must be refused too, or this control means nothing")
	}
	cached["code-b"] = true
	if _, held := holdOne(t, set, clock, ResidentRunInfo{Language: "go", CodeKey: "code-b"}); !held {
		t.Error("a run whose code IS cached was refused")
	}
	if _, held := holdOne(t, set, clock, ResidentRunInfo{Language: "go"}); held {
		t.Error("a run with no CodeKey was held with a cache attached: EvictCode can never reach it, so it would be a pin nothing sees")
	}
	set.codeCached = nil
	if _, held := holdOne(t, set, clock, ResidentRunInfo{Language: "go"}); !held {
		t.Error("with no cache attached a run with no CodeKey was refused")
	}
	if _, held := holdOne(t, set, clock, ResidentRunInfo{Language: "go", CodeKey: "code-zzz"}); !held {
		t.Error("with no cache attached a run was refused")
	}
}

// THE ORDERING THAT MAKES THE CHECK SOUND, forced and not hoped for. The cache removes an entry under its own lock and reports it AFTER releasing that lock, so an eviction can
// land between admission's "is it still cached?" and the park. That is safe only because the check and the park are ONE critical section of the set's mutex: EvictCode, which
// needs the same mutex, then waits for the run to be parked and finds it. This row makes the callback launch the eviction at exactly that moment and wait for it. With the check
// inside the critical section the eviction cannot finish while the callback is still running, so the run is parked and then sent back. With the check outside it (the mutant)
// the eviction finishes first, finds nothing, and the run is held for good.
func TestAnEvictionBetweenTheCacheCheckAndTheParkStillSendsTheRunBack(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	set := newTestResidentSet(t, residentTestConfig(clock, nil))
	evictDone := make(chan struct{})
	var once sync.Once
	set.codeCached = func(key string) bool {
		once.Do(func() {
			go func() { set.EvictCode(key); close(evictDone) }()
			select {
			case <-evictDone: // the eviction completed while the check was still in progress: the check is not inside the park's critical section
			case <-time.After(150 * time.Millisecond): // it is blocked on the set's mutex, as it must be
			}
		})
		return true // the entry was still there when asked; it is removed immediately after
	}
	out, held := holdOne(t, set, clock, ResidentRunInfo{Language: "go", CodeKey: "code-a"})
	if !held {
		t.Fatal("PRECONDITION FAILED: the run was refused at admission, so the interleaving under test never happened")
	}
	mustOutcome(t, out, parkRefused, "a run whose code was evicted between the cache check and the park")
	select {
	case <-evictDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the eviction never finished")
	}
	if n := set.Len(); n != 0 {
		t.Fatalf("%d run(s) still held after the eviction: it was held against code that is gone", n)
	}
}
