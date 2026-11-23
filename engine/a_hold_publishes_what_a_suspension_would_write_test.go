package engine

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// cleat#4162. A run that is HELD through a wait does not return to the worker, and the worker writes a run's query state only when its execution returns, so what the guest set
// before the wait was invisible to every client for the whole hold. parkOrSuspend therefore publishes it first (publishForHold) and REFUSES the hold if it cannot, because a refusal
// suspends and a suspension writes it. These tests are about that one place, with a fake wake source, so they hold for any wake source.

// publishRecorder stands in for the store's PublishQueryState and records, for each call, what it was given and how many times the wake source had been asked at that moment: a publish
// that happens AFTER the wait began is not a publish "before the hold".
type publishRecorder struct {
	wake        *fixedOutcomeWake
	got         []map[string]string
	wakeCallsAt []int
	err         error
}

func (p *publishRecorder) publish(_ context.Context, state map[string]string) error {
	p.got = append(p.got, state)
	p.wakeCallsAt = append(p.wakeCallsAt, p.wake.calls)
	return p.err
}

func heldSessionWithPublisher(t *testing.T, state map[string]string) (*execSession, *fixedOutcomeWake, *publishRecorder) {
	t.Helper()
	wake := &fixedOutcomeWake{out: parkWoke}
	s := sessionWithWake(wake, func(context.Context) error { return nil })
	rec := &publishRecorder{wake: wake}
	s.engine.publishState = rec.publish
	s.queryState = state
	if !s.canPark() {
		t.Fatalf("PRECONDITION FAILED: the session cannot park, so this measured nothing about the hold")
	}
	return s, wake, rec
}

var oneTimerWait = parkRequest{Kind: wakeTimer, DeadlineMs: 1, Reason: "cleat_sleep(10ms)"}

func TestAHoldPublishesTheQueryStateBeforeItWaits(t *testing.T) {
	s, wake, rec := heldSessionWithPublisher(t, map[string]string{"status": "awaiting_approval", "approval_wait": "entered"})

	if got := s.parkOrSuspend(context.Background(), oneTimerWait); got != parkWoke {
		t.Fatalf("parkOrSuspend = %v, want parkWoke: the run was not held (holdPublishFailures=%v)", got, s.holdPublishFailures)
	}
	if len(rec.got) != 1 {
		t.Fatalf("the state was published %d times, want exactly 1", len(rec.got))
	}
	if !reflect.DeepEqual(rec.got[0], map[string]string{"status": "awaiting_approval", "approval_wait": "entered"}) {
		t.Errorf("published %v, want the guest's whole query state", rec.got[0])
	}
	if rec.wakeCallsAt[0] != 0 {
		t.Errorf("the state was published after the wake source had been asked %d time(s): it must be published BEFORE the hold begins", rec.wakeCallsAt[0])
	}
	if wake.calls != 1 {
		t.Errorf("the wake source was asked %d times, want 1", wake.calls)
	}
}

// The published copy is a snapshot: the guest keeps writing to s.queryState while it runs, and the next admission compares against what was WRITTEN, not against a map that has
// since changed under it.
func TestAHoldPublishesASnapshotNotTheLiveMap(t *testing.T) {
	s, _, rec := heldSessionWithPublisher(t, map[string]string{"status": "awaiting_approval"})
	s.parkOrSuspend(context.Background(), oneTimerWait)
	s.queryState["status"] = "approval_received" // the guest moves on
	if rec.got[0]["status"] != "awaiting_approval" {
		t.Errorf("the recorded publish changed to %v when the guest later set a key: the hold published the live map, not a copy", rec.got[0])
	}
}

// (d) State the guest sets AFTER one wake and BEFORE the next wait is published at the next admission, and an unchanged state is not written again.
func TestAHoldPublishesOnlyWhatHasChangedSinceTheLastPublish(t *testing.T) {
	s, _, rec := heldSessionWithPublisher(t, map[string]string{"status": "awaiting_approval"})
	s.parkOrSuspend(context.Background(), oneTimerWait)
	s.parkOrSuspend(context.Background(), oneTimerWait) // the same state: nothing to write
	if len(rec.got) != 1 {
		t.Fatalf("after two holds with the same state the store was written %d times, want 1", len(rec.got))
	}
	s.queryState["approval_polls"] = "1" // set after the wake, before the next wait
	s.parkOrSuspend(context.Background(), oneTimerWait)
	if len(rec.got) != 2 || rec.got[1]["approval_polls"] != "1" || rec.got[1]["status"] != "awaiting_approval" {
		t.Errorf("publishes = %v, want a second one carrying approval_polls=1 and the status", rec.got)
	}
}

