package engine

// cleat#2139. #2060's acceptance criterion asked that, while each of the
// three SQL Server retention sweeps runs, "the concurrent writer's p99
// insert latency stays within a stated bound of its no-sweep baseline".
// #2138 fixed the CAUSE (lock escalation on event_history, via
// mssqlEventRowChunk's row-bounded deletes -- see
// mssql_retention_2060_lock_escalation_test.go, this package's own sibling
// file, for that regression test) but the latency half of the acceptance
// criterion had no harness. This is that harness.
//
// PLACEMENT: the issue's own text says both "the engine suite has no
// harness for the latency half" (true) and "in the scale/soak suite, run a
// concurrent writer" (tests/scale is a different, external package). This
// lives in engine/ instead, alongside #2060/#2138's own regression test,
// because it reuses that test's already-reviewed private-database and
// seeding infrastructure directly (mssqlPrivateRCSIDatabase,
// seedMSSQL2060Workflows, openMSSQLTenantStore -- all unexported, so only
// reachable from within this package). Duplicating that machinery into
// tests/scale to satisfy the issue's literal placement would be a second,
// drifting copy of code this package already has reviewed and working;
// reusing it here does not.
//
// RCSI: measured ON only (Azure SQL Database's own default, #2059/#982),
// not both arms the way the escalation test covers. The escalation itself
// -- a table lock blocking the writer -- does not depend on RCSI: RCSI
// governs whether READS get a blocking-free snapshot, not whether a
// DELETE's own lock footprint escalates, so the writer-contention question
// this test asks is not expected to differ by RCSI setting. Covering both
// would double this test's already-substantial runtime (6 x 6000-workflow
// seeds) for a dimension the underlying mechanism does not interact with.
//
// BASELINE AND TREATMENT USE FRESH DATABASES, run back to back within one
// t.Run per arm rather than reusing one across both: the same reasoning
// the escalation test gives for one private database per ARM applies one
// level further -- a baseline measured against a database that a sweep
// has already touched is not measuring "no sweep", it is measuring
// "sweep, then quiet after".

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// mssql2139WriterConcurrency and mssql2139WriterSamples: the same shape as
// tests/scale/latency_test.go's TestLatencyP99 (4 goroutines, samples in
// the low hundreds), reused here rather than invented independently so the
// two suites' idea of "moderate concurrent load" does not silently
// diverge.
const (
	mssql2139WriterConcurrency = 4
	mssql2139WriterSamples     = 200
)

// mssql2139WriterLatencies runs mssql2139WriterConcurrency goroutines,
// each repeatedly creating a fresh, non-terminal ('running', no
// completed_at) workflow_instances row under writerDef and appending one
// event_history row to it -- never a row any of the three retention
// sweeps' predicates can match, since none of them touch a row with no
// completed_at and a non-terminal status. Returns one latency and one
// start time per sample (mssql2139WriterSamples total each), which the
// caller uses to prove -- per SAMPLE, not just as an aggregate window --
// how much of this writer's run genuinely raced a concurrently-running
// sweep.
func mssql2139WriterLatencies(t *testing.T, ctx context.Context, admin *sql.DB, store *MSSQLStore, writerDef, tenantID string) (latencies []time.Duration, starts []time.Time) {
	t.Helper()
	latencies = make([]time.Duration, mssql2139WriterSamples)
	starts = make([]time.Time, mssql2139WriterSamples)
	var wg sync.WaitGroup
	var idx atomic.Int64

	sem := make(chan struct{}, mssql2139WriterConcurrency)
	for i := 0; i < mssql2139WriterSamples; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			n := idx.Add(1)
			id := fmt.Sprintf("mssql-2139-writer-%s-%d-%d", writerDef, n, time.Now().UnixNano())
			// Raw INSERT, same as seedMSSQL2060Workflows -- there is no
			// store-level CreateWorkflow method; every workflow_instances
			// row in these tests is seeded directly. status='running',
			// completed_at NULL: never matched by any of the three
			// sweeps' predicates (all key off a terminal status and/or a
			// non-null completed_at).
			if _, err := admin.ExecContext(ctx,
				`INSERT INTO workflow_instances (id, def_name, def_version, status, tenant_id) VALUES (@p1, @p2, 1, 'running', @p3)`,
				id, writerDef, tenantID); err != nil {
				t.Errorf("create writer workflow: %v", err)
				return
			}

			start := time.Now()
			err := store.AppendEventHistory(ctx, id, EventRecord{
				Step: 1, EventType: EventTypeCall, Service: "svc", Op: "op",
				Request: "{}", Response: `{"ok":true}`,
			})
			d := time.Since(start)
			if err != nil {
				t.Errorf("append event history for writer workflow %s: %v", id, err)
				return
			}

			starts[n-1] = start
			latencies[n-1] = d
		}()
	}
	wg.Wait()

	return latencies, starts
}

