package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestFinalizeRetryBudgetSurvivesNWayContention is cleat#2033.
//
// A SEPARATE question from TestFinalizeRetriesADeadlockOnEventCount (cleat#1883),
// which proves finalizeWorkflowSegmentRetrying retries a deadlock AT ALL. This
// test proves the retry BUDGET is enough -- a question #1883's own racer count
// (8) was not chosen to answer, and did not: cleat#2033 is this exact test's
// shape (N finalizers racing one fence) catching CI red at round 11 of a
// production run, with the retry loop already in place and already running.
//
// WHY MORE RACERS CHANGES THE SHAPE OF THE PROBLEM, NOT JUST ITS SIZE. The
// start path's retry budget (startNewRunUnderIdempotencyKey, same file) is
// justified by "the population of contenders only shrinks": each attempt
// either commits or finds the winner already committed, so a losing retry
// can only resolve, never re-collide. That does not hold here. With N
// finalizers released together, a losing attempt can deadlock against
// ANOTHER loser rather than the eventual winner -- neither side has
// committed yet -- so more than one retry is the ordinary case under this
// contention, not the tail #1883's 8-racer measurement suggested.
//
// MEASURED, not assumed. Instrumenting every exit (success or a
// non-retryable error, e.g. ErrFenceLost) across 1000 rounds of 12
// concurrent racers against a real MySQL 8.4.11, with the retry bound
// raised to 64 so no run was truncated: mean 2.65 attempts, p50=3, p90=5,
// p99=7, deepest ever reached was the 9th attempt (0-indexed 8) -- never
// beyond it in 12,000 calls. At the OLD bound of 8, that tail is exactly
// where it bites: a separate 1000-round/12-racer run at maxAttempts=8
// exhausted 52 of 12,000 racer-calls (0.43%) -- cleat#2033's failure,
// reproduced locally. The same sample size at maxAttempts=16 (this fix)
// measured 0 of 12,000.
//
// racers=12, not #1883's 8: the exhaustion this test exists to catch was
// rare even at the OLD bound (0.43% per racer-call) and a SMALL round count
// at racers=8 would under-detect it for the same reason CLAUDE.md's own
// "a regression test that catches its own defect only 40% of the time"
// lesson warns about. On the machine that measured this, a round had
// roughly a 4.6% chance of showing at least one exhaustion on the unfixed
// code (46 of 1000 rounds) -- BUT THAT RATE IS HOST-SPECIFIC, not a
// property of the bug: cleat-review's own machine measured 2.6% per round
// (1-6 bad rounds per 100, 9 of 10 runs catching it). 200 rounds gives
// >99% detection even at the SLOWER host's rate (1-(0.974)^200 ≈ 99.8%,
// against 1-(0.954)^200 ≈ 99.99% on the faster one), which is why the
// round count below is 200 rather than the 100 that was enough on only
// one of the two machines that have run this. Falsified by reverting this
// function's maxAttempts to 8 and confirming failure before trusting the
// fix (see the PR, not a comment here -- the constant is restored by the
// time this merges and a stale "confirmed red" claim here would be
// exactly the rotted-comment trap CLAUDE.md warns about).
//
// MYSQL ONLY, for the same reason as #1883's sibling test: Postgres's MVCC
// does not gap-lock a range the way InnoDB does under REPEATABLE READ, and
// this mechanism is specific to that.
func TestFinalizeRetryBudgetSurvivesNWayContention(t *testing.T) {
	const (
		racers = 12
		rounds = 200
	)

	backend := &MySQLBackend{}
	t.Run(backend.Name(), func(t *testing.T) {
		store, teardown := backend.Setup(t)
		defer teardown()
		ctx := context.Background()
		setupTestData(t, store)
		truncateAll(t, store)

		const defName = "finalize-retry-budget"
		if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
			Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("DeployWorkflowDef: %v", err)
		}

		for round := 0; round < rounds; round++ {
			raceFinalizeSegmentsWideFanIn(t, ctx, store, defName, round, racers)
		}
	})
}

// raceFinalizeSegmentsWideFanIn is raceFinalizeSegments
// (finalize_retries_a_mysql_deadlock_on_event_count_test.go) at a racer count
// chosen to make a retry-budget gap observable rather than a retry-exists gap
// -- the same assertion (nothing but ErrFenceLost may reach the caller), at a
// contention level #1883's test was not sized to exercise.
func raceFinalizeSegmentsWideFanIn(t *testing.T, ctx context.Context, store WorkflowStore, defName string, round, racers int) {
	t.Helper()

	id := fmt.Sprintf("finalize-budget-racer-%d-%d", round, time.Now().UnixNano())
	if _, _, err := store.StartNewRun(ctx, id, defName, 1,
		json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	claimed, err := store.ClaimWorkflow(ctx, "worker-1")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimWorkflow: %v %v", claimed, err)
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		errs  []error
		start = make(chan struct{})
	)

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release together, so the window is actually contended

			recs := []EventRecord{{
				Step:        i,
				EventType:   EventTypeSignalReceived,
				SignalName:  fmt.Sprintf("sig-%d", i),
				TimestampMs: time.Now().UnixMilli(),
			}}
			err := store.FinalizeWorkflowSegment(ctx, claimed.ID, "worker-1",
				claimed.Generation, recs, "done", `{"ok":true}`, "", "", nil, time.Time{})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		if errors.Is(err, ErrFenceLost) {
			continue // expected: exactly one racer wins the fence
		}
		t.Errorf("round %d: an unretried-or-under-budgeted error reached the caller: %v", round, err)
		if isDeadlockOrLockWait(err) {
			t.Logf("  ^ this is cleat#2033: the retry budget was exhausted under "+
				"%d-way contention, not that retries never happened", racers)
		}
	}
}
