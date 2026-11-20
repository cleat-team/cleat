package engine

import (
	"context"
	"reflect"
	"testing"
)

// cleat#4211, the lock half. A replay of AcquireLock or ReleaseLock used to accept ANY recorded event of its own type at the step: a workflow whose code
// now takes lock "b" where its history took lock "a" consumed the record for "a" and was answered as though it held "b". The lock key is the one field
// of the call that cannot legitimately differ between a run and its replay, so a record that names another key is a divergence, reported once and not
// consumed, as every other replay reports one (cleat#4157, #4201).
//
// Both halves are here because "diverges over a different key" is also true of a call that diverges over everything. The own-key rows consume exactly
// that one step and count nothing, and they use the SAME recorded events as the different-key rows, differing only in the key the guest asks for.

func TestAReplayedLockCallForADifferentKeyDiverges(t *testing.T) {
	ctx := context.Background()
	rows := []struct {
		name  string
		event EventRecord
		call  func(s *execSession, key string) int64
		// own is what the call returns over a record for ITS key; "asked a different key" must NOT return this.
		own int64
	}{
		{"AcquireLock, acquired", EventRecord{Step: 0, EventType: EventTypeAcquireLock, LockKey: "a", LockTTLMs: 1000, LockAcquired: 1},
			func(s *execSession, key string) int64 { return s.AcquireLock(ctx, nil, key, 1000) }, packAcquireLockResult(true, 0)},
		// A record of a FAILED call names its key too, and the identity is checked before the error is decoded: a different key is a divergence
		// and not "the lock call failed".
		{"AcquireLock, recorded failure", EventRecord{Step: 0, EventType: EventTypeAcquireLock, LockKey: "a", LockTTLMs: 1000, Err: "boom"},
			func(s *execSession, key string) int64 { return s.AcquireLock(ctx, nil, key, 1000) }, packAcquireLockResult(false, 1)},
		{"ReleaseLock", EventRecord{Step: 0, EventType: EventTypeReleaseLock, LockKey: "a"},
			func(s *execSession, key string) int64 { return s.ReleaseLock(ctx, nil, key) }, 0},
		{"ReleaseLock, recorded failure", EventRecord{Step: 0, EventType: EventTypeReleaseLock, LockKey: "a", Err: "boom"},
			func(s *execSession, key string) int64 { return s.ReleaseLock(ctx, nil, key) }, 1},
	}
	for _, r := range rows {
		t.Run(r.name+"/a different key diverges and consumes nothing", func(t *testing.T) {
			s := replaySessionOver([]EventRecord{r.event})
			m := newReplayMetrics(t)
			s.engine.Metrics = m
			if got := r.call(s, "b"); got != 1 {
				t.Errorf("asking for lock %q over a record for %q returned %#x, want 0x1 (error code 1, a divergence)", "b", "a", got)
			}
			if s.stepCount != 0 || !s.isReplay {
				t.Errorf("stepCount = %d, isReplay = %v: a record that names another lock must be left for the call it belongs to, with replay still on", s.stepCount, s.isReplay)
			}
			if n := replayFailureCount(t, m); n != 1 {
				t.Errorf("cleat_replay_failures_total = %d, want 1: the divergence was not counted", n)
			}
			if len(s.stepViolations) != 0 {
				t.Errorf("a spec reported itself in violation: %v", s.stepViolations)
			}
		})
		t.Run(r.name+"/its own key is consumed and counts nothing", func(t *testing.T) {
			s := replaySessionOver([]EventRecord{r.event})
			m := newReplayMetrics(t)
			s.engine.Metrics = m
			if got := r.call(s, "a"); got != r.own {
				t.Errorf("asking for lock %q over a record for %q returned %#x, want %#x", "a", "a", got, r.own)
			}
			if s.stepCount != 1 {
				t.Errorf("stepCount = %d, want 1: the call over its own record must consume exactly that step", s.stepCount)
			}
			if n := replayFailureCount(t, m); n != 0 {
				t.Errorf("cleat_replay_failures_total = %d over a matching record, want 0", n)
			}
		})
	}
}

