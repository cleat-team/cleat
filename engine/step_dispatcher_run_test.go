package engine

// W2a of cleat#4152: the dispatcher reads a MULTI-RECORD step's run (a spec with a Role). No real host call is on it yet; these are the properties
// the first one must meet, over fake specs, each with a control that runs.

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	runPrefixEvent EventType = "fake_run_prefix"
	runFinalEvent  EventType = "fake_run_final"
	runOtherEvent  EventType = "fake_run_other"
)

func runRec(step int, et EventType, name string) EventRecord {
	return EventRecord{Step: step, EventType: et, ScopeKey: name}
}

// runSpec is a fake multi-record spec: prefix and final records are told apart by event type, and each names itself in ScopeKey (what Identity
// compares). decodeSaw receives the run DecodeRun was handed; prefixSaw the c.Prefix Fresh was handed.
func runSpec(ran *int32, decodeSaw *[]EventRecord, prefixSaw *[]EventRecord) *stepSpec {
	return &stepSpec{
		Name: "fake_run", Effect: effectEffect, Records: recordsMany, Want: runFinalEvent,
		Identity: func(a stepArgs, rec *EventRecord) string {
			if rec.ScopeKey != a.s1 {
				return "name " + a.s1 + ", recorded " + rec.ScopeKey
			}
			return ""
		},
		Role: func(a stepArgs, rec *EventRecord) recordRole {
			switch rec.EventType {
			case runPrefixEvent:
				return rolePrefix
			case runFinalEvent:
				return roleFinal
			}
			return roleForeign
		},
		DecodeRun: func(c *stepCtx, run []EventRecord) int64 {
			*decodeSaw = append([]EventRecord(nil), run...)
			return 7
		},
		Fresh: func(c *stepCtx) int64 {
			atomic.AddInt32(ran, 1)
			*prefixSaw = c.Prefix
			c.Record(EventRecord{Step: c.s.stepCount, EventType: runFinalEvent, ScopeKey: c.args.s1})
			return 9
		},
	}
}

func TestAMultiRecordStepReadsItsWholeRun(t *testing.T) {
	ctx := context.Background()
	hist := func() []EventRecord {
		return []EventRecord{runRec(0, runPrefixEvent, "a"), runRec(1, runPrefixEvent, "a"), runRec(2, runFinalEvent, "a")}
	}
	var ran int32
	var dec, pre []EventRecord
	s := stepSession(nil, hist())
	if got := s.step(ctx, nil, runSpec(&ran, &dec, &pre), stepArgs{s1: "a"}); got != 7 {
		t.Fatalf("a complete run returned %d, want DecodeRun's 7", got)
	}
	if len(dec) != 3 || dec[2].EventType != runFinalEvent {
		t.Errorf("DecodeRun was handed %d records (last %v), want the whole run of 3 ending in the final", len(dec), dec)
	}
	if s.stepCount != 3 || !s.isReplay || ran != 0 {
		t.Errorf("after a complete run: step=%d isReplay=%v fresh ran %d times, want 3, true (the NEXT call leaves), 0", s.stepCount, s.isReplay, ran)
	}
	if len(s.stepViolations) != 0 {
		t.Errorf("a complete run reported %v", s.stepViolations)
	}
	// A run of one record (just the final) is a run too.
	one := stepSession(nil, []EventRecord{runRec(0, runFinalEvent, "a")})
	var dec1, pre1 []EventRecord
	if got := one.step(ctx, nil, runSpec(&ran, &dec1, &pre1), stepArgs{s1: "a"}); got != 7 || len(dec1) != 1 || one.stepCount != 1 {
		t.Errorf("a run of just the final returned %d with %d records at step %d, want 7, 1, 1", got, len(dec1), one.stepCount)
	}
}

