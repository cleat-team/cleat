package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// A schedule created with no input at all has to be storable, on every
// dialect.
//
// workflow_schedules.input is `NOT NULL DEFAULT '{}'` in all three shipped
// schemas, and a column default only applies when the INSERT omits the column.
// CreateSchedule names it explicitly, so a nil json.RawMessage is bound as SQL
// NULL and the constraint rejects the row -- the default never gets a chance
// to apply. On Postgres that surfaces at the API as
//
//	500 pq: null value in column "input" of relation "workflow_schedules"
//	violates not-null constraint (23502)
//
// which is a driver error leaking through a handler that had already decided
// the field was optional: handleCreateSchedule requires name, cron and
// def_name, and nothing else.
//
// MSSQL was fixed for this in IMPROVEMENT-PLAN 3.16 and the other two dialects
// were not. That is not an oversight so much as a reporting asymmetry: SQL
// Server guards the column with `CHECK (ISJSON(input) = 1)`, so the bad value
// failed loudly there and got a helper -- scheduleInputJSON -- whose doc
// comment states the dialect-independent reason ("the column is NOT NULL with
// a '{}' default in the shipped schema"). It was applied to one of the three
// INSERTs that needed it.
//
// Nothing caught the remaining two because every other schedule test in this
// package passes `Input: json.RawMessage("{}")` explicitly, and both non-HTTP
// callers supply an input of their own: `cleat schedule create` defaults its
// --input flag, and guest code goes through ScheduleCron. The HTTP API is the
// only caller that can leave it empty, and it had no store-level test.
func TestCreateSchedule_AnOmittedInputBecomesAnEmptyObject(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			for _, tc := range []struct {
				name  string
				input json.RawMessage
				want  string
			}{
				// The case the API can produce and no test covered.
				{"no input at all", nil, "{}"},
				// json.RawMessage("") is a distinct value from nil and reaches
				// the driver the same way: len()==0, bound as NULL.
				{"an empty raw message", json.RawMessage(``), "{}"},
				// The existing behaviour, asserted here so the normalisation
				// cannot be implemented by overwriting every input with "{}".
				{"an object is left alone", json.RawMessage(`{"key":"value"}`), `{"key":"value"}`},
			} {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					name := "sched-input-" + tc.name
					if err := store.CreateSchedule(ctx, Schedule{
						Name:           name,
						DefName:        "test-workflow",
						EntryPoint:     "main",
						CronExpression: "0 7 * * *",
						Input:          tc.input,
						Enabled:        true,
						NextRunAt:      time.Now().Add(time.Hour),
					}); err != nil {
						t.Fatalf("CreateSchedule with %s: %v\n\n"+
							"This is what POST /api/schedules does when the caller "+
							"omits \"input\", which handleCreateSchedule permits.", tc.name, err)
					}

					// Read back through ListSchedules rather than raw SQL: the
					// value has to survive the round trip as JSON, not merely
					// satisfy the constraint on the way in.
					//
					// sameJSON, not a byte comparison: a jsonb column re-renders
					// on read, so a textual assertion would fail on a correct
					// store. See its doc comment.
					got := findSchedule(t, store, ctx, name)
					if !sameJSON(t, string(got.Input), tc.want) {
						t.Errorf("input stored as %s, want %s", got.Input, tc.want)
					}
				})
			}
		})
	}
}