// A release is the one step the dispatcher does NOT refuse (`Refuse: neverRefuse`), and the reason is measured in
// a_lock_call_in_each_stop_state_test.go. This is the dispatcher-level statement of it, with a default step run in the same states as the control, so
// that "was not refused" cannot be read from a state that refuses nothing.
func TestAStepThatNeverRefusesIsNotRefusedWhereADefaultStepIs(t *testing.T) {
	ctx := context.Background()
	closedShutdown := make(chan struct{})
	close(closedShutdown)
	reasons := []struct {
		name string
		opts []EngineOption
		mod  func(*execSession)
	}{
		{"a defer segment's body", []EngineOption{WithDeferPhase()}, nil},
		{"a recorded event that could not be written", nil, func(s *execSession) { s.eventWriteFailure = context.DeadlineExceeded }},
		{"graceful shutdown", []EngineOption{WithShutdownSignal(closedShutdown)}, nil},
		{"a defer draining on a shutting-down worker", []EngineOption{WithShutdownSignal(closedShutdown)}, func(s *execSession) { s.inDeferDrain = true }},
	}
	for _, r := range reasons {
		for _, arrival := range []string{"fresh entry", "replay frontier"} {
			history := func() []EventRecord {
				if arrival == "fresh entry" {
					return nil
				}
				return []EventRecord{{Step: 0, EventType: fakeEvent}}
			}
			t.Run(r.name+"/"+arrival, func(t *testing.T) {
				run := func(never bool) (got int64, ran int32) {
					s := stepSession(r.opts, history())
					if r.mod != nil {
						r.mod(s)
					}
					spec := fakeSpec(effectEffect, recordsOne, &ran, recordOneFake)
					if never {
						spec.Refuse = neverRefuse
					}
					if len(s.history) > 0 {
						// Consume the record the replay frontier starts on, so the next call is the first one PAST the history.
						if first := s.step(ctx, nil, spec, stepArgs{}); first != 7 {
							t.Fatalf("PRECONDITION FAILED: the step over its own record returned %d, want the decoded 7", first)
						}
						ran = 0
					}
					return s.step(ctx, nil, spec, stepArgs{}), ran
				}
				if got, ran := run(true); got != 9 || ran != 1 {
					t.Errorf("a step with Refuse: neverRefuse in %q: returned %d, ran %d times; want it to run once and return its own 9", r.name, got, ran)
				}
				// The control, same state, same fake: a default step (Refuse nil, so stopBeforeNewWork) IS refused in every state listed, the
				// shutdown drain included, because shutdown is the third condition of stopBeforeNewWork.
				if got, ran := run(false); got != callSuspendSentinel || ran != 0 {
					t.Errorf("CONTROL: a default step in %q returned %d, ran %d times; want the stop sentinel and no run, or this state refuses nothing and the row above proves nothing", r.name, got, ran)
				}
			})
		}
	}
}

