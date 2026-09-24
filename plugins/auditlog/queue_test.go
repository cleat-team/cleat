package auditlog

// The audit queue against real databases on all three dialects (cleat#2168): nothing is lost
// silently, however the database misbehaves, and a retry never appends an event twice.
//
// The invariant every test here ends on is recorded + lost == sent: an event is either on its
// chain or counted lost. Before #2168 the third case existed and was silent.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// faultDB wraps a PluginDB and misbehaves on demand.
type faultDB struct {
	plugin.PluginDB
	mu         sync.Mutex
	failBegin  int           // fail the next n transactions
	failAlways bool          // the database is down
	stall      chan struct{} // Begin blocks until this is closed
	delay      time.Duration // every Begin takes this long
	ackLost    int           // the next n commits succeed, and then report an error
	panicBegin int           // the next n transactions panic, as a driver bug would
	hang       chan struct{} // Begin blocks until this is closed AND IGNORES ctx, as lib/pq and go-mssqldb do while the database is paused
	begins     atomic.Int64
}

var errInjected = errors.New("injected: the database is unavailable")

func (f *faultDB) settings() (stall chan struct{}, delay time.Duration, fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fail = f.failAlways
	if f.failBegin > 0 {
		f.failBegin--
		fail = true
	}
	return f.stall, f.delay, fail
}

