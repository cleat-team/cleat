package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// cleat#1717. The retry that existed was `maxRetries = 5` on ONE of the six
// routes to eventFlushFailed, and an attempt count is not the quantity an
// operator can size: what a deployment knows is how long its database can be
// away, not how many INSERTs that is worth.
//
// These tests measure the replacement rather than asserting its arithmetic.
// Each one states what it would have printed under the old code.

// TestTheDefaultWindowReproducesTheHistoricalFiveAttempts pins the one property
// that makes DefaultFlushRetryWindow a non-change: an operator who sets nothing
// gets what the batch path has always done.
//
// 50+100+200+400 = 750ms of sleeping across five attempts. The deadline form
// reproduces it only because the deadline is checked BEFORE the sleep rather
// than against it -- checking whether the backoff would overshoot gives four.
// That is a one-line difference with no outward sign, which is why it is
// measured here and not left to the comment on retryFlushUntilDeadline.
func TestTheDefaultWindowReproducesTheHistoricalFiveAttempts(t *testing.T) {
	attempts := 0
	err := retryFlushUntilDeadline(context.Background(), 0, "probe", func() error {
		attempts++
		return errors.New("a transient error errIsRetryable does not recognise")
	})
	if err == nil {
		t.Fatal("retryFlushUntilDeadline returned nil for a function that never succeeds")
	}
	if attempts != 5 {
		t.Errorf("the default window bought %d attempts, want 5 (the historical maxRetries)", attempts)
	}
}

// TestHowManyAttemptsAWindowBuys is a measurement, not an assertion of a
// formula. The bounds are loose on purpose: time.After overshoots, and a test
// that pins an exact count at a sub-second window is a test that goes red on a
// loaded machine for no defect. What it exists to catch is a window that buys
// the WRONG ORDER OF MAGNITUDE of attempts -- a backoff cap that stopped
// scaling, or a loop that ignores its deadline.
//
// Deliberately short windows. A five-minute window is the case the flag exists
// for and is not testable in a unit test; flushRetryMaxBackoff's behaviour there
// is stated in its doc and reachable by reading, which is the honest limit of
// what this file measures.
func TestHowManyAttemptsAWindowBuys(t *testing.T) {
	for _, tc := range []struct {
		window  time.Duration
		atLeast int
		atMost  int
	}{
		{window: 100 * time.Millisecond, atLeast: 2, atMost: 4},
		{window: 750 * time.Millisecond, atLeast: 5, atMost: 5},
		{window: 2 * time.Second, atLeast: 6, atMost: 8},
	} {
		t.Run(tc.window.String(), func(t *testing.T) {
			attempts := 0
			start := time.Now()
			_ = retryFlushUntilDeadline(context.Background(), tc.window, "probe", func() error {
				attempts++
				return errors.New("still failing")
			})
			elapsed := time.Since(start)
			t.Logf("window=%v attempts=%d elapsed=%v maxBackoff=%v",
				tc.window, attempts, elapsed, flushRetryMaxBackoff(tc.window))
			if attempts < tc.atLeast || attempts > tc.atMost {
				t.Errorf("window %v bought %d attempts, want %d..%d", tc.window, attempts, tc.atLeast, tc.atMost)
			}
			// The overshoot bound the doc claims: at most one backoff past the
			// deadline. Stated as the cap rather than the actual last sleep,
			// since the cap is what bounds it in the worst case.
			if limit := tc.window + flushRetryMaxBackoff(tc.window) + 500*time.Millisecond; elapsed > limit {
				t.Errorf("window %v took %v, past the claimed bound of %v", tc.window, elapsed, limit)
			}
		})
	}
}

// TestAFenceLossIsNotRetried is the test this change most needed, and the
// reason errFlushRetryPointless exists.
//
// errIsRetryable's closing clause returns TRUE for any error it does not
// recognise -- deliberately, "better to retry a few times than drop events".
// ErrFenceLost is not a database error at all: another worker holds the claim,
// which is the ordinary outcome of reaping, and it is the commonest thing the
// direct flush path returns. Wrapping that path in a retry without this check
// would make every step of a reaped worker's remaining segment sleep out the
// whole window -- 750ms per step at the default, five minutes per step for the
// operator who sized the flag for a failover.
//
// WITHOUT errFlushRetryPointless this test reports 5 attempts and ~750ms.
func TestAFenceLossIsNotRetried(t *testing.T) {
	attempts := 0
	start := time.Now()
	err := retryFlushUntilDeadline(context.Background(), 5*time.Second, "probe", func() error {
		attempts++
		return fmt.Errorf("flush event: %w", ErrFenceLost)
	})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrFenceLost) {
		t.Errorf("got %v, want it to surface ErrFenceLost unchanged", err)
	}
	if attempts != 1 {
		t.Errorf("a fence loss was attempted %d times, want 1 -- retrying it can only fail again", attempts)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("a fence loss took %v to report; it should not have slept at all", elapsed)
	}

	// KNOWN-POSITIVE. Without this, the assertions above are satisfied by a
	// retryFlushUntilDeadline that never retries anything -- including a
	// genuinely transient error -- and the test would stay green over a change
	// that silently disabled the whole mechanism.
	retried := 0
	_ = retryFlushUntilDeadline(context.Background(), 200*time.Millisecond, "probe", func() error {
		retried++
		return errors.New("connection reset by peer")
	})
	if retried < 2 {
		t.Fatalf("UNMEASURED: a retryable error was attempted %d times, so this test cannot "+
			"distinguish 'fence loss is exempt' from 'nothing is ever retried'", retried)
	}
}

