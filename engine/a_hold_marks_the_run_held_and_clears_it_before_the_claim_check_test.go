package engine

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// cleat#4275. A run held through a wait is marked held in the store at admission, and the marker is cleared when the hold ends BEFORE the claim check returns, so that a run the guest is
// about to resume is never marked. That ordering is the safety argument of the fast reclaim: a marked row is reclaimed at the held window, which is only safe for a run that is not
// executing. These tests are about that one place (parkOrSuspend), with a fake wake source, so they hold for any wake source.

// orderRecorder logs the four things a hold does, in the order it does them.
type orderRecorder struct {
	events                 []string
	markErr, clearErr, bad error
}

func (o *orderRecorder) mark(context.Context) error {
	o.events = append(o.events, "mark")
	return o.markErr
}
func (o *orderRecorder) clear(context.Context) error {
	o.events = append(o.events, "clear")
	return o.clearErr
}
func (o *orderRecorder) check(context.Context) error {
	o.events = append(o.events, "claim check")
	return o.bad
}

type recordingWake struct{ o *orderRecorder }

func (w recordingWake) Wait(context.Context, parkRequest) parkOutcome {
	w.o.events = append(w.o.events, "wait")
	return parkWoke
}

func markedSession(o *orderRecorder) *execSession {
	s := sessionWithWake(recordingWake{o}, o.check)
	s.engine.markHeld, s.engine.clearHeld = o.mark, o.clear
	return s
}

func TestAHoldMarksTheRunBeforeItWaitsAndClearsItBeforeTheClaimCheck(t *testing.T) {
	o := &orderRecorder{}
	s := markedSession(o)
	if got := s.parkOrSuspend(context.Background(), oneTimerWait); got != parkWoke {
		t.Fatalf("parkOrSuspend = %v, want parkWoke (failures: %v %v)", got, s.holdPublishFailures, s.claimCheckFailures)
	}
	want := []string{"mark", "wait", "clear", "claim check"}
	if !reflect.DeepEqual(o.events, want) {
		t.Errorf("a hold did %v, want %v: the marker must be cleared BEFORE the claim check returns and the run goes back to its guest", o.events, want)
	}
}

func TestAHoldIsRefusedWhenTheRunCannotBeMarkedHeld(t *testing.T) {
	o := &orderRecorder{markErr: errors.New("the store is unreachable")}
	s := markedSession(o)
	if got := s.parkOrSuspend(context.Background(), oneTimerWait); got != parkRefused {
		t.Errorf("parkOrSuspend = %v, want parkRefused: the run was held with its marker unwritten", got)
	}
	if !reflect.DeepEqual(o.events, []string{"mark"}) {
		t.Errorf("a refused hold did %v, want only the failed mark: the wake source must not be asked", o.events)
	}
	if s.suspendErr == nil || len(s.holdPublishFailures) != 1 {
		t.Errorf("suspendErr=%v holdPublishFailures=%v, want a suspension and the one reason", s.suspendErr, s.holdPublishFailures)
	}
}

// A clear that fails, or is refused because the claim has moved, suspends the run instead of resuming it, and the claim check is not reached: the guest must not run with the marker
// possibly still set.
func TestAWakeWhoseMarkerCannotBeClearedSuspendsInsteadOfResuming(t *testing.T) {
	for name, err := range map[string]error{"the claim moved": ErrFenceLost, "the store could not be reached": errors.New("connection refused")} {
		t.Run(name, func(t *testing.T) {
			o := &orderRecorder{clearErr: err}
			s := markedSession(o)
			if got := s.parkOrSuspend(context.Background(), oneTimerWait); got != parkRefused {
				t.Errorf("parkOrSuspend = %v, want parkRefused: the run went back to its guest with its marker possibly set", got)
			}
			if !reflect.DeepEqual(o.events, []string{"mark", "wait", "clear"}) {
				t.Errorf("the hold did %v, want mark, wait, clear and no claim check", o.events)
			}
			if s.suspendErr == nil || len(s.claimCheckFailures) != 1 {
				t.Errorf("suspendErr=%v claimCheckFailures=%v, want a suspension and the one reason", s.suspendErr, s.claimCheckFailures)
			}
		})
	}
}

// A handle that can mark but cannot clear, or the reverse, installs neither: a mark with no clear would leave a run that went back to its guest looking held for the reaper.
func TestAResidentHandleWithOnlyHalfOfTheMarkerInstallsNeitherHalf(t *testing.T) {
	ok := func(context.Context) error { return nil }
	for name, info := range map[string]ResidentRunInfo{
		"mark only":  {ClaimCheck: ok, MarkHeld: ok},
		"clear only": {ClaimCheck: ok, ClearHeld: ok},
	} {
		t.Run(name, func(t *testing.T) {
			e := &Engine{}
			WithResidency(&ResidentRun{info: info})(e)
			if e.wakeSource == nil {
				t.Fatal("PRECONDITION FAILED: the handle was not installed at all, so this measured nothing about the marker")
			}
			if e.markHeld != nil || e.clearHeld != nil {
				t.Errorf("installed markHeld=%v clearHeld=%v from a handle with only one half", e.markHeld != nil, e.clearHeld != nil)
			}
		})
	}
	e := &Engine{}
	WithResidency(&ResidentRun{info: ResidentRunInfo{ClaimCheck: ok, MarkHeld: ok, ClearHeld: ok}})(e)
	if e.markHeld == nil || e.clearHeld == nil {
		t.Errorf("a handle with both halves installed markHeld=%v clearHeld=%v, want both", e.markHeld != nil, e.clearHeld != nil)
	}
}
