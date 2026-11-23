package engine

import (
	"context"
	"testing"
)

// cleat#4377: a deployment can see what the held runs may use and what they are charged, so a worker that refuses on memory is explainable from its own metrics.
// The budget is recorded once when the set is built; the charged figure rises by a run's charge when it is held and falls by the same when it leaves, so it returns
// to zero. Both are checked against numbers the test chose, and the control is that they differ from each other and from the run count: a gauge that mirrored cleat_resident_runs
// would read 1 here, not 24 MiB.

func TestTheResidentMemoryGaugesShowTheBudgetAndWhatIsChargedAndReturnToZero(t *testing.T) {
	clock := newFakeResidentClock(residentT)
	m := newReplayMetrics(t)
	const budget = 100 << 20
	set := newTestResidentSet(t, residentTestConfig(clock, func(c *ResidencyConfig) { c.Metrics = m; c.MemoryBudgetBytes = budget }))
	run := set.ForRun(ResidentRunInfo{Language: "go"})

	if got := residentCounter(t, m, "cleat_resident_memory_budget_bytes"); got != budget {
		t.Errorf("the budget gauge reads %d at start, want %d", got, budget)
	}
	if got := residentCounter(t, m, "cleat_resident_memory_charged_bytes"); got != 0 {
		t.Errorf("the charged gauge reads %d before anything is held, want 0", got)
	}

	held := park(run, context.Background(), timerReq(residentT+5000))
	awaitArmed(t, clock)
	want := int(defaultResidentInstanceBytes["go"])
	if got := residentCounter(t, m, "cleat_resident_memory_charged_bytes"); got != want {
		t.Errorf("one held Go run is charged %d bytes on the gauge, want %d (the table's figure)", got, want)
	}
	if runs := residentCounter(t, m, "cleat_resident_runs"); runs != 1 || runs == want {
		t.Errorf("the held-runs gauge reads %d with one run held", runs)
	}

	clock.fireAll()
	mustOutcome(t, held, parkDeadline, "the held run")
	if got := residentCounter(t, m, "cleat_resident_memory_charged_bytes"); got != 0 {
		t.Errorf("the charged gauge reads %d after the run left, want 0: it is not subtracting what it added", got)
	}
	if got := residentCounter(t, m, "cleat_resident_memory_budget_bytes"); got != budget {
		t.Errorf("the budget gauge moved to %d, want it fixed at %d", got, budget)
	}
}
