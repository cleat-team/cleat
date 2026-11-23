package engine

import (
	"context"
	"testing"
	"time"
)

// Python and C# are admitted for residency, and charged a figure per held instance (the larger measured one, rounded up). These tests pin the accounting, with no guest and
// no clock: that a language is held at all, what each is charged, that a charge is given back, and that the compiled component is deliberately NOT charged.

// holdOne parks a run and reports whether it was held (a timer was armed) or refused (it returned at once). The channel is the wait's outcome.
func holdOne(t *testing.T, set *ResidentSet, clock *fakeResidentClock, info ResidentRunInfo) (<-chan parkOutcome, bool) {
	t.Helper()
	info.ClaimCheck = func(context.Context) error { return nil }
	out := park(set.ForRun(info), context.Background(), timerReq(residentT+5000))
	select {
	case <-clock.armed:
		return out, true
	case got := <-out:
		if got != parkRefused {
			t.Fatalf("a wait that was not held ended with %d, want the refusal %d", got, parkRefused)
		}
		return out, false
	case <-time.After(10 * time.Second):
		t.Fatal("the wait neither parked nor returned")
		return nil, false
	}
}

func chargedBytes(s *ResidentSet) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

func TestGoPythonAndCSharpAreHeldAndEveryOtherLanguageIsRefused(t *testing.T) {
	for _, lang := range []string{"go", "python", "csharp"} {
		t.Run("held: "+lang, func(t *testing.T) {
			clock := newFakeResidentClock(residentT)
			set := newTestResidentSet(t, residentTestConfig(clock, nil))
			out, held := holdOne(t, set, clock, ResidentRunInfo{Language: lang})
			if !held {
				t.Fatalf("a %s run was refused with the default policy", lang)
			}
			clock.fireAll()
			mustOutcome(t, out, parkDeadline, lang)
		})
	}
	// Java, Rust and JavaScript run on wasmtime and have NO measured figure; "unknown, so probably small" is the wrong way to fail.
	for _, lang := range []string{"java", "rust", "javascript", "tinygo", ""} {
		t.Run("refused: "+lang, func(t *testing.T) {
			clock := newFakeResidentClock(residentT)
			set := newTestResidentSet(t, residentTestConfig(clock, nil))
			if _, held := holdOne(t, set, clock, ResidentRunInfo{Language: lang}); held {
				t.Fatalf("a run of language %q was held although no cost was measured for it", lang)
			}
		})
	}
}

// The numbers are the larger measured figure per language (WS-cleat1's and design section 9a's), rounded up; a change to them is a change to a measurement and has to say so here.
func TestTheChargesAreTheMeasuredFiguresRoundedUp(t *testing.T) {
	const mib = 1 << 20
	for lang, want := range map[string]uint64{"go": 24, "python": 4, "csharp": 18} {
		if got := defaultResidentInstanceBytes[lang] / mib; got != want {
			t.Errorf("%s per instance = %d MiB, want %d (Go: the larger measured figure, 19.6 MiB rounded up; Python 2.38 + 1 (the larger of the after-churn figures) and C# 16.78 + 1, each rounded up, once cleat#4387 and cleat#4386 are in the component)", lang, got, want)
		}
	}
}

func TestAnInstanceIsRefusedOneByteShortOfItsChargeAndHeldAtExactlyIt(t *testing.T) {
	const inst = 10 << 20
	for _, tc := range []struct {
		name   string
		budget uint64
		held   bool
	}{
		{"a budget one byte short of one instance", inst - 1, false},
		{"a budget of exactly one instance", inst, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeResidentClock(residentT)
			m := newReplayMetrics(t)
			set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) {
				c.Metrics = m
				c.InstanceBytes = map[string]uint64{"csharp": inst}
				c.MemoryBudgetBytes = tc.budget
			}))
			_, held := holdOne(t, set, clock, ResidentRunInfo{Language: "csharp"})
			if held != tc.held {
				t.Fatalf("held = %v, want %v", held, tc.held)
			}
			if !tc.held {
				if n := residentCounter(t, m, "cleat_resident_refusals_total", `reason="memory"`); n != 1 {
					t.Errorf("the refusal was counted %d time(s) as memory, want 1", n)
				}
				if got := chargedBytes(set); got != 0 {
					t.Errorf("a refused run left %d bytes charged", got)
				}
			}
		})
	}
}

func TestEveryChargeIsGivenBackWhenTheRunsLeave(t *testing.T) {
	const inst = 10 << 20
	clock := newFakeResidentClock(residentT)
	set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) {
		c.InstanceBytes = map[string]uint64{"python": inst}
		c.MemoryBudgetBytes = 3 * inst
	}))
	var outs []<-chan parkOutcome
	for i := 1; i <= 3; i++ {
		out, held := holdOne(t, set, clock, ResidentRunInfo{Language: "python"})
		if !held {
			t.Fatalf("run %d was refused (budget is three instances)", i)
		}
		outs = append(outs, out)
	}
	if got, want := chargedBytes(set), uint64(3*inst); got != want {
		t.Fatalf("three runs are charged %d, want %d", got, want)
	}
	if _, held := holdOne(t, set, clock, ResidentRunInfo{Language: "python"}); held {
		t.Fatalf("a fourth run was held past the budget")
	}
	clock.fireAll()
	for _, o := range outs {
		mustOutcome(t, o, parkDeadline, "a held run")
	}
	if got := chargedBytes(set); got != 0 {
		t.Fatalf("after every run left the set is still charged %d bytes: a charge was not given back", got)
	}
}

// THE COMPILED COMPONENT IS NOT CHARGED, and that is a decision, so it is pinned. An earlier version charged WS-cleat1's per-process fixed cost (Python 512 MiB, C# 82 MiB)
// once per distinct component. Compiled components live in an LRU cache that holds them whether or not anything is held, so a worker that ran the workflow already carries
// that memory, and charging it made the default budget refuse a SECOND Python definition for memory the worker was already using. At the default budget, the first and the
// second distinct Python definitions must both be held.
func TestTwoDistinctPythonDefinitionsAreBothHeldAtTheDefaultBudget(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	set := newTestResidentSet(t, residentTestConfig(clock, nil)) // MaxRuns 4, the default budget
	for i, name := range []string{"orders@1", "billing@1"} {
		if _, held := holdOne(t, set, clock, ResidentRunInfo{Language: "python", WorkflowID: name}); !held {
			t.Fatalf("python definition %d (%s) was refused at the default budget: a charge beyond the per-instance one has crept back in", i+1, name)
		}
	}
	if got, want := chargedBytes(set), uint64(2*4<<20); got != want {
		t.Fatalf("two python runs are charged %d bytes, want %d (two instances and nothing else)", got, want)
	}
}

// The default budget is what the table implies and no more. It is asserted as a formula so the doc's claim can be checked against it, and so the claim that it cannot refuse
// on memory (which is false for any guest that does work) is not made by accident again.
func TestTheDefaultBudgetIsTheDearestInstanceTimesMaxRuns(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	set := newTestResidentSet(t, residentTestConfig(clock, nil)) // MaxRuns 4, no explicit budget
	if got, want := set.cfg.MemoryBudgetBytes, uint64(24<<20)*4; got != want {
		t.Fatalf("the default budget is %d, want %d (4 x 24 MiB go, now the dearest)", got, want)
	}
}
