package engine

import (
	"errors"
	"fmt"
	"testing"
)

// IMPROVEMENT-PLAN 2.35, second half: the error *class* a ServiceCaller
// supplied survives into history, so replay and an operator's query both
// recover it instead of re-deriving one bit from ErrNonRetryable.
//
// Each test here was verified by breaking the thing it covers and watching it
// fail; the comment on each says what was broken.

// Breaking check: make recordedErrorClass return ce.Code.String()
// unconditionally and the ErrUnknown case fails -- an unclassified error and
// one classified as unknown become indistinguishable, which is the collision
// that kept ErrNonRetryable a bool.
func TestRecordedErrorClass(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"bare error", errors.New("boom"), ""},
		{"transient", NewTransientError("call", "", errors.New("conn reset")), "transient"},
		{"permanent", NewPermanentError("call", "", errors.New("no endpoint")), "permanent"},
		{"cancelled", NewCancelledError("call", "", errors.New("cancelled")), "cancelled"},
		{"timeout", NewTimeoutError("call", "", errors.New("deadline")), "timeout"},
		{
			"wrapped, because the retry loop adds context before this is reached",
			fmt.Errorf("attempt 3: %w", NewPermanentError("call", "", errors.New("no endpoint"))),
			"permanent",
		},
		{
			"explicit ErrUnknown records nothing, so it cannot be confused with unclassified",
			&CleatError{Code: ErrUnknown, Err: errors.New("who knows")},
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := recordedErrorClass(tc.err); got != tc.want {
				t.Errorf("recordedErrorClass = %q, want %q", got, tc.want)
			}
		})
	}
}

// The property that makes this change safe to ship: recording a class must not
// move any guest-visible code, because ErrNonRetryable is what the engine
// actually acted on and the two can legitimately disagree -- a guest's own
// nonRetryableErrors list can mark an ErrTransient failure non-retryable.
//
// Breaking check, and it is a different shape from the others here: this test
// pins a signature as much as a behaviour. recordedFailureCode takes the bool
// and nothing else, so giving it the class to consult stops this file
// compiling rather than making it fail. That is the intended tripwire -- the
// disagreeing pair below is there so the compile error lands next to a case
// that says why -- but it is worth knowing it is a compile error and not a red
// test, because a reader who deletes the case expecting a failure gets neither.
func TestRecordedClassDoesNotMoveTheGuestVisibleCode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		nonRetry    bool
		class       string
		wantCode    byte
		wantRetryOK bool
	}{
		{"transient, engine retried", false, "transient", callFailureCode, true},
		{"permanent", true, "permanent", callErrorUnknown, false},
		{"cancelled", true, "cancelled", callErrorUnknown, false},
		{"legacy event with no class at all", false, "", callFailureCode, true},
		{
			"class and bit disagree: guest policy overrode a transient error",
			true, "transient", callErrorUnknown, false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := EventRecord{Err: "failed", ErrNonRetryable: tc.nonRetry, ErrCode: tc.class}
			got := recordedFailureCode(rec.ErrNonRetryable)
			if got != tc.wantCode {
				t.Errorf("guest code = %d, want %d (the class must not change this)", got, tc.wantCode)
			}
			var retryable bool
			for _, g := range GuestCallErrorCodes() {
				if g.Code == got {
					retryable = g.Retryable
				}
			}
			if retryable != tc.wantRetryOK {
				t.Errorf("guest sees Retryable()=%v, want %v", retryable, tc.wantRetryOK)
			}
		})
	}
}

// The class has to survive the payload round trip, or it is recorded and lost.
//
// Breaking check: drop the ErrCode field from EventRecord's json tags and the
// class reads back empty.
func TestErrorClassSurvivesThePayloadRoundTrip(t *testing.T) {
	rec := EventRecord{
		Step: 4, EventType: EventTypeCall, Service: "billing", Op: "charge",
		Err: "no endpoint registered", ErrNonRetryable: true, ErrCode: "permanent",
	}
	payload, err := eventRecordToPayload(rec)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	// Through the real decoder, not json.Unmarshal into an EventRecord: the
	// payload is a hand-built map whose keys are not the struct's json tags,
	// so unmarshalling directly would test a serialisation the engine never
	// performs and would pass while the stored payload had no class in it.
	back := EventRecord{Step: rec.Step, EventType: rec.EventType}
	populateFromPayload(&back, payload)
	if back.ErrCode != "permanent" {
		t.Errorf("ErrCode = %q after round trip, want %q", back.ErrCode, "permanent")
	}
	if back.ErrNonRetryable != true {
		t.Errorf("ErrNonRetryable = %v after round trip, want true", back.ErrNonRetryable)
	}
}

