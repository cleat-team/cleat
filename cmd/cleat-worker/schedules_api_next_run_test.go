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

// TestACreatedScheduleHasARealNextRunAt is cleat#995.
//
// The handler built its engine.Schedule without NextRunAt, so the zero
// time.Time -- year 1 -- went to the store. That is not a cosmetic omission:
//
//	PostgreSQL  accepts it. 201, and the listing shows a healthy enabled
//	            schedule whose next run is 0001-01-01. The first scheduler tick
//	            treats it as overdue, and scheduleAdvance loops
//	            `for count <= catchUpLimit && probe.Before(now)` -- so with the
//	            default catch_up policy and limit of 60, a schedule created
//	            ONCE fires up to sixty times.
//	MySQL       rejects it. 500, "Incorrect datetime value: '0000-00-00'".
//	            The endpoint does not work on that dialect at all.
//
// WHY IT SURVIVED: this is one of three creation paths and the only one that
// omitted it. engine/schedules.go's ScheduleCron host call and cmd/cleat's CLI
// both compute NextCronTimeIn from a LoadScheduleLocation location. So the
// feature works when exercised any way except over HTTP, and every test that
// creates a schedule by another route passes.
//
// The assertion is on the value the STORE receives, not on the response code.
// A 201 is what the broken version already returned on PostgreSQL, so status
// alone cannot distinguish the two.
func TestACreatedScheduleHasARealNextRunAt(t *testing.T) {
	var got engine.Schedule
	ms := &mockStore{}
	ms.createScheduleFn = func(_ context.Context, s engine.Schedule) error {
		got = s
		return nil
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	before := time.Now()
	api.handleSchedulesList(rec, httptest.NewRequest(http.MethodPost, "/api/schedules",
		strings.NewReader(`{"name":"nightly","def_name":"wf","cron":"0 3 * * *","timezone":"UTC"}`)))

	if rec.Code != 201 {
		t.Fatalf("creating a schedule answered %d: %s", rec.Code, rec.Body.String())
	}
	if got.NextRunAt.IsZero() {
		t.Fatal("the store received a schedule whose NextRunAt is the zero time.\n\n" +
			"That is year 1, so PostgreSQL stores a schedule permanently overdue and the " +
			"next scheduler tick fires it up to catch_up_limit times, while MySQL rejects " +
			"the insert outright. The response was still 201.")
	}
	// A real next firing is in the future. Asserting the SIDE of now rather
	// than a duration: no tolerance to tune, and nothing that drifts on a slow
	// machine.
	if !got.NextRunAt.After(before) {
		t.Errorf("NextRunAt is %s, which is not after the request at %s -- a schedule that "+
			"is already due on creation is the overdue case in a smaller costume",
			got.NextRunAt.Format(time.RFC3339), before.Format(time.RFC3339))
	}
}

// TestAnInvalidTimezoneIsRefusedBeforeTheWrite pins that the refusal precedes
// the store call.
//
// IT DOES NOT TEST THE fellBack BRANCH the #995 fix adds, and an earlier
// version of this file claimed it did. Falsification found that: disabling that
// branch left this test green, because engine.ValidateTimezone rejects a bad
// name earlier in the handler. The branch is unreachable by any request.
//
// What it does test is still worth having -- that a rejected timezone produces
// no write -- and it was itself a vacuous pass first time round: it asserted
// `code != 201`, which a 404 from calling the wrong handler satisfies. A
// control that accepts every error accepts the error where the endpoint does
// not exist.
func TestAnInvalidTimezoneIsRefusedBeforeTheWrite(t *testing.T) {
	called := false
	ms := &mockStore{}
	ms.createScheduleFn = func(_ context.Context, _ engine.Schedule) error {
		called = true
		return nil
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleSchedulesList(rec, httptest.NewRequest(http.MethodPost, "/api/schedules",
		strings.NewReader(`{"name":"n","def_name":"wf","cron":"0 3 * * *","timezone":"Mars/Olympus_Mons"}`)))

	if rec.Code != 400 {
		t.Errorf("an unloadable timezone answered %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if called {
		t.Error("the store was called despite the timezone being invalid; the refusal must " +
			"come before the write")
	}
}
