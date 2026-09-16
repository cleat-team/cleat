package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// This file MEASURES what recordEvent does when a batch flush fails, because
// cleat#1717 is a control-flow reading and nothing in the tree has ever
// exercised the failure path:
//
//	grep -rln "eventFlushFailed" --include='*_test.go' engine/   -> blank, before this file
//
// An unexercised path is where a claim about behaviour is cheapest to get
// wrong in either direction, so this asserts what was observed rather than
// what the issue predicted.

// countingStore records whether the DIRECT flush fallback ran.
//
// That is the whole question cleat#1717 asks: recordEvent sets `flushed = true`
// for both arms of the batch branch, so a failed batch flush is claimed to skip
// the `if !flushed` fallback. Counting the store's per-step flush answers it by
// observation instead of by reading the control flow again.
type countingStore struct {
	WorkflowStore
	directFlushes atomic.Int64
}

func (c *countingStore) flushEventForStep(_ context.Context, _ string, _ EventRecord) error {
	c.directFlushes.Add(1)
	return nil
}

// deadConnector yields a *sql.DB handle that is non-nil and cannot be dialled.
type deadConnector struct{}

func (deadConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("deadConnector: this handle must never connect")
}
func (deadConnector) Driver() driver.Driver { return nil }

// newFailingBatchHarness builds an engine whose BATCH flush fails immediately
// and non-retryably.
//
// A CLOSED POOL RATHER THAN AN INJECTED ERROR, deliberately: `errPoolClosed`
// matches "sql: database is closed" exactly and `errIsRetryable` returns false
// for it, so `retryBatchFlush` does NOT run its exponential backoff. Every
// other error class this could use -- refused connection, timeout, an unknown
// error -- is retryable by that function's own default, so the test would spend
// its backoff before failing and would measure the retry path as well as this
// one. Two mechanisms in one measurement is the thing to avoid.
//
// SO DO NOT READ THIS AS THE FAILOVER CASE. "sql: database is closed" means the
// APPLICATION closed the pool -- shutdown. A database failover presents as
// connection reset / broken pipe / connection refused, all of which
// errIsRetryable accepts, and which therefore DO get the backoff. That is the
// one class deliberately excluded here, and it is measured separately by
// TestHowLongTheBatchRetryWindowActuallyIs. These tests pin the coupling and the
// control flow; they are not a model of an outage.
func newFailingBatchHarness(t *testing.T) (*execSession, *countingStore, *AdaptiveFlusher) {
	t.Helper()

	deadPool := sql.OpenDB(deadConnector{})
	if err := deadPool.Close(); err != nil {
		t.Fatalf("closing the injection pool: %v", err)
	}
	// PRECONDITION, asserted rather than assumed: if this stops producing the
	// exact string errPoolClosed matches, the flusher would retry and this
	// harness would silently measure the backoff path instead.
	if _, err := deadPool.ExecContext(context.Background(), "SELECT 1"); err == nil {
		t.Fatal("UNMEASURED: the closed pool accepted a statement")
	} else if !errPoolClosed(err) {
		t.Fatalf("UNMEASURED: closed-pool error is %q, which errPoolClosed does not match, "+
			"so retryBatchFlush would back off and this test would measure two things", err)
	}

	af := NewAdaptiveFlusher(deadPool, "t-1717", 10*time.Millisecond, 1, 1e9, 0, 0)
	af.mu.Lock()
	af.batchMode = true // force the batch arm; rate-based entry is a different subject
	af.mu.Unlock()

	reg := &TenantFlusherRegistry{flushers: map[string]*AdaptiveFlusher{"t-1717": af}}

	store := &countingStore{}
	livePool := sql.OpenDB(deadConnector{})
	t.Cleanup(func() { livePool.Close() })

	e := &Engine{db: livePool, workflowStore: store, flusherRegistry: reg, tenantID: "t-1717"}
	return &execSession{engine: e, workflowID: "wf-1717"}, store, af
}