// mssql2139OverlapFraction returns the fraction of starts that fall within
// [sweepStart, sweepEnd]. A whole-window check (does [writerStart,
// writerEnd] intersect [sweepStart, sweepEnd] at all) can be satisfied by
// a single straggling sample while the other 199 ran entirely before or
// after the sweep -- which is exactly what a fast sweep against a slower
// writer produces, and it is not a corner case: measured 2026-09-29, a
// sweep mutated to run in one unchunked pass over the whole batch (see
// the falsification below) finished in under half a second against a
// multi-second writer run, so the old any-overlap check passed while
// fewer than 5% of samples actually raced it -- diluting a genuine 26x
// per-sample spike down to noise in the aggregate P99.
func mssql2139OverlapFraction(starts []time.Time, sweepStart, sweepEnd time.Time) float64 {
	var inWindow int
	for _, s := range starts {
		if !s.Before(sweepStart) && !s.After(sweepEnd) {
			inWindow++
		}
	}
	return float64(inWindow) / float64(len(starts))
}

// mssql2139Percentiles sorts latencies (a copy, so the caller's slice keeps
// its sample order) and returns p50 and p99, matching
// tests/scale/latency_test.go's own p99Idx arithmetic exactly so the two
// suites' percentile definitions cannot silently diverge.
func mssql2139Percentiles(latencies []time.Duration) (p50, p99 time.Duration) {
	sorted := append([]time.Duration(nil), latencies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	n := len(sorted)
	p50 = sorted[n/2]
	p99Idx := int(math.Ceil(float64(n)*0.99)) - 1
	if p99Idx >= n {
		p99Idx = n - 1
	}
	p99 = sorted[p99Idx]
	return p50, p99
}

// mssql2139AttemptResult is what one measurement attempt reports to the retry loop below: the
// in-window treatment P99 it measured, the bound that same attempt's own baseline implies, and
// the pre-formatted line this attempt contributes to the final failure message if every attempt
// exceeds. Everything about HOW treP99/bound were measured is the caller's business; this type
// only carries what the retry decision needs.
type mssql2139AttemptResult struct {
	treP99 time.Duration
	bound  time.Duration
	log    string
}

// mssql2139RunWithRetry is cleat#3129's attempt/retry decision (cleat#3117), extracted out of
// TestMSSQLRetentionSweepConcurrentWriterLatencyStaysBounded's loop so the question cleat#3117
// leaves open -- "does the retry loop recover from a transient?" -- can be asked directly of
// THIS function, deterministically, rather than of real SQL Server contention. Two prior
// attempts to trigger a real transient on a shared CI-like machine (a 24-process CPU burst, a
// 7-second HOLDLOCK) both failed to reproduce one on demand -- see cleat#3117's comment history
// -- so TestMSSQL2139RetryLoopRecoversFromATransientAttempt below calls this same function with
// a fake measure that manufactures the transient instead.
//
// Calls measure(attempt) for attempt = 1..maxAttempts, stopping as soon as one attempt's treP99
// is within its own bound. measure is expected to call t.Fatalf itself for anything that is not
// a retryable timing miss (a sweep error, too little overlap to mean anything, a row-count
// precondition) -- that unwinds through this function exactly as it did when this code was
// inline in the caller's own for loop, since calling measure here is an ordinary nested function
// call on the same goroutine, not a new one; t.Fatalf's runtime.Goexit still unwinds correctly
// through it.
func mssql2139RunWithRetry(t *testing.T, label string, maxAttempts int, measure func(attempt int) mssql2139AttemptResult) (withinBound bool, attemptsRun int, perAttempt []string) {
	t.Helper()
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attemptsRun++
		r := measure(attempt)
		perAttempt = append(perAttempt, r.log)
		if r.treP99 <= r.bound {
			withinBound = true
			break
		}
		if attempt < maxAttempts {
			// Logged rather than silent, so a green CI run that spent an attempt is visible:
			// "the host was busy for a few seconds" and "this test is measuring noise" look
			// identical from the verdict alone.
			t.Logf("%s: attempt %d exceeded the bound (%v > %v, %+.0f%% over) -- retrying, because a "+
				"load spike inside this window is not the regression this measures, while a "+
				"regression exceeds the bound on every attempt",
				label, attempt, r.treP99, r.bound, float64(r.treP99)/float64(r.bound)*100-100)
		}
	}
	return withinBound, attemptsRun, perAttempt
}