func (f *faultDB) Begin(ctx context.Context) (plugin.PluginTx, error) {
	f.begins.Add(1)
	f.mu.Lock()
	boom := f.panicBegin > 0
	if boom {
		f.panicBegin--
	}
	f.mu.Unlock()
	if boom {
		panic("injected: the driver panicked")
	}
	f.mu.Lock()
	hang := f.hang
	f.mu.Unlock()
	if hang != nil {
		<-hang
	}
	stall, delay, fail := f.settings()
	if stall != nil {
		select {
		case <-stall:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	if fail {
		return nil, errInjected
	}
	tx, err := f.PluginDB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &faultTx{PluginTx: tx, f: f}, nil
}

func (f *faultDB) Exec(ctx context.Context, q string, a ...any) (int64, error) {
	f.mu.Lock()
	down := f.failAlways
	f.mu.Unlock()
	if down {
		return 0, errInjected
	}
	return f.PluginDB.Exec(ctx, q, a...)
}

func (f *faultDB) set(fn func(*faultDB)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

type faultTx struct {
	plugin.PluginTx
	f *faultDB
}

// Commit really commits, and then says it did not: the case a retry must not double.
func (t *faultTx) Commit() error {
	if err := t.PluginTx.Commit(); err != nil {
		return err
	}
	t.f.mu.Lock()
	lose := t.f.ackLost > 0
	if lose {
		t.f.ackLost--
	}
	t.f.mu.Unlock()
	if lose {
		return errors.New("injected: connection reset after the commit")
	}
	return nil
}

// queueRig is a running plugin over a faultDB, with the host's loss counter attached.
type queueRig struct {
	p      *Plugin
	db     *faultDB
	e      *chainEnv
	logs   *lockedLog
	cancel context.CancelFunc
	done   chan struct{}
	lost   [len(lossReasons)]atomic.Int64 // by lossReasons
}

func (r *queueRig) hookLost() int64 {
	var n int64
	for i := range r.lost {
		n += r.lost[i].Load()
	}
	return n
}

func (e *chainEnv) queueRig(t *testing.T, cfg Config) *queueRig {
	t.Helper()
	// Fast backoff, so a test that retries does not wait seconds.
	oldMin, oldMax := retryBackoffMin, retryBackoffMax
	retryBackoffMin, retryBackoffMax = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { retryBackoffMin, retryBackoffMax = oldMin, oldMax })

	p := e.plugin()
	cfg.RetentionDays = 90
	p.config = cfg
	r := &queueRig{p: p, e: e, db: &faultDB{PluginDB: p.db}}
	p.db = r.db
	p.buffer = make(chan queuedAuditEvent, cfg.bufferSize())
	r.logs = &lockedLog{}
	p.logger = slog.New(r.logs)
	p.eventsLost = func(name, reason string, n int64) {
		if name != "audit-log" {
			t.Errorf("the loss hook was called with plugin %q", name)
		}
		r.lost[lossIndex(reason)].Add(n)
	}
	return r
}

// run starts the plugin's background work and returns a stop function that waits for it.
func (r *queueRig) run(t *testing.T) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		_ = r.p.Run(ctx)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-r.done:
			case <-time.After(60 * time.Second):
				t.Error("Run did not return within 60s of being cancelled")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func (r *queueRig) send(tenant uuid.UUID, i int) {
	r.p.enqueueAudit(tenant, "u", "GET", fmt.Sprintf("/q/%d", i), 200, "10.0.0.1", "agent", time.Millisecond)
}

// settle waits until every sent event is either recorded or counted lost.
func (r *queueRig) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if r.p.q.recorded.Load()+r.p.q.lostTotal.Load() == r.p.q.sent.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("after 60s: sent %d, recorded %d, lost %d: events are unaccounted for",
		r.p.q.sent.Load(), r.p.q.recorded.Load(), r.p.q.lostTotal.Load())
}

// accounted asserts the invariant and that the host's counter and the table agree with it.
func (r *queueRig) accounted(t *testing.T, tenant uuid.UUID) (recorded, lost int64) {
	t.Helper()
	r.settle(t)
	sent, rec, lost := r.p.q.sent.Load(), r.p.q.recorded.Load(), r.p.q.lostTotal.Load()
	if rec+lost != sent {
		t.Errorf("sent %d, recorded %d + lost %d = %d: some events are unaccounted for", sent, rec, lost, rec+lost)
	}
	if hook := r.hookLost(); hook != lost {
		t.Errorf("the host was told of %d lost events, the plugin counted %d", hook, lost)
	}
	if rows := int64(r.e.rowCount(tenant)); rows != rec {
		t.Errorf("the table holds %d rows and the plugin says %d were recorded: a retry appended twice, or a row is uncounted", rows, rec)
	}
	rep, err := VerifyChain(context.Background(), r.e.plugin().db, r.e.d.dialect, tenant, VerifyOptions{})
	if err != nil || !rep.OK() || rep.Checked != rec {
		t.Errorf("the chain after the run: checked %d of %d recorded, %+v, %v", rep.Checked, rec, rep.Break, err)
	}
	return rec, lost
}

// accountedTenant checks one tenant's rows and chain, for a run with several tenants.
func (r *queueRig) accountedTenant(t *testing.T, tenant uuid.UUID) (rows int64, _ struct{}) {
	t.Helper()
	rows = int64(r.e.rowCount(tenant))
	rep, err := VerifyChain(context.Background(), r.e.plugin().db, r.e.d.dialect, tenant, VerifyOptions{})
	if err != nil || !rep.OK() || rep.Checked != rows {
		t.Errorf("chain of %s: checked %d of %d rows, %+v, %v", tenant, rep.Checked, rows, rep.Break, err)
	}
	return rows, struct{}{}
}

// A stalled database with a small queue: the requests that find no room wait, then give the event
// up AS A COUNTED LOSS. Before #2168 they were dropped with a Warn and no count, and the drain ran
// at 100 events per second regardless.
func TestNoAuditEventIsLostSilentlyWhenTheQueueFillsAgainstAStalledDatabase(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		r := e.queueRig(t, Config{BufferSize: 4, Workers: 2, EnqueueWaitMs: 80, RetryDeadlineMs: 60000})
		tenant := uuid.New()
		stall := make(chan struct{})
		r.db.set(func(f *faultDB) { f.stall = stall })
		r.run(t)

		for i := 0; i < 30; i++ {
			r.send(tenant, i)
		}
		if lost := r.p.q.lostTotal.Load(); lost < 20 {
			t.Fatalf("only %d of 30 events were lost against a queue of 4 and a database that never answers: "+
				"the queue is not bounded", lost)
		}
		close(stall) // the database recovers
		rec, lost := r.accounted(t, tenant)
		if rec < 4 || lost == 0 || r.p.q.lost[0].Load() != lost {
			t.Errorf("recorded %d, lost %d (buffer_full %d): want the queued ones recorded and the rest lost as buffer_full",
				rec, lost, r.p.q.lost[0].Load())
		}
		if len(r.logs.errors()) == 0 {
			t.Error("a lost event was not logged as an error")
		}
		if err := r.p.Health(); err == nil {
			t.Error("the audit log reports healthy after losing events")
		}
	})
}

