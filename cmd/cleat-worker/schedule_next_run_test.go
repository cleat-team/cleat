package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// A schedule created through POST /api/schedules must carry a next_run_at in
// the future, computed from its cron expression.
//
// workflow_schedules.next_run_at is `NOT NULL DEFAULT now()`, but a column
// default applies only when the INSERT omits the column, and CreateSchedule
// names every column. The handler left NextRunAt unset, so the zero time.Time
// was bound and the row stored 0001-01-01 -- measured, not inferred:
//
//	NextRunAt stored as 0001-01-01 00:00:00 +0000 UTC (292 years late)
//
// The scheduler selects due work with `next_run_at <= now()`, which year 1
// satisfies permanently. So every schedule created through the API fired on
// the next tick regardless of its cron expression -- a "0 7 * * *" daily job
// ran the moment it was created -- and logged "schedule was too far behind to
// catch up; instants were skipped" with a backlog of centuries.
//
// `cleat schedule create` was unaffected: it computes NextCronTimeIn itself.
// Only the HTTP path omitted it.
//
// TestAPISchedulesCreate already covered this endpoint and could not see it:
// its mockStore records that CreateSchedule was called and discards the
// Schedule. A mock that accepts anything cannot catch a value the database
// would reject, so the assertion here is on what the handler PASSES.
func TestAPISchedulesCreate_ComputesNextRunAtFromTheCronExpression(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"a daily schedule", `{"name":"daily","cron":"0 7 * * *","def_name":"w"}`},
		{"with a timezone", `{"name":"tz","cron":"0 7 * * *","def_name":"w","timezone":"America/New_York"}`},
		{"a frequent schedule", `{"name":"often","cron":"*/5 * * * *","def_name":"w"}`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var got engine.Schedule
			ms := &mockStore{}
			ms.createScheduleFn = func(ctx context.Context, s engine.Schedule) error {
				got = s
				return nil
			}
			api := newTestAPIServer(ms)

			before := time.Now()
			req := httptest.NewRequest(http.MethodPost, "/api/schedules", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			api.handleSchedulesList(w, req)

			if w.Code != 201 {
				t.Fatalf("create returned %d, want 201: %s", w.Code, w.Body.String())
			}
			if got.NextRunAt.IsZero() {
				t.Fatalf("NextRunAt is the zero time, which stores as 0001-01-01 and is " +
					"permanently due: the schedule fires on the next tick instead of at its cron time")
			}
			if !got.NextRunAt.After(before) {
				t.Errorf("NextRunAt = %v, want a time after %v -- a next run that is already "+
					"past makes the schedule due immediately", got.NextRunAt, before)
			}
			// And it has to be the cron expression's own next instant, not
			// just any future time: "now plus a bit" would pass the check
			// above while still firing at the wrong moment.
			loc, _ := engine.LoadScheduleLocation(got.Timezone)
			want := engine.NextCronTimeIn(got.CronExpression, before, loc)
			if got.NextRunAt.Sub(want).Abs() > time.Minute {
				t.Errorf("NextRunAt = %v, want the cron expression's next instant %v", got.NextRunAt, want)
			}
		})
	}
}
