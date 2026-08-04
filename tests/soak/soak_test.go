//go:build soak_test

// Package soak provides long-running soak tests for cleat.
//
// These tests run for extended periods (hours) to detect leaks and slow
// degradation that a short test cannot see. They are excluded from normal
// test runs via the build tag "soak_test", and are run by
// .github/workflows/soak.yml on a schedule -- deliberately not as a PR gate.
//
// Usage:
//
//	# Run for the default duration (1 hour)
//	go test -tags=soak_test -run TestSoakWorkflowMix -timeout=90m ./tests/soak/
//
//	# Run for 24 hours
//	SOAK_DURATION=24h go test -tags=soak_test -run TestSoakWorkflowMix -timeout=25h ./tests/soak/
//
// The -timeout must exceed SOAK_DURATION. It is the harness's bound on the
// whole binary, not the test's own budget; setting the two equal makes the
// panic race the assertions.
//
// # What it exercises
//
// The workload drives the real PostgresStore lifecycle -- insert, claim,
// append events, complete -- at a bounded concurrency, continuously, for the
// configured duration. Every error counted is an error the store returned.
//
// This is worth stating because it was not always so. The original version of
// this file opened a database, pinged it, assigned it to `_` with the comment
// "actual workload uses in-memory simulation", and then ran a loop whose body
// was time.Sleep(rand.Intn(50)ms) with `success := rand.Float64() > 0.05`. The
// error rate it measured against a 10% threshold was that hardcoded 5% coin
// flip; the workflow type it selected was passed into the goroutine and never
// referenced; EventsPerWorkflow was set and never read. An hour of that
// asserts that math/rand works. Nothing about cleat was under test, so
// scheduling it would have produced a green badge for an untested engine --
// which is worse than not running it at all.
//
// # Metrics
//
//   - Heap in use (runtime.MemStats.HeapAlloc)
//   - Goroutine count (runtime.NumGoroutine)
//   - Workflow throughput (workflows completed per second)
//   - Error rate (store errors / total attempts)
//
// The test fails if:
//   - Heap or goroutine count in the final window is materially above the
//     post-warmup baseline (see leakDetector.check)
//   - Throughput in the final window drops below 25% of the baseline window
//   - Error rate exceeds 1%
//   - Any completed workflow fails checksum verification
package soak

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"

	_ "github.com/lib/pq"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// soakConfig holds the configuration for a soak test run.
type soakConfig struct {
	// Duration is the total duration of the soak test.
	Duration time.Duration

	// SampleInterval is how often to collect metrics.
	SampleInterval time.Duration

	// MaxConcurrentWorkflows is the maximum number of workflows in flight.
	MaxConcurrentWorkflows int

	// VerifyEvery is the sampling rate for checksum verification: one in
	// every N completed workflows is re-verified. Verifying all of them
	// would make the run measure verification rather than the workload.
	VerifyEvery int
}

// defaultSoakConfig returns the default soak test configuration.
func defaultSoakConfig(t *testing.T) soakConfig {
	duration := 1 * time.Hour // default: 1 hour
	if s := os.Getenv("SOAK_DURATION"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			// Previously this fell back to the 1h default on a parse error,
			// so a typo in the CI input silently produced a run of the wrong
			// length -- and a 24h run that quietly took 1h looks exactly like
			// a 24h run that passed.
			t.Fatalf("SOAK_DURATION=%q: %v", s, err)
		}
		duration = d
	}

	// The harness timeout has to leave room for the run plus its teardown.
	// Set equal to (or below) SOAK_DURATION -- which is exactly what a
	// workflow written as `-timeout="${SOAK_DURATION}"` does -- the binary
	// panics at the deadline while the test is still unwinding, and every
	// assertion below is skipped. That failure reads as a hung test rather
	// than a misconfigured one, so it is caught here, in the first
	// millisecond, instead of hours later.
	if f := flag.Lookup("test.timeout"); f != nil {
		if d, err := time.ParseDuration(f.Value.String()); err == nil && d > 0 && d <= duration {
			t.Fatalf("-timeout=%v must exceed SOAK_DURATION=%v: the harness would kill the run before it finished", d, duration)
		}
	}

	cfg := soakConfig{
		Duration:               duration,
		SampleInterval:         30 * time.Second,
		MaxConcurrentWorkflows: 50,
		VerifyEvery:            100,
	}

	// The leak and throughput checks compare a baseline window against a
	// final window, and need enough samples for each to be more than noise.
	if min := time.Duration(4*samplesPerWindow) * cfg.SampleInterval; cfg.Duration < min {
		cfg.SampleInterval = cfg.Duration / time.Duration(4*samplesPerWindow)
	}
	return cfg
}