// The point of the wait: a request that finds the queue full waits for a worker instead of
// dropping. Slow, not down: every event must be recorded and none lost.
func TestARequestThatFindsTheQueueFullWaitsForRoomInsteadOfDroppingTheEvent(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		r := e.queueRig(t, Config{BufferSize: 2, Workers: 1, EnqueueWaitMs: 20000})
		tenant := uuid.New()
		r.db.set(func(f *faultDB) { f.delay = 150 * time.Millisecond })
		r.run(t)
		for i := 0; i < 8; i++ {
			r.send(tenant, i)
		}
		rec, lost := r.accounted(t, tenant)
		if rec != 8 || lost != 0 {
			t.Errorf("recorded %d, lost %d: a slow database with an enqueue wait of 20s must lose nothing", rec, lost)
		}
	})
}

// A failed append is retried, and each event lands exactly once.
func TestAnEventIsRetriedThroughAFailingDatabaseAndRecordedOnce(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		r := e.queueRig(t, Config{Workers: 2, RetryDeadlineMs: 30000})
		tenant := uuid.New()
		r.db.set(func(f *faultDB) { f.failBegin = 6 })
		r.run(t)
		for i := 0; i < 5; i++ {
			r.send(tenant, i)
		}
		rec, lost := r.accounted(t, tenant)
		if rec != 5 || lost != 0 {
			t.Errorf("recorded %d, lost %d: six failed attempts inside a 30s deadline must lose nothing", rec, lost)
		}
		if got := r.db.begins.Load(); got < 11 {
			t.Errorf("only %d transactions began for 5 events with 6 injected failures: the failures were not retried", got)
		}
	})
}

// THE IDEMPOTENCY CASE. The commit succeeds and the acknowledgement is lost, so the worker sees a
// failure for an event that IS on the chain. The retry must find it by id and not append it again;
// without that it hit the primary key until its deadline and reported a recorded event as lost.
func TestARetryAfterALostCommitAcknowledgementDoesNotAppendTwice(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		r := e.queueRig(t, Config{Workers: 1, RetryDeadlineMs: 30000})
		tenant := uuid.New()
		r.db.set(func(f *faultDB) { f.ackLost = 3 })
		r.run(t)
		for i := 0; i < 6; i++ {
			r.send(tenant, i)
		}
		rec, lost := r.accounted(t, tenant)
		if rec != 6 || lost != 0 {
			t.Errorf("recorded %d, lost %d: an event whose commit was acknowledged late is recorded once, not lost and not doubled", rec, lost)
		}
	})
}

// A database that stays down: the event is given up on at the deadline, loudly and counted, and
// nothing hangs.
func TestAnEventTheDatabaseNeverTakesIsCountedLostAtTheRetryDeadline(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		r := e.queueRig(t, Config{Workers: 2, RetryDeadlineMs: 250})
		tenant := uuid.New()
		r.db.set(func(f *faultDB) { f.failAlways = true })
		r.run(t)
		for i := 0; i < 3; i++ {
			r.send(tenant, i)
		}
		r.settle(t)
		if got := r.p.q.lost[1].Load(); got != 3 || r.p.q.recorded.Load() != 0 {
			t.Errorf("insert_failed %d, recorded %d: want 3 lost and none recorded", got, r.p.q.recorded.Load())
		}
		if r.hookLost() != 3 {
			t.Errorf("the host was told of %d, want 3", r.hookLost())
		}
		if len(r.logs.errors()) == 0 || !strings.Contains(strings.Join(r.logs.errors(), "\n"), "AUDIT EVENT LOST") {
			t.Errorf("the loss was not logged as an error: %v", r.logs.errors())
		}
		if err := r.p.Health(); err == nil || !strings.Contains(err.Error(), "insert_failed 3") {
			t.Errorf("Health() = %v, want an error naming the loss", err)
		}
	})
}

// A loss is an Error log at most once a second per reason, and the count stays exact.
func TestALossIsLoggedOncePerSecondButCountedEveryTime(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		r := e.queueRig(t, Config{Workers: 1, RetryDeadlineMs: 1})
		tenant := uuid.New()
		r.db.set(func(f *faultDB) { f.failAlways = true })
		r.run(t)
		for i := 0; i < 60; i++ {
			r.send(tenant, i)
		}
		r.settle(t)
		if r.p.q.lostTotal.Load() != 60 || r.hookLost() != 60 {
			t.Errorf("counted %d and reported %d, want 60 of each", r.p.q.lostTotal.Load(), r.hookLost())
		}
		logged := 0
		for _, l := range r.logs.errors() {
			if strings.Contains(l, "AUDIT EVENT LOST") {
				logged++
			}
		}
		if logged == 0 || logged > 5 {
			t.Errorf("%d Error logs for 60 losses inside a few seconds, want a handful (one per second)", logged)
		}
	})
}

