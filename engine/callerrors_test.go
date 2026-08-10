package engine

import (
	"context"
	"errors"
	"testing"
)

// guestRetryable reports whether the guest SDK's CallError.Retryable() returns
// true for code, and guestCodeNamed returns the byte value of a named member of
// the guest enum.
//
// The engine's tests need to assert what a workflow author would observe, and
// they cannot ask the SDK directly -- engine must not import the cleat/ module.
// Routing them through the same table the cleat/-side contract test validates is
// what keeps this from being the engine agreeing with itself: if the table
// diverges from the SDK, callerror_contract_test.go fails, not these.
//
// An unknown code is not retryable, matching the guest's `default:` arm.
// guestCodeNamed fatals rather than returning a zero value, because a silently
// zero want would make an assertion pass against callErrorUnknown.

func guestRetryable(code byte) bool {
	for _, e := range GuestCallErrorCodes() {
		if e.Code == code {
			return e.Retryable
		}
	}
	return false
}

func guestCodeNamed(t *testing.T, name string) byte {
	t.Helper()
	for _, e := range GuestCallErrorCodes() {
		if e.Name == name {
			return e.Code
		}
	}
	t.Fatalf("guestCallErrorCodes has no %q member", name)
	return 0
}

// TestCallErrorConstantsMatchGuestTable keeps the three constants the engine
// actually packs in step with guestCallErrorCodes, the table that describes the
// guest SDK's enum.
//
// This is only half the chain. It proves engine agrees with its own mirror; the
// cleat/ module's callerror_contract_test.go proves the mirror agrees with the
// SDK. Both halves are needed, and they have to live on opposite sides of the
// boundary because engine cannot import the SDK module -- that import is the
// module cycle that made `go install` impossible. See engine/callerrors.go.
func TestCallErrorConstantsMatchGuestTable(t *testing.T) {
	byName := map[string]byte{}
	for _, e := range GuestCallErrorCodes() {
		byName[e.Name] = e.Code
	}

	for _, tc := range []struct {
		name string
		got  byte
	}{
		{"Unknown", callErrorUnknown},
		{"Unavailable", callErrorUnavailable},
		{"InvalidRequest", callErrorInvalidRequest},
	} {
		want, ok := byName[tc.name]
		if !ok {
			t.Errorf("guestCallErrorCodes has no %q entry, so nothing checks callError%s "+
				"against the guest SDK at all", tc.name, tc.name)
			continue
		}
		if tc.got != want {
			t.Errorf("callError%s = %d, but guestCallErrorCodes says %s = %d",
				tc.name, tc.got, tc.name, want)
		}
	}
}

// TestEngineFailuresAreNotReportedAsRetryable is the regression test for
// IMPROVEMENT-PLAN 2.15.
//
// Every failure path in durablecalls.go, heartbeats.go and plugins.go used to
// pack the literal 1 -- the guest's CallErrorTimeout, which the guest's
// CallError.Retryable() reports as retryable. A replay divergence, a cancelled
// workflow and an ambiguous call outcome were all handed to the workflow author
// as "the call timed out, try again".
//
// The three below are failures the *engine* produced, as opposed to failures a
// service reported, and none of them can be fixed by calling again.
func TestEngineFailuresAreNotReportedAsRetryable(t *testing.T) {
	for _, tc := range []struct {
		what string
		code byte
	}{
		{"a cancelled workflow", callErrorUnknown},
		{"a replay divergence", callErrorUnknown},
		{"an ambiguous call outcome", callErrorUnknown},
	} {
		if guestRetryable(tc.code) {
			t.Errorf("%s is reported to the guest as retryable (code %d)", tc.what, tc.code)
		}
	}
}

// TestCallFailureCodeStaysRetryable pins the deliberate half of the change.
//
// A call the *service* failed keeps reporting as retryable, because the engine
// genuinely does not know why it failed and the previous hardcoded Timeout was
// retryable too. Changing that would silently alter the behaviour of every
// workflow branching on Retryable(). What changed is only the claim that the
// call timed out.
func TestCallFailureCodeStaysRetryable(t *testing.T) {
	if !guestRetryable(callFailureCode) {
		t.Errorf("callFailureCode = %d is no longer retryable; that is a behaviour "+
			"change for every workflow that branches on Retryable(), and needs to be "+
			"a deliberate decision rather than a side effect", callFailureCode)
	}
	if callFailureCode == guestCodeNamed(t, "Timeout") {
		t.Errorf("callFailureCode is back to CallErrorTimeout, which claims the call " +
			"timed out when the engine has no idea what happened")
	}
}

// erroringCaller fails every call.
type erroringCaller struct{ err error }