// ---------------------------------------------------------------------------
// Leak detection
// ---------------------------------------------------------------------------

// samplesPerWindow is the number of samples in the baseline and final
// comparison windows.
const samplesPerWindow = 5

// leakDetector tracks heap and goroutine metrics over time.
//
// It compares an early baseline window against the final window (see
// windows for how each is placed), rather than looking for a run of
// consecutive increases as this originally did. That older check ran on
// every sample and fired on 5 strictly increasing readings, which for noisy
// data has probability 1/5! per window -- across the ~120 windows of a
// one-hour run it was near-certain to fire at least once by chance. That is
// why the memory arm of it had been demoted to a t.Logf("WARNING"): it was
// too flaky to assert on, so it asserted nothing. A windowed comparison is
// both robust enough to fail the build on and sensitive to the thing that
// actually matters, which is the trend across the whole run and not the
// direction of five adjacent readings.
type leakDetector struct {
	mu               sync.Mutex
	heapSamples      []uint64 // HeapAlloc in bytes
	goroutineSamples []int
}

func newLeakDetector() *leakDetector {
	return &leakDetector{}
}

func (d *leakDetector) sample() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	d.mu.Lock()
	defer d.mu.Unlock()

	// HeapAlloc, and the field is named for it. The doc comment used to
	// promise "RSS memory (read from /proc/self/status or /proc/self/statm)"
	// over a field commented "RSS in bytes" that was only ever fed
	// MemStats.Alloc. Live heap is the right instrument for the leak class
	// this test can actually detect -- retained Go objects -- and it is
	// portable, which /proc is not.
	d.heapSamples = append(d.heapSamples, m.HeapAlloc)
	d.goroutineSamples = append(d.goroutineSamples, runtime.NumGoroutine())
}

// windows returns the baseline and final windows of a sample series, or
// false if there are not yet enough samples for both.
//
// The baseline begins a tenth of the way into the run: not at the very start,
// because process warmup, connection pool fill and the first GC cycles land
// there and would mask real growth behind them -- but at a *fraction* of the
// run rather than a fixed sample index.
//
// That distinction was measured, not reasoned about. With the baseline pinned
// at samples 5..9 the detector's sensitivity depended on how long the run was:
// on a short run those samples sit a quarter of the way in, so a perfectly
// linear leak has only reached ~2.5x of its baseline by the end and slips
// under any threshold loose enough not to fire on noise. A deliberately
// injected 4KB-per-workflow leak -- 140 MB over 30 seconds -- was not caught.
// Anchoring the baseline to a fraction of the run gives a linear leak the same
// lever arm whether the run is 30 seconds or 24 hours, and the same injected
// leak then fails the test.
func windows[T any](samples []T) (baseline, final []T, ok bool) {
	if len(samples) < 3*samplesPerWindow {
		return nil, nil, false
	}
	start := len(samples) / 10
	if start+2*samplesPerWindow > len(samples) {
		start = len(samples) - 2*samplesPerWindow
	}
	return samples[start : start+samplesPerWindow], samples[len(samples)-samplesPerWindow:], true
}

// median returns the median of a copy of the given samples.
func median[T uint64 | int](samples []T) float64 {
	sorted := make([]T, len(samples))
	copy(sorted, samples)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	n := len(sorted)
	if n%2 == 1 {
		return float64(sorted[n/2])
	}
	return float64(sorted[n/2-1]+sorted[n/2]) / 2
}

