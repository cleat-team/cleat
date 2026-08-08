package engine

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// ClaimDueSchedule is the compare-and-swap that decides which worker owns a
// firing instant. These tests are the reason the scheduler can promise
// at-least-once rather than "somewhere between zero and N".

func createDueSchedule(t *testing.T, store WorkflowStore, ctx context.Context, name string, dueAt time.Time) {
	t.Helper()
	if err := store.CreateSchedule(ctx, Schedule{
		Name:           name,
		DefName:        "test-workflow",
		CronExpression: "* * * * *",
		Input:          json.RawMessage(`{}`),
		Enabled:        true,
		NextRunAt:      dueAt,
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
}

func TestClaimDueSchedule_AdvancesWhenTheInstantMatches(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()
			due := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
			createDueSchedule(t, store, ctx, "claim-ok", due)

			// Read it back, because the stored value is what a caller will
			// compare against and the database's precision is not necessarily
			// the one we wrote.
			got := findSchedule(t, store, ctx, "claim-ok")
			next := got.NextRunAt.Add(time.Minute)

			claimed, err := store.ClaimDueSchedule(ctx, "claim-ok", got.NextRunAt, next)
			if err != nil {
				t.Fatalf("ClaimDueSchedule: %v", err)
			}
			if !claimed {
				t.Fatal("ClaimDueSchedule reported not-claimed for a matching instant")
			}

			after := findSchedule(t, store, ctx, "claim-ok")
			if !after.NextRunAt.UTC().Truncate(time.Second).Equal(next.UTC().Truncate(time.Second)) {
				t.Errorf("next_run_at = %s, want %s",
					after.NextRunAt.UTC().Format(time.RFC3339), next.UTC().Format(time.RFC3339))
			}
		})
	}
}

// TestClaimDueSchedule_RefusesWhenTheInstantMoved is the CAS half. A caller
// holding a stale NextRunAt must be told it lost, not silently overwrite
// whoever won.
func TestClaimDueSchedule_RefusesWhenTheInstantMoved(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()
			due := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
			createDueSchedule(t, store, ctx, "claim-stale", due)

			got := findSchedule(t, store, ctx, "claim-stale")
			winner := got.NextRunAt.Add(time.Minute)

			claimed, err := store.ClaimDueSchedule(ctx, "claim-stale", got.NextRunAt, winner)
			if err != nil || !claimed {
				t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
			}

			// A second caller still holding the ORIGINAL instant.
			loser := got.NextRunAt.Add(2 * time.Hour)
			claimed, err = store.ClaimDueSchedule(ctx, "claim-stale", got.NextRunAt, loser)
			if err != nil {
				t.Fatalf("second claim: %v", err)
			}
			if claimed {
				t.Error("a caller holding a stale instant was told it claimed the schedule")
			}

			after := findSchedule(t, store, ctx, "claim-stale")
			if after.NextRunAt.UTC().Truncate(time.Second).Equal(loser.UTC().Truncate(time.Second)) {
				t.Error("the losing caller's next_run_at overwrote the winner's")
			}
		})
	}
}

// TestClaimDueSchedule_ExactlyOneOfNConcurrentClaimsWins is the two-worker
// race from the plan, generalised to eight.
//
// This is the assertion that a firing instant has ONE owner in a fleet.
// GetDueSchedules takes row locks, but its own transaction commits before the
// caller has started anything, so every worker polling in that window sees the
// same row as due. Without the CAS they would all fire it.
//
// -count is what makes this meaningful: a race that reproduces sometimes will
// pass a single run. Run it with -count=20 -race when changing any of this.
func TestClaimDueSchedule_ExactlyOneOfNConcurrentClaimsWins(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()
			due := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
			createDueSchedule(t, store, ctx, "claim-race", due)
			got := findSchedule(t, store, ctx, "claim-race")

			const workers = 8
			var (
				wg      sync.WaitGroup
				mu      sync.Mutex
				wins    int
				errs    []error
				release = make(chan struct{})
			)
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-release // start together, to make the race as tight as possible
					// Every worker computes the SAME next instant, exactly as
					// the scheduler does: it is derived from the schedule, not
					// from the worker.
					claimed, err := store.ClaimDueSchedule(ctx, "claim-race", got.NextRunAt, got.NextRunAt.Add(time.Minute))
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						errs = append(errs, err)
						return
					}
					if claimed {
						wins++
					}
				}(i)
			}
			close(release)
			wg.Wait()

			for _, err := range errs {
				t.Errorf("concurrent claim error: %v", err)
			}
			if wins != 1 {
				t.Errorf("%d of %d concurrent claims won the same instant, want exactly 1", wins, workers)
			}
		})
	}
}
