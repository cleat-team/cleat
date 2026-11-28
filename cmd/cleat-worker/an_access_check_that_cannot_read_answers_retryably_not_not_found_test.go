package main

// cleat#3785. Every external route that addresses a definition, or a run of one, asks first whether the
// definition is `internal`, and it FAILS CLOSED when it cannot find out: an unreadable class is never served.
// That half is the security property and stays.
//
// What this pins is the ANSWER. It used to be the route's own not-found, so a database blip on a present,
// enabled definition read as "workflow definition not found" -- which tells a client the workflow does not
// exist and not to retry -- and the swallowed error was logged nowhere, so an operator could not tell a
// blip from a miss. A store error is now 503 (retryable) with a log line; 404 is kept for "does not exist"
// and "internal", which are still the same bytes.
//
// The tables are the ones an_internal_definition_is_not_reachable_over_http_test.go drives, so every guarded
// route is covered the way the internal case covers it, and a route added there is covered here.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
)

const driverError = "driver: connection reset by peer"

// drive is serve() (that file) with the whole recorder back, because the Retry-After header and the status
// together are what a client reads.
func drive(t *testing.T, entry apiEntry, api *apiServer, r externalRoute) *httptest.ResponseRecorder {
	t.Helper()
	body := r.body
	if body == "" && (r.method == http.MethodPost || r.method == http.MethodPut) {
		body = `{}`
	}
	// A deadline, because the stream route serves a readable definition and then does not return until its
	// client goes away, which a test recorder never does. Without it the control below hangs on that one row
	// until the package timeout kills the whole run -- and with it every test after it, silently.
	ctx, cancel := context.WithTimeout(auth.WithTenantID(context.Background(), exposureTestTenant), 300*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(r.method, r.path, strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	entry(api, rec, req)
	return rec
}

// capturedLog swaps the default logger for the duration of a test. The handlers log through slog directly
// (apiServer has no logger), as the concurrency-key handler does.
func capturedLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// definitionsUnreadable is a store that serves, except that every read of a DEFINITION fails.
func definitionsUnreadable() (*mockStore, *bool) {
	ms := storeServing(engine.ExposureAuth, true)
	started := new(bool)
	fail := errors.New(driverError)
	ms.getWorkflowDefFn = func(context.Context, string, int) (*engine.WorkflowDef, error) { return nil, fail }
	ms.listWorkflowDefsFn = func(context.Context, string) ([]engine.WorkflowDef, error) { return nil, fail }
	ms.startNewRunFn = func(context.Context, string, string, int, json.RawMessage, string, string, int) (string, bool, error) {
		*started = true
		return "must-not-exist", true, nil
	}
	return ms, started
}

func TestEveryGuardedRouteAnswers503WhenItCannotReadTheDefinitionsClass(t *testing.T) {
	for _, table := range []struct {
		name string
		rows []externalRoute
	}{{"workflows", everyWorkflowRoute}, {"instances", everyInstanceRoute}} {
		table := table
		entry := entryFor(t, table.rows)
		for _, r := range table.rows {
			r := r
			if r.collection {
				continue // the listing omits rather than refuses; it has its own test below
			}
			t.Run(table.name+"/"+r.name, func(t *testing.T) {
				logs := capturedLog(t)
				ms, started := definitionsUnreadable()
				rec := drive(t, entry, newTestAPIServer(ms), r)

				if rec.Code != http.StatusServiceUnavailable {
					t.Fatalf("%s answered %d %s when the definition could not be read; want 503, which tells a client to retry "+
						"(a 404 tells it the workflow does not exist)", r.handler, rec.Code, strings.TrimSpace(rec.Body.String()))
				}
				if got := rec.Header().Get("Retry-After"); got == "" {
					t.Errorf("%s: 503 without a Retry-After", r.handler)
				}
				if strings.Contains(rec.Body.String(), driverError) {
					t.Errorf("%s put the driver's error in the response body:\n%s", r.handler, rec.Body.String())
				}
				if !strings.Contains(logs.String(), driverError) {
					t.Errorf("%s swallowed the error: nothing was logged, so an operator cannot tell a store blip from a miss.\nlog: %q", r.handler, logs.String())
				}
				if *started {
					t.Errorf("%s STARTED a run although the definition's class could not be read: the check must fail closed", r.handler)
				}
			})
		}
	}
}

// The control that makes the 503 above mean something: the same rows, with the definition readable, are NOT
// 503. Without it, "every route answers 503" holds for a fixture that answers 503 to everything.
func TestTheSameRoutesAreNotUnavailableWhenTheDefinitionCanBeRead(t *testing.T) {
	for _, table := range []struct {
		name string
		rows []externalRoute
	}{{"workflows", everyWorkflowRoute}, {"instances", everyInstanceRoute}} {
		table := table
		entry := entryFor(t, table.rows)
		for _, r := range table.rows {
			r := r
			if r.collection {
				continue
			}
			t.Run(table.name+"/"+r.name, func(t *testing.T) {
				rec := drive(t, entry, newTestAPIServer(storeServing(engine.ExposureAuth, true)), r)
				if rec.Code == http.StatusServiceUnavailable {
					t.Errorf("%s answered 503 with a readable definition: the fixture, not the access check, is what is unavailable:\n%s", r.handler, rec.Body.String())
				}
			})
		}
	}
}

// 404 stays for what it is for: a definition that is internal, and one that does not exist. Unchanged by
// this fix, and pinned here so the fix cannot be "make everything a 503".
func TestAnInternalOrAbsentDefinitionIsStillANotFoundAndNotAnUnavailable(t *testing.T) {
	for _, table := range []struct {
		name string
		rows []externalRoute
	}{{"workflows", everyWorkflowRoute}, {"instances", everyInstanceRoute}} {
		table := table
		entry := entryFor(t, table.rows)
		for _, r := range table.rows {
			r := r
			if r.collection {
				continue
			}
			t.Run(table.name+"/"+r.name, func(t *testing.T) {
				internal := drive(t, entry, newTestAPIServer(storeServing(engine.ExposureInternal, true)), r)
				absent := drive(t, entry, newTestAPIServer(storeServing("", false)), r)
				for what, rec := range map[string]*httptest.ResponseRecorder{"internal": internal, "absent": absent} {
					if rec.Code == http.StatusServiceUnavailable || rec.Code < 400 {
						t.Errorf("%s: a %s definition answered %d %s", r.handler, what, rec.Code, strings.TrimSpace(rec.Body.String()))
					}
				}
			})
		}
	}
}

// A run that cannot be READ is not a run that does not exist. Where the guard reads the run first, an error
// there used to come back as the route's not-found too.
func TestNoGuardedRouteAnswersNotFoundWhenTheStoreErrorsReadingTheRun(t *testing.T) {
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
			t.Run(table.name+"/"+r.name, func(t *testing.T) {
				logs := capturedLog(t)
				ms := storeServing(engine.ExposureAuth, true)
				ms.getWorkflowByIDFn = func(context.Context, string) (*engine.WorkflowInstance, error) {
					return nil, errors.New(driverError)
				}
				// A store that cannot read a run cannot read it by either method. The terminal-run route reads
				// through GetTerminalRun, and leaving that one answering nil modelled a store that says "no such
				// run" -- which is a 404 and correct.
				ms.getTerminalRunFn = func(context.Context, string) (*engine.WorkflowInstance, error) {
					return nil, errors.New(driverError)
				}
				rec := drive(t, entry, newTestAPIServer(ms), r)
				if rec.Code == http.StatusNotFound || rec.Code < 500 {
					t.Errorf("%s answered %d %s when the store errored reading the run; want a 5xx (retryable), not a not-found:\n%s",
						r.handler, rec.Code, strings.TrimSpace(rec.Body.String()), logs.String())
				}
			})
		}
	}
}

