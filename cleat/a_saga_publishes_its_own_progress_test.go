package cleat

import (
	"errors"
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