// check reports leak findings, or nil if the run looks flat.
//
// The workload holds a fixed number of workflows in flight, so both metrics
// should be flat across the run. The multipliers are the tolerance for a
// process that is still allocating and still scheduling: growth under them
// is not evidence of anything, growth over them is not explainable by noise
// in a workload whose shape never changes.
func (d *leakDetector) check() []error {
	d.mu.Lock()
	defer d.mu.Unlock()

	const (
		heapGrowthFactor      = 3.0
		goroutineGrowthFactor = 2.0
		// Below this, a factor is meaningless: a handful of goroutines
		// either way is scheduling noise, not a leak.
		goroutineFloor = 20.0
	)

	var errs []error
	if base, final, ok := windows(d.heapSamples); ok {
		b, f := median(base), median(final)
		if b > 0 && f > b*heapGrowthFactor {
			errs = append(errs, fmt.Errorf("heap grew %.1fx over the run: baseline %.1f MB, final %.1f MB (samples: baseline %v, final %v)",
				f/b, b/1024/1024, f/1024/1024, base, final))
		}
	}
	if base, final, ok := windows(d.goroutineSamples); ok {
		b, f := median(base), median(final)
		if f > goroutineFloor && f > b*goroutineGrowthFactor {
			errs = append(errs, fmt.Errorf("goroutines grew %.1fx over the run: baseline %.0f, final %.0f (samples: baseline %v, final %v)",
				f/b, b, f, base, final))
		}
	}
	return errs
}

// report returns a formatted summary of the most recent sample.
func (d *leakDetector) report() string {
	d.mu.Lock()
	defer d.mu.Unlock()

	var heapStr, gorStr string
	if n := len(d.heapSamples); n > 0 {
		heapStr = fmt.Sprintf("%.2f MB", float64(d.heapSamples[n-1])/1024/1024)
	}
	if n := len(d.goroutineSamples); n > 0 {
		gorStr = fmt.Sprintf("%d", d.goroutineSamples[n-1])
	}
	return fmt.Sprintf("heap=%s goroutines=%s", heapStr, gorStr)
}

// ---------------------------------------------------------------------------
// Workload generator
// ---------------------------------------------------------------------------

// workflowType is a synthetic workflow shape. The event mix varies the row
// sizes and the number of rows per append, so the run is not one statement
// repeated for an hour.
type workflowType struct {
	name   string
	events []engine.EventRecord
}

func call(op string, payload string) engine.EventRecord {
	return engine.EventRecord{EventType: engine.EventTypeCall, Service: "soak", Op: op, Request: payload, Response: `{"ok":true}`}
}

var workflowTypes = []workflowType{
	{
		name: "sequential",
		events: []engine.EventRecord{
			call("start", `{}`),
			call("process", `{"n":1}`),
			call("finish", `{}`),
		},
	},
	{
		name: "wide",
		events: []engine.EventRecord{
			call("start", `{}`),
			// A deliberately large request body: the store paths handle
			// payload sizes very differently, and an hour of tiny rows
			// exercises none of that.
			call("bulk", fmt.Sprintf(`{"blob":%q}`, strings0(8192))),
			call("finish", `{}`),
		},
	},
	{
		name: "signals",
		events: []engine.EventRecord{
			{EventType: engine.EventTypeAwaitSignals, SignalNames: "go", TimeoutMs: 60000},
			{EventType: engine.EventTypeSignalReceived, SignalName: "go", Response: `{"v":1}`},
			call("after-signal", `{}`),
		},
	},
	{
		name: "promises",
		events: []engine.EventRecord{
			{EventType: engine.EventTypeCreatePromise, Op: "p1"},
			{EventType: engine.EventTypeAwaitPromise, Op: "p1"},
			{EventType: engine.EventTypePromiseResolved, Op: "p1", Response: `{"v":1}`},
			call("after-promise", `{}`),
		},
	},
}

