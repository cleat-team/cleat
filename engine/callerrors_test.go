package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/cleat-team/cleat/cleat"
)

// TestCallErrorCodesMatchGuestSDK keeps the engine-local copies of the enum
// honest. engine does not import cleat in non-test code, so nothing but this
// test stops the two drifting apart -- and a drifted code is worse than no
// code, because the guest's `switch e.Code` silently falls through to default.
func TestCallErrorCodesMatchGuestSDK(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  byte
		want cleat.CallErrorCode
	}{
		{"Unknown", callErrorUnknown, cleat.CallErrorUnknown},
		{"Unavailable", callErrorUnavailable, cleat.CallErrorUnavailable},
		{"InvalidRequest", callErrorInvalidRequest, cleat.CallErrorInvalidRequest},
	} {
		if int(tc.got) != int(tc.want) {
			t.Errorf("callError%s = %d, but cleat.CallError%s = %d", tc.name, tc.got, tc.name, tc.want)
		}
	}
}

// TestEngineFailuresAreNotReportedAsRetryable is the regression test for
// IMPROVEMENT-PLAN 2.15.
//
// Every failure path in durablecalls.go, heartbeats.go and plugins.go used to
// pack the literal 1 -- cleat.CallErrorTimeout, which cleat.CallError.Retryable
// reports as retryable. A replay divergence, a cancelled workflow and an
// ambiguous call outcome were all handed to the workflow author as "the call
// timed out, try again".
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
		e := &cleat.CallError{Code: cleat.CallErrorCode(tc.code)}
		if e.Retryable() {
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
	e := &cleat.CallError{Code: cleat.CallErrorCode(callFailureCode)}
	if !e.Retryable() {
		t.Errorf("callFailureCode = %d is no longer retryable; that is a behaviour "+
			"change for every workflow that branches on Retryable(), and needs to be "+
			"a deliberate decision rather than a side effect", callFailureCode)
	}
	if callFailureCode == byte(cleat.CallErrorTimeout) {
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
// code the guest would decode. Before 2.15 every one of these returned
// cleat.CallErrorTimeout.
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