// An event written before this field existed carries no key and must read back
// as "no class recorded" -- not as a class -- and must keep its old
// retryability. Every call failure in every existing event_history was written
// that way.
//
// Breaking check: give ErrCode a non-empty default and this fails.
func TestLegacyEventHasNoClassAndKeepsItsRetryability(t *testing.T) {
	// A payload exactly as it was written before this field existed: through
	// the real decoder, for the reason the round-trip test above gives.
	legacy := []byte(`{"service":"billing","operation":"charge","error":"boom"}`)
	rec := EventRecord{Step: 1, EventType: EventTypeCall}
	populateFromPayload(&rec, legacy)
	if rec.Err != "boom" {
		t.Fatalf("decoder did not read the legacy payload at all: Err = %q", rec.Err)
	}
	if rec.ErrCode != "" {
		t.Errorf("ErrCode = %q for a pre-2.35 event, want empty", rec.ErrCode)
	}
	if got := recordedFailureCode(rec.ErrNonRetryable); got != callFailureCode {
		t.Errorf("legacy event replays as code %d, want %d -- upgrading must not change "+
			"the retry behaviour of workflows already in flight", got, callFailureCode)
	}
}

// Compaction rewrites history into its own struct, so a field missing there is
// silently dropped for exactly the old history most likely to be compacted --
// which is how ErrNonRetryable was lost once already (S4a).
//
// Breaking check: remove either the ce.ErrCode write or the rec.ErrCode read in
// compaction.go and this fails.
func TestErrorClassSurvivesCompaction(t *testing.T) {
	original := []EventRecord{{
		Step: 0, EventType: EventTypeCall, Service: "billing", Op: "charge",
		Err: "no endpoint registered", ErrNonRetryable: true, ErrCode: "permanent",
	}}
	restored := buildFullHistoryFromCompaction(nil, extractCompactionState(original))
	if len(restored) != 1 {
		t.Fatalf("restored %d events, want 1", len(restored))
	}
	if restored[0].ErrCode != "permanent" {
		t.Errorf("ErrCode = %q after compaction, want %q -- compaction would undo 2.35 "+
			"for the oldest history", restored[0].ErrCode, "permanent")
	}
	if !restored[0].ErrNonRetryable {
		t.Error("ErrNonRetryable lost through compaction")
	}
}

// The wiring test, and the one that would have caught this being recorded
// nowhere: everything above exercises recordedErrorClass and the payload in
// isolation, which all still pass if DurableCallWithRetry never sets the
// field. This drives the real fresh path with a real classified error and
// reads the event the engine actually recorded.
//
// Breaking check: remove `ErrCode: recordedErrorClass(lastErr)` from the
// EventRecord literal in durablecalls.go and only this test fails.
func TestAFreshCallRecordsTheClassTheCallerSupplied(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantClass string
		wantNonRe bool
	}{
		{
			"permanent: the engine stops early and records why",
			NewPermanentError("call", "", errors.New("no endpoint registered")),
			"permanent", true,
		},
		{
			"transient: retries run out, and the class is still the caller's",
			NewTransientError("call", "", errors.New("connection reset")),
			"transient", false,
		},
		{
			"unclassified caller records no class, exactly as before 2.35",
			errors.New("connection reset"),
			"", false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rec := callWithRetry(t, tc.err)
			if rec.ErrCode != tc.wantClass {
				t.Errorf("recorded ErrCode = %q, want %q", rec.ErrCode, tc.wantClass)
			}
			if rec.ErrNonRetryable != tc.wantNonRe {
				t.Errorf("recorded ErrNonRetryable = %v, want %v", rec.ErrNonRetryable, tc.wantNonRe)
			}
		})
	}
}