func TestAHistoryThatEndsInsideARunContinuesFreshFromItsPrefix(t *testing.T) {
	ctx := context.Background()
	var ran int32
	var dec, pre []EventRecord
	s := stepSession(nil, []EventRecord{runRec(0, runPrefixEvent, "a"), runRec(1, runPrefixEvent, "a")})
	if got := s.step(ctx, nil, runSpec(&ran, &dec, &pre), stepArgs{s1: "a"}); got != 9 {
		t.Fatalf("a history ending inside its run returned %d, want Fresh's 9", got)
	}
	if len(pre) != 2 || ran != 1 || len(dec) != 0 {
		t.Errorf("Fresh saw %d prefix records and ran %d times, DecodeRun ran with %d records; want 2, 1, 0", len(pre), ran, len(dec))
	}
	if s.isReplay {
		t.Error("replay was not left when the history ended inside the run")
	}
	if len(s.stepViolations) != 0 {
		t.Errorf("leaving replay after consuming the prefix reported %v: the prefix was not consumed in full", s.stepViolations)
	}
	// And the refusal applies at that boundary, after the prefix is consumed: nothing runs, the prefix is not re-read.
	var ran2 int32
	var dec2, pre2 []EventRecord
	stopping := stepSession([]EngineOption{WithDeferPhase()}, []EventRecord{runRec(0, runPrefixEvent, "a")})
	if got := stopping.step(ctx, nil, runSpec(&ran2, &dec2, &pre2), stepArgs{s1: "a"}); got != callSuspendSentinel || ran2 != 0 {
		t.Errorf("a stopping session ending inside a run returned %#x after %d runs, want the stop sentinel and 0", got, ran2)
	}
	// CONTROL: the same history in a quiet session ran Fresh (above), so the refusal is the refusal.
}

func TestAForeignRecordInARunIsADivergenceThatConsumesNothing(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		hist  []EventRecord
		wantN string
	}{
		{"first", []EventRecord{runRec(0, runOtherEvent, "a")}, "step 0"},
		{"after a prefix", []EventRecord{runRec(0, runPrefixEvent, "a"), runRec(1, runOtherEvent, "a")}, "step 1"},
		{"after two", []EventRecord{runRec(0, runPrefixEvent, "a"), runRec(1, runPrefixEvent, "a"), runRec(2, runOtherEvent, "a")}, "step 2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var ran int32
			var dec, pre []EventRecord
			s := stepSession(nil, c.hist)
			got := s.step(ctx, nil, runSpec(&ran, &dec, &pre), stepArgs{s1: "a"})
			if got&0xFF != 1 {
				t.Fatalf("returned %#x, want errCode 1 (a divergence)", got)
			}
			if s.stepCount != 0 || !s.isReplay || len(dec) != 0 || ran != 0 {
				t.Errorf("a divergence consumed or ran something: step=%d isReplay=%v decode=%d fresh=%d, want 0, true, 0, 0", s.stepCount, s.isReplay, len(dec), ran)
			}
		})
	}
	// The message names the FOREIGN record, not the first of the run.
	var ran int32
	var dec, pre []EventRecord
	s := stepSession(nil, []EventRecord{runRec(0, runPrefixEvent, "a"), runRec(1, runOtherEvent, "a")})
	buf := make([]byte, 256)
	ctxb := context.WithValue(ctx, wasmMemBufKey{}, buf)
	got := s.step(ctxb, nil, runSpec(&ran, &dec, &pre), stepArgs{s1: "a", outPtr: 0, outMaxLen: 256})
	msg := string(buf[:int(got>>32)])
	if !strings.Contains(msg, "step 1") || !strings.Contains(msg, string(runOtherEvent)) {
		t.Errorf("the divergence message %q does not name the foreign record at step 1 (%s)", msg, runOtherEvent)
	}
}

func TestARunEndsWithoutAFinalOnlyWhenTheSpecSaysSo(t *testing.T) {
	ctx := context.Background()
	hist := func() []EventRecord {
		return []EventRecord{runRec(0, runPrefixEvent, "a"), runRec(1, runOtherEvent, "a")}
	}
	var ran int32
	var dec, pre []EventRecord
	sp := runSpec(&ran, &dec, &pre)
	sp.EndsWithoutFinal = true
	s := stepSession(nil, hist())
	if got := s.step(ctx, nil, sp, stepArgs{s1: "a"}); got != 7 {
		t.Fatalf("a prefix followed by a foreign record, EndsWithoutFinal, returned %#x, want DecodeRun's 7", got)
	}
	if len(dec) != 1 || dec[0].EventType != runPrefixEvent || s.stepCount != 1 {
		t.Errorf("DecodeRun was handed %d records at step %d, want just the prefix (1) and the foreign record left unconsumed (step 1)", len(dec), s.stepCount)
	}
	// CONTROL: the same history without the flag is a divergence (TestAForeignRecordInARunIsADivergenceThatConsumesNothing).
	var ran2 int32
	var dec2, pre2 []EventRecord
	plain := stepSession(nil, hist())
	if got := plain.step(ctx, nil, runSpec(&ran2, &dec2, &pre2), stepArgs{s1: "a"}); got&0xFF != 1 || plain.stepCount != 0 {
		t.Errorf("CONTROL: without EndsWithoutFinal the same history returned %#x at step %d, want a divergence at step 0", got, plain.stepCount)
	}
}

