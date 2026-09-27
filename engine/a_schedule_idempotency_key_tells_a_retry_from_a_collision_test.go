package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// POST /api/schedules honours Idempotency-Key. cleat#1495.
//
// The endpoint could not tell a retried request from a genuine name collision,
// because both arrive as a second create under a taken name. This drives the
// store directly on every registered backend, because the discrimination is
// made there -- each dialect has to recognise its own uniqueness violation and
// then ASK whether the key is held, and a unit test at the HTTP boundary can
// only see what happens after all of that has already gone right.
func TestAScheduleIdempotencyKeyTellsARetryFromACollision(t *testing.T) {
	base := func() Schedule {
		return Schedule{
			Name:           "nightly-report",
			DefName:        "test-workflow",
			EntryPoint:     "Handle",
			CronExpression: "0 3 * * *",
			Input:          json.RawMessage(`{"region":"eu"}`),
			NextRunAt:      time.Now().Add(time.Hour),
		}
	}

	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			// 1. First request carrying a key is an ordinary create.
			first := base()
			first.IdempotencyKey = "key-A"
			if err := store.CreateSchedule(ctx, first); err != nil {
				t.Fatalf("first create with a key: %v", err)
			}

			// 2. The same request again is a REPLAY, not a collision. This is
			//    the whole point: before this, it was 409 schedule_exists.
			if err := store.CreateSchedule(ctx, first); !errors.Is(err, ErrScheduleIdempotentReplay) {
				t.Errorf("repeating an identical keyed request returned %v\n\n"+
					"want ErrScheduleIdempotentReplay. The caller is retrying its own "+
					"request; answering ErrScheduleExists tells it somebody else's name "+
					"is in the way, which is the defect cleat#1495 exists to fix.", err)
			}

			// 3. NEXT_RUN_AT MUST NOT BE IN THE DIGEST. It is recomputed from
			//    the clock on every request, so a retry a moment later carries a
			//    different one. If this reports a mismatch, the digest has been
			//    widened to include it and EVERY retry now fails closed.
			later := first
			later.NextRunAt = first.NextRunAt.Add(37 * time.Minute)
			if err := store.CreateSchedule(ctx, later); !errors.Is(err, ErrScheduleIdempotentReplay) {
				t.Errorf("a retry differing only in next_run_at returned %v\n\n"+
					"want ErrScheduleIdempotentReplay. next_run_at is derived from the "+
					"cron expression and the clock (see handleCreateSchedule), so two "+
					"identical retries always disagree on it. Including it in the digest "+
					"makes every retry a mismatch.", err)
			}

			// 4. Normalisation: omitting timezone and sending the default are
			//    the same request, because they create the same schedule.
			defaulted := first
			defaulted.Timezone = DefaultScheduleTimezone
			if err := store.CreateSchedule(ctx, defaulted); !errors.Is(err, ErrScheduleIdempotentReplay) {
				t.Errorf("a retry sending the default timezone explicitly returned %v\n\n"+
					"want ErrScheduleIdempotentReplay. It creates the identical schedule, "+
					"so it must digest identically -- otherwise a client library that "+
					"fills in defaults can never successfully retry.", err)
			}

			// 5. Same key, different INPUT is a mismatch, not a replay.
			otherInput := base()
			otherInput.IdempotencyKey = "key-A"
			otherInput.Input = json.RawMessage(`{"region":"us"}`)
			if err := store.CreateSchedule(ctx, otherInput); !errors.Is(err, ErrIdempotencyKeyInputMismatch) {
				t.Errorf("reusing a key with different input returned %v\n\n"+
					"want ErrIdempotencyKeyInputMismatch (cleat#1170). Replaying here "+
					"would tell the caller its schedule was created while silently "+
					"discarding the input it actually sent.", err)
			}

			// 6. Same key, different CRON EXPRESSION is also a mismatch, and
			//    this case is why the digest covers the whole request rather
			//    than the input alone. An input-only digest replays here and
			//    the caller is told 201 about a schedule that fires on somebody
			//    else's timetable.
			otherCron := base()
			otherCron.IdempotencyKey = "key-A"
			otherCron.CronExpression = "0 4 * * *"
			if err := store.CreateSchedule(ctx, otherCron); !errors.Is(err, ErrIdempotencyKeyInputMismatch) {
				t.Errorf("reusing a key with a different cron expression returned %v\n\n"+
					"want ErrIdempotencyKeyInputMismatch. If this replays, the digest "+
					"covers only `input` and every other field of the request can be "+
					"changed under a reused key without a word to the caller.", err)
			}

			// 7. A DIFFERENT key under the same name is a real collision, and
			//    still says so. The fix must not swallow the case it started
			//    from.
			otherKey := base()
			otherKey.IdempotencyKey = "key-B"
			err := store.CreateSchedule(ctx, otherKey)
			if !errors.Is(err, ErrScheduleExists) {
				t.Errorf("a different key under a taken name returned %v\n\n"+
					"want ErrScheduleExists. Two callers choosing one name are a genuine "+
					"collision whatever keys they present; ErrScheduleExists has not been "+
					"reversed by cleat#1495, only relieved of answering a second question.", err)
			}
			if errors.Is(err, ErrScheduleIdempotentReplay) {
				t.Error("a different key under a taken name was reported as a replay: " +
					"the key is not being compared at all, and any two requests now " +
					"deduplicate against each other")
			}

			// 8. A KEYLESS request can never be a replay -- there is nothing to
			//    replay against. It gets the collision answer it always got.
			keyless := base()
			if err := store.CreateSchedule(ctx, keyless); !errors.Is(err, ErrScheduleExists) {
				t.Errorf("a keyless duplicate returned %v, want ErrScheduleExists", err)
			}

			// 9. A SECOND keyless schedule under a FREE name must be accepted.
			//    On SQL Server a unique index treats NULLs as equal, so an
			//    unfiltered index would refuse this -- breaking creation for
			//    every caller that never heard of idempotency. See
			//    migrations/mssql/067.
			//     TWO of them, not one, and the count is the whole test. The
			//     first schedule above carries key-A, so a single keyless
			//     create leaves exactly one row holding NULL -- and one row
			//     cannot collide with anything. A version of this assertion
			//     that created only "weekly-report" passed with the NULL
			//     binding deliberately removed, which is to say it asserted
			//     nothing at all. It takes a SECOND keyless row to make the
			//     index's treatment of NULL observable.
			for _, name := range []string{"weekly-report", "monthly-report"} {
				free := base()
				free.Name = name
				if err := store.CreateSchedule(ctx, free); err != nil {
					t.Errorf("keyless schedule %q was refused: %v\n\n"+
						"Two rows now hold NULL in idempotency_key. A duplicate-key error "+
						"here means either the unique index is not filtered on this dialect "+
						"(see migrations/mssql/067) or the absent key is being bound as \"\" "+
						"rather than NULL -- and distinct \"\"s are all the SAME value, so "+
						"the index would permit at most one keyless schedule per tenant.",
						name, err)
				}
			}

			// 10. Exactly two schedules exist. Every replay above must have
			//     created NOTHING -- a replay that quietly inserted a row would
			//     satisfy every assertion above and be the worse bug.
			got, err := store.ListSchedules(ctx)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(got) != 3 {
				names := make([]string, 0, len(got))
				for _, s := range got {
					names = append(names, s.Name)
				}
				t.Errorf("after 1 create, 3 replays, 2 mismatches, 2 collisions and 2 "+
					"unrelated creates there are %d schedules (%v), want 3: "+
					"%q, \"weekly-report\" and \"monthly-report\"", len(got), names, first.Name)
			}

			// 11. The key is never handed back out. It is `json:"-"` on the
			//     struct, and ListSchedules does not select it.
			for _, s := range got {
				if s.IdempotencyKey != "" {
					t.Errorf("ListSchedules returned schedule %q carrying its "+
						"idempotency key %q. Knowing a key lets a caller join or "+
						"displace another caller's retry; it goes in and does not "+
						"come back.", s.Name, s.IdempotencyKey)
				}
			}
		})
	}
}
