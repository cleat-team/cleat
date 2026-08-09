package engine

// S7: adaptive_flush.go had 0.0% coverage across all 21 functions (22 after
// B4 added partitionFencedBatch), including NewTenantFlusherRegistry, which
// cmd/cleat-worker/main.go:817 calls at worker startup whenever batch
// flushing is not disabled -- i.e. on the default configuration. Re-derive
// the before/after numbers with:
//
//	go test ./engine/ -run 'XXXNoTestsMatch' -coverprofile=/tmp/cov.out -covermode=atomic -p 1
//	go tool cover -func=/tmp/cov.out | grep adaptive_flush.go
//
// (CLEAT_TEST_POSTGRES/CLEAT_TEST_MYSQL unset for that command is
// deliberate: coverage is a static property of which lines a test executes,
// not of what a live database does, and the DB-backed tests below still run
// against Postgres in this package's normal `go test` invocation.)

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// NewAdaptiveFlusher / SetEncryption / simple accessors
// ---------------------------------------------------------------------------

func TestNewAdaptiveFlusher_Defaults(t *testing.T) {
	af := NewAdaptiveFlusher(nil, "tenant-1", 0, 0, 0, 0, 0)
	if af.maxWait != 8*time.Millisecond {
		t.Errorf("maxWait = %v, want 8ms default", af.maxWait)
	}
	if af.maxBatch != 200 {
		t.Errorf("maxBatch = %d, want 200 default", af.maxBatch)
	}
	if af.enterThreshold != 500.0 {
		t.Errorf("enterThreshold = %v, want 500.0 default", af.enterThreshold)
	}
	if af.exitThreshold != 250.0 {
		t.Errorf("exitThreshold = %v, want 250.0 default", af.exitThreshold)
	}
	if af.tenantID != "tenant-1" {
		t.Errorf("tenantID = %q, want %q", af.tenantID, "tenant-1")
	}
}

func TestNewAdaptiveFlusher_ExplicitValues(t *testing.T) {
	af := NewAdaptiveFlusher(nil, "t", 5*time.Millisecond, 10, 100.0, 50.0, 0)
	if af.maxWait != 5*time.Millisecond || af.maxBatch != 10 || af.enterThreshold != 100.0 || af.exitThreshold != 50.0 {
		t.Errorf("explicit values not preserved: %+v", af)
	}
}

func TestAdaptiveFlusher_SetEncryption(t *testing.T) {
	af := NewAdaptiveFlusher(nil, "t", 0, 0, 0, 0, 0)
	enc := &PayloadEncryption{}
	af.SetEncryption(true, enc)
	if !af.encryptSensitivePayloads || af.encryption != enc {
		t.Errorf("SetEncryption(true, enc) did not stick: encrypt=%v enc=%v", af.encryptSensitivePayloads, af.encryption)
	}
	af.SetEncryption(false, nil)
	if af.encryptSensitivePayloads || af.encryption != nil {
		t.Errorf("SetEncryption(false, nil) did not clear: encrypt=%v enc=%v", af.encryptSensitivePayloads, af.encryption)
	}
}

func TestAdaptiveFlusher_InBatchMode_GetRate_Stats(t *testing.T) {
	af := NewAdaptiveFlusher(nil, "t", 0, 0, 0, 0, 0)
	if af.InBatchMode() {
		t.Error("a fresh flusher should start in direct mode")
	}
	af.mu.Lock()
	af.batchMode = true
	af.rateEWMA = 42.5
	af.mu.Unlock()
	if !af.InBatchMode() {
		t.Error("InBatchMode did not observe batchMode=true")
	}
	if got := af.GetRate(); got != 42.5 {
		t.Errorf("GetRate() = %v, want 42.5", got)
	}

	af.directFlushes.Add(3)
	af.batchFlushes.Add(2)
	af.batchedEvents.Add(7)
	d, b, e := af.Stats()
	if d != 3 || b != 2 || e != 7 {
		t.Errorf("Stats() = (%d, %d, %d), want (3, 2, 7)", d, b, e)
	}
}

// ---------------------------------------------------------------------------
// Flush: direct mode
// ---------------------------------------------------------------------------

