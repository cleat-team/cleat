package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// recordEvent must not return until its event is durable, or the flush has
// failed and said so. cleat#1670.
//
// That ordering is what bounds the crash window docs/durable-calls.md §2
// describes: the window opens when the external service returns and closes when
// the flush commits, and because recordEvent blocks, it cannot outlive the host
// call. Once the guest resumes, the event is on disk. Making the flush
// fire-and-forget is an obvious performance change -- it takes a commit off the
// hot path -- and it would extend that window across the guest's next durable
// step, its next sleep, and everything after.
//
// # Why the existing tests do not cover it
//
// tests/crash/crash_test.go's TestEventsArePersistedDuringExecution sleeps TWO
// SECONDS before counting, and says why: "the adaptive flusher batches with an
// 8ms window, so this is generous by three orders of magnitude". Two seconds
// proves the event is durable EVENTUALLY. It cannot tell "durable before the
// call returned" from "durable within two seconds of it" -- identical today,
// divergent after such a change. engine/adaptive_flush_test.go covers the
// flusher's own modes and rate transitions, not the engine's ordering around
// it.
//
// # No clock in here, deliberately
//
// The obvious test -- "durable within N ms of the return" -- goes green on a
// machine that is merely fast, and this repository has paid for wall-clock
// assertions more than once. This asserts an ORDER instead, from a counter both
// sides stamp, and it is sound in both worlds:
//
//	synchronous     flush stamps 1, return stamps 2  -> 1 < 2, passes
//	fire-and-forget return stamps 1, flush stamps 2  -> 2 < 1, fails
//
// The wait on flushDone before comparing is what makes the second case fail
// rather than read a flush that has not happened yet as a pass -- with no wait,
// a fire-and-forget flush leaves the stamp at zero and `0 < returnSeq` is true.
// That wait cannot hang: the flush runs in both worlds, only later in one.

// orderingStore observes when a per-step flush COMPLETES.
//
// The embedded WorkflowStore is nil on purpose. This test drives exactly one
// method, and any other the flush path reaches will panic here rather than
// return a zero value that quietly satisfies the caller -- see
// scripts/check-blind-doubles.sh for why a double that answers everything is
// worse than one that answers nothing.
type orderingStore struct {
	WorkflowStore

	seq      *atomic.Int64
	flushSeq atomic.Int64
	flushErr error
	flushed  chan struct{}
	calls    atomic.Int64
}

func (o *orderingStore) flushEventForStep(_ context.Context, _ string, _ EventRecord) error {
	o.calls.Add(1)
	o.flushSeq.Store(o.seq.Add(1))
	close(o.flushed)
	return o.flushErr
}

// refusingConnector yields a non-nil *sql.DB that cannot be used.
//
// recordEvent and flushEvent both gate on `e.db != nil`, so the handle has to
// exist. It must never be dialled: this test asserts that the flush goes
// through the store's perStepEventFlusher path, and a handle that could connect
// would let a wrong path succeed quietly. Connecting fails loudly instead.
type refusingConnector struct{}

func (refusingConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("this test must not reach the database: the flush is " +
		"supposed to go through the store's flushEventForStep")
}
func (refusingConnector) Driver() driver.Driver { return nil }

// newOrderingHarness builds the smallest engine that reaches flushEventForStep.
//
// workerID and generation are left zero so fencingEnabled() is false and the
// flush path does not call Heartbeat -- which the nil embedded store would
// panic on, correctly, since this test is not about fencing.
func newOrderingHarness(t *testing.T, flushErr error) (*execSession, *orderingStore) {
	t.Helper()
	seq := &atomic.Int64{}
	store := &orderingStore{seq: seq, flushErr: flushErr, flushed: make(chan struct{})}
	db := sql.OpenDB(refusingConnector{})
	t.Cleanup(func() { db.Close() })

	e := &Engine{db: db, workflowStore: store}
	return &execSession{engine: e, workflowID: "wf-1670"}, store
}