// The scans that decide what a host stop site is read the SOURCE, and the source says which specs are never refused. This holds that reading to the
// registry, which has the actual values: two derivations that can disagree. The negative controls are the behaviour before the scan knew the class,
// which is what reported ReleaseLock as a call the SDKs must guard and put the Java count at 29 when the true number is 28.
func TestTheStopSiteScanReadsNeverRefuseFromTheSpecDeclaration(t *testing.T) {
	fromSource := neverRefusedSpecs(t)
	// Function values can be compared only by their code pointer, which is what makes this a reading of the VALUES and not of the same text.
	neverPtr := reflect.ValueOf(neverRefuse).Pointer()
	byValue := 0
	for _, sp := range stepSpecs {
		if sp.Refuse != nil && reflect.ValueOf(sp.Refuse).Pointer() == neverPtr {
			byValue++
		}
	}
	if len(fromSource) != byValue {
		t.Errorf("the scan found %d never-refused spec declaration(s) in the source %v, the registry holds %d: one of the two readings is wrong", len(fromSource), fromSource, byValue)
	}
	// A spec that is never REFUSED but can still answer with the stop is a host STOP site (W4, cleat#4152): the two sets differ by exactly the specs that
	// suspend. awaitPromiseStep is in the first and must NOT be in the second, which is the set hostStopSites reads; the refusal counts read the first.
	if !fromSource["awaitPromiseStep"] || neverStopSpecs(t)["awaitPromiseStep"] {
		t.Errorf("awaitPromiseStep: in the never-refused set %v, in the never-a-stop-site set %v; want true and false: it never refuses but it suspends, so it is a stop site, and not a refusal site",
			fromSource["awaitPromiseStep"], neverStopSpecs(t)["awaitPromiseStep"])
	}
	if !neverStopSpecs(t)["releaseLockStep"] {
		t.Errorf("releaseLockStep is not in the never-a-stop-site set: it never refuses and never suspends, so it is no stop site at all (the Java count of 28)")
	}
	for _, c := range []struct {
		name  string
		decls map[string]string
		want  map[string]bool
	}{
		{"a plain neverRefuse spec is in the never-a-stop-site set", map[string]string{"a": "Refuse: neverRefuse,\n Fresh: func(c *stepCtx) int64 { return 0 }"}, map[string]bool{"a": true}},
		{"KNOWN-POSITIVE: a neverRefuse spec that returns suspendingAwaitResult() is a site, so it is out", map[string]string{"a": "Refuse: neverRefuse,\n Fresh: func(c *stepCtx) int64 { return c.s.suspendingAwaitResult() }"}, map[string]bool{}},
		{"a neverRefuse spec that returns the sentinel itself is a site, so it is out", map[string]string{"a": "Refuse: neverRefuse,\n Fresh: func(c *stepCtx) int64 { return callSuspendSentinel }"}, map[string]bool{}},
		{"a spec with any other Refuse is not in the set", map[string]string{"a": "Refuse: someOther,\n Fresh: func(c *stepCtx) int64 { return 0 }"}, map[string]bool{}},
		{"a spec that merely names the call without calling it is still in the set", map[string]string{"a": "Refuse: neverRefuse,\n Fresh: func(c *stepCtx) int64 { f := c.s.suspendingAwaitResult; _ = f; return 0 }"}, map[string]bool{"a": true}},
	} {
		got := neverStopIn(c.decls)
		if len(got) != len(c.want) || got["a"] != c.want["a"] {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	if !fromSource["releaseLockStep"] {
		t.Errorf("the scan did not find releaseLockStep, which is declared `Refuse: neverRefuse`: %v", fromSource)
	}
	for _, refused := range []string{"acquireLockStep", "createPromiseStep", "resolvePromiseStep", "rejectPromiseStep"} {
		if fromSource[refused] {
			t.Errorf("the scan reports %s as never refused; it is refused by the dispatcher", refused)
		}
	}
	if releaseLockStep.Refuse == nil || reflect.ValueOf(releaseLockStep.Refuse).Pointer() != neverPtr || acquireLockStep.Refuse != nil {
		t.Errorf("the registry's values disagree with the declarations the scan read: release Refuse set %v, acquire Refuse set %v", releaseLockStep.Refuse != nil, acquireLockStep.Refuse != nil)
	}

	// THE SET MUST DECIDE SOMETHING (W4, cleat#4152). In the real tree awaitPromiseStep follows its wrapper, so the scan attributes the spec's own
	// suspendingAwaitResult() to AwaitPromise by adjacency and the right answer comes out whichever set it reads; a check that never decides anything
	// cannot be seen to be wrong. Here the spec is declared BEFORE the call, where no adjacency reaches it, so only the set can make the call a site:
	// a spec that never refuses but suspends is a stop site, and one that never refuses and never suspends is not.
	suspends := "package p\n\nvar zStep = stepSpec{\n\tRefuse: neverRefuse,\n\tFresh: func(c *stepCtx) int64 { return c.s.suspendingAwaitResult() },\n}\n\n"
	quiet := "package p\n\nvar zStep = stepSpec{\n\tRefuse: neverRefuse,\n\tFresh: func(c *stepCtx) int64 { return 0 },\n}\n\n"
	hostCall := "func (s *execSession) Z() int64 {\n\treturn s.step(ctx, m, &zStep, stepArgs{})\n}\n"
	for _, c := range []struct {
		name string
		src  string
		want bool
	}{
		{"KNOWN-POSITIVE: a never-refused spec that suspends, declared before its call, is a stop site", suspends + hostCall, true},
		{"a never-refused spec that never suspends is not a stop site", quiet + hostCall, false},
	} {
		if got := hostStopSitesIn(map[string]string{"z.go": c.src})["Z"]; got != c.want {
			t.Errorf("%s: Z is a stop site = %v, want %v", c.name, got, c.want)
		}
	}
	// CONTROL: the suspending spec DECLARES neverRefuse, so a scan that read the declared set (as it did before W4) would have missed it. The two
	// sets differ on exactly this spec, which is what the rows above are about.
	if d := specDeclsIn(map[string]string{"z.go": suspends}); !declaresNeverRefuse(d["zStep"]) || neverStopIn(d)["zStep"] {
		t.Errorf("the suspending spec declares neverRefuse %v and is in the never-a-stop-site set %v; want true and false", declaresNeverRefuse(d["zStep"]), neverStopIn(d)["zStep"])
	}

	call := func(spec string) string {
		return "func (s *execSession) X() int64 {\n\treturn s.step(ctx, m, &" + spec + ", stepArgs{})\n}\n"
	}
	for _, c := range []struct {
		name      string
		body      string
		unrefused map[string]bool
		want      bool
	}{
		{"a never-refused spec is not a site", call("releaseLockStep"), fromSource, false},
		{"CONTROL, the behaviour before the scan knew the class: the same call IS counted", call("releaseLockStep"), map[string]bool{}, true},
		{"a refusable spec is a site", call("acquireLockStep"), fromSource, true},
		{"a spec the scan cannot read is a site (over-count fails loudly, an under-count would not)", "func (s *execSession) X() int64 {\n\treturn s.step(ctx, m, someVariable, stepArgs{})\n}\n", fromSource, true},
		{"no step call is not a site", "func (s *execSession) X() int64 {\n\treturn 0\n}\n", fromSource, false},
		{"a comment quoting the call is not a site", "func (s *execSession) X() int64 {\n\t// s.step(ctx, m, &acquireLockStep, args) would refuse\n\treturn 0\n}\n", fromSource, false},
	} {
		if got := goesThroughStep(c.body, c.unrefused); got != c.want {
			t.Errorf("%s: goesThroughStep = %v, want %v", c.name, got, c.want)
		}
	}
}