func TestAdaptiveFlusher_Flush_DirectMode(t *testing.T) {
	af := NewAdaptiveFlusher(nil, "t", 0, 0, 0, 0, 0)
	// batchMode defaults to false and updateRate needs a very high rate to
	// flip it, which a single call cannot produce -- so this stays direct.
	done, useBatch := af.Flush(context.Background(), "wf-1", EventRecord{Step: 0}, "cksum", "", 0)
	if useBatch || done != nil {
		t.Errorf("Flush in direct mode: useBatch=%v done=%v, want (false, nil)", useBatch, done)
	}
	if d, _, _ := af.Stats(); d != 1 {
		t.Errorf("directFlushes = %d, want 1", d)
	}
}

// ---------------------------------------------------------------------------
// updateRate: mode transitions, driven explicitly rather than by real time.
//
// CLAUDE.md: "if an assertion depends on wall-clock time, remove the timing
// rather than widening it." updateRate gates on wall-clock elapsed time
// (>= 100ms since the last sample) and computes a rate from the delta in the
// package-level freshStepCount counter. Both are set directly here --
// lastSample pushed into the past, freshStepCount bumped by a known amount --
// so the test asserts on the rate arithmetic and the threshold comparison,
// not on real time actually passing.
// ---------------------------------------------------------------------------

func TestAdaptiveFlusher_UpdateRate_EntersBatchMode(t *testing.T) {
	af := NewAdaptiveFlusher(nil, "t", 0, 0, 10.0, 5.0, 0) // low thresholds, easy to cross
	before := atomic.LoadInt64(&freshStepCount)
	af.lastCount = before
	af.lastSample = time.Now().Add(-1 * time.Second) // force updateRate to compute

	// 1000 steps over the ~1 second window just staged gives a rate far
	// above enterThreshold=10, regardless of how long this line actually
	// takes to run.
	atomic.AddInt64(&freshStepCount, 1000)

	af.updateRate()

	if !af.InBatchMode() {
		t.Errorf("rate should have crossed enterThreshold=10 and switched to batch mode; rate = %v", af.GetRate())
	}
}

func TestAdaptiveFlusher_UpdateRate_ExitsBatchMode(t *testing.T) {
	af := NewAdaptiveFlusher(nil, "t", 0, 0, 10.0, 5.0, 0)
	af.mu.Lock()
	af.batchMode = true
	af.rateEWMA = 100 // seed high so the EWMA needs a real low sample to drag it under exitThreshold
	af.mu.Unlock()

	before := atomic.LoadInt64(&freshStepCount)
	af.lastCount = before
	af.lastSample = time.Now().Add(-1 * time.Second)
	// No steps bumped: rate this sample = 0. EWMA with alpha=0.2 moves
	// 100 -> 80, which is still above exitThreshold=5, so one sample is not
	// enough -- drive it down across several samples the same explicit way
	// updateRate itself would see them, each one 100ms+ apart by
	// lastSample manipulation rather than by sleeping.
	for i := 0; i < 40; i++ {
		af.lastSample = time.Now().Add(-1 * time.Second)
		af.updateRate()
	}

	if af.InBatchMode() {
		t.Errorf("rate should have decayed under exitThreshold=5 and switched back to direct mode; rate = %v", af.GetRate())
	}
}

func TestAdaptiveFlusher_UpdateRate_SkipsWithinSampleWindow(t *testing.T) {
	af := NewAdaptiveFlusher(nil, "t", 0, 0, 1.0, 0.5, 0)
	af.lastSample = time.Now() // "just sampled"
	before := af.rateEWMA
	atomic.AddInt64(&freshStepCount, 1_000_000) // would swamp any threshold if it were counted
	af.updateRate()
	if af.rateEWMA != before {
		t.Errorf("updateRate computed a new rate within the 100ms sample window: rateEWMA changed from %v to %v", before, af.rateEWMA)
	}
}

// ---------------------------------------------------------------------------
// prepareEntry
// ---------------------------------------------------------------------------

func TestAdaptiveFlusher_PrepareEntry_Basic(t *testing.T) {
	af := NewAdaptiveFlusher(nil, "tenant-x", 0, 0, 0, 0, 0)
	rec := EventRecord{
		Step: 3, EventType: EventTypeCall, Service: "svc", Op: "op",
		Request: "req", Response: "resp", DurationMs: 12,
	}
	entry, err := af.prepareEntry("wf-1", rec, "cksum-1")
	if err != nil {
		t.Fatalf("prepareEntry: %v", err)
	}
	if entry.workflowID != "wf-1" || entry.step != 3 {
		t.Errorf("entry identity = (%q, %d), want (wf-1, 3)", entry.workflowID, entry.step)
	}
	if len(entry.params) != 31 {
		t.Fatalf("params has %d entries, want 31 (matching insertEventSQL)", len(entry.params))
	}
	if entry.params[0] != "wf-1" || entry.params[1] != 3 {
		t.Errorf("params[0:2] = %v, want [wf-1 3]", entry.params[0:2])
	}
	if entry.params[29] != "cksum-1" || entry.params[30] != "tenant-x" {
		t.Errorf("params[29:31] = %v, want [cksum-1 tenant-x]", entry.params[29:31])
	}
}