// stampedRecord carries its own timestamp so recordEvent does not reach for the
// clock-domain reconciliation, which is a different subject with its own tests.
func stampedRecord(step int) EventRecord {
	return EventRecord{Step: step, EventType: EventTypeCall, Service: "payments",
		Op: "Ship", TimestampMs: 1_700_000_000_000}
}

func TestADurableCallsEventIsDurableBeforeRecordEventReturns(t *testing.T) {
	s, store := newOrderingHarness(t, nil)

	s.recordEvent(stampedRecord(0))
	returnSeq := store.seq.Add(1)

	// The flush has run in both worlds by now, or will; waiting is what stops a
	// not-yet-run flush reading as a pass. It cannot hang on correct code.
	<-store.flushed
	flushSeq := store.flushSeq.Load()

	if store.calls.Load() != 1 {
		t.Fatalf("the flush ran %d times, want 1 -- recordEvent did not reach "+
			"flushEventForStep, so the order below says nothing", store.calls.Load())
	}
	if flushSeq >= returnSeq {
		t.Errorf("the flush completed at %d and recordEvent returned at %d: the call "+
			"returned BEFORE its event was durable.\n\n"+
			"That unbounds the crash window docs/durable-calls.md §2 describes. It is "+
			"bounded today only because recordEvent blocks -- on the batch path a "+
			"receive on the flusher's done channel, on the direct path an inline "+
			"flushEvent. If the flush has been made fire-and-forget, the window now "+
			"extends past the guest resuming, across its next durable step and its "+
			"next sleep, and every event recorded in that interval is lost to a crash "+
			"that used to be survivable.", flushSeq, returnSeq)
	}
}

// The exception, asserted as the exception. cleat#1670.
//
// recordEvent blocks and then CONTINUES on failure: ErrFenceLost at Debug,
// anything else at Error, and the guest resumes with no durable event. So the
// unqualified property is false today, on purpose for the fence-lost case --
// the claim was lost, another worker owns the workflow, and this one must not
// write.
//
// Asserting it here is what stops the test above being written against a
// property the engine does not have, which would fail against correct
// behaviour rather than against the change it is meant to catch.
func TestRecordEventReturnsAfterAFailedFlushAndDoesNotAdvanceTheChain(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"fence lost", ErrFenceLost},
		{"any other failure", errors.New("connection reset")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, store := newOrderingHarness(t, tc.err)
			before := s.lastChecksum

			s.recordEvent(stampedRecord(0))

			<-store.flushed
			if store.calls.Load() != 1 {
				t.Fatalf("the flush ran %d times, want 1", store.calls.Load())
			}
			// Returning at all is the assertion: recordEvent has no error
			// result and does not abort the session.
			if s.lastChecksum != before {
				t.Errorf("the checksum chain advanced to %q after a failed flush; it must "+
					"not, or the next event chains from a predecessor that was never "+
					"written", s.lastChecksum)
			}
			if s.stepCount != 1 {
				t.Errorf("stepCount = %d, want 1: the in-memory session advances even when "+
					"the write fails, which is what makes the step re-execute on replay "+
					"rather than corrupt the history", s.stepCount)
			}
		})
	}
}

// The known-positive. cleat#1670.
//
// Every assertion above is satisfied by a test harness that cannot tell the two
// orders apart, and the tree is correct today, so a green run proves nothing on
// its own. This drives the SAME comparison against a deliberately
// fire-and-forget flush and requires it to report the violation.
//
// It does not call recordEvent -- it cannot, since the engine is correct. It
// reproduces what a fire-and-forget recordEvent would do to the counter, which
// is the thing the comparison has to catch.
func TestTheOrderingComparisonReportsAFireAndForgetFlush(t *testing.T) {
	_, store := newOrderingHarness(t, nil)

	// What a fire-and-forget recordEvent would do: dispatch the flush and
	// return without waiting for it.
	go func() { _ = store.flushEventForStep(context.Background(), "wf-1670", stampedRecord(0)) }()
	returnSeq := store.seq.Add(1)

	<-store.flushed
	flushSeq := store.flushSeq.Load()

	if flushSeq < returnSeq {
		t.Fatalf("the comparison accepted a fire-and-forget flush: flush stamped %d, "+
			"return stamped %d.\n\n"+
			"Then TestADurableCallsEventIsDurableBeforeRecordEventReturns is green "+
			"whichever order the engine uses, and it is not a guard. The likely cause "+
			"is the wait on flushed being removed: without it the flush may not have "+
			"stamped at all, leaving 0, and 0 is less than every return stamp.",
			flushSeq, returnSeq)
	}
}