// The listing cannot refuse, so it omits internal runs -- and when it cannot tell which are internal it must
// not return the page at all. It always failed closed; it answered 500 with the driver's text in the body.
func TestTheListingAnswers503AndLogsWhenItCannotReadWhichDefinitionsAreInternal(t *testing.T) {
	logs := capturedLog(t)
	ms := storeServing(engine.ExposureAuth, true)
	pageFetched := false
	listed := ms.listWorkflowsFn
	ms.listWorkflowsFn = func(ctx context.Context, f engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
		pageFetched = true
		return listed(ctx, f)
	}
	ms.listWorkflowDefsFn = func(context.Context, string) ([]engine.WorkflowDef, error) {
		return nil, errors.New(driverError)
	}

	var listRow externalRoute
	for _, r := range everyWorkflowRoute {
		if r.handler == "handleWorkflowsList" {
			listRow = r
		}
	}
	rec := drive(t, entryFor(t, everyWorkflowRoute), newTestAPIServer(ms), listRow)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("the listing answered %d %s; want 503", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("503 without a Retry-After")
	}
	if strings.Contains(rec.Body.String(), driverError) {
		t.Errorf("the driver's error is in the response body:\n%s", rec.Body.String())
	}
	if !strings.Contains(logs.String(), driverError) {
		t.Errorf("the error was not logged: %q", logs.String())
	}
	if pageFetched {
		t.Error("the page was fetched although internal runs could not be told apart: that is the disclosure the check exists to prevent")
	}
}

