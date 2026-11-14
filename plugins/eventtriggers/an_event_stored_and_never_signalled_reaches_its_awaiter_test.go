package eventtriggers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// cleat#3835: an event that is stored and never signalled reaches its awaiter.
//
// PublishEvent commits the event and signals the awaiters as a separate step.
// When that step fails -- the publisher dies, or every SignalWorkflow attempt
// errors -- the event is stored, unprocessed, with an awaiter registered for it.
// reconcileAwaiters is the sweep that finds such a pair and delivers.
//
// EVERY DELIVERY IS COUNTED AT THE SIGNAL, NOT AT THE SWEEP'S RETURN VALUE. A
// sweep that reports one delivery and sent two would pass a test that read only
// its return value, and "one delivery out of N workers" is the property.

// signalLog records what a fake engine was asked to signal, and lets a test
// decide per workflow whether the call fails.
type signalLog struct {
	mu    sync.Mutex
	calls []signalCall
	fail  func(workflowID string) error
}

type signalCall struct {
	workflowID, name, payload string
}

func (l *signalLog) signal(_ context.Context, workflowID, name, payload string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, signalCall{workflowID, name, payload})
	if l.fail != nil {
		if err := l.fail(workflowID); err != nil {
			return err
		}
	}
	return nil
}

func (l *signalLog) forWorkflow(id string) []signalCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []signalCall
	for _, c := range l.calls {
		if c.workflowID == id {
			out = append(out, c)
		}
	}
	return out
}

func (l *signalLog) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.calls)
}

type reconcileFixture struct {
	be        testutil.PluginTestBackend
	dialect   plugin.Dialect
	db        *engine.SQLDBAdapter
	tenant    uuid.UUID
	ctx       context.Context // scoped to tenant
	quiet     *slog.Logger
	logBuf    *bytes.Buffer
	signals   *signalLog
	newWorker func() *Plugin
}

// purge empties both tables across every tenant.
//
// THE SWEEP IS CROSS-TENANT, AND THE TEST DATABASE OUTLIVES A TEST. Rows a
// previous test or run left behind -- a stranded awaiter with an aged event,
// which is exactly what these tests build -- are in the sweep's scope, so
// they are purged before and after rather than left to be delivered to.
func (f *reconcileFixture) purge(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	conn := f.be.CrossTenantConn(t, ctx, "cleat#3835: the reconciler sweeps every tenant, so its test starts from empty tables")
	for _, q := range []string{`DELETE FROM event_awaiters`, `DELETE FROM ingested_events`} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("purge on %s (%s): %v", f.be.Name, q, err)
		}
	}
}

// newReconcileFixture builds the schema, and a way to make several Plugin
// instances -- "workers" -- that share this database and this fake engine.
func newReconcileFixture(t *testing.T, be testutil.PluginTestBackend) *reconcileFixture {
	t.Helper()
	ctx := context.Background()
	dialect := plugin.Dialect(string(be.Dialect))
	testutil.SetupFullSchema(t, be.DB, be.Dialect)

	seed := &Plugin{}
	if err := seed.Init(ctx, &plugin.Environment{Dialect: dialect, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: seed, Healthy: true}}); err != nil {
		t.Fatalf("migrations on %s: %v", be.Name, err)
	}

	f := &reconcileFixture{
		be:      be,
		dialect: dialect,
		db:      &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect},
		tenant:  uuid.New(),
		quiet:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		logBuf:  &bytes.Buffer{},
		signals: &signalLog{},
	}
	f.ctx = plugin.ForTenant(ctx, f.tenant)
	// strand() publishes against a dead engine, and PublishEvent would sleep
	// through signalRetryDelays on every one of them.
	savedDelays := signalRetryDelays
	signalRetryDelays = nil
	t.Cleanup(func() { signalRetryDelays = savedDelays })
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("worker log:\n%s", f.logBuf.String())
		}
	})
	f.newWorker = func() *Plugin {
		p := &Plugin{}
		if err := p.Init(ctx, &plugin.Environment{
			DB: f.db, Dialect: dialect,
			Logger:         slog.New(slog.NewTextHandler(f.logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})),
			SignalWorkflow: f.signals.signal,
		}); err != nil {
			t.Fatalf("Init worker: %v", err)
		}
		return p
	}
	return f
}

