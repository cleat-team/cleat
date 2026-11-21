package engine

import (
	"context"
	"testing"
	"time"
)

// Python and C# are admitted for residency, and charged what WS-cleat1 measured (component-held-instances-2026-11-20): a figure per held instance, and a FIXED figure
// per component that is charged once while any run of it is held and given back with the last. These tests pin the accounting, with no guest and no clock: that a
// language is held at all, that the fixed cost is neither dropped nor charged per instance, that it is per COMPONENT, and that every charge is given back.

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
	for lang, want := range map[string]uint64{"go": 24, "python": 17, "csharp": 66} {
		if got := defaultResidentInstanceBytes[lang] / mib; got != want {
			t.Errorf("%s per instance = %d MiB, want %d (the larger measured figure, rounded up: 19.6 / 16.2 / 65.7, each after a guest made garbage)", lang, got, want)
		}
	}
	for lang, want := range map[string]uint64{"python": 512, "csharp": 82} {
		if got := defaultResidentFixedBytes[lang] / mib; got != want {
			t.Errorf("%s fixed = %d MiB, want %d (measured 493 to 510 / 81, rounded up)", lang, got, want)
		}
	}
	if _, ok := defaultResidentFixedBytes["go"]; ok {
		t.Errorf("go has a fixed charge: none was measured for it")
	}
}

func TestTheFixedCostIsChargedOnceForAComponentAndGivenBackWithItsLastRun(t *testing.T) {
	const inst, fixed = 10 << 20, 100 << 20
	clock := newFakeResidentClock(residentT)
	set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) {
		c.InstanceBytes = map[string]uint64{"python": inst}
		c.FixedBytes = map[string]uint64{"python": fixed}
		c.MemoryBudgetBytes = fixed + 3*inst
	}))
	info := ResidentRunInfo{Language: "python", Component: "orders@1"}

	var outs []<-chan parkOutcome
	for i := 1; i <= 3; i++ {
		out, held := holdOne(t, set, clock, info)
		if !held {
			t.Fatalf("run %d of one component was refused: the fixed cost was charged again (budget fixed + 3 instances)", i)
		}
		outs = append(outs, out)
	}
	if got, want := chargedBytes(set), uint64(fixed+3*inst); got != want {
		t.Fatalf("three runs of one component are charged %d, want %d (one fixed + three instances)", got, want)
	}
	// The budget is exactly full, so a fourth is refused for memory and not for anything else (MaxRuns is 4).
	if _, held := holdOne(t, set, clock, info); held {
		t.Fatalf("a fourth run was held past the budget")
	}

	clock.fireAll()
	for _, o := range outs {
		mustOutcome(t, o, parkDeadline, "a held run")
	}
	// Eventually consistent: the release happens in the waiter before it returns, so by now every charge is back.
	if got := chargedBytes(set); got != 0 {
		t.Fatalf("after every run left the set is still charged %d bytes: a fixed or per-instance charge was not given back", got)
	}
}

func TestTheFixedCostIsPerComponentNotPerLanguage(t *testing.T) {
	const inst, fixed = 10 << 20, 100 << 20
	cfg := func(c *ResidencyConfig) {
		c.InstanceBytes = map[string]uint64{"python": inst}
		c.FixedBytes = map[string]uint64{"python": fixed}
		c.MemoryBudgetBytes = fixed + 2*inst // room for ONE component's fixed cost plus two instances
	}
	t.Run("a second distinct component pays its own fixed cost", func(t *testing.T) {
		clock := newFakeResidentClock(residentT)
		set := newTestResidentSet(t, residentTestConfig(clock, cfg))
		if _, held := holdOne(t, set, clock, ResidentRunInfo{Language: "python", Component: "orders@1"}); !held {
			t.Fatal("the first run was refused")
		}
		if _, held := holdOne(t, set, clock, ResidentRunInfo{Language: "python", Component: "billing@1"}); held {
			t.Fatal("a run of a DIFFERENT python component was held on the first one's fixed cost: the module cache is keyed by the component's bytes, so each pays its own")
		}
	})
	t.Run("CONTROL: the same component's second run fits", func(t *testing.T) {
		clock := newFakeResidentClock(residentT)
		set := newTestResidentSet(t, residentTestConfig(clock, cfg))
		for i := 1; i <= 2; i++ {
			if _, held := holdOne(t, set, clock, ResidentRunInfo{Language: "python", Component: "orders@1"}); !held {
				t.Fatalf("run %d of the same component was refused", i)
			}
		}
	})
	t.Run("a new VERSION of a definition is another component", func(t *testing.T) {
		clock := newFakeResidentClock(residentT)
		set := newTestResidentSet(t, residentTestConfig(clock, cfg))
		if _, held := holdOne(t, set, clock, ResidentRunInfo{Language: "python", Component: "orders@1"}); !held {
			t.Fatal("the first run was refused")
		}
		if _, held := holdOne(t, set, clock, ResidentRunInfo{Language: "python", Component: "orders@2"}); held {
			t.Fatal("orders@2 was held on orders@1's fixed cost")
		}
	})
}

