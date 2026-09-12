package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// A schedule mutation on a name that does not exist answers 404, not 200.
// cleat#1297.
//
// Every one of these endpoints answered `200 {"status":"disabled"}` for a name
// that was never there, because the store discarded the statement result. The
// operator-facing consequence is the reason this is a bug rather than an
// untidiness: `disable` is what someone reaches for during an incident, and a
// 200 saying `disabled` is an authoritative answer to the wrong question while
// the schedule keeps firing.
func scheduleReq(t *testing.T, api *apiServer, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	api.handleSchedules(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func notFoundScheduleServer(t *testing.T) *apiServer {
	t.Helper()
	ms := &mockStore{}
	ms.setScheduleEnabledFn = func(context.Context, string, bool) error {
		return engine.ErrScheduleNotFound
	}
	ms.deleteScheduleFn = func(context.Context, string) error {
		return engine.ErrScheduleNotFound
	}
	return newTestAPIServer(ms)
}

func TestDisablingAnAbsentScheduleIs404(t *testing.T) {
	rec := scheduleReq(t, notFoundScheduleServer(t),
		http.MethodPost, "/api/schedules/does-not-exist/disable")

	if rec.Code != 404 {
		t.Fatalf("disable on an absent schedule answered %d, want 404. Body: %s",
			rec.Code, rec.Body.String())
	}

	// The discriminator matters as much as the code. writeScheduleError's own
	// doc comment gives the reason: a client must be able to tell two
	// responses apart without parsing prose, and the prose is dialect-specific
	// when it comes from a driver.
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["detail"] != "schedule_not_found" {
		t.Errorf("detail is %q, want \"schedule_not_found\"", body["detail"])
	}
}

func TestEnablingAnAbsentScheduleIs404(t *testing.T) {
	rec := scheduleReq(t, notFoundScheduleServer(t),
		http.MethodPost, "/api/schedules/does-not-exist/enable")
	if rec.Code != 404 {
		t.Errorf("enable on an absent schedule answered %d, want 404", rec.Code)
	}
}

func TestDeletingAnAbsentScheduleIs404(t *testing.T) {
	rec := scheduleReq(t, notFoundScheduleServer(t),
		http.MethodDelete, "/api/schedules/does-not-exist")
	if rec.Code != 404 {
		t.Errorf("delete on an absent schedule answered %d, want 404", rec.Code)
	}
}

// The positive control. Without it, a handler that answered 404 to everything
// would pass all three tests above -- and "it rejects the bad case" is
// satisfied by a handler that rejects every case.
func TestMutatingAPresentScheduleStillSucceeds(t *testing.T) {
	api := newTestAPIServer(&mockStore{}) // nil hooks: the store reports success

	for _, tc := range []struct {
		name, method, path, want string
	}{
		{"disable", http.MethodPost, "/api/schedules/present/disable", "disabled"},
		{"enable", http.MethodPost, "/api/schedules/present/enable", "enabled"},
		{"delete", http.MethodDelete, "/api/schedules/present", "deleted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := scheduleReq(t, api, tc.method, tc.path)
			if rec.Code != 200 {
				t.Fatalf("%s on a present schedule answered %d, want 200. Body: %s",
					tc.name, rec.Code, rec.Body.String())
			}
			var body map[string]string
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["status"] != tc.want {
				t.Errorf("status is %q, want %q", body["status"], tc.want)
			}
		})
	}
}

// An unclassified store failure stays a 500. The fix must not turn every
// schedule error into a 404 -- a database outage reported as "not found"
// sends an operator looking for a typo.
func TestAnUnclassifiedScheduleFailureIsStill500(t *testing.T) {
	ms := &mockStore{}
	ms.setScheduleEnabledFn = func(context.Context, string, bool) error {
		return context.DeadlineExceeded
	}
	rec := scheduleReq(t, newTestAPIServer(ms),
		http.MethodPost, "/api/schedules/present/disable")
	if rec.Code != 500 {
		t.Errorf("an unclassified store failure answered %d, want 500", rec.Code)
	}
}