func TestWhatAFailedBatchFlushActuallyDoes(t *testing.T) {
	s, store, _ := newFailingBatchHarness(t)

	before := s.lastChecksum
	outcome := s.recordEvent(EventRecord{
		Step: 0, EventType: EventTypeCall, Service: "payments", Op: "Ship",
		TimestampMs: 1_700_000_000_000,
	})

	t.Logf("outcome              = %v (eventFlushFailed=%v)", outcome, eventFlushFailed)
	t.Logf("direct fallback ran  = %d time(s)", store.directFlushes.Load())
	t.Logf("events in s.history  = %d", len(s.history))
	t.Logf("lastChecksum advanced= %v", s.lastChecksum != before)

	if outcome != eventFlushFailed {
		t.Errorf("outcome = %v, want eventFlushFailed -- the harness did not make the "+
			"batch flush fail, so everything below is UNMEASURED", outcome)
	}

	// THE CLAIM UNDER TEST. cleat#1717 reads the control flow as skipping the
	// fallback; this observes it.
	if got := store.directFlushes.Load(); got != 0 {
		t.Errorf("the direct fallback ran %d time(s) after a failed batch flush, where "+
			"today it runs 0.\n\n"+
			"THIS TEST PINS CURRENT BEHAVIOUR, IT DOES NOT ENDORSE IT. If you moved "+
			"`flushed = true` into the success arm in engine/lifecycle.go, this failure is "+
			"your change working. That is a live decision on cleat#1717 -- what a session "+
			"should do when an event cannot be made durable -- and it needs the decision "+
			"recorded, not just the assertion updated. See the sibling test for why the "+
			"obvious objection (the flusher already retried) is weaker than it looks.", got)
	}

	// The event is in memory but was never made durable, and execution
	// continued -- recordEvent returned normally rather than aborting.
	if len(s.history) != 1 {
		t.Errorf("s.history has %d events, want 1: the event is appended before the "+
			"flush is attempted, so a failed flush still leaves it in memory", len(s.history))
	}
	if s.lastChecksum != before {
		t.Errorf("lastChecksum advanced despite the flush failing; the chain would then " +
			"claim an event that is not in the database")
	}
}

// TestABatchFailureIsSharedAcrossWorkflows measures the coupling that decides
// whether the missing fallback matters.
//
// WHY IT DECIDES IT. The obvious objection to reinstating a direct fallback is
// that `retryBatchFlush` has already run exponential backoff, so a further
// attempt is an attempt after the component whose job is retrying gave up. That
// objection holds only if the batch is the right unit of failure. It is not: one
// AdaptiveFlusher batches events from EVERY workflow of a tenant that this
// worker is running, and a DB error sends the SAME error to every entry --
//
//	for _, entry := range batch { if entry.done != nil { entry.done <- err } }
//
// -- so retrying the batch cannot separate an entry that would succeed on its
// own from the one that poisoned it. A per-event direct flush is a DIFFERENT
// statement, not a repeat of the failed one.
//
// Note what this does and does not show. The injected failure is pool-level, so
// it would sink a direct flush too; what is measured here is the COUPLING (N
// workflows, one fate), not that a fallback would have rescued them. Showing
// the rescue needs a failure attributable to one row, which needs a live
// database and is not this test.
func TestABatchFailureIsSharedAcrossWorkflows(t *testing.T) {
	s1, store, af := newFailingBatchHarness(t)

	// Two events from two DIFFERENT workflows in one batch.
	af.mu.Lock()
	af.maxBatch = 2
	af.mu.Unlock()

	s2 := &execSession{engine: s1.engine, workflowID: "wf-1717-other"}

	rec := func(step int) EventRecord {
		return EventRecord{Step: step, EventType: EventTypeCall, Service: "payments",
			Op: "Ship", TimestampMs: 1_700_000_000_000}
	}

	var o1, o2 eventPersistence
	done := make(chan struct{})
	go func() { o1 = s1.recordEvent(rec(0)); close(done) }()
	o2 = s2.recordEvent(rec(0))
	<-done

	t.Logf("workflow A outcome = %v", o1)
	t.Logf("workflow B outcome = %v", o2)
	t.Logf("direct fallbacks   = %d", store.directFlushes.Load())

	if o1 != eventFlushFailed || o2 != eventFlushFailed {
		t.Errorf("outcomes were %v and %v, want both eventFlushFailed: the point of this "+
			"test is that ONE batch failure is reported to EVERY workflow in the batch, "+
			"so anything else means the batch did not actually fail as a unit", o1, o2)
	}
	if got := store.directFlushes.Load(); got != 0 {
		t.Errorf("the direct fallback ran %d time(s); neither workflow gets one today", got)
	}
}