// Shutdown drains what is queued instead of writing one batch of 100 and exiting.
func TestShutdownDrainsTheQueueBeforeReturning(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		r := e.queueRig(t, Config{BufferSize: 200, Workers: 4, ShutdownDrainMs: 60000})
		tenant := uuid.New()
		r.db.set(func(f *faultDB) { f.delay = 20 * time.Millisecond })
		stop := r.run(t)
		for i := 0; i < 150; i++ { // more than the old 100-per-tick batch
			r.send(tenant, i)
		}
		stop() // cancel immediately: most are still queued
		rec, lost := r.accounted(t, tenant)
		if rec != 150 || lost != 0 {
			t.Errorf("recorded %d, lost %d after a shutdown with a 60s drain: want all 150", rec, lost)
		}
		// Once stopped, an event is refused at once and counted, not parked in a queue nobody reads.
		before := r.p.q.lostTotal.Load()
		start := time.Now()
		r.send(tenant, 999)
		if r.p.q.lostTotal.Load() != before+1 || r.p.q.lost[2].Load() != 1 || time.Since(start) > time.Second {
			t.Errorf("an event after shutdown: lost %d -> %d (shutdown %d) in %s, want one immediate shutdown loss",
				before, r.p.q.lostTotal.Load(), r.p.q.lost[2].Load(), time.Since(start))
		}
	})
}

// Past the drain deadline whatever is left is counted lost as "shutdown".
func TestWhatShutdownCannotDrainIsCountedLostAsShutdown(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		r := e.queueRig(t, Config{BufferSize: 50, Workers: 2, RetryDeadlineMs: 60000, ShutdownDrainMs: 300})
		tenant := uuid.New()
		// Every attempt takes a second and then fails, so the pool gets through only a few events
		// inside the 300ms drain and the rest are still in the queue when the deadline passes: the
		// final sweep is what counts those.
		r.db.set(func(f *faultDB) { f.failBegin = 1 << 30; f.delay = time.Second }) // fail in Begin, after the delay: failAlways would fail ensureHead first, instantly
		stop := r.run(t)
		for i := 0; i < 12; i++ {
			r.send(tenant, i)
		}
		start := time.Now()
		stop()
		if took := time.Since(start); took > 20*time.Second {
			t.Errorf("shutdown took %s with a 300ms drain deadline", took)
		}
		if left := len(r.p.buffer); left != 0 {
			t.Errorf("%d events are still in the queue after shutdown returned", left)
		}
		r.settle(t)
		// Up to two events (one per worker) were inside their 1s call when the 300ms drain ran out: shutdown
		// counts those as shutdown_inflight and sweeps the rest as shutdown. How many were in flight depends
		// on when each worker last took an event, so the test pins the sum and the bound, not the split.
		sd, inflight := r.p.q.lost[lossIndex(lossShutdown)].Load(), r.p.q.lost[lossIndex(lossShutdownInflight)].Load()
		if sd+inflight != 12 || inflight > 2 || r.p.q.lost[lossIndex(lossInsertFailed)].Load() != 0 {
			t.Errorf("shutdown %d + shutdown_inflight %d, want 12 in all with at most 2 in flight (insert_failed %d)",
				sd, inflight, r.p.q.lost[lossIndex(lossInsertFailed)].Load())
		}
		// When the stuck calls return (failing, after their second) the worker must not count them
		// again: shutdown already owns them.
		time.Sleep(1500 * time.Millisecond)
		if total := r.p.q.lostTotal.Load(); total != 12 || r.p.q.recorded.Load() != 0 {
			t.Errorf("after the stuck calls returned: lost %d (want 12, counted once each), recorded %d (want 0)", total, r.p.q.recorded.Load())
		}
	})
}