func (c *erroringCaller) Call(_ context.Context, _, _, _ string) (string, error) {
	return "", c.err
}

// callErrorCodeOf extracts the guest-visible classification from a packed
// DurableCall result.
func callErrorCodeOf(result int64) byte { return byte((uint64(result) >> 8) & 0xFF) }
func errCodeOf(result int64) byte       { return byte(uint64(result) & 0xFF) }

// TestReplayFailuresAreClassified drives the real replay paths and reads the
// code the guest would decode. Before 2.15 every one of these returned the
// guest's CallErrorTimeout.
func TestReplayFailuresAreClassified(t *testing.T) {
	tests := []struct {
		name    string
		history []EventRecord
		want    byte
		why     string
	}{
		{
			name:    "divergence: history has a different event type",
			history: []EventRecord{{Step: 0, EventType: EventTypeSleep}},
			want:    callErrorUnknown,
			why:     "a divergence is a bug in the workflow code; calling again diverges again",
		},
		{
			name:    "divergence: history has a different service",
			history: []EventRecord{{Step: 0, EventType: EventTypeCall, Service: "other", Op: "op"}},
			want:    callErrorUnknown,
			why:     "as above",
		},
		{
			name:    "ambiguous outcome",
			history: []EventRecord{{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Err: pendingSentinel}},
			want:    callErrorUnknown,
			why:     "the call may have succeeded; retrying risks a duplicate side effect",
		},
		{
			name:    "recorded service failure",
			history: []EventRecord{{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Err: "boom"}},
			want:    callFailureCode,
			why:     "must equal what the fresh path returned for the same failure",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &execSession{
				engine:   NewEngine(nil, &mockCaller{}),
				isReplay: true,
				history:  tc.history,
			}
			result := s.DurableCall(context.Background(), nil, "svc", "op", `{}`, 0, 0)

			if got := errCodeOf(result); got == 0 {
				t.Fatalf("errCode = 0, so the guest would read this failure as success")
			}
			if got := callErrorCodeOf(result); got != tc.want {
				t.Errorf("callErrorCode = %d, want %d -- %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestFreshAndReplayAgreeOnRecordedFailure is the determinism invariant, and
// the reason the classification cannot go further without persisting the code.
//
// A recorded call failure is replayed from EventRecord.Err, a bare string. If
// the fresh path derived a class the replay path cannot, the same step would be
// retryable on the first run and non-retryable on the replay of it.
func TestFreshAndReplayAgreeOnRecordedFailure(t *testing.T) {
	fresh := &execSession{engine: NewEngine(nil, &erroringCaller{err: errors.New("boom")})}
	freshResult := fresh.DurableCall(context.Background(), nil, "svc", "op", `{}`, 0, 0)

	replay := &execSession{
		engine:   NewEngine(nil, &mockCaller{}),
		isReplay: true,
		history:  []EventRecord{{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Err: "boom"}},
	}
	replayResult := replay.DurableCall(context.Background(), nil, "svc", "op", `{}`, 0, 0)

	if f, r := callErrorCodeOf(freshResult), callErrorCodeOf(replayResult); f != r {
		t.Errorf("the same failure classifies as %d when fresh and %d on replay; "+
			"a workflow would change its retry behaviour between the first run and "+
			"the replay of it", f, r)
	}
	if f, r := errCodeOf(freshResult), errCodeOf(replayResult); f != r {
		t.Errorf("errCode differs: fresh %d, replay %d", f, r)
	}
}

// nonRetryableErr self-reports as non-retryable via the RetryableError
// interface engine/helpers.go already honours. It is the only machine-readable
// signal a ServiceCaller can send today.
type nonRetryableErr struct{ msg string }

func (e *nonRetryableErr) Error() string   { return e.msg }
func (e *nonRetryableErr) Retryable() bool { return false }

// callWithRetry drives the real retry loop against a caller that always fails
// with err, and returns the packed result plus the event that was recorded.
func callWithRetry(t *testing.T, err error) (int64, EventRecord) {
	t.Helper()
	s := &execSession{engine: NewEngine(nil, &erroringCaller{err: err})}
	// maxAttempts 5 with 1ms intervals: enough that "stopped early" is
	// distinguishable from "ran out of attempts".
	result := s.DurableCallWithRetry(context.Background(), nil, "svc", "op", `{}`,
		5, 1, 100, 1, "", 0, 0)
	if len(s.history) != 1 {
		t.Fatalf("expected exactly one recorded event, got %d", len(s.history))
	}
	return result, s.history[0]
}

// TestNonRetryableCallIsNotReportedAsRetryable is the regression test for the
// contradiction 2.35 was blocking.
//
// isDefinitelyNonRetryable breaks the retry loop precisely because the error
// is not worth retrying -- and the engine then reported callFailureCode, which
// the guest's CallError.Retryable() says *is* retryable. A workflow branching on
// err.Retryable(), which is what the guest SDK offers, would go on to retry a
// call the engine had just decided against. For a non-idempotent operation the
// caller marked non-retryable, that is a duplicate side effect.
func TestNonRetryableCallIsNotReportedAsRetryable(t *testing.T) {
	result, rec := callWithRetry(t, &nonRetryableErr{msg: "bad request"})

	code := callErrorCodeOf(result)
	if guestRetryable(code) {
		t.Errorf("the engine stopped retrying because isDefinitelyNonRetryable said so, "+
			"then told the guest the call is retryable (code %d)", code)
	}
	if !rec.ErrNonRetryable {
		t.Error("the classification was not recorded on the event, so replay cannot reproduce it")
	}
}

// TestRetriesExhaustedStaysRetryable pins the other half. Running out of
// attempts is not the same as being told not to try: the error was retryable,
// the budget simply ran out, and calling again later may well work.
func TestRetriesExhaustedStaysRetryable(t *testing.T) {
	result, rec := callWithRetry(t, errors.New("connection reset"))

	code := callErrorCodeOf(result)
	if !guestRetryable(code) {
		t.Errorf("a transient failure that exhausted its retries is reported as non-retryable (code %d)", code)
	}
	if rec.ErrNonRetryable {
		t.Error("a retryable failure was recorded as non-retryable")
	}
}

// TestFreshAndReplayAgreeOnNonRetryableFailure is the determinism guarantee,
// and the reason the bit has to be persisted at all.
//
// It replays the event the fresh run actually recorded rather than a
// hand-written literal -- a literal would let the test agree with itself while
// the writer and the reader disagreed.
func TestFreshAndReplayAgreeOnNonRetryableFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"non-retryable", &nonRetryableErr{msg: "bad request"}},
		{"retries exhausted", errors.New("connection reset")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			freshResult, rec := callWithRetry(t, tc.err)

			replay := &execSession{
				engine:   NewEngine(nil, &mockCaller{}),
				isReplay: true,
				history:  []EventRecord{rec},
			}
			replayResult := replay.DurableCall(context.Background(), nil, "svc", "op", `{}`, 0, 0)

			if f, r := callErrorCodeOf(freshResult), callErrorCodeOf(replayResult); f != r {
				t.Errorf("classifies as %d when fresh and %d on replay; the workflow would "+
					"change its retry behaviour between the first run and the replay of it", f, r)
			}
		})
	}
}

// TestErrNonRetryableRoundTripsThroughPayload closes the loop the two paths
// above assume. They pass an EventRecord straight from one to the other; in
// production it goes through the payload JSONB and back, and a bit that does
// not survive that trip would make replay disagree with the fresh run in
// exactly the way this whole mechanism exists to prevent.
func TestErrNonRetryableRoundTripsThroughPayload(t *testing.T) {
	for _, want := range []bool{true, false} {
		rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op",
			Request: `{}`, Err: "boom", ErrNonRetryable: want}

		payload, err := eventRecordToPayload(rec)
		if err != nil {
			t.Fatalf("eventRecordToPayload: %v", err)
		}

		got := EventRecord{Step: 0, EventType: EventTypeCall}
		populateFromPayload(&got, payload)
		if got.ErrNonRetryable != want {
			t.Errorf("ErrNonRetryable=%v did not survive the payload round trip (payload: %s)", want, payload)
		}
	}
}

// TestLegacyFailureReplaysAsRetryable pins the compatibility default.
//
// Every call failure already in every event_history in existence was written
// without this key. They must keep replaying as callFailureCode -- the code
// they were recorded under -- or upgrading the engine silently changes the
// retry behaviour of workflows that are mid-flight.
func TestLegacyFailureReplaysAsRetryable(t *testing.T) {
	legacy := []byte(`{"service":"svc","operation":"op","error":"boom"}`)
	rec := EventRecord{Step: 0, EventType: EventTypeCall}
	populateFromPayload(&rec, legacy)

	if rec.ErrNonRetryable {
		t.Fatal("a payload with no error_non_retryable key read back as non-retryable")
	}
	if got := recordedFailureCode(rec.ErrNonRetryable); got != callFailureCode {
		t.Errorf("legacy failure replays as %d, want callFailureCode (%d): upgrading the engine "+
			"would change the retry behaviour of workflows already in flight", got, callFailureCode)
	}
}
