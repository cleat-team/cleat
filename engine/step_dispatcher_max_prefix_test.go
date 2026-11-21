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

// ForeignFirst (cleat#4152, W5): a call whose FIRST record belongs to another call can be answered without consuming it and without leaving
// replay. Over the fake multi-record spec, each with a control that runs.
func TestAForeignFirstRecordIsAnsweredWithoutConsumingItWhenTheSpecSaysSo(t *testing.T) {
	ctx := context.Background()
	answer := func(a stepArgs) (int64, bool) { return 42, a.i1 <= 0 }
	hist := []EventRecord{runRec(0, runOtherEvent, "x")}
	call := func(sp *stepSpec, a stepArgs, h []EventRecord) (int64, *execSession) {
		s := stepSession(nil, h)
		buf := make([]byte, 256)
		a.outPtr, a.outMaxLen = 0, 256
		return s.step(context.WithValue(ctx, wasmMemBufKey{}, buf), nil, sp, a), s
	}
	var ran int32
	var dec, pre []EventRecord

	// The answer holds: the hook's result, NOTHING consumed, and the session still replaying (the record belongs to the next call).
	sp := runSpec(&ran, &dec, &pre)
	sp.ForeignFirst = answer
	got, s := call(sp, stepArgs{s1: "a", i1: 0}, hist)
	if got != 42 || s.stepCount != 0 || !s.isReplay {
		t.Errorf("a foreign first record with a ForeignFirst that answers: result %d, step %d, replaying %v; want 42, 0, true", got, s.stepCount, s.isReplay)
	}
	// CONTROL: the same call when the hook declines is the ordinary divergence, so the answer above came from the hook and not from the shape.
	got, s = call(sp, stepArgs{s1: "a", i1: 5}, hist)
	if got&0xFF != 1 || s.stepCount != 0 {
		t.Errorf("a foreign first record with a ForeignFirst that declines returned %#x at step %d, want a divergence consuming nothing", got, s.stepCount)
	}
	// CONTROL: and a spec with no hook is the ordinary divergence too.
	got, s = call(runSpec(&ran, &dec, &pre), stepArgs{s1: "a", i1: 0}, hist)
	if got&0xFF != 1 || s.stepCount != 0 {
		t.Errorf("a foreign first record with no ForeignFirst returned %#x at step %d, want a divergence", got, s.stepCount)
	}
	// The hook is for a FOREIGN first record only: a record whose Role is none of the three is a bug in the spec and is a divergence ALWAYS.
	unknown := runSpec(&ran, &dec, &pre)
	unknown.ForeignFirst = answer
	unknown.Role = func(a stepArgs, rec *EventRecord) recordRole { return recordRole(99) }
	got, s = call(unknown, stepArgs{s1: "a", i1: 0}, hist)
	if got&0xFF != 1 || s.stepCount != 0 {
		t.Errorf("an unknown Role with a ForeignFirst that would answer returned %#x at step %d, want a divergence (a spec bug is never a legitimate answer)", got, s.stepCount)
	}
	// A foreign record AFTER the run has begun is not the first record, and the hook is not consulted: [prefix, other] ends without a final as before.
	sp.EndsWithoutFinal = true
	got, s = call(sp, stepArgs{s1: "a", i1: 0}, []EventRecord{runRec(0, runPrefixEvent, "a"), runRec(1, runOtherEvent, "x")})
	if got != 7 || s.stepCount != 1 {
		t.Errorf("[prefix, other]: result %d at step %d, want DecodeRun's 7 at step 1 (the hook is for the first record only)", got, s.stepCount)
	}
	// A SINGLE-record step may carry the hook too (cleat#4152, W1: poll_update). It used to be refused at validation, and the test here pinned that as a mistake;
	// it is the shape a poll needs, so the rule is gone and these rows say what it does instead.
	var ranSingle int32
	single := fakeSpec(effectEffect, recordsOne, &ranSingle, recordOneFake)
	single.ForeignFirst = func(stepArgs) (int64, bool) { return 42, true }
	if err := single.validate(); err != nil {
		t.Errorf("validate on a single-record step with a ForeignFirst returned %v, want nil", err)
	}
	other := []EventRecord{{Step: 0, EventType: EventTypeSideEffect}} // another call's record: not fakeEvent
	sSingle := stepSession(nil, other)
	if got := sSingle.step(ctx, nil, single, stepArgs{}); got != 42 || sSingle.stepCount != 0 || !sSingle.isReplay || ranSingle != 0 {
		t.Errorf("single-record step, another call's record: result %d at step %d, replaying=%v, ran %d; want the hook's 42, nothing consumed, still replaying, Fresh not run",
			got, sSingle.stepCount, sSingle.isReplay, ranSingle)
	}
	// The hook that says "not answerable" leaves the divergence, which consumes nothing.
	single.ForeignFirst = func(stepArgs) (int64, bool) { return 42, false }
	sSingle = stepSession(nil, other)
	if got := sSingle.step(ctx, nil, single, stepArgs{}); got&0xFF != 1 || sSingle.stepCount != 0 || !sSingle.isReplay {
		t.Errorf("single-record step, hook says not answerable: result %#x at step %d, want a divergence consuming nothing", got, sSingle.stepCount)
	}
	// Its OWN record is still consumed and decoded, whatever the hook would say: the hook is for another call's record only.
	own := []EventRecord{{Step: 0, EventType: fakeEvent}}
	single.ForeignFirst = func(stepArgs) (int64, bool) {
		t.Error("the hook was asked about the step's own record")
		return 42, true
	}
	sSingle = stepSession(nil, own)
	if got := sSingle.step(ctx, nil, single, stepArgs{}); got != 7 || sSingle.stepCount != 1 {
		t.Errorf("single-record step, its own record: result %d at step %d, want Decode's 7 at step 1", got, sSingle.stepCount)
	}
	// A single-record step that records NOTHING without declining is still a violation, and one that declines on a path it LISTED is not: the shape check is relaxed only by
	// RecordsNothing under a name in RecordsNothingOn (cleat#4313 made that opt-in; this test used to rely on any RecordsNothing relaxing it).
	forgot := fakeSpec(effectEffect, recordsOne, &ranSingle, func(*stepCtx) {})
	sForgot := stepSession(nil, nil)
	sForgot.step(ctx, nil, forgot, stepArgs{})
	if len(sForgot.stepViolations) != 1 {
		t.Errorf("a single-record step that recorded nothing and did not decline reported %v, want exactly one spec violation", sForgot.stepViolations)
	}
	declines := fakeSpec(effectEffect, recordsOne, &ranSingle, func(c *stepCtx) { c.RecordsNothing("nothing to record") })
	declines.RecordsNothingOn = []string{"nothing to record"}
	sDeclines := stepSession(nil, nil)
	sDeclines.step(ctx, nil, declines, stepArgs{})
	if len(sDeclines.stepViolations) != 0 {
		t.Errorf("a single-record step that declined to record reported %v, want none", sDeclines.stepViolations)
	}
}
