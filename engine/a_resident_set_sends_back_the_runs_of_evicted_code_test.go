package engine

import (
	"testing"
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