// The row says when the request finished, not when the append happened.
func TestTheRowCarriesTheTimeOfTheRequestNotTheTimeOfTheAppend(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		r := e.queueRig(t, Config{Workers: 1})
		tenant := uuid.New()
		stall := make(chan struct{})
		r.db.set(func(f *faultDB) { f.stall = stall })
		r.run(t)
		before := time.Now().UTC().Truncate(time.Microsecond)
		r.send(tenant, 1)
		time.Sleep(700 * time.Millisecond) // queued, and stalled at the database
		close(stall)
		r.accounted(t, tenant)
		ts := e.tsOf(tenant, 1)
		if ts.Before(before) || ts.After(before.Add(400*time.Millisecond)) {
			t.Errorf("the row's timestamp is %s, the request finished at about %s: it was stamped when the append ran, 700ms later", ts, before)
		}
	})
}

// Seq order is the chain's order and may differ from timestamp order, and nothing that reads the
// chain assumes otherwise.
func TestChainOrderIsNotTimestampOrder(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
		// Appended in this order, with these request times: seq 1 has the LATEST timestamp.
		for i, offset := range []time.Duration{50, 10, 40, 20, 30} {
			ev := newQueuedEvent(tenant, "u", "GET", fmt.Sprintf("/o/%d", i), 200, "10.0.0.1", "agent", time.Millisecond)
			ev.ts = base.Add(offset * time.Second)
			if err := p.appendEvent(ev, false); err != nil {
				t.Fatal(err)
			}
		}
		if e.tsOf(tenant, 1).Before(e.tsOf(tenant, 2)) {
			t.Fatal("the fixture did not put the timestamps out of seq order, so this measures nothing")
		}
		rep, err := VerifyChain(context.Background(), p.db, e.d.dialect, tenant, VerifyOptions{})
		if err != nil || !rep.OK() || rep.Checked != 5 {
			t.Fatalf("verify over a chain whose timestamps are out of order: %+v %v", rep, err)
		}
		_, body := e.get(p, tenant, "/audit/export")
		evs, cp, _ := exportLines(t, body)
		if cp == nil || len(evs) != 5 || cp.HeadSeq != 5 {
			t.Fatalf("the export of an out-of-order chain: %d events, checkpoint %+v", len(evs), cp)
		}
		for i, ev := range evs {
			if ev.Seq == nil || *ev.Seq != int64(i+1) {
				t.Fatalf("the export is not in seq order: record %d has seq %v", i, ev.Seq)
			}
		}
		// Retention removes a prefix by seq, and a row whose timestamp is older than a later row's
		// is not removed before it.
		cutoff := base.Add(35 * time.Second)
		n, err := e.plugin().retainTenant(context.Background(), tenant, cutoff)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("retention removed %d rows: seq 1 has the latest timestamp, so the expired prefix by seq is empty", n)
		}
	})
}

// A time-windowed export of a chain whose timestamps are out of seq order: the window selects by
// timestamp, so a record can be OUTSIDE it while its chain neighbours are inside, and the export
// carries the gap rather than refusing. The records still come out in seq order and still link.
func TestARangedExportOfAnOutOfOrderChainHasGapsInSeqAndStaysInSeqOrder(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
		for i, offset := range []time.Duration{50, 10, 40, 20, 30} { // seq 1..5
			ev := newQueuedEvent(tenant, "u", "GET", fmt.Sprintf("/o/%d", i), 200, "10.0.0.1", "agent", time.Millisecond)
			ev.ts = base.Add(offset * time.Second)
			if err := p.appendEvent(ev, false); err != nil {
				t.Fatal(err)
			}
		}
		// [15s, 45s] holds the requests made at 40s (seq 3), 20s (seq 4) and 30s (seq 5). Seq 2 (10s) is
		// outside although its chain neighbours are inside. In TIMESTAMP order these would be 4, 5, 3, so
		// the assertion below fails an export that sorted by time.
		q := "/audit/export?from=" + url.QueryEscape(base.Add(15*time.Second).Format(time.RFC3339Nano)) +
			"&to=" + url.QueryEscape(base.Add(45*time.Second).Format(time.RFC3339Nano))
		code, body := e.get(p, tenant, q)
		if code != 200 {
			t.Fatalf("a ranged export of an out-of-order chain = %d: %s", code, body)
		}
		evs, cp, _ := exportLines(t, body)
		if cp == nil || len(evs) != 3 {
			t.Fatalf("want three records, got %d, checkpoint %+v", len(evs), cp)
		}
		for i, want := range []int64{3, 4, 5} {
			if evs[i].Seq == nil || *evs[i].Seq != want {
				t.Fatalf("record %d is not seq %d: a ranged export must stay in seq order", i, want)
			}
		}
	})
}