// The schedules listing hides an internal definition's NAME, so it too must know which are internal. When it
// cannot find out it must not list the schedules (that would name them); it answers 503 and logs, as the run
// listing does.
func TestTheSchedulesListAnswers503AndLogsWhenItCannotReadWhichDefinitionsAreInternal(t *testing.T) {
	logs := capturedLog(t)
	ms := scheduleListingFor(engine.ExposureInternal, "wfd")
	ms.listWorkflowDefsFn = func(context.Context, string) ([]engine.WorkflowDef, error) {
		return nil, errors.New(driverError)
	}
	api := newTestAPIServer(ms)
	req := httptest.NewRequest(http.MethodGet, "/api/schedules", nil)
	req = req.WithContext(auth.WithTenantID(req.Context(), exposureTestTenant))
	rec := httptest.NewRecorder()
	api.handleSchedulesList(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("the schedules listing answered %d %s; want 503", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("503 without a Retry-After")
	}
	if strings.Contains(rec.Body.String(), driverError) || strings.Contains(rec.Body.String(), "wfd") {
		t.Errorf("the response carries the driver's error or an internal definition's name:\n%s", rec.Body.String())
	}
	if !strings.Contains(logs.String(), driverError) {
		t.Errorf("the error was not logged: %q", logs.String())
	}
}

// exposureOnlyStore serves the class through the narrow reader and fails the test if the whole definition is
// read instead -- which is the read this change exists to stop making.
type exposureOnlyStore struct {
	*mockStore
	t     *testing.T
	class engine.ExposureClass
	found bool
	err   error
	reads int
}

func (e *exposureOnlyStore) GetWorkflowDefExposure(context.Context, string, int) (engine.ExposureClass, bool, error) {
	e.reads++
	return e.class, e.found, e.err
}

func (e *exposureOnlyStore) GetWorkflowDef(context.Context, string, int) (*engine.WorkflowDef, error) {
	e.t.Error("the whole definition was read to learn its exposure class, although the store can answer the narrow question")
	return nil, errors.New("must not be called")
}

func TestTheClassIsReadThroughTheNarrowReaderWhenTheStoreHasOne(t *testing.T) {
	api := &apiServer{}
	for _, c := range []struct {
		name         string
		class        engine.ExposureClass
		found        bool
		err          error
		wantInternal bool
		wantErr      bool
	}{
		{"internal", engine.ExposureInternal, true, nil, true, false},
		{"auth", engine.ExposureAuth, true, nil, false, false},
		{"public", engine.ExposurePublic, true, nil, false, false},
		{"stored empty is the default, which is not internal", "", true, nil, false, false},
		{"absent is not internal (the route produces its own miss)", "", false, nil, false, false},
		{"unreadable is an error and not an answer", engine.ExposureInternal, true, errors.New(driverError), false, true},
	} {
		st := &exposureOnlyStore{mockStore: &mockStore{}, t: t, class: c.class, found: c.found, err: c.err}
		internal, err := api.definitionIsInternal(context.Background(), st, "wfd", 1)
		if st.reads != 1 {
			t.Errorf("%s: the narrow reader was consulted %d times, want 1", c.name, st.reads)
		}
		if internal != c.wantInternal || (err != nil) != c.wantErr {
			t.Errorf("%s: definitionIsInternal = (%v, %v), want (internal=%v, err=%v)", c.name, internal, err, c.wantInternal, c.wantErr)
		}
	}
}
