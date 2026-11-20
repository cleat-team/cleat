package engine

// W5 of cleat#4152: a run may be bounded in how many prefix records it holds (stepSpec.MaxPrefix). An await holds at most one, and Identity cannot
// say so: a polling loop that times out records [await, await, ...] with the SAME names each time.

import (
	"context"
	"strings"
	"testing"
)

func TestARunHoldsAtMostMaxPrefixPrefixRecords(t *testing.T) {
	ctx := context.Background()
	run := func(max int, hist []EventRecord, calls int) (sizes []int, results []int64, prefixes [][]EventRecord, finalStep int, replaying bool) {
		s := stepSession(nil, hist)
		for i := 0; i < calls; i++ {
			var ran int32
			var dec, pre []EventRecord
			sp := runSpec(&ran, &dec, &pre)
			sp.EndsWithoutFinal = true
			sp.MaxPrefix = max
			results = append(results, s.step(ctx, nil, sp, stepArgs{s1: "a"}))
			sizes = append(sizes, len(dec))
			prefixes = append(prefixes, pre)
		}
		return sizes, results, prefixes, s.stepCount, s.isReplay
	}
	two := func(types ...EventType) []EventRecord {
		var h []EventRecord
		for i, et := range types {
			h = append(h, runRec(i, et, "a"))
		}
		return h
	}

	// The polling loop that times out and then delivers: two awaits with the SAME names, then the delivery.
	hist := two(runPrefixEvent, runPrefixEvent, runFinalEvent)
	sizes, results, _, step, _ := run(1, hist, 2)
	if sizes[0] != 1 || sizes[1] != 2 || step != 3 {
		t.Errorf("with MaxPrefix 1: the calls read runs of %v and ended at step %d, want [1 2] and 3 (the first times out alone, the second pairs its await with the delivery)", sizes, step)
	}
	if results[0] != 7 || results[1] != 7 {
		t.Errorf("with MaxPrefix 1: results %#x, want both DecodeRun's 7", results)
	}

	// CONTROL: without the bound the first call's run swallows the second call's await and the delivery, which is the defect this field removes.
	sizes, _, _, step, _ = run(0, hist, 1)
	if sizes[0] != 3 || step != 3 {
		t.Fatalf("CONTROL: with no bound the first call read a run of %d records and ended at step %d, want 3 and 3; the bound has nothing to stop", sizes[0], step)
	}

	// A bounded run whose second await is the END of history: the first call times out alone, the second ENDS INSIDE its run and continues fresh from
	// its prefix.
	hist = two(runPrefixEvent, runPrefixEvent)
	sizes, _, prefixes, step, replaying := run(1, hist, 2)
	if sizes[0] != 1 || len(prefixes[1]) != 1 || step != 3 || replaying {
		t.Errorf("[await, await] at the end of history: first run %d, second call's Prefix %d, step %d, replaying %v; want 1, 1, 3, false (the second call continues fresh and records its outcome)", sizes[0], len(prefixes[1]), step, replaying)
	}

	// A bound of one does not stop a run of its own prefix and final: [await, delivery] is one run.
	sizes, _, _, step, _ = run(1, two(runPrefixEvent, runFinalEvent), 1)
	if sizes[0] != 2 || step != 2 {
		t.Errorf("[await, delivery] with MaxPrefix 1: read %d records and ended at step %d, want 2 and 2", sizes[0], step)
	}
}

func TestMaxPrefixIsValidatedForItsShape(t *testing.T) {
	var ran int32
	var d, p []EventRecord
	mut := func(f func(sp *stepSpec)) *stepSpec { sp := *runSpec(&ran, &d, &p); f(&sp); return &sp }
	// CONTROL: the same spec with a sensible bound is valid, so the rows below are refused for MaxPrefix and nothing else.
	if err := mut(func(sp *stepSpec) { sp.MaxPrefix = 1 }).validate(); err != nil {
		t.Fatalf("PRECONDITION FAILED: a multi-record spec with MaxPrefix 1 does not validate: %v", err)
	}
	dec := func(*stepCtx, *EventRecord) int64 { return 0 }
	for name, c := range map[string]*stepSpec{
		"a negative bound": mut(func(sp *stepSpec) { sp.MaxPrefix = -1 }),
		"a bound on a single-record step": mut(func(sp *stepSpec) {
			sp.Role = nil
			sp.DecodeRun = nil
			sp.Decode = dec
			sp.MaxPrefix = 1
		}),
	} {
		err := c.validate()
		if err == nil {
			t.Errorf("validate accepted %s", name)
		} else if !strings.Contains(err.Error(), "MaxPrefix") {
			t.Errorf("%s was refused for the wrong reason: %q does not name MaxPrefix", name, err.Error())
		}
	}
}
