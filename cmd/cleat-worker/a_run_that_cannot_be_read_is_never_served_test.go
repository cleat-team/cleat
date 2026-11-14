package main

// cleat#3799. A route that addresses a run asks first whether the run's definition is `internal`, and the
// question starts with a read of the run. refuseIfInternalRun used to answer "not refused" when that read
// FAILED, on the reasoning that the handler reads the run again and produces its own miss. That holds for a
// handler that does read it again. It does not hold for signal and cancel, which go straight to the action, so
// with the run unreadable the guard let the request through whatever the run was.
//
// The property this pins is the one the guard exists for: a check that cannot find out must not serve. It
// fails closed on an unreadable run exactly as it already does on an unreadable definition class
// (cleat#3785): a 503 with Retry-After, the error logged, nothing done.
//
// IT HOLDS ON EVERY RUN-ADDRESSED ROUTE, WITH OR WITHOUT A TENANT (cleat#3866 extended cleat#3799). Signal and
// cancel authorise through callerOwnsTarget, which reads the run itself when the request carries a tenant, and the
// history, events, state, query-state and promise-list routes read it through runExists; each of those used to
// answer 500 with the driver's text in the body. They now go through the same writeAccessCheckUnavailable as the
// guard. With no tenant in the request context callerOwnsTarget returns without reading, so there the guard's read
// is the only one before the action (the cleat#3799 fail-open).
//
// Every run-addressed route in the two tables is driven with the run unreadable and the definition INTERNAL, so
// that serving the request is the wrong answer on both counts. Which of them go through refuseIfInternalRun
// (the function that changed) is read off the source rather than listed here, so a route added to the tables
// and guarded that way is held to the 503 without anyone remembering to add it.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
)

// driveWithoutATenant is drive (that file) with no tenant in the request context, which is what a worker started
// without --require-auth serves. A deadline for the same reason drive has one.
func driveWithoutATenant(t *testing.T, entry apiEntry, api *apiServer, r externalRoute) *httptest.ResponseRecorder {
	t.Helper()
	body := r.body
	if body == "" && (r.method == http.MethodPost || r.method == http.MethodPut) {
		body = `{}`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(r.method, r.path, strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	entry(api, rec, req)
	return rec
}

func TestARunThatCannotBeReadIsNeverServed(t *testing.T) {
	driven := 0
	for _, table := range []struct {
		name string
		rows []externalRoute
	}{{"workflows", everyWorkflowRoute}, {"instances", everyInstanceRoute}} {
		table := table
		entry := entryFor(t, table.rows)
		for _, r := range table.rows {
			r := r
			if r.collection || !strings.Contains(r.path, "/RUN") {
				continue // only the run-addressed routes read a run
			}
			for _, withTenant := range []bool{false, true} {
				withTenant := withTenant
				variant := "no tenant"
				if withTenant {
					variant = "tenant"
				}
				t.Run(table.name+"/"+r.name+"/"+variant, func(t *testing.T) {
					logs := capturedLog(t)
					ms := storeServing(engine.ExposureInternal, true)
					fail := errors.New(driverError)
					ms.getWorkflowByIDFn = func(context.Context, string) (*engine.WorkflowInstance, error) { return nil, fail }
					ms.getTerminalRunFn = func(context.Context, string) (*engine.WorkflowInstance, error) { return nil, fail }
					var rec *httptest.ResponseRecorder
					if withTenant {
						rec = drive(t, entry, newTestAPIServer(ms), r)
					} else {
						rec = driveWithoutATenant(t, entry, newTestAPIServer(ms), r)
					}
					driven++

					// An unreadable run is not served, and it is a retryable 503, not a 5xx of the driver's making.
					if rec.Code != http.StatusServiceUnavailable {
						t.Errorf("%s answered %d %s with the run unreadable and its definition internal; want 503, which tells a client to retry",
							r.handler, rec.Code, strings.TrimSpace(rec.Body.String()))
					}
					if rec.Header().Get("Retry-After") == "" {
						t.Errorf("%s: 503 without a Retry-After", r.handler)
					}
					if strings.Contains(rec.Body.String(), driverError) {
						t.Errorf("%s put the driver's error in the response body:\n%s", r.handler, rec.Body.String())
					}
					if !strings.Contains(logs.String(), driverError) {
						t.Errorf("%s swallowed the error: nothing was logged.\nlog: %q", r.handler, logs.String())
					}
				})
			}
		}
	}
	if driven == 0 {
		t.Fatal("no run-addressed route was driven")
	}
}

// The admin helpers read the run too and are not in the route tables above, so they are driven directly: the
// authorisation every /api/admin/instances/* route without a named tenant goes through, and the dead-letter
// reprocess handler. (adminTargetStore's named-tenant branch, which opens another tenant's store, is not driven.)
func TestTheAdminReadsOfARunAnswer503WhenItCannotBeRead(t *testing.T) {
	failing := func() *mockStore {
		ms := storeServing(engine.ExposureInternal, true)
		fail := errors.New(driverError)
		ms.getWorkflowByIDFn = func(context.Context, string) (*engine.WorkflowInstance, error) { return nil, fail }
		return ms
	}
	for _, c := range []struct {
		name string
		call func(s *apiServer, w http.ResponseWriter, r *http.Request)
	}{
		{"callerOwnsTarget", func(s *apiServer, w http.ResponseWriter, r *http.Request) { s.callerOwnsTarget(w, r, "RUN") }},
		{"handleDeadLetterReprocess", func(s *apiServer, w http.ResponseWriter, r *http.Request) { s.handleDeadLetterReprocess(w, r, "RUN") }},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			logs := capturedLog(t)
			ctx, cancel := context.WithTimeout(auth.WithTenantID(context.Background(), exposureTestTenant), time.Second)
			defer cancel()
			req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{}`)).WithContext(ctx)
			rec := httptest.NewRecorder()
			c.call(newTestAPIServer(failing()), rec, req)
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
				t.Errorf("%s answered %d %s (Retry-After %q); want 503 with Retry-After",
					c.name, rec.Code, strings.TrimSpace(rec.Body.String()), rec.Header().Get("Retry-After"))
			}
			if strings.Contains(rec.Body.String(), driverError) {
				t.Errorf("%s put the driver's error in the response body:\n%s", c.name, rec.Body.String())
			}
			if !strings.Contains(logs.String(), driverError) {
				t.Errorf("%s swallowed the error: nothing was logged", c.name)
			}
		})
	}
}