// TestMSSQL2139RetryLoopRecoversFromATransientAttempt answers cleat#3117's one item its own
// author could not responsibly close: "recovery from a transient, demonstrated against the
// in-process retry loop." Two real attempts to manufacture a transient on a shared CI-like
// machine (cleat#3117's comment history: a 24-process CPU burst, a 7-second HOLDLOCK) both
// failed to reproduce one on demand, and the author declined a third rather than load a
// machine other sessions are actively using for a low-probability payoff.
//
// This does not repeat that experiment. It calls mssql2139RunWithRetry -- the actual, real
// function TestMSSQLRetentionSweepConcurrentWriterLatencyStaysBounded uses, not a
// reimplementation of it -- with a fake measure that manufactures the transient directly:
// attempt 1 reports a treP99 comfortably over its bound (the transient), attempt 2 reports one
// comfortably under (the recovery). No database, no goroutines racing real contention, no load
// on this or any other machine: the question this asks is purely "does the retry loop's own
// control flow accept a later clean attempt after an earlier noisy one", which is answerable
// deterministically because it is a property of the code, not of SQL Server's scheduler.
//
// What this does NOT claim: it is not evidence that real MSSQL contention manifests as a
// transient this loop would catch -- that remains the two real CI instances cleat#3117's
// comment history already found (both recovered via an external workflow re-run, predating
// this loop, not via mssql2139RunWithRetry itself). This closes the narrower, cleaner question:
// given a transient, does the loop's own accept/retry logic work correctly.
func TestMSSQL2139RetryLoopRecoversFromATransientAttempt(t *testing.T) {
	var calls []int
	withinBound, attemptsRun, perAttempt := mssql2139RunWithRetry(t, "fake-arm", 3, func(attempt int) mssql2139AttemptResult {
		calls = append(calls, attempt)
		if attempt == 1 {
			// The transient: comfortably over bound, the shape a loaded runner produces.
			return mssql2139AttemptResult{
				treP99: 500 * time.Millisecond,
				bound:  250 * time.Millisecond,
				log:    fmt.Sprintf("attempt %d: over bound (fake)", attempt),
			}
		}
		// The recovery: comfortably under, as the same scenario measures on a quiet runner.
		return mssql2139AttemptResult{
			treP99: 80 * time.Millisecond,
			bound:  250 * time.Millisecond,
			log:    fmt.Sprintf("attempt %d: within bound (fake)", attempt),
		}
	})

	if !withinBound {
		t.Fatalf("withinBound = false, want true -- the loop did not recover from the manufactured transient")
	}
	if attemptsRun != 2 {
		t.Fatalf("attemptsRun = %d, want 2 -- the loop should stop as soon as an attempt lands within bound", attemptsRun)
	}
	if got := len(calls); got != 2 {
		t.Fatalf("measure was called %d time(s), want exactly 2 -- a third, unnecessary attempt ran after recovery", got)
	}
	if len(perAttempt) != 2 {
		t.Fatalf("perAttempt has %d entries, want 2 -- the log for a spent attempt went missing", len(perAttempt))
	}
}

// TestMSSQL2139RetryLoopStillFailsWhenEveryAttemptExceeds is the companion falsification this
// file's own rule asks for ("a rule with two clauses needs one mutation per clause"): the retry
// loop existing must not mean it can no longer fail. Every attempt here exceeds its bound, so
// the loop must exhaust maxAttempts and report withinBound=false -- this is what catches a
// persistent regression (mssqlEventRowChunk 2000->10000, the real test's own documented
// known-positive, fails on all three attempts; this is that behavior, isolated from the
// database).
func TestMSSQL2139RetryLoopStillFailsWhenEveryAttemptExceeds(t *testing.T) {
	var calls []int
	withinBound, attemptsRun, perAttempt := mssql2139RunWithRetry(t, "fake-arm", 3, func(attempt int) mssql2139AttemptResult {
		calls = append(calls, attempt)
		return mssql2139AttemptResult{
			treP99: 900 * time.Millisecond,
			bound:  250 * time.Millisecond,
			log:    fmt.Sprintf("attempt %d: over bound (fake persistent regression)", attempt),
		}
	})

	if withinBound {
		t.Fatalf("withinBound = true, want false -- a regression that exceeds the bound on every attempt must still fail")
	}
	if attemptsRun != 3 {
		t.Fatalf("attemptsRun = %d, want 3 -- a persistent regression must spend every attempt, not stop early", attemptsRun)
	}
	if got := len(calls); got != 3 {
		t.Fatalf("measure was called %d time(s), want exactly 3", got)
	}
	if len(perAttempt) != 3 {
		t.Fatalf("perAttempt has %d entries, want 3 -- the final failure message would be missing an attempt's numbers", len(perAttempt))
	}
}