func (f *reconcileFixture) env() *plugin.Environment {
	return &plugin.Environment{DB: f.db, Dialect: f.dialect, Logger: f.quiet, SignalWorkflow: f.signals.signal}
}

// strand stores an event whose signalling failed: the awaiter is registered
// first, then PublishEvent runs against an engine that rejects every signal, so
// the event is committed and the awaiter row is left standing.
func (f *reconcileFixture) strand(t *testing.T, workflowID, eventType string, body json.RawMessage, keys []string) {
	t.Helper()
	k1, k2, k3, err := keySlots(keys)
	if err != nil {
		t.Fatalf("keySlots: %v", err)
	}
	if err := registerAwaiterCore(f.ctx, f.db, f.dialect, f.quiet, f.tenant.String(), workflowID, eventType, k1, k2, k3); err != nil {
		t.Fatalf("register awaiter: %v", err)
	}
	dead := &plugin.Environment{DB: f.db, Dialect: f.dialect, Logger: f.quiet,
		SignalWorkflow: func(context.Context, string, string, string) error { return errors.New("engine unreachable") }}
	if _, err := PublishEvent(f.ctx, f.db, f.quiet, dead, uuid.New(), f.tenant, eventType, body, keys); err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}
}

// age makes every stored event older than the grace period.
func (f *reconcileFixture) age(t *testing.T) {
	t.Helper()
	var q string
	switch f.dialect {
	case plugin.DialectMySQL:
		q = `UPDATE ingested_events SET received_at = DATE_SUB(NOW(6), INTERVAL 5 MINUTE)`
	case plugin.DialectMSSQL:
		q = `UPDATE ingested_events SET received_at = DATEADD(minute, -5, SYSUTCDATETIME())`
	default:
		q = `UPDATE ingested_events SET received_at = NOW() - INTERVAL '5 minutes'`
	}
	if _, err := f.db.Exec(f.ctx, q); err != nil {
		t.Fatalf("age events on %s: %v", f.be.Name, err)
	}
}