func TestIdentityIsCheckedOnEveryRecordOfARunBeforeAnyIsConsumed(t *testing.T) {
	ctx := context.Background()
	var ran int32
	var dec, pre []EventRecord
	// The FIRST record names another key: it used to escape the comparison (cleat#4211, SetScope), so it is checked too.
	for _, c := range []struct {
		name string
		hist []EventRecord
	}{
		{"first record", []EventRecord{runRec(0, runPrefixEvent, "OTHER"), runRec(1, runFinalEvent, "a")}},
		{"middle record", []EventRecord{runRec(0, runPrefixEvent, "a"), runRec(1, runPrefixEvent, "OTHER"), runRec(2, runFinalEvent, "a")}},
		{"final record", []EventRecord{runRec(0, runPrefixEvent, "a"), runRec(1, runFinalEvent, "OTHER")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := stepSession(nil, c.hist)
			got := s.step(ctx, nil, runSpec(&ran, &dec, &pre), stepArgs{s1: "a"})
			if got&0xFF != 1 || s.stepCount != 0 || len(dec) != 0 {
				t.Errorf("a record naming another key returned %#x, step=%d, decoded %d records; want a divergence consuming nothing", got, s.stepCount, len(dec))
			}
		})
	}
	// CONTROL: every record naming this call's key replays.
	s := stepSession(nil, []EventRecord{runRec(0, runPrefixEvent, "a"), runRec(1, runPrefixEvent, "a"), runRec(2, runFinalEvent, "a")})
	if got := s.step(ctx, nil, runSpec(&ran, &dec, &pre), stepArgs{s1: "a"}); got != 7 {
		t.Errorf("CONTROL: a run naming this call's key everywhere returned %#x, want 7", got)
	}
}

func TestAMultiRecordSpecIsValidatedForItsShape(t *testing.T) {
	var ran int32
	var d, p []EventRecord
	good := runSpec(&ran, &d, &p)
	if err := good.validate(); err != nil {
		t.Fatalf("PRECONDITION FAILED: the fake multi-record spec does not validate: %v", err)
	}
	mut := func(f func(sp *stepSpec)) *stepSpec { sp := *runSpec(&ran, &d, &p); f(&sp); return &sp }
	dec := func(*stepCtx, *EventRecord) int64 { return 0 }
	// Each fixture is built to reach ITS rule, and the error is asserted by TEXT: a spec refused for the wrong reason is a failure, because then
	// the rule under test decided nothing and its removal would go unseen (cleat-review3, on the hooks' validation test).
	for name, c := range map[string]struct {
		spec *stepSpec
		want string
	}{
		"a Role and a Decode":          {mut(func(sp *stepSpec) { sp.Decode = dec }), "needs exactly a DecodeRun"},
		"a Role and no DecodeRun":      {mut(func(sp *stepSpec) { sp.DecodeRun = nil }), "needs exactly a DecodeRun"},
		"a Role that records nothing":  {mut(func(sp *stepSpec) { sp.Records = recordsNone }), "records nothing"},
		"no Role but a DecodeRun":      {mut(func(sp *stepSpec) { sp.Role = nil; sp.Decode = dec }), "single-record step"},
		"no Role but EndsWithoutFinal": {mut(func(sp *stepSpec) { sp.Role = nil; sp.DecodeRun = nil; sp.EndsWithoutFinal = true; sp.Decode = dec }), "single-record step"},
		"no Role and no Decode either": {mut(func(sp *stepSpec) { sp.Role = nil; sp.DecodeRun = nil }), "single-record step"},
	} {
		err := c.spec.validate()
		if err == nil {
			t.Errorf("CONTROL: validate accepted a spec with %s", name)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("CONTROL: a spec with %s was refused for the wrong reason: %q does not name %q", name, err.Error(), c.want)
		}
	}
}