// The batch arm, against a real database. cleat#1670.
//
// The test above drives the DIRECT flush, which is the arm a low-rate worker
// takes -- and the one the cleat-ports harness runs. The batch arm is the
// riskier of the two to lose: recordEvent's wait there is a bare
// `if err := <-done`, whose own comment notes it "was a select with one case
// and no default", and a reader asking why the hot path blocks on a batch at
// all would not be obviously wrong. A mutation that removes it is invisible to
// the unit test above, which installs no flusher.
//
// THE ASSERTION IS THE ROW, not a counter. The instant recordEvent returns,
// the event must already be SELECTable. That is the property stated directly
// rather than through a stand-in, and it needs no clock and no ordering
// stamp: either the row is there when the call returns or it is not.
//
// maxBatch is 1 so the batch fills on the first event and commits immediately,
// rather than waiting out the 8ms timer -- the timer would make this a test
// about how long a batch waits, which is a different subject with its own
// tests in adaptive_flush_test.go.
func TestTheBatchArmAlsoFlushesBeforeRecordEventReturns(t *testing.T) {
	// No env check of its own: testutil.TestDB gates on CLEAT_TEST_POSTGRES
	// **or** CLEAT_TEST_DB and skips when neither is set. A private check on
	// CLEAT_TEST_POSTGRES alone -- which is what this had first -- skips
	// wherever CI sets only CLEAT_TEST_DB, which is every job in ci.yml. The
	// test would have run nowhere and reported ok.
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	const wfID = "wf-1670-batch"
	seedWorkflowInstance(t, db, testutil.DialectPostgres, wfID)
	if _, err := db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, wfID); err != nil {
		t.Fatalf("clear event_history: %v", err)
	}

	// A real tenant UUID: the batch insert writes tenant_id as a uuid column,
	// and "" fails the cast with `invalid input syntax for type uuid` -- which
	// recordEvent logs and swallows, so the harness would look like a defect in
	// the engine rather than a defect in itself.
	af := NewAdaptiveFlusher(db, DefaultTenantUUID, 8*time.Millisecond, 1, 0, 0, 0)
	af.mu.Lock()
	af.batchMode = true // rather than driving the rate EWMA past its threshold
	af.mu.Unlock()

	// Pre-seeded rather than let For() build one, so the flusher under test is
	// the one configured above: For() creates from r.config on a miss, which
	// would silently give back an 8ms/200 flusher and turn this into a test of
	// the timer.
	reg := &TenantFlusherRegistry{flushers: map[string]*AdaptiveFlusher{DefaultTenantUUID: af}}
	e := &Engine{db: db, tenantID: DefaultTenantUUID,
		workflowStore: NewPostgresStore(db), flusherRegistry: reg}
	s := &execSession{engine: e, workflowID: wfID}

	s.recordEvent(stampedRecord(0))

	// No wait, deliberately: the question is whether it was durable at the
	// moment of return, so anything that yields here would answer a weaker one.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM event_history WHERE workflow_id = $1`, wfID).Scan(&n); err != nil {
		t.Fatalf("count event_history: %v", err)
	}
	if n != 1 {
		t.Errorf("event_history holds %d rows for %s the instant recordEvent returned, want 1.\n\n"+
			"On the batch arm recordEvent waits on the flusher's done channel, so the "+
			"commit has happened by the time it returns. If that receive has been "+
			"removed, the guest resumes before its event is durable and the crash "+
			"window docs/durable-calls.md §2 bounds now extends past the host call.", n, wfID)
	}
}