func TestAdaptiveFlusher_PrepareEntry_EncryptionSuccess(t *testing.T) {
	enc, err := NewPayloadEncryption("MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=") // 32 bytes, base64
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	af := NewAdaptiveFlusher(nil, "t", 0, 0, 0, 0, 0)
	af.SetEncryption(true, enc)

	rec := EventRecord{Step: 0, EventType: EventTypeCall, Request: "secret-request", Response: "secret-response"}
	entry, err := af.prepareEntry("wf-1", rec, "cksum")
	if err != nil {
		t.Fatalf("prepareEntry with encryption: %v", err)
	}
	// params[5] is request, params[6] is response (see insertEventSQL order).
	reqNS, ok := entry.params[5].(sql.NullString)
	if !ok || !reqNS.Valid || reqNS.String == "secret-request" {
		t.Errorf("request was not encrypted: params[5] = %#v", entry.params[5])
	}
}

func TestAdaptiveFlusher_PrepareEntry_EncryptionError(t *testing.T) {
	af := NewAdaptiveFlusher(nil, "t", 0, 0, 0, 0, 0)
	// Zero-value PayloadEncryption has a nil key: aes.NewCipher(nil) fails,
	// so EncryptString fails on the very first field -- the same technique
	// TestFlushEvent_EncryptionError (flush_test.go) uses for flushEvent's
	// own encryption branch.
	af.SetEncryption(true, &PayloadEncryption{})

	rec := EventRecord{Step: 0, EventType: EventTypeCall, Request: "req"}
	_, err := af.prepareEntry("wf-1", rec, "cksum")
	if err == nil {
		t.Fatal("prepareEntry with a broken encryptor: want an error, got nil")
	}
}

// ---------------------------------------------------------------------------
// jsonNull / payloadJSONRaw
// ---------------------------------------------------------------------------

func TestJsonNull(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want interface{}
	}{
		{"valid string", sql.NullString{String: "x", Valid: true}, "x"},
		{"invalid string", sql.NullString{Valid: false}, nil},
		{"valid int64", sql.NullInt64{Int64: 7, Valid: true}, int64(7)},
		{"invalid int64", sql.NullInt64{Valid: false}, nil},
		{"passthrough", "already-plain", "already-plain"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := jsonNull(c.in); got != c.want {
				t.Errorf("jsonNull(%#v) = %#v, want %#v", c.in, got, c.want)
			}
		})
	}
}

func TestPayloadJSONRaw(t *testing.T) {
	got := payloadJSONRaw(sql.NullString{String: `{"a":1}`, Valid: true})
	raw, ok := got.(json.RawMessage)
	if !ok || string(raw) != `{"a":1}` {
		t.Errorf("payloadJSONRaw(valid) = %#v, want json.RawMessage(`{\"a\":1}`)", got)
	}
	if got := payloadJSONRaw(sql.NullString{Valid: false}); got != nil {
		t.Errorf("payloadJSONRaw(invalid) = %#v, want nil", got)
	}
	if got := payloadJSONRaw("not-a-nullstring"); got != nil {
		t.Errorf("payloadJSONRaw(wrong type) = %#v, want nil", got)
	}
}

// ---------------------------------------------------------------------------
// errPoolClosed / errIsRetryable
// ---------------------------------------------------------------------------

func TestErrPoolClosed(t *testing.T) {
	if errPoolClosed(nil) {
		t.Error("nil should not be pool-closed")
	}
	if !errPoolClosed(errors.New("sql: database is closed")) {
		t.Error("\"sql: database is closed\" should be pool-closed")
	}
	if !errPoolClosed(errors.New("sql: connection is already closed")) {
		t.Error("\"sql: connection is already closed\" should be pool-closed")
	}
	if errPoolClosed(errors.New("some other error")) {
		t.Error("an unrelated error should not be pool-closed")
	}
}

func TestErrIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"pool closed", errors.New("sql: database is closed"), false},
		// Wrapped, not bare: sql.ErrConnDone's own Error() text is exactly
		// "sql: connection is already closed", which is one of the two exact
		// strings errPoolClosed matches -- so the bare sentinel is caught by
		// the pool-closed check first and never reaches the
		// errors.Is(err, sql.ErrConnDone) branch this case means to exercise.
		// Not a B4 concern (pre-existing, and out of this stream's file
		// ownership) but worth a comment: errIsRetryable's own doc says
		// ErrConnDone is retryable, and a bare sql.ErrConnDone silently is
		// not, for a reason that has nothing to do with connections.
		{"bad conn (wrapped)", fmt.Errorf("pool exhausted: %w", sql.ErrConnDone), true},
		{"deadlock", errors.New("deadlock detected"), true},
		{"serialization failure", errors.New("could not serialize access"), true},
		{"mssql deadlock code", errors.New("Error 1205: deadlock"), true},
		{"connection refused", errors.New("dial tcp: connection refused"), true},
		{"unknown error defaults retryable", errors.New("something unexpected"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := errIsRetryable(c.err); got != c.want {
				t.Errorf("errIsRetryable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestErrIsRetryable_BareErrConnDoneCollidesWithPoolClosed pins the finding
// documented on TestErrIsRetryable's "bad conn (wrapped)" case: a bare
// sql.ErrConnDone is classified non-retryable, contradicting errIsRetryable's
// own doc comment ("driver-level errors ... are transient because the pool
// will provide a fresh connection"), because its Error() text happens to be
// exactly one of the two strings errPoolClosed matches. This pins the
// current (surprising) behavior rather than the documented intent, so a fix
// changes a red test rather than an already-green one.
func TestErrIsRetryable_BareErrConnDoneCollidesWithPoolClosed(t *testing.T) {
	if got := errIsRetryable(sql.ErrConnDone); got != false {
		t.Errorf("errIsRetryable(sql.ErrConnDone) = %v, want false (current behavior, not the documented intent -- see comment)", got)
	}
}

// ---------------------------------------------------------------------------
// retryBatchFlush
// ---------------------------------------------------------------------------

// TestRetryBatchFlush_ContextCancelledDuringBackoff exercises the retry loop
// without a real sleep dictating the assertion: the DB is nil, so the very
// first ExecContext panics... no -- af.db is nil and ExecContext on a nil
// *sql.DB panics, which is not what this test wants. Instead this uses a
// DB pointing at a closed connection, so every attempt fails identically
// and deterministically, and cancels the context immediately so the test
// does not wait through real backoff sleeps at all: the first backoff's
// `case <-ctx.Done()` fires before `case <-time.After(backoff)` because
// ctx is already cancelled when the loop reaches it.
func TestRetryBatchFlush_ContextCancelledDuringBackoff(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://invalid:invalid@127.0.0.1:1/invalid?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	af := &AdaptiveFlusher{db: db}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the loop starts

	err = retryBatchFlush(ctx, af, []byte(`[]`), 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("retryBatchFlush with a pre-cancelled context: err = %v, want context.Canceled", err)
	}
}

// TestRetryBatchFlush_EventuallySucceeds is the DB-backed positive case: a
// real event, using a real batch flusher against the real test database,
// with no forced failures -- confirms the success path (attempt 0, no
// backoff at all) persists the row, independent of the retry machinery
// tested above in isolation.
func TestRetryBatchFlush_EventuallySucceeds(t *testing.T) {
	store, teardown := (&PostgresBackend{}).Setup(t)
	defer teardown()
	ctx := context.Background()

	wfID := newIntentWorkflow(t, ctx, store, "retry-batch-flush")
	af := NewAdaptiveFlusher(rawDBOf(t, store), DefaultTenantUUID, 0, 0, 0, 0, 0)

	entry, err := af.prepareEntry(wfID, EventRecord{Step: 0, EventType: EventTypeCall, Response: "ok"}, "cksum")
	if err != nil {
		t.Fatalf("prepareEntry: %v", err)
	}
	eventsJSON := mustMarshalOneEntryAsBatch(t, entry)

	if err := retryBatchFlush(ctx, af, eventsJSON, 1); err != nil {
		t.Fatalf("retryBatchFlush: %v", err)
	}

	hist, err := store.LoadEventHistory(ctx, wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(hist) != 1 || hist[0].Response != "ok" {
		t.Fatalf("history = %+v, want one event with response \"ok\"", hist)
	}
}

// ---------------------------------------------------------------------------
// Flush / flushAndNotify / onTimer / Run: batch-mode integration, real DB.
//
// These force batch mode directly (af.batchMode = true) rather than driving
// updateRate over real traffic -- the mode-transition arithmetic is already
// covered above in isolation, and mixing the two concerns into one test
// would make a failure here ambiguous about which one broke.
// ---------------------------------------------------------------------------

func TestAdaptiveFlusher_Flush_BatchFillTriggersImmediateFlush(t *testing.T) {
	store, teardown := (&PostgresBackend{}).Setup(t)
	defer teardown()
	ctx := context.Background()

	wfID := newIntentWorkflow(t, ctx, store, "batch-fill")
	af := NewAdaptiveFlusher(rawDBOf(t, store), DefaultTenantUUID, time.Hour /* timer must not fire */, 2, 0, 0, 0)
	af.mu.Lock()
	af.batchMode = true
	af.mu.Unlock()

	done1, useBatch1 := af.Flush(ctx, wfID, EventRecord{Step: 0, EventType: EventTypeCall, Response: "first"}, "cksum-0", "", 0)
	if !useBatch1 {
		t.Fatal("first Flush in batch mode: useBatch = false")
	}
	done2, useBatch2 := af.Flush(ctx, wfID, EventRecord{Step: 1, EventType: EventTypeCall, Response: "second"}, "cksum-1", "", 0)
	if !useBatch2 {
		t.Fatal("second Flush in batch mode: useBatch = false")
	}

	// maxBatch=2 was just reached by the second Flush, which triggers an
	// immediate flush in a goroutine. Block on the done channels -- a real
	// synchronization primitive, not a sleep -- rather than guessing how
	// long the goroutine takes.
	if err := waitDone(t, done1); err != nil {
		t.Fatalf("first entry's done channel: %v", err)
	}
	if err := waitDone(t, done2); err != nil {
		t.Fatalf("second entry's done channel: %v", err)
	}

	hist, err := store.LoadEventHistory(ctx, wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("history has %d events, want 2", len(hist))
	}
}

func TestAdaptiveFlusher_OnTimer_FlushesPartialBatch(t *testing.T) {
	store, teardown := (&PostgresBackend{}).Setup(t)
	defer teardown()
	ctx := context.Background()

	wfID := newIntentWorkflow(t, ctx, store, "timer-flush")
	// maxWait small enough to keep the test fast, maxBatch high enough that
	// only the timer -- not a full batch -- triggers the flush.
	af := NewAdaptiveFlusher(rawDBOf(t, store), DefaultTenantUUID, 5*time.Millisecond, 1000, 0, 0, 0)
	af.mu.Lock()
	af.batchMode = true
	af.mu.Unlock()

	done, useBatch := af.Flush(ctx, wfID, EventRecord{Step: 0, EventType: EventTypeCall, Response: "timer"}, "cksum", "", 0)
	if !useBatch {
		t.Fatal("Flush in batch mode: useBatch = false")
	}

	// Blocks on the done channel the timer's flushAndNotify closes -- bounded
	// by a generous timeout so a genuine hang fails the test instead of
	// hanging the suite, not by guessing the timer's exact firing time.
	if err := waitDoneWithTimeout(t, done, 2*time.Second); err != nil {
		t.Fatalf("onTimer flush: %v", err)
	}

	hist, err := store.LoadEventHistory(ctx, wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("history has %d events, want 1", len(hist))
	}
}

func TestAdaptiveFlusher_Run_FlushesRemainingOnShutdown(t *testing.T) {
	store, teardown := (&PostgresBackend{}).Setup(t)
	defer teardown()
	ctx := context.Background()

	wfID := newIntentWorkflow(t, ctx, store, "run-shutdown")
	af := NewAdaptiveFlusher(rawDBOf(t, store), DefaultTenantUUID, time.Hour, 1000, 0, 0, 0)
	af.mu.Lock()
	af.batchMode = true
	af.mu.Unlock()

	done, useBatch := af.Flush(ctx, wfID, EventRecord{Step: 0, EventType: EventTypeCall, Response: "shutdown"}, "cksum", "", 0)
	if !useBatch {
		t.Fatal("Flush in batch mode: useBatch = false")
	}

	runCtx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		af.Run(runCtx)
		close(runDone)
	}()
	cancel() // Run's <-ctx.Done() unblocks and it flushes whatever is queued.

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	if err := waitDoneWithTimeout(t, done, 2*time.Second); err != nil {
		t.Fatalf("Run's shutdown flush: %v", err)
	}

	hist, err := store.LoadEventHistory(ctx, wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("history has %d events, want 1", len(hist))
	}
}

// ---------------------------------------------------------------------------
// partitionFencedBatch / flushAndNotify's fencing (B4)
// ---------------------------------------------------------------------------

func TestPartitionFencedBatch_NoFencedEntries(t *testing.T) {
	af := &AdaptiveFlusher{}
	batch := []batchEntry{{workflowID: "wf-1"}, {workflowID: "wf-2"}}
	held, lost, err := af.partitionFencedBatch(context.Background(), batch)
	if err != nil {
		t.Fatalf("partitionFencedBatch: %v", err)
	}
	if len(held) != 2 || len(lost) != 0 {
		t.Errorf("held=%d lost=%d, want held=2 lost=0 (no entry asked for fencing)", len(held), len(lost))
	}
}

func TestPartitionFencedBatch_HeldAndLost(t *testing.T) {
	store, teardown := (&PostgresBackend{}).Setup(t)
	defer teardown()
	ctx := context.Background()

	liveID := newIntentWorkflow(t, ctx, store, "batch-fence-live")
	staleID := newIntentWorkflow(t, ctx, store, "batch-fence-stale")

	liveWF, err := store.ClaimWorkflow(ctx, "worker-live")
	if err != nil || liveWF == nil || liveWF.ID != liveID {
		t.Fatalf("ClaimWorkflow (live): wf=%v err=%v", liveWF, err)
	}
	staleWF, err := store.ClaimWorkflow(ctx, "worker-stale")
	if err != nil || staleWF == nil || staleWF.ID != staleID {
		t.Fatalf("ClaimWorkflow (stale): wf=%v err=%v", staleWF, err)
	}
	staleGeneration := staleWF.Generation

	reaped, err := store.ReapStaleInstances(ctx, -1*time.Second)
	if err != nil {
		t.Fatalf("ReapStaleInstances: %v", err)
	}
	if reaped < 1 {
		t.Fatalf("ReapStaleInstances reclaimed %d, want >= 1", reaped)
	}
	// Reap reclaims BOTH (their heartbeat_at is equally "stale" under a
	// negative timeout) -- reclaim the live one back for its owner, the way
	// buildZombieWriterScenario's worker-B does, so only staleID stays lost.
	liveWF2, err := store.ClaimWorkflow(ctx, "worker-live")
	if err != nil || liveWF2 == nil || liveWF2.ID != liveID {
		t.Fatalf("re-ClaimWorkflow (live): wf=%v err=%v", liveWF2, err)
	}

	af := &AdaptiveFlusher{db: rawDBOf(t, store)}
	batch := []batchEntry{
		{workflowID: liveID, workerID: "worker-live", generation: liveWF2.Generation},
		{workflowID: staleID, workerID: "worker-stale", generation: staleGeneration},
		{workflowID: "no-fencing-requested", workerID: ""},
	}

	held, lost, err := af.partitionFencedBatch(ctx, batch)
	if err != nil {
		t.Fatalf("partitionFencedBatch: %v", err)
	}
	if len(held) != 2 {
		t.Fatalf("held has %d entries, want 2 (live + unfenced): %+v", len(held), held)
	}
	if len(lost) != 1 || lost[0].workflowID != staleID {
		t.Fatalf("lost = %+v, want exactly the stale entry (%s)", lost, staleID)
	}
}

// TestFlushAndNotify_PartialFence is the end-to-end version: a batch with
// one live and one stale entry, flushed through the real Flush ->
// flushAndNotify path, must persist the live event and report ErrFenceLost
// on the stale one's own done channel without touching event_history for it.
func TestFlushAndNotify_PartialFence(t *testing.T) {
	store, teardown := (&PostgresBackend{}).Setup(t)
	defer teardown()
	ctx := context.Background()

	liveID := newIntentWorkflow(t, ctx, store, "flushnotify-live")
	staleID := newIntentWorkflow(t, ctx, store, "flushnotify-stale")

	liveWF, err := store.ClaimWorkflow(ctx, "worker-live")
	if err != nil || liveWF == nil || liveWF.ID != liveID {
		t.Fatalf("ClaimWorkflow (live): wf=%v err=%v", liveWF, err)
	}
	staleWF, err := store.ClaimWorkflow(ctx, "worker-stale")
	if err != nil || staleWF == nil || staleWF.ID != staleID {
		t.Fatalf("ClaimWorkflow (stale): wf=%v err=%v", staleWF, err)
	}
	staleGeneration := staleWF.Generation

	reaped, err := store.ReapStaleInstances(ctx, -1*time.Second)
	if err != nil {
		t.Fatalf("ReapStaleInstances: %v", err)
	}
	if reaped < 1 {
		t.Fatalf("ReapStaleInstances reclaimed %d, want >= 1", reaped)
	}
	liveWF2, err := store.ClaimWorkflow(ctx, "worker-live")
	if err != nil || liveWF2 == nil || liveWF2.ID != liveID {
		t.Fatalf("re-ClaimWorkflow (live): wf=%v err=%v", liveWF2, err)
	}

	af := NewAdaptiveFlusher(rawDBOf(t, store), DefaultTenantUUID, time.Hour, 1000, 0, 0, 0)
	af.mu.Lock()
	af.batchMode = true
	af.mu.Unlock()

	liveDone, ok := af.Flush(ctx, liveID, EventRecord{Step: 0, EventType: EventTypeCall, Response: "live"}, "cksum-live", "worker-live", liveWF2.Generation)
	if !ok {
		t.Fatal("live Flush: useBatch = false")
	}
	staleDone, ok := af.Flush(ctx, staleID, EventRecord{Step: 0, EventType: EventTypeCall, Response: "stale"}, "cksum-stale", "worker-stale", staleGeneration)
	if !ok {
		t.Fatal("stale Flush: useBatch = false")
	}

	// Force the accumulated batch out now instead of waiting on the timer.
	af.mu.Lock()
	batch := af.events
	af.events = nil
	af.mu.Unlock()
	af.flushAndNotify(ctx, batch)

	if err := waitDoneWithTimeout(t, liveDone, 2*time.Second); err != nil {
		t.Fatalf("live entry's done channel: %v", err)
	}
	staleErr := <-staleDone
	if !errors.Is(staleErr, ErrFenceLost) {
		t.Fatalf("stale entry's done channel: err = %v, want ErrFenceLost", staleErr)
	}

	liveHist, err := store.LoadEventHistory(ctx, liveID)
	if err != nil {
		t.Fatalf("LoadEventHistory (live): %v", err)
	}
	if len(liveHist) != 1 || liveHist[0].Response != "live" {
		t.Fatalf("live history = %+v, want one event with response \"live\"", liveHist)
	}

	staleHist, err := store.LoadEventHistory(ctx, staleID)
	if err != nil {
		t.Fatalf("LoadEventHistory (stale): %v", err)
	}
	if len(staleHist) != 0 {
		t.Fatalf("B4 regression: the stale entry persisted despite a lost fence: %+v", staleHist)
	}
}

// ---------------------------------------------------------------------------
// TenantFlusherRegistry
// ---------------------------------------------------------------------------

func TestNewTenantFlusherRegistry_Defaults(t *testing.T) {
	r := NewTenantFlusherRegistry(nil, FlusherConfig{})
	if r.config.MaxWait != 8*time.Millisecond || r.config.MaxBatch != 200 ||
		r.config.EnterThreshold != 500.0 || r.config.ExitThreshold != 250.0 {
		t.Errorf("registry did not apply FlusherConfig defaults: %+v", r.config)
	}
}

func TestTenantFlusherRegistry_ForCreatesAndReuses(t *testing.T) {
	r := NewTenantFlusherRegistry(nil, FlusherConfig{})
	af1 := r.For("tenant-a")
	af2 := r.For("tenant-a")
	if af1 != af2 {
		t.Error("For called twice with the same tenant returned two different flushers")
	}
	af3 := r.For("tenant-b")
	if af3 == af1 {
		t.Error("For gave two different tenants the same flusher")
	}
}

func TestTenantFlusherRegistry_Remove(t *testing.T) {
	r := NewTenantFlusherRegistry(nil, FlusherConfig{})
	af1 := r.For("tenant-a")
	r.Remove("tenant-a")
	af2 := r.For("tenant-a")
	if af1 == af2 {
		t.Error("For after Remove returned the removed flusher instead of a fresh one")
	}
}

func TestTenantFlusherRegistry_SetEncryption(t *testing.T) {
	r := NewTenantFlusherRegistry(nil, FlusherConfig{})
	af := r.For("tenant-a") // created before SetEncryption
	enc := &PayloadEncryption{}
	r.SetEncryption(true, enc)
	if !af.encryptSensitivePayloads || af.encryption != enc {
		t.Error("SetEncryption did not propagate to an existing flusher")
	}

	af2 := r.For("tenant-b") // created after SetEncryption
	if !af2.encryptSensitivePayloads || af2.encryption != enc {
		t.Error("a flusher created after SetEncryption did not inherit the registry's settings")
	}
}

func TestTenantFlusherRegistry_Shutdown_FlushesPending(t *testing.T) {
	store, teardown := (&PostgresBackend{}).Setup(t)
	defer teardown()
	ctx := context.Background()

	wfID := newIntentWorkflow(t, ctx, store, "registry-shutdown")
	r := NewTenantFlusherRegistry(rawDBOf(t, store), FlusherConfig{MaxWait: time.Hour, MaxBatch: 1000})
	af := r.For(DefaultTenantUUID)
	af.mu.Lock()
	af.batchMode = true
	af.mu.Unlock()

	done, useBatch := af.Flush(ctx, wfID, EventRecord{Step: 0, EventType: EventTypeCall, Response: "registry-shutdown"}, "cksum", "", 0)
	if !useBatch {
		t.Fatal("Flush in batch mode: useBatch = false")
	}

	r.Shutdown() // synchronous: flushAndNotify is called directly, not via goroutine

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown's flush: %v", err)
		}
	default:
		t.Fatal("Shutdown returned without closing the pending entry's done channel")
	}

	hist, err := store.LoadEventHistory(ctx, wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("history has %d events, want 1", len(hist))
	}
}

// ---------------------------------------------------------------------------
// test helpers local to this file
// ---------------------------------------------------------------------------

// waitDone blocks on a Flush-returned done channel with a short default
// timeout, so a genuine deadlock fails the test rather than hanging the
// suite.
func waitDone(t *testing.T, done chan error) error {
	t.Helper()
	return waitDoneWithTimeout(t, done, 2*time.Second)
}

func waitDoneWithTimeout(t *testing.T, done chan error, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		t.Fatalf("done channel did not resolve within %v", timeout)
		return nil
	}
}

// mustMarshalOneEntryAsBatch reproduces flushAndNotify's own event-shaping
// step for a single entry, so retryBatchFlush -- which expects the same
// jsonb_populate_recordset-shaped payload flushAndNotify builds -- can be
// exercised directly without going through the batching machinery above it.
func mustMarshalOneEntryAsBatch(t *testing.T, entry batchEntry) []byte {
	t.Helper()
	p := entry.params
	event := map[string]interface{}{
		"workflow_id": p[0], "step": p[1], "event_type": p[2],
		"service": jsonNull(p[3]), "operation": jsonNull(p[4]),
		"request": jsonNull(p[5]), "response": jsonNull(p[6]), "error": jsonNull(p[7]),
		"duration_ms": jsonNull(p[8]), "signal_names": jsonNull(p[9]), "timeout_ms": jsonNull(p[10]),
		"signal_name": jsonNull(p[11]), "signal_payload": jsonNull(p[12]),
		"defer_description": jsonNull(p[13]), "defer_id": jsonNull(p[14]),
		"child_name": jsonNull(p[15]), "child_input": jsonNull(p[16]), "run_id": jsonNull(p[17]), "new_input": jsonNull(p[18]),
		"plugin_name": jsonNull(p[19]), "plugin_func": jsonNull(p[20]), "plugin_input": jsonNull(p[21]),
		"plugin_output": jsonNull(p[22]), "plugin_error": jsonNull(p[23]),
		"promise_name": jsonNull(p[24]), "promise_id": jsonNull(p[25]), "promise_result": jsonNull(p[26]), "promise_error": jsonNull(p[27]),
		"payload": payloadJSONRaw(p[28]), "checksum": p[29], "tenant_id": p[30], "created_at": time.Now(),
	}
	b, err := json.Marshal([]map[string]interface{}{event})
	if err != nil {
		t.Fatalf("marshal batch entry: %v", err)
	}
	return b
}