func (f *reconcileFixture) awaiterCount(t *testing.T, workflowID string) int {
	t.Helper()
	var n int
	if err := plugin.ScanRow(f.db.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM event_awaiters WHERE workflow_id = $1`, workflowID), &n); err != nil {
		t.Fatalf("count awaiters: %v", err)
	}
	return n
}

func eachBackend(t *testing.T, fn func(t *testing.T, f *reconcileFixture)) {
	t.Helper()
	for _, be := range testutil.NewPluginTestBackends(t) {
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			f := newReconcileFixture(t, be)
			f.purge(t)
			defer f.purge(t) // before be.Cleanup closes the database
			fn(t, f)
		})
	}
}

func TestAStoredEventNeverSignalledReachesItsAwaiter(t *testing.T) {
	eachBackend(t, func(t *testing.T, f *reconcileFixture) {
		const eventType = "order.paid"
		keys := []string{"order-42"}
		body := json.RawMessage(`{"b":2,"a":[1,2],"n":123456789012345678901234567890}`)

		w := f.newWorker()
		f.strand(t, "wf-stranded", eventType, body, keys)
		if got := f.signals.total(); got != 0 {
			t.Fatalf("UNMEASURED: the stranding step reached the fake engine %d time(s); the setup did not strand anything", got)
		}

		// CONTROL: inside the grace period the event is not stranded, it is
		// merely new, and the sweep must leave it for its publisher.
		if n := w.reconcileAwaiters(context.Background(), time.Now()); n != 0 || f.signals.total() != 0 {
			t.Fatalf("a fresh event was reconciled: delivered=%d signals=%d, want 0 -- the sweep is racing publishers", n, f.signals.total())
		}

		f.age(t)
		var stored []byte
		var storedAgeMs int64
		k1s, k2s, k3s, _ := keySlots(keys)
		if err := f.db.QueryRow(f.ctx, oldestStrandedEvent.For(f.dialect),
			f.tenant, eventType, k1s, k2s, k3s).Scan(&stored, &storedAgeMs); err != nil {
			t.Fatalf("UNMEASURED: read the stored event back: %v", err)
		}
		// The age the sweep logs is the database's own, so it is right whatever
		// the session time zone: the events were aged by five minutes.
		if storedAgeMs < 4*60*1000 || storedAgeMs > 6*60*1000 {
			t.Errorf("event age read back as %d ms on %s, want about 300000", storedAgeMs, f.be.Name)
		}
		if n := w.reconcileAwaiters(context.Background(), time.Now()); n != 1 {
			t.Fatalf("sweep delivered %d, want 1 (log: %s)", n, f.logBuf.String())
		}
		calls := f.signals.forWorkflow("wf-stranded")
		if len(calls) != 1 {
			t.Fatalf("workflow was signalled %d times, want exactly 1", len(calls))
		}
		if calls[0].name != "__evt:"+eventType {
			t.Errorf("signal name = %q, want %q: the reconciler must use the name signalAwaiters uses", calls[0].name, "__evt:"+eventType)
		}
		if got := f.awaiterCount(t, "wf-stranded"); got != 0 {
			t.Errorf("awaiter rows left after a delivery: %d, want 0", got)
		}
		if got := w.awaitersReconciled.Load(); got != 1 {
			t.Errorf("awaitersReconciled = %d, want 1", got)
		}
		for _, want := range []string{`level=INFO`, `reconciled a stranded event`, `tenant_id=` + f.tenant.String(), `event_type=` + eventType, `event_age_ms=`} {
			if !bytes.Contains(f.logBuf.Bytes(), []byte(want)) {
				t.Errorf("INFO log is missing %q:\n%s", want, f.logBuf.String())
			}
		}

		// A second sweep has nothing left: the row it would have taken is gone.
		if n := w.reconcileAwaiters(context.Background(), time.Now()); n != 0 || len(f.signals.forWorkflow("wf-stranded")) != 1 {
			t.Errorf("a second sweep re-delivered: delivered=%d signals=%d", n, len(f.signals.forWorkflow("wf-stranded")))
		}

		// PAYLOAD. The reconciled payload goes through the same function a
		// direct publish uses. Publish the same body directly and compare what
		// each handed the engine.
		f.signals.mu.Lock()
		f.signals.calls = nil
		f.signals.mu.Unlock()
		k1, k2, k3, _ := keySlots(keys)
		if err := registerAwaiterCore(f.ctx, f.db, f.dialect, f.quiet, f.tenant.String(), "wf-direct", eventType, k1, k2, k3); err != nil {
			t.Fatalf("register: %v", err)
		}
		if _, err := PublishEvent(f.ctx, f.db, f.quiet, f.env(), uuid.New(), f.tenant, eventType, body, keys); err != nil {
			t.Fatalf("direct publish: %v", err)
		}
		direct := f.signals.forWorkflow("wf-direct")
		if len(direct) != 1 {
			t.Fatalf("direct publish signalled %d times, want 1", len(direct))
		}
		reconciled := calls[0].payload
		// What the reconciler hands the engine is what is STORED, through the
		// same function, unaltered.
		if reconciled != string(stored) {
			t.Errorf("reconciled payload is not the stored event_data:\n reconciled %s\n stored     %s", reconciled, stored)
		}
		// And whether the stored form is the published one depends on the column.
		// SQL Server keeps the text as published, so a reconciled payload is
		// byte-identical to a directly published one there. PostgreSQL (JSONB)
		// re-renders the text, and MySQL's JSON column re-renders it AND
		// narrows a number past float64, so neither is. That is a property of
		// storage, shared by the claim path, and is recorded rather than hidden.
		if reconciled != direct[0].payload {
			if f.dialect == plugin.DialectMSSQL {
				t.Errorf("reconciled payload differs from the directly published one on %s:\n reconciled %s\n direct     %s", f.be.Name, reconciled, direct[0].payload)
			}
			t.Logf("MEASURED on %s: published bytes are NOT preserved through storage:\n reconciled %s\n direct     %s", f.be.Name, reconciled, direct[0].payload)
		}
	})
}

// A stranded event is delivered ONCE however many workers sweep. Both workers
// run, concurrently, against the same stranded awaiter, many times over; the
// engine must see exactly one signal per awaiter.
func TestTwoWorkersSweepingOneStrandedAwaiterDeliverOnce(t *testing.T) {
	eachBackend(t, func(t *testing.T, f *reconcileFixture) {
		const rounds = 12
		a, b := f.newWorker(), f.newWorker()

		for i := 0; i < rounds; i++ {
			wf := fmt.Sprintf("wf-race-%d", i)
			f.strand(t, wf, "race.event", json.RawMessage(`{"i":1}`), nil)
		}
		f.age(t)

		var wg sync.WaitGroup
		start := make(chan struct{})
		var mu sync.Mutex
		reported := 0
		for _, w := range []*Plugin{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				n := w.reconcileAwaiters(context.Background(), time.Now())
				mu.Lock()
				reported += n
				mu.Unlock()
			}()
		}
		close(start)
		wg.Wait()

		for i := 0; i < rounds; i++ {
			wf := fmt.Sprintf("wf-race-%d", i)
			if got := len(f.signals.forWorkflow(wf)); got != 1 {
				t.Errorf("%s: signalled %d times by two sweeping workers, want exactly 1", wf, got)
			}
		}
		if reported != rounds {
			t.Errorf("the two sweeps together report %d deliveries, want %d", reported, rounds)
		}
	})
}

func TestAFailedReconcileDeliveryKeepsTheAwaiterAndBacksOff(t *testing.T) {
	eachBackend(t, func(t *testing.T, f *reconcileFixture) {
		w := f.newWorker()
		f.strand(t, "wf-flaky", "flaky.event", json.RawMessage(`{}`), nil)
		f.age(t)

		f.signals.fail = func(string) error { return errors.New("engine unreachable") }
		saved := signalRetryDelays
		signalRetryDelays = nil
		t.Cleanup(func() { signalRetryDelays = saved })

		t0 := time.Now()
		if n := w.reconcileAwaiters(context.Background(), t0); n != 0 {
			t.Fatalf("delivered %d on a failing engine", n)
		}
		if got := f.awaiterCount(t, "wf-flaky"); got != 1 {
			t.Fatalf("awaiter rows after a failed delivery: %d, want 1 -- the registration must be put back", got)
		}
		afterFirst := len(f.signals.forWorkflow("wf-flaky"))
		if afterFirst != 1 {
			t.Fatalf("first sweep made %d attempts, want 1", afterFirst)
		}

		// Inside the backoff window the sweep does not try again.
		w.reconcileAwaiters(context.Background(), t0.Add(reconcileBackoffInitial-time.Second))
		if got := len(f.signals.forWorkflow("wf-flaky")); got != afterFirst {
			t.Errorf("retried inside the backoff window: %d attempts, want %d", got, afterFirst)
		}

		// After it, it does, and a recovered engine gets the delivery.
		f.signals.fail = nil
		if n := w.reconcileAwaiters(context.Background(), t0.Add(reconcileBackoffInitial+time.Second)); n != 1 {
			t.Fatalf("after the backoff, delivered %d, want 1", n)
		}
		if got := f.awaiterCount(t, "wf-flaky"); got != 0 {
			t.Errorf("awaiter rows after the recovered delivery: %d, want 0", got)
		}
	})
}

func TestAnAwaiterWhoseWorkflowIsGoneIsDroppedNotRetried(t *testing.T) {
	eachBackend(t, func(t *testing.T, f *reconcileFixture) {
		w := f.newWorker()
		f.strand(t, "wf-gone", "gone.event", json.RawMessage(`{}`), nil)
		f.age(t)
		f.signals.fail = func(string) error { return plugin.ErrWorkflowNotFound }

		if n := w.reconcileAwaiters(context.Background(), time.Now()); n != 0 {
			t.Errorf("delivered %d to a workflow that does not exist", n)
		}
		if got := f.awaiterCount(t, "wf-gone"); got != 0 {
			t.Errorf("a gone workflow's awaiter was put back: %d rows, want 0", got)
		}
	})
}

// A permanently failing awaiter in backoff must not keep a deliverable one from
// being reached. The page is smaller than the failing set, and the good
// awaiter sorts LAST, so only a scan that pages past the failures reaches it.
func TestFailingAwaitersDoNotStarveADeliverableOne(t *testing.T) {
	eachBackend(t, func(t *testing.T, f *reconcileFixture) {
		saved, savedDelays := reconcilePageSize, signalRetryDelays
		reconcilePageSize, signalRetryDelays = 3, nil
		t.Cleanup(func() { reconcilePageSize, signalRetryDelays = saved, savedDelays })

		w := f.newWorker()
		const bad = 10
		for i := 0; i < bad; i++ {
			f.strand(t, fmt.Sprintf("wf-bad-%d", i), "starve.event", json.RawMessage(`{}`), nil)
		}
		f.strand(t, "wf-good", "starve.event", json.RawMessage(`{}`), nil)
		// The highest id on every dialect's ordering is all ones.
		if _, err := f.db.Exec(f.ctx, `UPDATE event_awaiters SET id = $1 WHERE workflow_id = $2`,
			"ffffffff-ffff-ffff-ffff-ffffffffffff", "wf-good"); err != nil {
			t.Fatalf("pin the good awaiter last: %v", err)
		}
		f.age(t)
		f.signals.fail = func(id string) error {
			if id != "wf-good" {
				return errors.New("this workflow can never be signalled")
			}
			return nil
		}

		t0 := time.Now()
		w.reconcileAwaiters(context.Background(), t0)
		if got := len(f.signals.forWorkflow("wf-good")); got != 1 {
			t.Fatalf("the deliverable awaiter was signalled %d times behind %d failing ones with a page of 3, want 1", got, bad)
		}

		// Second sweep, everything bad is in backoff and still fills the pages.
		w.reconcileAwaiters(context.Background(), t0.Add(time.Second))
		var attempts []string
		for i := 0; i < bad; i++ {
			attempts = append(attempts, fmt.Sprintf("%s=%d", fmt.Sprintf("wf-bad-%d", i), len(f.signals.forWorkflow(fmt.Sprintf("wf-bad-%d", i)))))
		}
		sort.Strings(attempts)
		for _, a := range attempts {
			if a[len(a)-1] != '1' {
				t.Errorf("a failing awaiter was retried inside its backoff: %v", attempts)
				break
			}
		}
	})
}

// An event for another tenant, or other correlation keys, is not a stranded
// event for this awaiter.
func TestTheSweepOnlyDeliversAnEventThatMatchesTheAwaiter(t *testing.T) {
	eachBackend(t, func(t *testing.T, f *reconcileFixture) {
		w := f.newWorker()
		const eventType = "match.event"
		k1, k2, k3, _ := keySlots([]string{"mine"})
		if err := registerAwaiterCore(f.ctx, f.db, f.dialect, f.quiet, f.tenant.String(), "wf-mine", eventType, k1, k2, k3); err != nil {
			t.Fatalf("register: %v", err)
		}
		dead := &plugin.Environment{DB: f.db, Dialect: f.dialect, Logger: f.quiet,
			SignalWorkflow: func(context.Context, string, string, string) error { return errors.New("down") }}

		// Same tenant and type, different key.
		if _, err := PublishEvent(f.ctx, f.db, f.quiet, dead, uuid.New(), f.tenant, eventType, json.RawMessage(`{"k":"other-key"}`), []string{"someone-else"}); err != nil {
			t.Fatalf("publish other key: %v", err)
		}
		// Same type and key, other tenant.
		other := uuid.New()
		if _, err := PublishEvent(plugin.ForTenant(context.Background(), other), f.db, f.quiet, dead, uuid.New(), other, eventType, json.RawMessage(`{"k":"other-tenant"}`), []string{"mine"}); err != nil {
			t.Fatalf("publish other tenant: %v", err)
		}
		f.age(t)
		if n := w.reconcileAwaiters(context.Background(), time.Now()); n != 0 || f.signals.total() != 0 {
			t.Fatalf("delivered %d (signals %d) for events that do not match the awaiter", n, f.signals.total())
		}
		// The sweep above is held up by TWO layers that each refuse a
		// non-matching event: the scan, and the payload read behind it. Break
		// either alone and that assertion stays green, because the other
		// still refuses. So the scan is asked directly.
		scanCtx := plugin.AcrossAllTenants(context.Background(), "cleat#3835: assert the scan, not the sweep")
		rows, err := f.db.Query(scanCtx, stuckAwaiters(10).For(f.dialect), zeroUUID)
		if err != nil {
			t.Fatalf("UNMEASURED: run the scan directly: %v", err)
		}
		scanned := 0
		for rows.Next() {
			scanned++
		}
		rows.Close()
		if scanned != 0 {
			t.Fatalf("the scan returned %d awaiter(s) for events that match none of them", scanned)
		}

		// KNOWN POSITIVE: the same sweep does deliver once a matching event exists.
		if _, err := PublishEvent(f.ctx, f.db, f.quiet, dead, uuid.New(), f.tenant, eventType, json.RawMessage(`{"k":"mine"}`), []string{"mine"}); err != nil {
			t.Fatalf("publish matching: %v", err)
		}
		f.age(t)
		if n := w.reconcileAwaiters(context.Background(), time.Now()); n != 1 {
			t.Fatalf("known positive: delivered %d, want 1 -- the negative above proves nothing", n)
		}
		// The payload is the MATCHING event's, though two older ones of the same
		// type sit ahead of it in seq order.
		got := f.signals.forWorkflow("wf-mine")
		if len(got) != 1 || !bytes.Contains([]byte(got[0].payload), []byte(`"k"`)) || bytes.Contains([]byte(got[0].payload), []byte("other")) {
			t.Errorf("delivered the wrong event's payload: %+v", got)
		}
	})
}

func TestReconcileBackoffDoublesToItsCap(t *testing.T) {
	var b reconcileBackoff
	k := awaiterKey{workflowID: "w"}
	now := time.Unix(0, 0)
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for i, w := range want {
		if got := b.failed(k, now); got != w {
			t.Errorf("failure %d: backoff %v, want %v", i+1, got, w)
		}
	}
	if b.due(k, now) {
		t.Errorf("a failed key is due immediately")
	}
	b.clear(k)
	if !b.due(k, now) {
		t.Errorf("a cleared key is not due")
	}
	b.failed(k, now)
	b.retain(map[awaiterKey]struct{}{})
	if !b.due(k, now) {
		t.Errorf("retain did not forget a key that is no longer stranded")
	}
}


// --require-signal-auth without a grant: the worker refuses every signal this
// plugin sends, and asking again gets the same answer. The reconciler must say
// so once and go to the longest backoff, not retry it at the pace of a
// transient failure on every worker for as long as the waiter waits.
func TestARefusedSignalIsNotRetriedAndIsReportedOnce(t *testing.T) {
	eachBackend(t, func(t *testing.T, f *reconcileFixture) {
		// The real retry schedule is left in place on purpose: a retry would show
		// as a second attempt. Only its sleeps are shortened.
		saved := signalRetryDelays
		signalRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
		t.Cleanup(func() { signalRetryDelays = saved })

		w := f.newWorker()
		f.strand(t, "wf-refused", "refused.event", json.RawMessage(`{}`), nil)
		f.age(t)
		f.signals.fail = func(string) error {
			return fmt.Errorf("signal auth denied: event-triggers not in allowed_signals of wf-refused: %w", plugin.ErrSignalNotAllowed)
		}

		t0 := time.Now()
		if n := w.reconcileAwaiters(context.Background(), t0); n != 0 {
			t.Fatalf("delivered %d to a workflow that refuses the signal", n)
		}
		if got := len(f.signals.forWorkflow("wf-refused")); got != 1 {
			t.Errorf("a refusal was attempted %d times in one sweep, want 1: it will not succeed on a retry", got)
		}
		if got := f.awaiterCount(t, "wf-refused"); got != 1 {
			t.Errorf("awaiter rows after a refusal: %d, want 1 -- the registration is not the reconciler's to drop", got)
		}

		// Straight to the longest backoff, not the first rung.
		w.reconcileAwaiters(context.Background(), t0.Add(reconcileBackoffMax-time.Second))
		if got := len(f.signals.forWorkflow("wf-refused")); got != 1 {
			t.Errorf("retried inside the longest backoff: %d attempts, want 1", got)
		}
		w.reconcileAwaiters(context.Background(), t0.Add(reconcileBackoffMax+time.Second))
		if got := len(f.signals.forWorkflow("wf-refused")); got != 2 {
			t.Errorf("not retried after the longest backoff: %d attempts, want 2", got)
		}

		if got := bytes.Count(f.logBuf.Bytes(), []byte("the worker refuses this plugin's signal")); got != 1 {
			t.Errorf("the refusal was reported %d times across two attempts, want exactly 1:\n%s", got, f.logBuf.String())
		}
	})
}

// A sweep stops when its time budget is spent, after at least one awaiter, and
// the next sweep carries on: every awaiter attempted leaves the due set.
func TestASweepStopsAtItsBudgetAndTheNextOneCarriesOn(t *testing.T) {
	eachBackend(t, func(t *testing.T, f *reconcileFixture) {
		saved := reconcileSweepBudget
		reconcileSweepBudget = time.Nanosecond
		t.Cleanup(func() { reconcileSweepBudget = saved })

		w := f.newWorker()
		const n = 3
		for i := 0; i < n; i++ {
			f.strand(t, fmt.Sprintf("wf-budget-%d", i), "budget.event", json.RawMessage(`{}`), nil)
		}
		f.age(t)
		for sweep := 1; sweep <= n; sweep++ {
			if got := w.reconcileAwaiters(context.Background(), time.Now()); got != 1 {
				t.Fatalf("sweep %d delivered %d with a spent budget, want exactly 1 (at least one, no more)", sweep, got)
			}
		}
		if got := w.reconcileAwaiters(context.Background(), time.Now()); got != 0 {
			t.Errorf("a sweep after all were delivered delivered %d", got)
		}
		if got := f.signals.total(); got != n {
			t.Errorf("the engine saw %d signals, want %d", got, n)
		}
	})
}

// Shutdown reaches the sweep: cancelling the context it was handed stops it
// between awaiters, and a delivery in progress sees the cancellation.
func TestShutdownStopsASweepBetweenAwaiters(t *testing.T) {
	eachBackend(t, func(t *testing.T, f *reconcileFixture) {
		w := f.newWorker()
		for i := 0; i < 4; i++ {
			f.strand(t, fmt.Sprintf("wf-stop-%d", i), "stop.event", json.RawMessage(`{}`), nil)
		}
		f.age(t)

		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		var sawCancelled bool
		f.signals.fail = nil
		w.env.SignalWorkflow = func(ctx context.Context, wf, name, payload string) error {
			cancel() // the worker is shutting down as the first delivery is made
			// context.AfterFunc runs on its own goroutine, so the derived
			// context is cancelled shortly after, not synchronously.
			select {
			case <-ctx.Done():
				sawCancelled = true
			case <-time.After(2 * time.Second):
			}
			return f.signals.signal(ctx, wf, name, payload)
		}
		w.reconcileAwaiters(parent, time.Now())
		if got := f.signals.total(); got != 1 {
			t.Errorf("a sweep whose context was cancelled went on to %d deliveries, want 1", got)
		}
		if !sawCancelled {
			t.Errorf("the per-awaiter context did not carry the cancellation: a retry sleep would outlive shutdown")
		}
	})
}
