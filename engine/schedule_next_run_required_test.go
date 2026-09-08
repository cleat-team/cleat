package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// next_run_at is NOT NULL DEFAULT now() on all three dialects, and the default
// is unreachable: every CreateSchedule names the column in its INSERT and
// passes the field, so a zero is written rather than defaulted. What a zero
// then means diverges three ways -- PostgreSQL stores year 1, MySQL rejects
// `0000-00-00` outright, SQL Server accepts it.
//
// Schedule.Validate makes that one loud error everywhere. This drives EVERY
// registered backend rather than one, because the placement is not what
// prevents divergence -- the test is. A closely related fix in this repo
// shipped with its regression test hardcoded to a single dialect: the fix was
// applied per dialect, the test guarded one, and the rest stayed broken while
// the suite stayed green. Covering a third of the surface reads identically to
// covering all of it in any summary anyone will read.
func TestEveryDialectRejectsAScheduleWithNoNextRunAt(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			base := Schedule{
				Name:           "needs-a-next-run",
				DefName:        "test-workflow",
				EntryPoint:     "Handle",
				CronExpression: "0 3 * * *",
				Input:          json.RawMessage(`{}`),
				Enabled:        true,
			}

			// Zero NextRunAt: refused, identically, on every backend.
			err := store.CreateSchedule(ctx, base)
			if err == nil {
				t.Fatal("a schedule with no next_run_at was accepted.\n\n" +
					"The column's DEFAULT cannot save it -- CreateSchedule names " +
					"next_run_at in its INSERT, so the zero value is written. On " +
					"this dialect that either stores a schedule two millennia " +
					"overdue or fails with a driver error, and the two look " +
					"nothing alike to a caller.")
			}
			if !errors.Is(err, ErrScheduleNextRunUnset) {
				t.Errorf("CreateSchedule returned %v\n\nwant it to wrap "+
					"ErrScheduleNextRunUnset. If this backend fails for its own "+
					"reason instead, the contract is not uniform: the caller sees "+
					"a different error per dialect, which is the divergence "+
					"Validate exists to remove.", err)
			}

			// And the same schedule WITH one is accepted -- so the test is
			// about the missing field, not about the schedule being unusable.
			ok := base
			ok.NextRunAt = time.Now().Add(time.Hour)
			if err := store.CreateSchedule(ctx, ok); err != nil {
				t.Fatalf("the same schedule with next_run_at set was rejected: %v", err)
			}
		})
	}
}

// TestAPastNextRunAtIsAccepted is the scope boundary, asserted rather than
// implied.
//
// Only the ZERO value is refused. A next_run_at in the past is exactly what a
// deliberately backdated schedule looks like, and the misfire policy exists to
// decide what to do about the firings it missed -- rejecting it would break
// catch-up, which is a supported feature with its own flag and its own tests.
//
// "The caller forgot to set it" and "the caller set it to something unusual"
// are different bugs, and only the first has a safe uniform answer.
func TestAPastNextRunAtIsAccepted(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)

			sch := Schedule{
				Name:           "deliberately-backdated",
				DefName:        "test-workflow",
				EntryPoint:     "Handle",
				CronExpression: "0 3 * * *",
				Input:          json.RawMessage(`{}`),
				Enabled:        true,
				NextRunAt:      time.Now().Add(-48 * time.Hour),
			}
			if err := store.CreateSchedule(context.Background(), sch); err != nil {
				t.Errorf("a backdated schedule was rejected: %v\n\n"+
					"Only the zero value is refused. A past next_run_at is what a "+
					"catch-up backfill looks like, and misfire_policy exists to "+
					"decide what to do with the firings it missed.", err)
			}
		})
	}
}