// (e) A publish that fails REFUSES the hold: the wake source is never asked, the run suspends (which writes the state), and the reason is kept.
func TestAHoldIsRefusedWhenItsQueryStateCannotBePublished(t *testing.T) {
	s, wake, rec := heldSessionWithPublisher(t, map[string]string{"status": "awaiting_approval"})
	rec.err = errors.New("the store is unreachable")

	got := s.parkOrSuspend(context.Background(), oneTimerWait)

	if got != parkRefused {
		t.Errorf("parkOrSuspend = %v, want parkRefused: the run was held with its state unpublished", got)
	}
	if wake.calls != 0 {
		t.Errorf("the wake source was asked %d time(s): the run was held anyway", wake.calls)
	}
	if s.suspendErr == nil {
		t.Errorf("no suspension was set: the run would not stop, and nothing would write its state")
	}
	if len(s.holdPublishFailures) != 1 {
		t.Errorf("holdPublishFailures = %v, want the one reason", s.holdPublishFailures)
	}
	// And a LATER hold must try again: a failed publish is not remembered as written.
	rec.err = nil
	s2 := s
	s2.suspendErr = nil
	if got := s2.parkOrSuspend(context.Background(), oneTimerWait); got != parkWoke || len(rec.got) != 2 {
		t.Errorf("after the store recovered the next wait was %v with %d publish attempts, want a hold and a second attempt: a failed publish was remembered as written", got, len(rec.got))
	}
}

func TestAHandleThatCannotPublishHoldsARunWithNoStateAndRefusesOneWithSome(t *testing.T) {
	wake := &fixedOutcomeWake{out: parkWoke}
	s := sessionWithWake(wake, func(context.Context) error { return nil }) // publishState nil: this handle cannot publish

	if got := s.parkOrSuspend(context.Background(), oneTimerWait); got != parkWoke {
		t.Fatalf("a run with no query state was %v, want it held: there is nothing to publish, and refusing would switch residency off for every guest that sets none", got)
	}
	s.queryState = map[string]string{"status": "x"}
	if got := s.parkOrSuspend(context.Background(), oneTimerWait); got != parkRefused {
		t.Errorf("a run WITH query state on a handle that cannot publish was %v, want parkRefused: it would be held with its state invisible, which is the defect", got)
	}
	if len(s.holdPublishFailures) != 1 {
		t.Errorf("holdPublishFailures = %v, want the one reason", s.holdPublishFailures)
	}
}

// Under noPerStepFlush the segment's events stay in memory until the execution returns. A hold keeps them there across the wait, which is a wider loss window than a
// suspension, so a hold with unflushed events is refused.
func TestAHoldIsRefusedWhileEventsAreUnflushed(t *testing.T) {
	s, wake, rec := heldSessionWithPublisher(t, nil)
	s.engine.noPerStepFlush = true
	s.unflushed = []unflushedEvent{{rec: EventRecord{Step: 1}}}

	if got := s.parkOrSuspend(context.Background(), oneTimerWait); got != parkRefused {
		t.Errorf("parkOrSuspend = %v, want parkRefused: events that exist only in memory would be held across the wait", got)
	}
	if wake.calls != 0 || len(rec.got) != 0 {
		t.Errorf("the wake source was asked %d time(s) and %d publishes were made: a refusal does neither", wake.calls, len(rec.got))
	}

	// CONTROL: the same session with its events flushed, or with per-step flush on, is held. Without it "refused" would be what every hold in this mode gets.
	s.unflushed = nil
	if got := s.parkOrSuspend(context.Background(), oneTimerWait); got != parkWoke {
		t.Errorf("with no unflushed events the run was %v, want it held", got)
	}
	s.unflushed = []unflushedEvent{{rec: EventRecord{Step: 2}}}
	s.engine.noPerStepFlush = false
	if got := s.parkOrSuspend(context.Background(), oneTimerWait); got != parkWoke {
		t.Errorf("with per-step flush on the run was %v, want it held: that mode flushes each event as it is recorded", got)
	}
}