// failingStore fails every per-step flush, so the direct path's retry policy --
// if it has one -- is observable by counting calls.
type failingStore struct {
	WorkflowStore
	calls atomic.Int64
	err   error
}

func (f *failingStore) flushEventForStep(_ context.Context, _ string, _ EventRecord) error {
	f.calls.Add(1)
	return f.err
}

// TestHowManyTimesAFlushIsTriedBeforeItIsReportedFailed answers the question the
// owner's steer on cleat#1717 turns on: fail-fast is only safe if everything
// reaching eventFlushFailed has already been retried and judged hopeless.
//
// It has not. `retryBatchFlush` is the ONLY retry in the picture, it covers one
// of the routes to eventFlushFailed, and it returns immediately on a
// non-retryable error:
//
//	if !errIsRetryable(err) { return err }
//
// The DIRECT path -- which is what a low-rate workflow uses, since that is what
// "adaptive" adapts to -- has no retry loop at all. This measures that rather
// than asserting it from a reading, because reasoning about this file has been
// wrong twice today.
func TestHowManyTimesAFlushIsTriedBeforeItIsReportedFailed(t *testing.T) {
	store := &failingStore{err: errors.New("injected: the database refused this write")}
	livePool := sql.OpenDB(deadConnector{})
	t.Cleanup(func() { livePool.Close() })

	// No flusher registry at all -> getAdaptiveFlusher() returns nil -> the
	// direct path. This is the low-rate mode, not an exotic configuration.
	e := &Engine{db: livePool, workflowStore: store}
	s := &execSession{engine: e, workflowID: "wf-1717-direct"}

	outcome := s.recordEvent(EventRecord{
		Step: 0, EventType: EventTypeCall, Service: "payments", Op: "Ship",
		TimestampMs: 1_700_000_000_000,
	})

	t.Logf("direct-path flush attempts = %d", store.calls.Load())
	t.Logf("outcome                    = %v", outcome)

	if outcome != eventFlushFailed {
		t.Fatalf("UNMEASURED: outcome = %v, want eventFlushFailed -- the injected error "+
			"did not reach the outcome, so the attempt count below means nothing", outcome)
	}
	if got := store.calls.Load(); got != 1 {
		t.Errorf("the direct path made %d flush attempts, want 1.\n\n"+
			"If this is now >1 somebody added a retry to the direct path, which changes "+
			"the fail-fast-vs-retry trade on cleat#1717: the argument for fail-fast is "+
			"that the failure was already retried, and today that is true for exactly one "+
			"of the routes to eventFlushFailed (the batch INSERT) and false for this one.", got)
	}
}