// The operator's numbers are bounded: zero and negative mean the default, and a huge value is held at
// a cap instead of allocating, spawning or blocking without limit. retry_deadline shorter than
// enqueue_wait is allowed: they bound different things (a request's wait, an event's retry).
func TestQueueConfigIsBounded(t *testing.T) {
	c := Config{}
	if c.bufferSize() != defaultBufferSize || c.workers() != defaultWorkers || c.enqueueWait() != defaultEnqueueWait ||
		c.retryDeadline() != defaultRetryDeadline || c.shutdownDrain() != defaultShutdownDrain {
		t.Error("the zero Config does not give the defaults")
	}
	c = Config{BufferSize: -5, Workers: -1, EnqueueWaitMs: -1, RetryDeadlineMs: -1, ShutdownDrainMs: -1}
	if c.bufferSize() != defaultBufferSize || c.workers() != defaultWorkers || c.enqueueWait() != defaultEnqueueWait ||
		c.retryDeadline() != defaultRetryDeadline || c.shutdownDrain() != defaultShutdownDrain {
		t.Error("negative values do not give the defaults")
	}
	c = Config{BufferSize: 1 << 40, Workers: 1 << 20, EnqueueWaitMs: 1 << 30, RetryDeadlineMs: 1 << 30, ShutdownDrainMs: 1 << 30}
	if c.bufferSize() != maxBufferSize || c.workers() != maxWorkers || c.enqueueWait() != maxEnqueueWait ||
		c.retryDeadline() != maxRetryDeadline || c.shutdownDrain() != maxShutdownDrain {
		t.Errorf("huge values are not held at the caps: %d %d %s %s %s",
			c.bufferSize(), c.workers(), c.enqueueWait(), c.retryDeadline(), c.shutdownDrain())
	}
	// The host waits 30s for plugins to stop (cmd/cleat-worker/main.go), so the drain must stay below it.
	if c.shutdownDrain() >= 30*time.Second {
		t.Errorf("shutdown_drain may reach %s, which is not below the host's 30s", c.shutdownDrain())
	}
	c = Config{EnqueueWaitMs: 5000, RetryDeadlineMs: 100}
	if c.retryDeadline() >= c.enqueueWait() {
		t.Error("a retry_deadline shorter than enqueue_wait was rewritten")
	}
}

// THE STALL THAT IGNORES ctx. faultDB's stall selects on ctx.Done(), which lib/pq and go-mssqldb do
// not do while the database is paused, so that fake hid this: a worker inside a call that never
// returns held shutdown open past shutdown_drain and past the host's 30s, and the buffered events
// were never counted. Now shutdown is a timer: it returns at the deadline, counts the buffered
// events as shutdown and the ones inside a call as shutdown_inflight.
func TestShutdownReturnsAtItsDeadlineWhileTheDriverIgnoresCancellation(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		r := e.queueRig(t, Config{BufferSize: 16, Workers: 2, EnqueueWaitMs: 5000, ShutdownDrainMs: 400})
		tenant := uuid.New()
		hang := make(chan struct{})
		r.db.set(func(f *faultDB) { f.hang = hang })
		r.run(t)
		defer close(hang) // frees the stuck workers when the test ends

		for i := 0; i < 6; i++ {
			r.send(tenant, i)
		}
		deadline := time.Now().Add(10 * time.Second)
		for r.db.begins.Load() < 2 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if r.db.begins.Load() < 2 {
			t.Fatal("the two workers never entered the hung call, so this measures nothing")
		}

		start := time.Now()
		r.cancel()
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not return: shutdown is waiting on workers stuck in the driver")
		}
		if took := time.Since(start); took > 3*time.Second {
			t.Errorf("shutdown took %s with shutdown_drain_ms 400", took)
		}
		if got := r.lost[lossIndex(lossShutdown)].Load(); got != 4 {
			t.Errorf("%d buffered events counted lost as shutdown, want 4", got)
		}
		if got := r.lost[lossIndex(lossShutdownInflight)].Load(); got != 2 {
			t.Errorf("%d in-flight events counted lost as shutdown_inflight, want 2", got)
		}
		if err := r.p.Health(); err == nil || !strings.Contains(err.Error(), "shutdown_inflight 2") {
			t.Errorf("Health() = %v, want it to report the in-flight loss", err)
		}
	})
}

