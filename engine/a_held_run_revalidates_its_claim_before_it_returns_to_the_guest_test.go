package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// cleat#4162, PR 2c. A run that is HELD in memory is also a row in the store, and a terminate, a cancel or a reclaim made while it is held changes that row; the
// worker would learn of it only at its next heartbeat. Measured on a real worker (cmd/cleat-worker/a_terminated_held_run_does_not_act_test.go): when the held run's
// timer fired before the heartbeat did, the guest woke and made an outbound call that a suspended run could not have made, because a suspended run has no claim to win
// once the run is terminated. So a held run proves it still owns its claim before it goes back to the guest, from the one place it goes back from (parkOrSuspend).
//
// These tests are about THAT place, with a fake wake source, so that they hold for any wake source and not only the timer: the signal wake source (PR 2b) has to get
// the check without asking for it.

// fixedOutcomeWake is a wake source that answers a fixed outcome and counts how often it was asked (park_test.go has a richer one; this needs only the count).
type fixedOutcomeWake struct {
	out   parkOutcome
	calls int
}

func (w *fixedOutcomeWake) Wait(context.Context, parkRequest) parkOutcome { w.calls++; return w.out }

func sessionWithWake(w wakeSource, check func(context.Context) error) *execSession {
	s := stepSession([]EngineOption{}, nil)
	s.engine.wakeSource = w
	s.engine.claimCheck = check
	return s
}

func TestAHeldRunThatNoLongerHoldsItsClaimIsSuspendedAtItsWakeForEveryKindOfWake(t *testing.T) {
	lost := errors.New("the store has this run assigned to nobody")
	for _, req := range []parkRequest{
		{Kind: wakeTimer, DeadlineMs: 1, Reason: "cleat_sleep(10ms)"},
		{Kind: wakeSignal, Key: "approve", Reason: "waiting for signal approve"},
		{Kind: wakePromise, Key: "p", Reason: "waiting for promise p"},
	} {
		for _, out := range []parkOutcome{parkWoke, parkDeadline} {
			t.Run(req.Kind.String()+"/"+map[parkOutcome]string{parkWoke: "woke", parkDeadline: "deadline"}[out], func(t *testing.T) {
				wake := &fixedOutcomeWake{out: out}
				asked := 0
				s := sessionWithWake(wake, func(context.Context) error { asked++; return lost })
				if !s.canPark() {
					t.Fatalf("PRECONDITION FAILED: the session cannot park, so this measured nothing about the wake")
				}

				got := s.parkOrSuspend(context.Background(), req)

				if wake.calls != 1 {
					t.Fatalf("PRECONDITION FAILED: the wake source was asked %d times, want 1: the run was not held", wake.calls)
				}
				if got != parkRefused {
					t.Errorf("a held run that lost its claim came back as %v, want parkRefused: it would go on to act", got)
				}
				if s.suspendErr == nil {
					t.Errorf("no suspension was set, so the run would not stop at the wait")
				}
				if asked != 1 {
					t.Errorf("the claim was checked %d times, want once", asked)
				}
				if len(s.claimCheckFailures) != 1 || !strings.Contains(s.claimCheckFailures[0], "assigned to nobody") {
					t.Errorf("claimCheckFailures = %v, want the reason recorded once", s.claimCheckFailures)
				}
			})
		}
	}
}

// A claim that could not be checked is treated as lost. The check's error says which it was, and the engine does not read it: both suspend, because suspending is
// always safe and a wrong guess costs one replay, while resuming a run the store no longer has is the hazard.
func TestAHeldRunWhoseClaimCannotBeCheckedIsSuspendedToo(t *testing.T) {
	wake := &fixedOutcomeWake{out: parkDeadline}
	s := sessionWithWake(wake, func(context.Context) error { return context.DeadlineExceeded })

	got := s.parkOrSuspend(context.Background(), parkRequest{Kind: wakeTimer, DeadlineMs: 1, Reason: "cleat_sleep(10ms)"})

	if got != parkRefused || s.suspendErr == nil {
		t.Errorf("an unanswerable claim check let the run resume: outcome %v, suspension set = %v", got, s.suspendErr != nil)
	}
}

// The known positive for the two tests above: a claim that IS still held changes nothing, so the check is not what makes a hold suspend.
func TestAHeldRunThatStillHoldsItsClaimIsResumedAsBefore(t *testing.T) {
	for _, out := range []parkOutcome{parkWoke, parkDeadline} {
		wake := &fixedOutcomeWake{out: out}
		asked := 0
		s := sessionWithWake(wake, func(context.Context) error { asked++; return nil })

		got := s.parkOrSuspend(context.Background(), parkRequest{Kind: wakeTimer, DeadlineMs: 1, Reason: "cleat_sleep(10ms)"})

		if got != out {
			t.Errorf("outcome %v with the claim held, want the wake source's %v", got, out)
		}
		if s.suspendErr != nil {
			t.Errorf("a suspension was set for a run whose claim is held: %v", s.suspendErr)
		}
		if asked != 1 || len(s.claimCheckFailures) != 0 {
			t.Errorf("the claim was checked %d times with %v recorded, want once and nothing recorded", asked, s.claimCheckFailures)
		}
	}
}

// A wait that was never held asks nobody: there is no hold to have outlived anything, and the suspension it sets is the whole answer.
func TestAWaitThatIsRefusedDoesNotAskForTheClaim(t *testing.T) {
	wake := &fixedOutcomeWake{out: parkRefused}
	asked := 0
	s := sessionWithWake(wake, func(context.Context) error { asked++; return nil })

	if got := s.parkOrSuspend(context.Background(), parkRequest{Kind: wakeTimer, DeadlineMs: 1, Reason: "cleat_sleep(10ms)"}); got != parkRefused {
		t.Fatalf("outcome %v, want parkRefused", got)
	}
	if asked != 0 {
		t.Errorf("the claim was checked %d times for a wait that was not held", asked)
	}
}

// No check at all is how every engine without residency runs, and how a test that assigns a wake source runs: unchanged.
func TestAnEngineWithNoClaimCheckBehavesAsBefore(t *testing.T) {
	wake := &fixedOutcomeWake{out: parkWoke}
	s := sessionWithWake(wake, nil)
	if got := s.parkOrSuspend(context.Background(), parkRequest{Kind: wakeTimer, DeadlineMs: 1, Reason: "r"}); got != parkWoke || s.suspendErr != nil {
		t.Errorf("outcome %v, suspension %v, want parkWoke and none", got, s.suspendErr)
	}
}

// The omission cannot be silent. A residency handle with no claim check installs no wake source, so every wait suspends: nothing is ever held that cannot be
// revalidated. The handle is the production wake source, so this is the one way production can reach parkOrSuspend with a hold.
func TestAResidencyHandleWithNoClaimCheckHoldsNothing(t *testing.T) {
	cfg := DefaultResidencyConfig()
	cfg.MaxRuns = 2
	set := newTestResidentSet(t, cfg)

	without := NewEngine(nil, nil, WithResidency(set.ForRun(ResidentRunInfo{Language: "go"})))
	if without.wakeSource != nil || without.claimCheck != nil {
		t.Errorf("a handle with no ClaimCheck installed a wake source (%#v) or a check", without.wakeSource)
	}

	with := NewEngine(nil, nil, WithResidency(set.ForRun(ResidentRunInfo{Language: "go", ClaimCheck: claimStillHeld})))
	if with.wakeSource == nil || with.claimCheck == nil {
		t.Errorf("a handle WITH a ClaimCheck installed no wake source or no check: wake %#v, check set = %v", with.wakeSource, with.claimCheck != nil)
	}
}
