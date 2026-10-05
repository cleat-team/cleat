package cleat

import (
	"errors"
	"strings"
	"testing"
)

// A saga publishes what it is doing, so an author does not have to write
// SetQueryState at every step boundary. cleat#2627 half (i).
//
// THE TESTS LIVE HERE RATHER THAN IN order-lifecycle, and that is a deliberate
// placement: the mechanism is the SDK's, order-lifecycle is its first consumer,
// and the scenario's line count is itself an acceptance number (see
// scripts/dbos-pair-loc.sh). A test added there would be correct and would
// quietly spend part of the saving it is testing.
//
// The example's own tests still cover the published contract end to end -- they
// assert status, failed_step, compensated and unwind_failed -- which is why
// they pass unchanged across this change. What they do NOT cover is
// `current_step`, which nothing asserted before, so it is pinned here.

// collectQueryState returns a HostCalls and a pointer to the key/value pairs it
// publishes, in order.
func collectQueryState() (HostCalls, *[]string) {
	var published []string
	h := NewHostCalls(HostCallsOptions{
		DurableLog:    func(string) {},
		SetQueryState: func(k, v string) { published = append(published, k+"="+v) },
	})
	return h, &published
}

func TestASagaRunPublishesTheStepItIsAboutToRun(t *testing.T) {
	h, published := collectQueryState()

	s := NewSaga()
	s.AddStep("reserve", func(HostCalls) (string, error) { return "", nil }, nil)
	s.AddStep("charge", func(HostCalls) (string, error) { return "", nil }, nil)

	res, err := s.RunWithResult(h)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Before each step's Forward, not after: a poller that catches the run
	// mid-step should see the step it is IN, and a run that dies inside a step
	// should have published that step rather than the previous one.
	want := []string{"current_step=reserve", "current_step=charge"}
	if len(*published) < len(want) {
		t.Fatalf("published %v, want it to start with %v", *published, want)
	}
	for i, w := range want {
		if (*published)[i] != w {
			t.Errorf("publish %d = %q, want %q", i, (*published)[i], w)
		}
	}

	if !contains(*published, "status=done") {
		t.Errorf("published %v, want status=done on a completed run", *published)
	}
	if !contains(*published, "compensated=") {
		t.Errorf("published %v, want compensated cleared on a completed run -- a poller that "+
			"read it before must still find the key", *published)
	}

	if got := res.Completed; len(got) != 2 || got[0] != "reserve" || got[1] != "charge" {
		t.Errorf("Completed = %v, want [reserve charge] in execution order", got)
	}
	if len(res.Unwound) != 0 || len(res.UnwindFailed) != 0 {
		t.Errorf("a completed saga unwound nothing, got Unwound=%v UnwindFailed=%v", res.Unwound, res.UnwindFailed)
	}
}

func TestASagaRunPublishesTheFailureAndDistinguishesUnwoundFromUnwindFailed(t *testing.T) {
	h, published := collectQueryState()

	s := NewSaga()
	s.AddStep("reserve", func(HostCalls) (string, error) { return "", nil },
		func(HostCalls) error { return nil })
	s.AddStep("charge", func(HostCalls) (string, error) { return "", nil },
		// Runs AND FAILS: the step is not unwound, and the two lists must differ.
		func(HostCalls) error { return errors.New("refund declined") })
	s.AddStep("dispatch", func(HostCalls) (string, error) { return "", errors.New("no driver") }, nil)

	res, err := s.RunWithResult(h)
	if err == nil {
		t.Fatal("the dispatch step failed, so RunWithResult should have returned an error")
	}

	if !contains(*published, "failed_step=dispatch") {
		t.Errorf("published %v, want failed_step=dispatch -- the step whose Forward returned an error", *published)
	}
	if !contains(*published, "status=failed") {
		t.Errorf("published %v, want status=failed", *published)
	}
	if !contains(*published, "compensated=reserve") {
		t.Errorf("published %v, want compensated=reserve: the reservation's release ran and succeeded", *published)
	}
	if !contains(*published, "unwind_failed=charge") {
		t.Errorf("published %v, want unwind_failed=charge: the refund RAN and FAILED, which is the "+
			"line an operator has to act on and is not the same as unwound", *published)
	}

	if got := res.Completed; len(got) != 2 {
		t.Errorf("Completed = %v, want the two steps whose forwards succeeded", got)
	}
	if got := res.Unwound; len(got) != 1 || got[0] != "reserve" {
		t.Errorf("Unwound = %v, want [reserve] only", got)
	}
	if got := res.UnwindFailed; len(got) != 1 || got[0] != "charge" {
		t.Errorf("UnwindFailed = %v, want [charge] only -- and deliberately disjoint from Unwound, "+
			"because a compensation that fails has not undone its step", got)
	}
}