// The 5-minute window must EXPIRE: an audit log that lost an event once and has been fine since
// must stop degrading /healthz. Health reads lastLoss, so the test backdates it.
func TestTheHealthWindowExpires(t *testing.T) {
	p := &Plugin{}
	if err := p.Health(); err != nil {
		t.Fatalf("a plugin that has lost nothing reports %v", err)
	}
	p.q.lostTotal.Store(1)
	p.q.lastLoss.Store(time.Now().Add(-healthWindow + time.Minute).UnixNano())
	if err := p.Health(); err == nil {
		t.Error("a loss 4 minutes ago does not degrade Health()")
	}
	p.q.lastLoss.Store(time.Now().Add(-healthWindow - time.Second).UnixNano())
	if err := p.Health(); err != nil {
		t.Errorf("a loss just past the window still degrades Health(): %v", err)
	}
}

// The keys live in the flat plugin config shared by every plugin, so they carry the plugin's prefix.
func TestQueueConfigKeysArePrefixed(t *testing.T) {
	var c Config
	err := json.Unmarshal([]byte(`{"audit_buffer_size":11,"audit_workers":7,"audit_enqueue_wait_ms":250,`+
		`"audit_retry_deadline_ms":3000,"audit_shutdown_drain_ms":4000,"workers":99,"buffer_size":99}`), &c)
	if err != nil {
		t.Fatal(err)
	}
	if c.bufferSize() != 11 || c.workers() != 7 || c.enqueueWait() != 250*time.Millisecond ||
		c.retryDeadline() != 3*time.Second || c.shutdownDrain() != 4*time.Second {
		t.Errorf("the audit_ keys were not read: %d %d %s %s %s", c.bufferSize(), c.workers(), c.enqueueWait(), c.retryDeadline(), c.shutdownDrain())
	}
	// The unprefixed names belong to no one in a shared config, and are ignored.
	if (Config{}).workers() != defaultWorkers {
		t.Error("the zero config does not give the default")
	}
}

// Run's return is bounded by the drain, not by drain + enqueue wait: an enqueue waiting for room holds the
// queue's read lock, and shutdown needs the write lock. cleat-review measured drain 3s + wait 20s returning at
// 19.8s, which at the caps (25s + 30s) is past the host's 30s.
func TestShutdownDoesNotWaitOutAnEnqueueThatIsWaitingForRoom(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		r := e.queueRig(t, Config{BufferSize: 1, Workers: 1, EnqueueWaitMs: 20000, ShutdownDrainMs: 300})
		tenant := uuid.New()
		hang := make(chan struct{})
		r.db.set(func(f *faultDB) { f.hang = hang }) // the one worker never returns, so the queue stays full
		r.run(t)
		defer close(hang)

		r.send(tenant, 0) // taken by the worker, stuck in the driver
		deadline := time.Now().Add(10 * time.Second)
		for r.db.begins.Load() < 1 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		r.send(tenant, 1) // fills the queue's one slot
		waiting := make(chan struct{})
		go func() {
			defer close(waiting)
			r.send(tenant, 2) // finds it full, and waits up to 20s for room
		}()
		time.Sleep(200 * time.Millisecond) // let it reach the wait

		start := time.Now()
		r.cancel()
		select {
		case <-r.done:
		case <-time.After(15 * time.Second):
			t.Fatal("Run did not return: shutdown is waiting out the enqueue wait")
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("Run took %s with shutdown_drain_ms 300 and enqueue_wait_ms 20000: shutdown waited on the enqueue", took)
		}
		select {
		case <-waiting:
		case <-time.After(5 * time.Second):
			t.Error("the request waiting for room was not released by shutdown")
		}
		// All three are accounted: one in flight, two counted lost as shutdown (the queued one and the waiter).
		if got := r.p.q.lostTotal.Load(); got != 3 {
			t.Errorf("%d events counted lost, want 3 (queued 1 + waiting 1 as shutdown, 1 shutdown_inflight)", got)
		}
	})
}

