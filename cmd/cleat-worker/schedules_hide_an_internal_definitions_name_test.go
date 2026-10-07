package main

// cleat#3001, the schedules member of the class.
//
// A route that does not ADDRESS a workflow can still disclose an `internal`
// definition's NAME in its payload, and -- being a collection -- it cannot
// answer 404 for one member. GET /api/schedules is such a route: each schedule
// names its target definition, so listing schedules would disclose the name the
// per-definition 404 exists to hide.
//
// Unlike the run list, the schedule is not dropped: a cron-triggered internal
// workflow is legitimate, so only the target's NAME is hidden and the schedule
// stays visible. Both halves are asserted, because hiding the name must not
// become hiding the schedule.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
)

// scheduleListingFor returns a store serving one definition of the given class
// under "wfd", and one schedule named "nightly" whose target is defName.
func scheduleListingFor(class engine.ExposureClass, defName string) *mockStore {
	ms := storeServing(class, true)
	ms.listSchedulesFn = func(_ context.Context) ([]engine.Schedule, error) {
		return []engine.Schedule{{
			Name: "nightly", DefName: defName, CronExpression: "0 3 * * *",
		}}, nil
	}
	return ms
}

func getSchedules(t *testing.T, ms *mockStore) string {
	t.Helper()
	api := newTestAPIServer(ms)
	req := httptest.NewRequest(http.MethodGet, "/api/schedules", nil)
	req = req.WithContext(auth.WithTenantID(req.Context(), exposureTestTenant))
	rec := httptest.NewRecorder()
	api.handleSchedulesList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/schedules answered %d, want 200: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// The disclosure: a schedule targeting an `internal` definition must not name it.
func TestTheSchedulesListDoesNotNameAnInternalDefinition(t *testing.T) {
	body := getSchedules(t, scheduleListingFor(engine.ExposureInternal, "wfd"))

	if strings.Contains(body, "wfd") {
		t.Errorf("GET /api/schedules named an `internal` definition, which discloses it on a "+
			"route the per-definition 404 cannot cover:\n%s", body)
	}
	// And the schedule is KEPT -- hiding the name must not have become hiding
	// the schedule, which would strand a legitimate operational object.
	if !strings.Contains(body, "nightly") {
		t.Errorf("the schedule was dropped rather than its target's name hidden; an internal "+
			"workflow's schedule is legitimate and must stay visible:\n%s", body)
	}
}

// The control that stops the test above passing vacuously or on a mutant that
// blanked every def_name: the SAME schedule with an EXTERNAL target keeps its
// name, so the hiding is keyed on the class rather than applied indiscriminately.
func TestTheSchedulesListNamesAnExternalDefinition(t *testing.T) {
	body := getSchedules(t, scheduleListingFor(engine.ExposurePublic, "wfd"))

	if !strings.Contains(body, "wfd") {
		t.Errorf("an EXTERNAL definition's name was hidden, so the filter is not keyed on the "+
			"class:\n%s", body)
	}
}
