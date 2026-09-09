package cleat

import (
	"errors"
	"testing"
	"time"
)

// TestNoBackoffAfterTheLastAttempt is the property behind DBOS's
// test_step_retries_no_final_sleep: a retry budget of N attempts waits N-1
// times, and the last failure is not followed by a wait.
//
// It is not implied by any budget test. A loop that slept after its final
// failure would still make exactly MaxAttempts calls, still fail, and still
// return the same error -- it would simply hold the workflow for one more
// backoff interval on its way out. With exponential backoff that wasted
// interval is the LARGEST one, so the defect costs more the more attempts a
// policy allows, and nothing about the outcome reveals it.
//
// Counted, not timed. The obvious port-level version of this is a wall-clock
// assertion -- run the policy and check the elapsed time -- which CLAUDE.md's
// rule about removing timing rather than widening it argues against, and which
// would need seconds of real backoff to separate the cases. DurableSleep is a
// closure on HostCallsOptions, so the SDK path can be asked directly how many
// times it slept.
//
// SCOPE, stated because the obvious reading is wider than the truth. This
// covers the SDK-side loop only (cleat/runtime.go). There are two retry loops:
// the host-side one in engine/durablecalls.go runs when the policy fits the
// host budget, and it has the same `attempt < maxAttempts` guard -- verified by
// reading it, NOT by a test. It sleeps with real host time rather than through
// an injectable closure, so counting its backoffs needs a clock seam that does
// not exist yet.
//
// So the host path is unguarded against this regression. Said here rather than
// left implied, because a green test named for a property invites the
// assumption that the property is covered everywhere it applies.
func TestNoBackoffAfterTheLastAttempt(t *testing.T) {
	for _, maxAttempts := range []int{1, 2, 3, 5} {
		t.Run(attemptsName(maxAttempts), func(t *testing.T) {
			calls, sleeps := 0, 0
			h := NewHostCalls(HostCallsOptions{
				DurableCall: func(_, _, _ string) (string, error) {
					calls++
					return "", errors.New("always fails")
				},
				DurableSleep: func(ms int64) { sleeps++ },
			})

			_, err := h.DurableCallWithOptions(CallOptions{
				Retry: &RetryPolicy{
					MaxAttempts:        maxAttempts,
					InitialInterval:    time.Millisecond,
					BackoffCoefficient: 2,
					MaxInterval:        time.Second,
				},
			}, "svc", "op", `{}`)

			if err == nil {
				t.Fatal("a call that always fails returned no error; the fixture is not " +
					"exercising the retry path and the sleep count below means nothing")
			}
			// The control. Without it, a loop that gave up after one attempt
			// would report zero sleeps and pass every assertion below.
			if calls != maxAttempts {
				t.Fatalf("made %d attempts, expected %d -- the policy was not honoured, so "+
					"the backoff count is not evidence about backoff", calls, maxAttempts)
			}
			if want := maxAttempts - 1; sleeps != want {
				t.Errorf("slept %d times over %d attempts, expected %d.\n\n"+
					"%d attempts means %d backoffs: each failure that is about to be "+
					"retried waits, and the last one is not retried. A sleep here is "+
					"invisible in the result -- same calls, same error -- and with "+
					"exponential backoff it is the LARGEST interval that is wasted.",
					sleeps, maxAttempts, want, maxAttempts, want)
			}
		})
	}
}

func attemptsName(n int) string {
	switch n {
	case 1:
		return "1_attempt_never_sleeps"
	case 2:
		return "2_attempts_sleep_once"
	default:
		return string(rune('0'+n)) + "_attempts_sleep_n_minus_1"
	}
}