// Once shutdown has given up waiting, no worker starts a new attempt: the event is counted lost as
// shutdown and the database is never touched. A Plugin with no database at all shows it, since an
// attempt would panic (counted insert_failed) instead.
func TestNoAttemptStartsOnceShutdownHasGivenUp(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p.q.abandoned.Store(true)
	ev := newQueuedEvent(uuid.New(), "u", "GET", "/x", 200, "1.1.1.1", "ua", time.Millisecond)
	p.q.sent.Add(1)
	p.persistGuarded(ev)
	if got := p.q.lost[lossIndex(lossShutdown)].Load(); got != 1 {
		t.Errorf("shutdown losses %d, want 1", got)
	}
	if p.q.lost[lossIndex(lossInsertFailed)].Load() != 0 || p.q.recorded.Load() != 0 {
		t.Error("an attempt was started after shutdown gave up (it panicked on the missing database, or wrote)")
	}
}

// A panic while recording is a counted loss, not a dead worker: the pool keeps its size, the event in
// hand is counted, and the worker process is not taken down (cmd/cleat-worker's
// TestEveryStartedGoroutineIsRecovered is what requires the goroutines themselves to be guarded).
func TestAPanicWhileRecordingIsACountedLossAndThePoolKeepsWorking(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		r := e.queueRig(t, Config{BufferSize: 16, Workers: 1, EnqueueWaitMs: 5000})
		tenant := uuid.New()
		r.db.set(func(f *faultDB) { f.panicBegin = 1 })
		r.run(t)
		for i := 0; i < 5; i++ {
			r.send(tenant, i)
		}
		rec, lost := r.accounted(t, tenant)
		if lost != 1 || rec != 4 || r.p.q.lost[lossIndex(lossInsertFailed)].Load() != 1 {
			t.Errorf("recorded %d, lost %d: want the one event in hand counted lost (insert_failed) and the other four "+
				"recorded by the SAME single worker", rec, lost)
		}
		found := false
		for _, l := range r.logs.errors() {
			if strings.Contains(l, "panic while recording") {
				found = true
			}
		}
		if !found {
			t.Errorf("the panic was not in the Error log: %v", r.logs.errors())
		}
	})
}

// Many workers appending to a few tenants at once: the head lock serialises each chain, so every
// chain must verify with every recorded event in it. A pool that raced on the head would show as a
// break, a duplicate seq, or a count that does not add up.
//
// This does NOT pin the head lock: removing FOR UPDATE and the seq CAS leaves it green (measured), because
// UNIQUE (tenant_id, seq) turns the race into a failed insert and the retry absorbs it. The lock is pinned by
// TestConcurrentAppendersNeverForkAChain, which appends directly. What this pins is the pool end to end:
// nothing lost, nothing twice, every chain verifies.
func TestConcurrentWorkersKeepEveryTenantsChainIntact(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		r := e.queueRig(t, Config{BufferSize: 64, Workers: 8, EnqueueWaitMs: 20000})
		tenants := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
		r.run(t)
		const perTenant = 40
		var wg sync.WaitGroup
		for _, tn := range tenants {
			for g := 0; g < 4; g++ {
				wg.Add(1)
				go func(tn uuid.UUID, g int) {
					defer wg.Done()
					for i := 0; i < perTenant/4; i++ {
						r.send(tn, g*1000+i)
					}
				}(tn, g)
			}
		}
		wg.Wait()
		r.settle(t)
		if lost := r.p.q.lostTotal.Load(); lost != 0 {
			t.Fatalf("%d events were lost with a healthy database and a 20s wait", lost)
		}
		for _, tn := range tenants {
			rec, _ := r.accountedTenant(t, tn)
			if rec != perTenant {
				t.Errorf("tenant %s: %d rows, want %d", tn, rec, perTenant)
			}
		}
	})
}

// lockedLog records Error-level log lines, safely: workers write them while the test reads.
type lockedLog struct {
	mu   sync.Mutex
	errs []string
}

func (l *lockedLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *lockedLog) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *lockedLog) WithGroup(string) slog.Handler            { return l }
func (l *lockedLog) Handle(_ context.Context, r slog.Record) error {
	if r.Level < slog.LevelError {
		return nil
	}
	line := r.Message
	r.Attrs(func(a slog.Attr) bool {
		line += " " + a.Key + "=" + a.Value.String()
		return true
	})
	l.mu.Lock()
	l.errs = append(l.errs, line)
	l.mu.Unlock()
	return nil
}

// errors returns a copy of the Error lines so far.
func (l *lockedLog) errors() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.errs...)
}