func TestMSSQLRetentionSweepConcurrentWriterLatencyStaysBounded(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}
	raw := testutil.MSSQLTestDB(t)
	ctx := context.Background()

	arms := []struct {
		name   string
		def    string
		status string
		sweep  func(*MSSQLStore) (int64, error)
	}{
		{
			name: "DeleteExpiredEvents", def: "mssql-2139-expired", status: "done",
			sweep: func(s *MSSQLStore) (int64, error) { return s.DeleteExpiredEvents(ctx, time.Now()) },
		},
		{
			name: "DeleteCompletedWorkflows", def: "mssql-2139-completed", status: "done",
			sweep: func(s *MSSQLStore) (int64, error) { return s.DeleteCompletedWorkflows(ctx, time.Now()) },
		},
		{
			name: "DeleteDeadLetteredWorkflows", def: "mssql-2139-deadletter", status: "dead_lettered",
			sweep: func(s *MSSQLStore) (int64, error) { return s.DeleteDeadLetteredWorkflows(ctx, time.Now()) },
		},
	}

	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			tid := DefaultTenantUUID
			completedAt := time.Now().Add(-2 * time.Hour)
			writerDef := arm.def + "-writer"

			db, dsn := mssqlPrivateRCSIDatabase(t, raw, "2139_"+strings.ToLower(arm.name), true)
			t.Setenv("CLEAT_TEST_MSSQL", dsn)
			admin := testutil.MSSQLAdminDB(t, db)

			store := openMSSQLTenantStore(t, tid)
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: arm.def, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("deploy %s: %v", arm.def, err)
			}
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: writerDef, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("deploy %s: %v", writerDef, err)
			}

			// Seed the sweep's own target data -- same size and shape as
			// the #2060 escalation test (mssql2060WorkflowCount workflows
			// at mssql2060EventsPerWorkflow events each), comfortably past
			// SQL Server's lock-escalation threshold, so the sweep this
			// runs against is a REALISTIC one, not a token amount that
			// would finish before the writer notices it.
			//
			// Seeded BEFORE the baseline is measured, not after: a cold
			// baseline (measured on an empty database, before this seed)
			// differs from treatment in DATA VOLUME as well as in whether
			// a sweep is running, and that confound is not hypothetical --
			// cleat-review measured this test's own five baseline trials
			// at 32ms-264ms P99, and detecting the #2138 regression this
			// test falsifies against needs a baseline under roughly 214ms
			// ((958-100)/4, from the boundFactor/boundFloor below). A warm
			// baseline, taken on the SAME already-seeded database the
			// treatment runs against, removes data volume as a variable
			// between the two arms entirely.
			ids := make([]string, mssql2060WorkflowCount)
			for i := range ids {
				ids[i] = fmt.Sprintf("mssql-2139-%s-%05d", arm.name, i)
			}
			seedMSSQL2060Workflows(t, ctx, admin, arm.def, tid, arm.status, ids, completedAt)

			var seededEvents int
			if err := admin.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM event_history eh JOIN workflow_instances wi ON wi.id = eh.workflow_id
				 WHERE wi.def_name = @p1`, arm.def).Scan(&seededEvents); err != nil {
				t.Fatalf("count seeded events: %v", err)
			}
			if want := len(ids) * mssql2060EventsPerWorkflow; seededEvents != want {
				t.Fatalf("precondition: seeded %d event_history rows, want %d -- the sweep below "+
					"would run against less data than intended", seededEvents, want)
			}

			// WARM-UP, discarded: a handful of the same insert+append
			// operations before any sample is recorded, so the baseline's
			// own first few samples are not paying for connection-pool
			// ramp-up or a cold query plan cache -- cost the sweep's own
			// samples never pay, since by then the pool has already
			// handled the whole baseline run.
			const mssql2139WarmUpSamples = 10
			for i := 0; i < mssql2139WarmUpSamples; i++ {
				id := fmt.Sprintf("mssql-2139-warmup-%s-%d-%d", writerDef, i, time.Now().UnixNano())
				if _, err := admin.ExecContext(ctx,
					`INSERT INTO workflow_instances (id, def_name, def_version, status, tenant_id) VALUES (@p1, @p2, 1, 'running', @p3)`,
					id, writerDef, tid); err != nil {
					t.Fatalf("warm-up insert: %v", err)
				}
				if err := store.AppendEventHistory(ctx, id, EventRecord{
					Step: 1, EventType: EventTypeCall, Service: "svc", Op: "op",
					Request: "{}", Response: `{"ok":true}`,
				}); err != nil {
					t.Fatalf("warm-up append: %v", err)
				}
			}
			// Cleaned up immediately: these rows must not count toward the
			// writerRowsRemaining precondition below, which expects exactly
			// 2*mssql2139WriterSamples (baseline+treatment, nothing else).
			if _, err := admin.ExecContext(ctx,
				`DELETE FROM event_history WHERE workflow_id IN (SELECT id FROM workflow_instances WHERE def_name = @p1)`,
				writerDef); err != nil {
				t.Fatalf("clean up warm-up event_history: %v", err)
			}
			if _, err := admin.ExecContext(ctx,
				`DELETE FROM workflow_instances WHERE def_name = @p1`, writerDef); err != nil {
				t.Fatalf("clean up warm-up workflow_instances: %v", err)
			}

			// MEASURED UP TO maxAttempts TIMES, FAILING ONLY IF EVERY ATTEMPT EXCEEDS THE BOUND --
			// because the baseline and the treatment are measured in DIFFERENT WINDOWS, and a loaded
			// runner skews one of them.
			//
			// WHAT THIS FIXES (cleat#3117). The bound is 4x the SAME RUN's baseline P99 plus a 100ms
			// floor, which is a comparison between two windows taken minutes apart on a host whose
			// own noise is wider than the tolerance: this test's own calibration notes record
			// baseline P99 spanning 32ms-264ms across 15 (arm, trial) pairs on ONE host, against a
			// 4x ratio allowance. A load spike landing inside the treatment window -- and not the
			// baseline's -- is therefore reported as "the sweep is materially slowing ordinary
			// writes", which is what happened: 279.02ms against a 244.85ms bound, a 14% overshoot,
			// on a head whose diff could not reach engine/ at all.
			//
			// WHAT IT DOES NOT DO: it does not weaken the bound. Every attempt is checked against
			// its OWN baseline with the same 4x + floor, and the test reddens if EVERY attempt
			// exceeds, so a persistent regression is still caught -- this test's own known-positive
			// (mssqlEventRowChunk 2000 -> 10000, which drives the treatment P99 to ~900ms) was
			// re-run against this loop rather than assumed, and fails it on every attempt.
			//
			// Nor is this "re-run CI hoping for green": it is a measurement protocol inside one test
			// process, bounded at maxAttempts, and an attempt is spent ONLY where the bound was
			// already exceeded. The RATE stays unmeasured -- one CI observation is all cleat#3117
			// has -- so this reduces the false-failure rate without quantifying it, and every
			// attempt's numbers are kept in the failure message rather than summarised away.
			const maxAttempts = 3
			// The bound's constants, hoisted out of the loop because the failure message below quotes
			// them too -- a message naming a value the code no longer uses is the stale-prose failure
			// this repo keeps paying for. Their calibration note sits beside the `bound :=` line.
			const boundFactor = 4.0
			const boundFloor = 100 * time.Millisecond

			// The attempt/retry decision itself lives in mssql2139RunWithRetry (cleat#3117), extracted
			// out of this closure so that function's recovery-from-a-transient behavior can be asked
			// directly of THIS code, deterministically -- see
			// TestMSSQL2139RetryLoopRecoversFromATransientAttempt below. Everything inside measure is
			// unchanged from before the extraction; only the retry/early-break control flow moved.
			withinBound, attemptsRun, perAttempt := mssql2139RunWithRetry(t, arm.name, maxAttempts, func(attempt int) mssql2139AttemptResult {
				if attempt > 1 {
					// The previous attempt's sweep consumed the seeded targets, so the scenario has to
					// be rebuilt before a retry -- otherwise the retry measures a sweep over an
					// already-swept table, which is not the scenario cleat#2139 asks about.
					mssql2139RebuildSweepTargets(t, ctx, admin, arm.def, tid, arm.status, ids, completedAt)
				}
				// BASELINE: writer alone against the already-seeded database,
				// nothing else running yet. Warm, not cold -- see above.
				baseline, _ := mssql2139WriterLatencies(t, ctx, admin, store, writerDef, tid)
				basP50, basP99 := mssql2139Percentiles(baseline)

				// TREATMENT: the writer runs on this goroutine while the sweep
				// runs concurrently on another. sweepDone/sweepErr/sweepStart/
				// sweepEnd let the main goroutine both wait for the sweep and
				// prove, below, that it genuinely overlapped the writer's own
				// sampling window -- the falsification cleat#2139 itself names
				// ("a sweep that finishes before the writer starts... will
				// produce a clean number that means nothing").
				var sweepDeleted int64
				var sweepErr error
				var sweepStart, sweepEnd time.Time
				sweepDone := make(chan struct{})
				go func() {
					defer close(sweepDone)
					sweepStart = time.Now()
					sweepDeleted, sweepErr = arm.sweep(store)
					sweepEnd = time.Now()
				}()

				treatment, writerStarts := mssql2139WriterLatencies(t, ctx, admin, store, writerDef, tid)
				<-sweepDone

				if sweepErr != nil {
					t.Fatalf("%s over %d workflows: %v", arm.name, len(ids), sweepErr)
				}
				if sweepDeleted == 0 {
					t.Fatalf("%s reported 0 rows deleted -- the latency comparison below would be "+
						"against a sweep that touched nothing", arm.name)
				}

				// PROVE OVERLAP AS A FRACTION, not merely that it is nonzero.
				// A single straggling sample satisfies "the windows
				// intersect" while the other 199 ran entirely before or
				// after the sweep, and a fast sweep against a slower writer
				// produces exactly that: measured 2026-09-29, mutating
				// mssqlInterleaveChunk from 20 to 6000 (one unchunked pass
				// over the whole batch, collapsing the interleaving cleat#2060
				// depends on) made the sweep finish in well under a second
				// against a multi-second writer run, so a whole-window
				// overlap check passed while under 5% of samples actually
				// raced it -- diluting a confirmed lock escalation (caught
				// independently by TestMSSQLRetentionSweepsCauseNoLockEscalation
				// against the same mutation) down to noise in the aggregate
				// P99. See the falsification below for that measurement in
				// full.
				var inWindow []time.Duration
				for i, s := range writerStarts {
					if !s.Before(sweepStart) && !s.After(sweepEnd) {
						inWindow = append(inWindow, treatment[i])
					}
				}
				overlapFraction := float64(len(inWindow)) / float64(len(treatment))
				const minOverlapFraction = 0.5
				if overlapFraction < minOverlapFraction {
					t.Fatalf("%s: only %.1f%% of the writer's %d samples fell inside the sweep's own "+
						"window (sweep %s .. %s) -- this run does not exercise the scenario cleat#2139 "+
						"asks about, and the latency numbers below are not evidence of anything",
						arm.name, overlapFraction*100, len(treatment),
						sweepStart.Format(time.RFC3339Nano), sweepEnd.Format(time.RFC3339Nano))
				}

				// The bound below is asserted over the IN-WINDOW samples
				// only, not the whole treatment run: a writer sample taken
				// after the sweep has already finished says nothing about
				// contention, and averaging it in with the samples that did
				// race the sweep is the same dilution the fraction check
				// above exists to catch, just moved from "did we overlap at
				// all" to "how good is our per-sample signal".
				treP50, treP99 := mssql2139Percentiles(inWindow)
				allP50, allP99 := mssql2139Percentiles(treatment)

				// The writer's own rows must survive: none of the three
				// sweeps' predicates should ever match a 'running',
				// completed_at-NULL row, but this is a fixture, not a
				// production guarantee, and a bug that widened a sweep's own
				// WHERE clause should fail THIS precondition rather than
				// silently reporting a favorable latency number over a writer
				// that the sweep partly deleted out from under itself.
				var writerRowsRemaining int
				if err := admin.QueryRowContext(ctx,
					`SELECT COUNT(*) FROM workflow_instances WHERE def_name LIKE @p1`,
					writerDef+"%").Scan(&writerRowsRemaining); err != nil {
					t.Fatalf("count surviving writer rows: %v", err)
				}
				wantWriterRows := attempt * 2 * mssql2139WriterSamples // baseline + treatment, per attempt
				if writerRowsRemaining != wantWriterRows {
					t.Fatalf("%s: %d writer workflow rows remain, want %d -- the sweep deleted rows "+
						"it should never have matched", arm.name, writerRowsRemaining, wantWriterRows)
				}

				t.Logf("%s: baseline P50=%v P99=%v (n=%d) | treatment(in-window) P50=%v P99=%v (n=%d of %d, %.0f%% overlap) | treatment(whole run) P50=%v P99=%v | %d rows swept",
					arm.name, basP50, basP99, len(baseline), treP50, treP99, len(inWindow), len(treatment),
					overlapFraction*100, allP50, allP99, sweepDeleted)

				assertAllMSSQL2139Sampled(t, arm.name, "baseline", baseline)
				assertAllMSSQL2139Sampled(t, arm.name, "treatment", treatment)

				// THE STATED BOUND. Measured 2026-09-29 against a local
				// scratch SQL Server container, five repeated trials of this
				// exact test after #2138 landed (all three arms, per trial):
				// treatment P99 ran 0.4x-1.3x baseline P99 -- never worse by
				// more than about a quarter, and usually BETTER (the sweep's
				// own chunked deletes appear to have no measurable effect on
				// the concurrent writer at all, which is the outcome #2138's
				// fix predicts). Baseline P99 itself ranged 32ms-264ms across
				// those 15 (arm, trial) pairs -- one outlier at 264ms against
				// a cluster at 32-46ms elsewhere, the same per-run CI-host
				// noise tail tests/scale/latency_test.go documents for its
				// own Postgres-only P99 test ("a continuum spanning three
				// orders of magnitude... any fixed threshold under ~700ms is
				// below its noise floor"). 4x plus a fixed 100ms floor sits
				// comfortably above every ratio actually observed (the worst
				// was 1.3x) while staying far enough below "no bound at all"
				// to still catch #2138 regressing -- see the falsification
				// below, which confirms it does.
				bound := time.Duration(float64(basP99)*boundFactor) + boundFloor
				return mssql2139AttemptResult{
					treP99: treP99,
					bound:  bound,
					log: fmt.Sprintf(
						"attempt %d: baseline P99=%v, treatment(in-window) P99=%v, bound=%v (%+.0f%% of bound)",
						attempt, basP99, treP99, bound, float64(treP99)/float64(bound)*100-100),
				}
			})
			if !withinBound {
				t.Errorf("%s: concurrent-writer P99 under the sweep exceeded the stated bound (%.1fx the "+
					"same attempt's baseline P99 + %v floor) on ALL %d attempt(s) -- the sweep is materially "+
					"slowing ordinary writes, which is what cleat#2138's row-chunking fix exists to prevent. "+
					"Per attempt:\n  %s",
					arm.name, boundFactor, boundFloor, attemptsRun, strings.Join(perAttempt, "\n  "))
			}
		})
	}
}

// assertAllMSSQL2139Sampled is mssql_retention... wait, tests/scale's own
// assertAllSampled, restated here: a zero-valued latency means a goroutine
// returned via t.Errorf before ever recording one, and a zero sorts to the
// front of the percentile computation, pulling both P50 and P99 down --
// exactly the failure mode that would make a broken writer look fast
// rather than broken.
func assertAllMSSQL2139Sampled(t *testing.T, arm, phase string, latencies []time.Duration) {
	t.Helper()
	var unsampled int
	for _, d := range latencies {
		if d == 0 {
			unsampled++
		}
	}
	if unsampled > 0 {
		t.Errorf("%s/%s: %d of %d samples were never recorded; the percentiles above are "+
			"computed over zeros and read lower than the truth", arm, phase, unsampled, len(latencies))
	}
}

// FALSIFICATION 1 -- DeleteExpiredEvents, mssqlEventRowChunk (applied by
// hand, verified, and reverted -- never committed). With mssqlEventRowChunk
// changed from 2000 to 10000 in mssql_schedules.go (#2138's per-statement
// row bound loosened enough to matter for a 30000-row sweep, but not so
// large that the whole sweep collapses into one near-instant statement --
// see the note on overlap below for why that distinction matters), the
// DeleteExpiredEvents arm's in-window treatment P99 jumped from ~13ms to
// 900ms at 90% overlap -- comfortably over the stated bound. Restored via
// content diff against a pre-mutation backup, re-verified green (all three
// arms). Confirmed 2026-09-29.
//
// An earlier attempt at this same falsification used 1000000 (this test's
// prior, pre-review version reported "958ms P99, a 26x spike" for exactly
// that value). That value no longer demonstrates the regression under THIS
// version of the test: a single DELETE TOP(1000000) clears all 30000 rows
// in one near-instant statement, so the sweep finishes before most of the
// writer's samples are even taken, and the test correctly refuses to
// certify a result over that thin an overlap (measured: 40% of samples
// in-window, below minOverlapFraction) rather than reporting a P99 number
// that would not mean what it looked like it meant. 10000 keeps the sweep
// slow enough to race the writer properly while still exceeding the bound.
//
// FALSIFICATION 2 -- DeleteCompletedWorkflows / DeleteDeadLetteredWorkflows,
// mssqlInterleaveChunk. The reasoning this comment carried until
// cleat-review's review of PR #2743 -- that these two arms' event_history
// removal "goes through the cascade-delete path... not the explicit
// mssqlEventRowChunk-bounded DELETE TOP" -- was wrong about the mechanism,
// though right that mssqlEventRowChunk does not move them: both call
// deleteWorkflowsBatchOnce, which interleaves deleteEventHistoryRowBoundedCommitting
// (the SAME function DeleteExpiredEvents uses) over mssqlInterleaveChunk-sized
// (20-workflow) id chunks (mssql_schedules.go:904-910) -- so each call's own
// event_history delete never approaches mssqlEventRowChunk rows regardless of
// its value, which is why raising it does nothing for these two arms.
//
// The correct known-positive is mssqlInterleaveChunk, not mssqlEventRowChunk,
// and it DOES cause a real regression -- but not one this latency harness can
// see, which is itself worth recording rather than papering over. Changed
// from 20 to 6000 (the whole batch in one interleave step, reproducing the
// first, rejected design deleteWorkflowsBatchOnce's own comment describes:
// "delete ALL of event_history for the whole batch first... THEN delete
// workflow_instances... in one shared transaction... +1 escalation"):
//
//   - TestMSSQLRetentionSweepsCauseNoLockEscalation (this package's sibling
//     regression test for cleat#2060, which reads sys.dm_db_index_operational_stats
//     directly rather than inferring contention from writer latency) FAILS
//     under this exact mutation, on DeleteCompletedWorkflows and
//     DeleteDeadLetteredWorkflows specifically -- "event_history lock
//     promotions rose by 1 sweeping 6000 workflows' events (6000 rows) in
//     one call". Confirmed 2026-09-29. This is the real, mechanism-accurate
//     known-positive cleat-review asked for, and it exists and passes.
//   - THIS test does not react to the same mutation: with the overlap
//     fraction fixed to 100% (a giant single-chunk sweep still finishes fast
//     enough that all 200 writer samples fall inside its window here), both
//     arms' in-window treatment P99 came in LOWER than baseline (e.g.
//     DeleteCompletedWorkflows: baseline P99 34-47ms, treatment P99
//     10-15ms), not higher. Measured directly, not inferred: the mutation
//     was applied, TestMSSQLRetentionSweepConcurrentWriterLatencyStaysBounded
//     was run against it, and it passed.
//
// So this harness has a confirmed blind spot for the mssqlInterleaveChunk
// regression on these two arms. The escalated lock is real (per the DMV
// counter) but does not manifest as materially higher writer latency in
// this measurement. TWO FURTHER MEASUREMENTS, both against the exact
// mutation above, settle WHY -- CLOSING this as the issue's conclusion
// rather than leaving it a hypothesis (cleat#2139, 2026-10-08):
//
//   - ORDER-SWAP IS NOT A FIX, AND IS ITS OWN CONFOUND. Measuring treatment
//     (concurrent with the sweep) FIRST and baseline (writer alone) SECOND
//     reverses the comparison -- under the mutation, treatment P99 came in
//     3.1-3.8x ABOVE baseline (e.g. DeleteDeadLetteredWorkflows: baseline
//     14.9ms, treatment 46.6ms). That looks like a fix until the SAME swap
//     is run as a negative control with mssqlInterleaveChunk left at 20:
//     treatment P99 is STILL 2.4-2.5x above baseline (13.4ms vs 31.7ms) with
//     no regression present at all. Whichever measurement runs SECOND in
//     this harness benefits from a warm-up/ordering advantage of comparable
//     size to the contention signal being hunted, in EITHER direction --
//     original order hides the regression inside it, swapped order would
//     manufacture a false one. Neither ordering is a reliable comparison.
//   - THE SLOW WRITER SAMPLES ARE NOT ESCALATION-SPECIFIC. Polling
//     sys.dm_exec_requests directly (3ms interval, a connection separate
//     from the writer and the sweep) for blocking_session_id <> 0 across
//     the whole sweep found ZERO blocking events in either case -- mutated
//     (8.1s sweep, 6/200 writer samples over 20ms, max 86.6ms) or
//     unmutated (4.4s sweep, 4/200 samples over 20ms, max 83.0ms). Within
//     noise of each other, mutation or not. So the handful of slow writer
//     samples any of these sweeps produces are not classic SQL Server lock
//     blocking at all (the DMV would show it), and are not specific to the
//     escalation this test is trying to catch -- they are a smaller,
//     sweep-general perturbation (plausibly I/O or CPU contention from a
//     multi-second DELETE competing with the writer, sharper under this
//     host's amd64-on-arm64 emulation -- see WORKSTREAM.md) that a
//     correctly-chunked sweep produces almost as much of as a broken one.
//
// CONCLUSION: the two-test split is the right final shape, not a stopgap.
// TestMSSQLRetentionSweepsCauseNoLockEscalation measures the escalation
// directly (a lock-promotion counter) and is the authoritative regression
// guard for cleat#2060/mssqlInterleaveChunk on these two arms.
// THIS test's writer-latency bound should be read as covering
// cleat#2138/mssqlEventRowChunk only, confirmed for DeleteExpiredEvents --
// not as a general contention detector for all three arms, because the
// contention these two arms' regression causes is smaller than, and not
// separable from, noise this same harness's own measurement order
// introduces.

// mssql2139RebuildSweepTargets re-creates the arm's sweep targets between measurement
// attempts, so a retry measures the same scenario as the first attempt rather than an
// already-swept table. It clears first instead of assuming the sweep took everything:
// the check above the bound only requires that SOME rows were deleted, so a sweep that
// removed a subset would otherwise collide with the ids being re-inserted.
func mssql2139RebuildSweepTargets(t *testing.T, ctx context.Context, admin *sql.DB, def, tid, status string,
	ids []string, completedAt time.Time) {
	t.Helper()
	if _, err := admin.ExecContext(ctx,
		`DELETE FROM event_history WHERE workflow_id IN (SELECT id FROM workflow_instances WHERE def_name = @p1)`,
		def); err != nil {
		t.Fatalf("clear %s's event_history before a retry: %v", def, err)
	}
	if _, err := admin.ExecContext(ctx, `DELETE FROM workflow_instances WHERE def_name = @p1`, def); err != nil {
		t.Fatalf("clear %s before a retry: %v", def, err)
	}
	seedMSSQL2060Workflows(t, ctx, admin, def, tid, status, ids, completedAt)
}