// strings0 returns a string of n 'x' bytes.
func strings0(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

// workloadMetrics tracks aggregate counters across the soak run.
//
// The counters are atomics rather than mutex-guarded fields because the
// sampling loop reads them every interval while every worker writes them;
// under -race the old mutex-per-record on the hot path was itself a
// meaningful share of what the run measured.
type workloadMetrics struct {
	total     atomic.Int64
	successes atomic.Int64
	failures  atomic.Int64
	verified  atomic.Int64
	startTime time.Time
}

func newWorkloadMetrics() *workloadMetrics {
	return &workloadMetrics{startTime: time.Now()}
}

func (m *workloadMetrics) record(success bool) {
	m.total.Add(1)
	if success {
		m.successes.Add(1)
	} else {
		m.failures.Add(1)
	}
}

func (m *workloadMetrics) throughput() float64 {
	elapsed := time.Since(m.startTime).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return float64(m.total.Load()) / elapsed
}

func (m *workloadMetrics) errorRate() float64 {
	total := m.total.Load()
	if total == 0 {
		return 0
	}
	return float64(m.failures.Load()) / float64(total)
}

// runOne drives a single workflow through the real store lifecycle: insert,
// claim, append its event mix, complete, and -- on a sample -- verify the
// checksum chain. It returns the first error the store returned.
func runOne(ctx context.Context, store *engine.PostgresStore, db *sql.DB, workerID string, wt workflowType, id string, verify bool) error {
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES ($1, 'test', 1, 'ready', '{}', $2)`, id, soakQueue); err != nil {
		return fmt.Errorf("insert %s: %w", id, err)
	}

	// ClaimWorkflow decides which workflow this worker gets; it need not be
	// the one just inserted. Everything downstream uses wf.ID and
	// wf.Generation, never the local id -- writing to a workflow this worker
	// does not hold would lose the generation fence, which is exactly the
	// bug tests/scale had.
	wf, err := store.ClaimWorkflow(ctx, workerID)
	if err != nil {
		return fmt.Errorf("claim by %s: %w", workerID, err)
	}
	if wf == nil {
		// Another worker took it. Not an error: the row is still ready and
		// some worker will claim it.
		return nil
	}

	events := make([]engine.EventRecord, len(wt.events))
	copy(events, wt.events)
	now := time.Now().UnixMilli()
	for i := range events {
		events[i].Step = i
		events[i].TimestampMs = now + int64(i)
	}
	if err := store.AppendEventHistoryBatch(ctx, wf.ID, events); err != nil {
		return fmt.Errorf("append to %s (%s): %w", wf.ID, wt.name, err)
	}

	if err := store.CompleteWorkflow(ctx, wf.ID, workerID, wf.Generation, `{"status":"success"}`, nil); err != nil {
		return fmt.Errorf("complete %s: %w", wf.ID, err)
	}

	if verify {
		// The checksum chain is the one correctness property that a long run
		// can degrade without any single operation failing. Appending events
		// across calls used to restart the chain (see IMPROVEMENT-PLAN 2.30);
		// a soak run that never verifies would not have noticed.
		if err := store.VerifyWorkflowEvents(ctx, wf.ID); err != nil {
			return fmt.Errorf("verify %s (%s): %w", wf.ID, wt.name, err)
		}
	}
	return nil
}

// runWorkloadLoop dispatches workflows through the store until ctx is
// cancelled, holding at most cfg.MaxConcurrentWorkflows in flight.
func runWorkloadLoop(ctx context.Context, cfg soakConfig, store *engine.PostgresStore, db *sql.DB, metrics *workloadMetrics, errs *errorSample) {
	var wg sync.WaitGroup
	defer wg.Wait()

	sem := make(chan struct{}, cfg.MaxConcurrentWorkflows)
	var seq atomic.Int64

	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(n int64) {
			defer wg.Done()
			defer func() { <-sem }()

			wt := workflowTypes[rand.Intn(len(workflowTypes))]
			id := fmt.Sprintf("soak-%d", n)
			verify := cfg.VerifyEvery > 0 && n%int64(cfg.VerifyEvery) == 0

			err := runOne(ctx, store, db, fmt.Sprintf("soak-worker-%d", n%16), wt, id, verify)
			if err != nil && ctx.Err() != nil {
				// Shutdown, not a failure: the context cancels mid-flight by
				// design when the duration elapses. Counting these would make
				// the error rate a function of how many workflows happened to
				// be in flight at the deadline.
				return
			}
			if err != nil {
				errs.add(err)
			}
			if verify && err == nil {
				metrics.verified.Add(1)
			}
			metrics.record(err == nil)
		}(seq.Add(1))
	}
}

// errorSample retains the first few distinct errors seen, so a failing run
// reports what went wrong instead of only how often.
type errorSample struct {
	mu   sync.Mutex
	errs []error
}

func (s *errorSample) add(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.errs) < 10 {
		s.errs = append(s.errs, err)
	}
}

func (s *errorSample) all() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]error(nil), s.errs...)
}

// ---------------------------------------------------------------------------
// Soak test
// ---------------------------------------------------------------------------

// soakQueue keeps this suite's workflows off the queues a live worker or
// another suite might be draining. A soak run against a shared database
// otherwise claims rows it did not create and reports the resulting
// generation conflicts as its own error rate.
const soakQueue = "queue-soak-tests"

func TestSoakWorkflowMix(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping soak test in short mode")
	}

	// testutil.TestDB builds the schema from migrations/postgres and fails,
	// rather than skips, when CLEAT_TEST_DB is set but unreachable. The
	// original code here opened the DSN, skipped on a ping error, and then
	// ignored the handle entirely -- so the suite could neither fail for
	// want of a database nor use the one it had.
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()

	// workflow_instances_def_name_def_version_fkey requires the definition
	// row; nothing else in this package creates it.
	if _, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points)
		VALUES ('test', 1, '\x00', '{}') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed workflow_defs(test, 1): %v", err)
	}
	db.Exec(`DELETE FROM event_history WHERE workflow_id LIKE 'soak-%'`)
	db.Exec(`DELETE FROM workflow_instances WHERE id LIKE 'soak-%'`)

	// soakQueue must be passed here, not just written into the rows:
	// ClaimWorkflows filters on `task_queue = ANY($2)`, which is the store's
	// queue list, and NewPostgresStore(db) with no queues polls "default"
	// only. Without this the claim returns nil for every workflow forever --
	// and runOne treats a nil claim as "someone else took it" and reports
	// success, so the run would count a million successes having executed
	// nothing. The completed-count assertion at the end is what catches that
	// if this line is ever lost.
	store := engine.NewPostgresStore(db, soakQueue)
	cfg := defaultSoakConfig(t)

	t.Logf("Soak test configuration:")
	t.Logf("  Duration: %v", cfg.Duration)
	t.Logf("  Sample interval: %v", cfg.SampleInterval)
	t.Logf("  Workflow types: %d", len(workflowTypes))
	t.Logf("  Max concurrent workflows: %d", cfg.MaxConcurrentWorkflows)
	t.Logf("  Verify every: %d", cfg.VerifyEvery)

	metrics := newWorkloadMetrics()
	detector := newLeakDetector()
	errs := &errorSample{}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Duration)
	defer cancel()

	var loopDone sync.WaitGroup
	loopDone.Add(1)
	go func() {
		defer loopDone.Done()
		runWorkloadLoop(ctx, cfg, store, db, metrics, errs)
	}()

	ticker := time.NewTicker(cfg.SampleInterval)
	defer ticker.Stop()

	var throughputSamples []uint64
	sampleCount := 0

sampling:
	for {
		select {
		case <-ctx.Done():
			break sampling
		case <-ticker.C:
			sampleCount++
			detector.sample()
			// Interval throughput, not cumulative: a cumulative average
			// converges and stops responding to degradation, so the final
			// window of a run that fell to zero still reads near its mean.
			completed := uint64(metrics.total.Load())
			var interval uint64
			if n := len(throughputSamples); n > 0 {
				interval = completed - sum(throughputSamples)
			} else {
				interval = completed
			}
			throughputSamples = append(throughputSamples, interval)

			t.Logf("[sample %d] %s interval=%d cumulative=%d throughput=%.1f wf/s error_rate=%.3f%%",
				sampleCount, detector.report(), interval, completed,
				metrics.throughput(), metrics.errorRate()*100)
		}
	}

	// Let the in-flight workflows unwind before reading the final counters.
	loopDone.Wait()

	total := metrics.total.Load()
	t.Logf("=== Soak Test Complete ===")
	t.Logf("  Duration: %v", cfg.Duration)
	t.Logf("  Total workflows: %d", total)
	t.Logf("  Succeeded: %d", metrics.successes.Load())
	t.Logf("  Failed: %d", metrics.failures.Load())
	t.Logf("  Chain-verified: %d", metrics.verified.Load())
	t.Logf("  Throughput: %.2f wf/s", metrics.throughput())
	t.Logf("  Error rate: %.3f%%", metrics.errorRate()*100)
	t.Logf("  Final: %s", detector.report())

	for _, err := range errs.all() {
		t.Errorf("workload error: %v", err)
	}

	// A run that did no work passes every ratio check below: 0 errors out of
	// 0, and a flat heap. That is the shape every one of the suites wired up
	// this week turned out to have, so it is asserted first.
	if total == 0 {
		t.Fatal("no workflows ran: the workload did nothing for the whole duration")
	}
	if metrics.verified.Load() == 0 && cfg.VerifyEvery > 0 && total > int64(cfg.VerifyEvery) {
		t.Errorf("no workflow had its checksum chain verified across %d workflows", total)
	}

	// The counters above are the workload's own account of itself. This is
	// the database's, and it is the assertion that makes the whole run
	// non-vacuous: runOne reports success both when it completes a workflow
	// and when ClaimWorkflow returns nil because another worker got there
	// first. If claiming were broken outright -- a queue mismatch, a fence
	// regression, a store built with the wrong task queue -- every call would
	// take the nil path and the run would report a perfect success rate
	// having executed nothing at all. Rows in 'done' cannot be faked that
	// way; only CompleteWorkflow puts them there.
	var done int64
	if err := db.QueryRow(`SELECT count(*) FROM workflow_instances WHERE id LIKE 'soak-%' AND status = 'done'`).Scan(&done); err != nil {
		t.Fatalf("count completed workflows: %v", err)
	}
	t.Logf("  Reached 'done' in the database: %d", done)
	if done == 0 {
		t.Fatalf("no workflow reached 'done' across %d attempts: the workload claimed and completed nothing", total)
	}
	// Not equality: the rows inserted in the final moments before the
	// deadline are still 'ready', and a claim that loses the race is counted
	// as an attempt without producing a completion.
	if float64(done) < float64(total)*0.5 {
		t.Errorf("only %d of %d workflows reached 'done' (%.0f%%); the workload is mostly not completing anything",
			done, total, float64(done)/float64(total)*100)
	}

	// 1%, not the 10% this used to allow. Against a healthy database every
	// one of these operations should succeed; 10% was chosen to sit above a
	// simulated 5% coin flip, and as a threshold on real store errors it
	// would let one workflow in twelve fail unnoticed.
	if rate := metrics.errorRate(); rate > 0.01 {
		t.Errorf("error rate %.3f%% exceeds 1%% threshold (%d failures of %d)",
			rate*100, metrics.failures.Load(), total)
	}

	for _, err := range detector.check() {
		t.Errorf("leak: %v", err)
	}

	if base, final, ok := windows(throughputSamples); ok {
		b, f := median(base), median(final)
		if b > 0 && f < b*0.25 {
			t.Errorf("throughput degraded to %.0f%% of baseline: %.0f workflows/interval -> %.0f (samples: baseline %v, final %v)",
				f/b*100, b, f, base, final)
		}
	}
}

// sum returns the total of a uint64 slice.
func sum(xs []uint64) uint64 {
	var t uint64
	for _, x := range xs {
		t += x
	}
	return t
}