// TestAContextCancellationEndsTheRetryImmediately covers the other half of
// errFlushRetryPointless. A cancelled context is not a transient condition, and
// a worker shutting down should not wait out the window per in-flight step.
func TestAContextCancellationEndsTheRetryImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	attempts := 0
	start := time.Now()
	err := retryFlushUntilDeadline(ctx, 5*time.Second, "probe", func() error {
		attempts++
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Errorf("attempted %d times against a cancelled context, want 1", attempts)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("took %v against a cancelled context", elapsed)
	}
}

// TestAPoolClosedErrorIsStillNotRetried guards the pre-existing exemption, which
// this change routes through a new function and could have dropped. A closed
// pool is application shutdown; it cannot recover, and #1720's characterization
// harness depends on it failing in one attempt.
func TestAPoolClosedErrorIsStillNotRetried(t *testing.T) {
	attempts := 0
	_ = retryFlushUntilDeadline(context.Background(), 5*time.Second, "probe", func() error {
		attempts++
		return errors.New("sql: database is closed")
	})
	if attempts != 1 {
		t.Errorf("a closed pool was attempted %d times, want 1", attempts)
	}
}

// TestARetryOfAnAlreadyCommittedFlushIsNotAFenceLoss is the safety argument for
// retrying the DIRECT path at all, measured rather than reasoned.
//
// The hazard: an attempt whose INSERT committed but whose acknowledgement was
// lost to a dropped connection. The retry re-runs the same statement, which
// conflicts, affects zero rows -- and afterFencedInsert reads zero rows as
// "maybe the fence was lost". If it stopped there, a successful write would be
// reported as ErrFenceLost and the step would be treated as unpersisted when it
// is in fact durable. It does not stop there: it asks Heartbeat, which still
// holds, and returns nil.
//
// Committing twice is how the lost acknowledgement is simulated, because from
// the database's side those are the same thing -- the same statement arriving
// twice under the same fence.
//
// BEFORE THIS CHANGE THE DIRECT PATH NEVER RETRIED, so this property was never
// exercised. It is asserted here because the change now depends on it.
func TestARetryOfAnAlreadyCommittedFlushIsNotAFenceLoss(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			wfID := newIntentWorkflow(t, ctx, store, "flush-retry-idempotent")

			wf, err := store.ClaimWorkflow(ctx, "worker-A")
			if err != nil || wf == nil || wf.ID != wfID {
				t.Fatalf("ClaimWorkflow: wf=%v err=%v", wf, err)
			}

			eng := NewEngine(nil, nil,
				WithDB(rawDBOf(t, store)),
				WithWorkflowStore(store),
				WithWorkerID("worker-A"),
				WithGeneration(wf.Generation),
				WithTenantID(DefaultTenantUUID))

			rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Response: "committed-once"}

			if err := eng.flushEvent(ctx, wfID, rec, ""); err != nil {
				t.Fatalf("UNMEASURED: the first flush failed (%v), so the second is not a retry "+
					"of a committed write and this test measures nothing", err)
			}
			if err := eng.flushEvent(ctx, wfID, rec, ""); err != nil {
				t.Fatalf("the retry of an already-committed flush returned %v, want nil. "+
					"A lost acknowledgement would be reported as an unpersisted step.", err)
			}

			// KNOWN-POSITIVE: the same call under a stale generation must
			// report ErrFenceLost. Without it, a flushEvent that had stopped
			// checking the fence altogether would satisfy everything above.
			stale := NewEngine(nil, nil,
				WithDB(rawDBOf(t, store)),
				WithWorkflowStore(store),
				WithWorkerID("worker-A"),
				WithGeneration(wf.Generation),
				WithTenantID(DefaultTenantUUID))
			if _, err := store.ReapStaleInstances(ctx, -1*time.Second, 0); err != nil {
				t.Fatalf("ReapStaleInstances: %v", err)
			}
			rec2 := EventRecord{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "op", Response: "from-zombie"}
			if err := stale.flushEvent(ctx, wfID, rec2, ""); !errors.Is(err, ErrFenceLost) {
				t.Fatalf("UNMEASURED: a stale generation flushed with err=%v, want ErrFenceLost. "+
					"This test cannot tell a held fence from an unchecked one.", err)
			}
		})
	}
}