// The owner's decision on cleat#2627 was to leave Run's signature alone: every
// existing caller -- ten of them in this tree, plus any external user -- gets
// the publishing without changing a line. That promise is a behaviour, so it is
// pinned rather than assumed.
func TestARunThatIgnoresTheResultStillPublishes(t *testing.T) {
	h, published := collectQueryState()

	s := NewSaga()
	s.AddStep("only_step", func(HostCalls) (string, error) { return "", nil }, nil)

	if err := s.Run(h); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !contains(*published, "current_step=only_step") {
		t.Errorf("published %v; Run's unchanged signature must still publish current_step, "+
			"or the ten call sites that never asked for a result get nothing", *published)
	}
	if !contains(*published, "status=done") {
		t.Errorf("published %v, want status=done from Run as well as RunWithResult", *published)
	}
}

// cleat#2967. current_step self-heals across a second saga on the same
// HostCalls -- the next run overwrites it on its own first step -- but
// failed_step and unwind_failed had nothing that overwrote them, so a saga
// that ran and succeeded AFTER an earlier failed one published status=done
// beside the FIRST saga's failed_step and unwind_failed. A poller reading
// that combination got a confident wrong answer.
//
// lastValue, not contains: a real query-state store overwrites a key on
// every SetQueryState, so what a poller sees after both runs is the LAST
// published value for each key, not whether some value was ever published --
// and "failed_step=dispatch" is in the log from the first run regardless of
// whether the bug this test exists for is fixed.
func TestASagaThatSucceedsAfterAFailedOneClearsTheEarlierFailureKeys(t *testing.T) {
	h, published := collectQueryState()

	failing := NewSaga()
	failing.AddStep("reserve", func(HostCalls) (string, error) { return "", nil },
		func(HostCalls) error { return nil })
	failing.AddStep("charge", func(HostCalls) (string, error) { return "", nil },
		// Compensate FAILS, so unwind_failed is non-empty -- the exact shape
		// that must be cleared by the second, successful saga below.
		func(HostCalls) error { return errors.New("refund declined") })
	failing.AddStep("dispatch", func(HostCalls) (string, error) { return "", errors.New("no driver") }, nil)

	if _, err := failing.RunWithResult(h); err == nil {
		t.Fatal("the dispatch step failed, so RunWithResult should have returned an error")
	}
	// PRECONDITION: the first saga must actually have published the stale
	// values the second half of this test checks are cleared -- a seed that
	// silently failed would make the assertions below pass for no reason.
	if last := lastValue(*published, "failed_step"); last != "failed_step=dispatch" {
		t.Fatalf("PRECONDITION FAILED: last published failed_step is %q, want \"failed_step=dispatch\"", last)
	}
	if last := lastValue(*published, "unwind_failed"); last != "unwind_failed=charge" {
		t.Fatalf("PRECONDITION FAILED: last published unwind_failed is %q, want \"unwind_failed=charge\"", last)
	}

	succeeding := NewSaga()
	succeeding.AddStep("ship", func(HostCalls) (string, error) { return "", nil }, nil)

	if _, err := succeeding.RunWithResult(h); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if last := lastValue(*published, "status"); last != "status=done" {
		t.Errorf("last published status is %q, want \"status=done\"", last)
	}
	if last := lastValue(*published, "failed_step"); last != "failed_step=" {
		t.Errorf("last published failed_step is %q, want it cleared to \"failed_step=\" -- "+
			"a poller would otherwise see the FIRST saga's failed step beside a status that says "+
			"the run is fine", last)
	}
	if last := lastValue(*published, "unwind_failed"); last != "unwind_failed=" {
		t.Errorf("last published unwind_failed is %q, want it cleared to \"unwind_failed=\"", last)
	}
	if last := lastValue(*published, "unwind_failed_count"); last != "unwind_failed_count=0" {
		t.Errorf("last published unwind_failed_count is %q, want \"unwind_failed_count=0\"", last)
	}
}

// contains is an EXACT match, deliberately. Every published pair this file
// asserts on is published in full, so a prefix or substring match would be
// looser than the claim being made -- "failed_step=dispatch_extra" would satisfy
// a prefix test for "failed_step=dispatch", and the assertion would then be
// reporting a value the saga never published.
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// lastValue returns the last "key=value" entry in haystack for the given key,
// or "" if the key was never published -- the last-write-wins read a real
// query-state store would give a poller, as opposed to contains' "was this
// ever published" question.
func lastValue(haystack []string, key string) string {
	prefix := key + "="
	var last string
	for _, s := range haystack {
		if strings.HasPrefix(s, prefix) {
			last = s
		}
	}
	return last
}