func TestOneInstanceIsRefusedWhenItsFixedCostAloneDoesNotFit(t *testing.T) {
	const inst, fixed = 10 << 20, 100 << 20
	for _, tc := range []struct {
		name   string
		budget uint64
		held   bool
	}{
		{"a budget one byte short of fixed plus one instance", fixed + inst - 1, false},
		{"a budget of exactly fixed plus one instance", fixed + inst, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeResidentClock(residentT)
			m := newReplayMetrics(t)
			set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) {
				c.Metrics = m
				c.InstanceBytes = map[string]uint64{"csharp": inst}
				c.FixedBytes = map[string]uint64{"csharp": fixed}
				c.MemoryBudgetBytes = tc.budget
			}))
			_, held := holdOne(t, set, clock, ResidentRunInfo{Language: "csharp", Component: "c@1"})
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

// A run with no component name is charged by language alone. It undercharges when several distinct components are held, and the doc says so; this pins that it
// is at least charged ONCE and given back, so that a caller that does not name a component is not free.
func TestARunWithNoComponentNameIsStillChargedItsFixedCostOnce(t *testing.T) {
	const inst, fixed = 10 << 20, 100 << 20
	clock := newFakeResidentClock(residentT)
	set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) {
		c.InstanceBytes = map[string]uint64{"python": inst}
		c.FixedBytes = map[string]uint64{"python": fixed}
		c.MemoryBudgetBytes = fixed + 2*inst
	}))
	for i := 1; i <= 2; i++ {
		if _, held := holdOne(t, set, clock, ResidentRunInfo{Language: "python"}); !held {
			t.Fatalf("run %d with no component name was refused", i)
		}
	}
	if got, want := chargedBytes(set), uint64(fixed+2*inst); got != want {
		t.Fatalf("charged %d, want %d", got, want)
	}
}

// The default budget is what the TABLE implies and no more. It is asserted as a formula so the doc's claim can be checked against it, and so the claim that it
// cannot refuse on memory (which is false for any guest that does work) is not made by accident again.
func TestTheDefaultBudgetIsTheDearestInstanceTimesMaxRunsPlusEachLanguagesFixedCost(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	set := newTestResidentSet(t, residentTestConfig(clock, nil)) // MaxRuns 4, no explicit budget
	want := uint64(66<<20)*4 + (512 << 20) + (82 << 20)
	if got := set.cfg.MemoryBudgetBytes; got != want {
		t.Fatalf("the default budget is %d, want %d (4 x 66 MiB csharp + 512 MiB python + 82 MiB csharp fixed)", got, want)
	}
	// A language with a fixed figure and no instance figure is not admitted, so it must not widen the budget.
	set2 := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) {
		c.InstanceBytes = map[string]uint64{"go": 24 << 20}
	}))
	if got, want := set2.cfg.MemoryBudgetBytes, uint64(24<<20)*4; got != want {
		t.Fatalf("a table with only go has budget %d, want %d: a fixed figure for a language that is not admitted widened the budget", got, want)
	}
}