// TestFenceLostCollapsesIntoTheSameOutcomeAsARealFailure pins a collapse that
// any policy built on cleat#1717's invariant has to work around.
//
// Six routes reach eventFlushFailed and ONE OF THEM IS NOT A DURABILITY
// FAILURE. ErrFenceLost means the run was reaped and another worker owns it --
// the event was not written because this worker no longer has the right to
// write it, which is the fencing working. The engine treats that correctly
// today: it logs at Debug rather than Error, and its comment says so.
//
// But the OUTCOME VALUE is the same one five genuine failures produce. Under
// the invariant -- a workflow may do no further work past a flush-failed
// event -- that matters: a correctly-reaped run would be indistinguishable
// from a workflow that cannot persist, and whichever policy is chosen
// (fail-fast or retry-to-deadline) would apply to it. Failing it is wrong and
// retrying it is wronger.
//
// This is a characterization test. If a distinct outcome is introduced, this
// fails, and that failure is the change working.
func TestFenceLostCollapsesIntoTheSameOutcomeAsARealFailure(t *testing.T) {
	livePool := sql.OpenDB(deadConnector{})
	t.Cleanup(func() { livePool.Close() })

	run := func(flushErr error) eventPersistence {
		store := &failingStore{err: flushErr}
		e := &Engine{db: livePool, workflowStore: store}
		s := &execSession{engine: e, workflowID: "wf-1717-fence"}
		return s.recordEvent(EventRecord{
			Step: 0, EventType: EventTypeCall, Service: "payments", Op: "Ship",
			TimestampMs: 1_700_000_000_000,
		})
	}

	reaped := run(ErrFenceLost)
	genuine := run(errors.New("injected: the database refused this write"))

	t.Logf("ErrFenceLost (run reassigned) -> %v", reaped)
	t.Logf("genuine write failure         -> %v", genuine)

	if reaped != genuine {
		t.Errorf("fence-lost now yields %v and a genuine failure %v.\n\n"+
			"They are no longer the same value, which is the distinction cleat#1717 "+
			"asks for -- update this test and record the decision.", reaped, genuine)
	}
	if reaped != eventFlushFailed {
		t.Errorf("fence-lost yields %v, want eventFlushFailed: this test exists to pin "+
			"that a reassigned run is reported as a flush failure", reaped)
	}
}

// retryableConnector fails with an error errIsRetryable accepts, so
// retryBatchFlush runs its full backoff.
//
// "connection reset" ON PURPOSE. errPoolClosed matches only "sql: database is
// closed", which means the APPLICATION closed the pool -- shutdown, not
// failover. A database failover presents as connection reset / broken pipe /
// connection refused, every one of which errIsRetryable returns true for. So
// this connector, not the closed pool used above, is the failover signature.
type retryableConnector struct{ attempts *atomic.Int64 }

func (r retryableConnector) Connect(context.Context) (driver.Conn, error) {
	r.attempts.Add(1)
	return nil, errors.New("connection reset by peer")
}
func (r retryableConnector) Driver() driver.Driver { return nil }

// TestHowLongTheBatchRetryWindowActuallyIs measures the window a batch flush
// gets before it gives up, because cleat#1717's goal is stated as graceful
// recovery across a database failover and the window is the whole question.
//
// MEASURED RATHER THAN DERIVED FROM THE CONSTANTS. maxRetries, baseBackoff and
// maxBackoff are function-local consts that a test cannot read, so re-deriving
// 50+100+200+400 here would be a second copy of the policy that drifts the
// moment anyone edits the loop. Timing the real call cannot drift.
//
// The number this pins is small against what it has to survive: streaming
// replication promotion is seconds, managed Multi-AZ failover tens of seconds.
func TestHowLongTheBatchRetryWindowActuallyIs(t *testing.T) {
	var attempts atomic.Int64
	pool := sql.OpenDB(retryableConnector{attempts: &attempts})
	t.Cleanup(func() { pool.Close() })

	af := NewAdaptiveFlusher(pool, "t-1717", 10*time.Millisecond, 1, 1e9, 0, 0)

	start := time.Now()
	err := retryBatchFlush(context.Background(), af, []byte(`[{"tenant_id":"t-1717"}]`), 1)
	elapsed := time.Since(start)

	t.Logf("retry window = %v over %d connect attempts, final err = %v",
		elapsed.Round(time.Millisecond), attempts.Load(), err)

	if err == nil {
		t.Fatal("UNMEASURED: the injected error did not survive the retry loop, so the " +
			"window below is not the failure window")
	}
	// Generous bounds: this pins the ORDER OF MAGNITUDE, not the exact schedule.
	// The point is that it is under a second, not that it is 750ms -- a tight
	// assertion here would be a timing test, which this file should not contain.
	if elapsed > 3*time.Second {
		t.Errorf("the batch retry window is now %v. If that was deliberate, this test is "+
			"the place the failover sizing on cleat#1717 was recorded -- update it and say "+
			"what window was chosen and why", elapsed)
	}
	if elapsed < 300*time.Millisecond {
		t.Errorf("the batch retry window collapsed to %v, which is short enough that the "+
			"backoff is probably not running at all -- check that the injected error is "+
			"still one errIsRetryable accepts", elapsed)
	}
}
