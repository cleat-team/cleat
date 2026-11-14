package main

// cleat#3878. A handler that answered a failed store call with `writeError(w, 500, err.Error())` sent the
// database driver's text to the client (hostnames, statement text, role names) and told it with a 500 that the
// request was not worth sending again. #3785, #3799 and #3866 fixed the routes where the failing read decides
// whether the request may proceed; this is the rest.
//
// The rule, decided by the coordinator on 2026-11-14: a DOMAIN error (not found, conflict) keeps its own status
// and message, which we wrote for the caller; anything else answers a fixed body with no text from the error, 503
// with Retry-After when a retry can succeed and 500 when it cannot, the full error is logged with a request id, and
// the same id is in the body.
//
// The routes are driven the way TestARunThatCannotBeReadIsNeverServed drives them, with one difference. Every store
// method that has no behaviour set fails with the driver's error, so whichever call a handler makes first after the
// guards is the one that fails; the run and its definition are served, so the guards let the request through to it.
// routesThatAnswerAStoreError pins which routes reach that call, so a handler that stops doing so (and the property
// stops being tested for it) fails here instead of passing quietly.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/lib/pq"
)

// failEveryUnsetStoreMethod gives every function field of the mock that has no behaviour yet one that returns its
// zero values and err. Fields already set (by storeServing) are left as they are.
func failEveryUnsetStoreMethod(t *testing.T, ms *mockStore, err error) int {
	t.Helper()
	errType := reflect.TypeOf((*error)(nil)).Elem()
	v := reflect.ValueOf(ms).Elem()
	n := 0
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.Kind() != reflect.Func || !f.IsNil() {
			continue
		}
		ft := f.Type()
		if ft.NumOut() == 0 || ft.Out(ft.NumOut()-1) != errType {
			continue
		}
		// The fields are unexported, so Set needs the addressable view of the same memory.
		f = reflect.NewAt(ft, unsafe.Pointer(f.UnsafeAddr())).Elem()
		f.Set(reflect.MakeFunc(ft, func([]reflect.Value) []reflect.Value {
			out := make([]reflect.Value, ft.NumOut())
			for j := 0; j < ft.NumOut()-1; j++ {
				out[j] = reflect.Zero(ft.Out(j))
			}
			out[ft.NumOut()-1] = reflect.ValueOf(err).Convert(errType)
			return out
		}))
		n++
	}
	return n
}

// requestIDOf pulls "request_id" out of a body without a JSON decoder dependency on the helper under test.
func requestIDOf(body string) string {
	const key = `"request_id":"`
	i := strings.Index(body, key)
	if i < 0 {
		return ""
	}
	rest := body[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func TestAStoreFailureIsNeverDescribedToTheClient(t *testing.T) {
	reached := map[string]bool{}
	for _, table := range []struct {
		name string
		rows []externalRoute
	}{{"workflows", everyWorkflowRoute}, {"instances", everyInstanceRoute}} {
		table := table
		entry := entryFor(t, table.rows)
		for _, r := range table.rows {
			r := r
			t.Run(table.name+"/"+r.name, func(t *testing.T) {
				logs := capturedLog(t)
				ms := storeServing(engine.ExposureAuth, true)
				if failEveryUnsetStoreMethod(t, ms, errors.New(driverError)) == 0 {
					t.Fatal("no store method was made to fail; the mock has changed shape")
				}
				rec := drive(t, entry, newTestAPIServer(ms), r)
				body := rec.Body.String()

				if strings.Contains(body, driverError) {
					t.Fatalf("%s put the driver's error in the response body (%d):\n%s", r.handler, rec.Code, body)
				}
				id := requestIDOf(body)
				if id == "" {
					return // answered before any store call failed (a refusal, a stream, a success)
				}
				reached[table.name+"/"+r.name] = true
				if rec.Code != http.StatusInternalServerError && rec.Code != http.StatusServiceUnavailable {
					t.Errorf("%s answered %d with a request id; want 500 or 503:\n%s", r.handler, rec.Code, body)
				}
				if rec.Header().Get("X-Request-Id") != id {
					t.Errorf("%s: X-Request-Id %q does not match the body's request_id %q", r.handler, rec.Header().Get("X-Request-Id"), id)
				}
				if !strings.Contains(logs.String(), driverError) || !strings.Contains(logs.String(), id) {
					t.Errorf("%s did not log the error against the request id %s.\nlog: %q", r.handler, id, logs.String())
				}
			})
		}
	}
	// Routes the generic failure above cannot reach because an earlier store call that SUCCEEDS has to be
	// served first (the promise is looked up before it is settled; the listing and the query-state read are
	// reached through storeServing's own behaviour). Each gets the one behaviour that lets it get that far.
	driverErr := errors.New(driverError)
	for _, x := range []struct {
		name    string
		route   externalRoute
		prepare func(ms *mockStore)
	}{
		{"resolve a promise", externalRoute{"handleResolvePromise", "resolve a promise", http.MethodPost, "/api/workflows/RUN/promises/P/resolve", false, `{"result":"r"}`},
			func(ms *mockStore) {
				ms.getPromiseFn = func(context.Context, string, string) (string, string, string, error) { return "", "", "", nil }
			}},
		{"reject a promise", externalRoute{"handleRejectPromise", "reject a promise", http.MethodPost, "/api/workflows/RUN/promises/P/reject", false, `{"reason":"r"}`},
			func(ms *mockStore) {
				ms.getPromiseFn = func(context.Context, string, string) (string, string, string, error) { return "", "", "", nil }
			}},
		{"get a run's query state by key", externalRoute{"handleGetWorkflow", "get a run by key", http.MethodGet, "/api/workflows/RUN?key=k", false, ""},
			func(ms *mockStore) {}},
		{"list runs", externalRoute{"handleWorkflowsList", "list runs", http.MethodGet, "/api/workflows/", true, ""},
			func(ms *mockStore) {
				ms.listWorkflowsFn = func(context.Context, engine.WorkflowFilter) ([]engine.WorkflowInstance, error) { return nil, driverErr }
				ms.countWorkflowsFn = func(context.Context, engine.WorkflowFilter) (int, error) { return 0, driverErr }
			}},
	} {
		x := x
		t.Run("extra/"+x.name, func(t *testing.T) {
			logs := capturedLog(t)
			ms := storeServing(engine.ExposureAuth, true)
			x.prepare(ms)
			failEveryUnsetStoreMethod(t, ms, driverErr)
			rec := drive(t, entryFor(t, []externalRoute{x.route}), newTestAPIServer(ms), x.route)
			body := rec.Body.String()
			if strings.Contains(body, driverError) {
				t.Fatalf("%s put the driver's error in the response body (%d):\n%s", x.route.handler, rec.Code, body)
			}
			id := requestIDOf(body)
			if id == "" || (rec.Code != http.StatusInternalServerError && rec.Code != http.StatusServiceUnavailable) {
				t.Fatalf("%s answered %d %s; want a 500 or 503 carrying a request id", x.route.handler, rec.Code, strings.TrimSpace(body))
			}
			if !strings.Contains(logs.String(), driverError) || !strings.Contains(logs.String(), id) {
				t.Errorf("%s did not log the error against the request id %s.\nlog: %q", x.route.handler, id, logs.String())
			}
		})
	}
	for _, want := range routesThatAnswerAStoreError {
		if !reached[want] {
			t.Errorf("%s no longer answers a failed store call through writeStoreError; the property is not tested for it", want)
		}
	}
}

// The two endpoints outside the route tables that listed or read things and answered 500 with the text.
func TestAStoreFailureOnAListIsNeverDescribedToTheClient(t *testing.T) {
	for _, c := range []struct {
		name string
		call func(s *apiServer, w http.ResponseWriter, r *http.Request)
	}{
		{"handleDefinitions", func(s *apiServer, w http.ResponseWriter, r *http.Request) { s.handleDefinitions(w, r) }},
		{"handleSchedulesList", func(s *apiServer, w http.ResponseWriter, r *http.Request) { s.handleSchedulesList(w, r) }},
		{"handleDeadLettersList", func(s *apiServer, w http.ResponseWriter, r *http.Request) { s.handleDeadLettersList(w, r) }},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			logs := capturedLog(t)
			ms := storeServing(engine.ExposureAuth, true)
			failEveryUnsetStoreMethod(t, ms, errors.New(driverError))
			ms.listWorkflowDefsFn = func(context.Context, string) ([]engine.WorkflowDef, error) {
				return nil, errors.New(driverError)
			}
			ms.listWorkflowsFn = func(context.Context, engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
				return nil, errors.New(driverError)
			}
			ms.countWorkflowsFn = func(context.Context, engine.WorkflowFilter) (int, error) {
				return 0, errors.New(driverError)
			}
			ctx, cancel := context.WithTimeout(auth.WithTenantID(context.Background(), exposureTestTenant), time.Second)
			defer cancel()
			req := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			c.call(newTestAPIServer(ms), rec, req)
			body := rec.Body.String()
			if strings.Contains(body, driverError) {
				t.Fatalf("%s put the driver's error in the response body (%d):\n%s", c.name, rec.Code, body)
			}
			if requestIDOf(body) == "" || (rec.Code != http.StatusInternalServerError && rec.Code != http.StatusServiceUnavailable) {
				t.Errorf("%s answered %d %s; want a 500 or 503 carrying a request id", c.name, rec.Code, strings.TrimSpace(body))
			}
			if !strings.Contains(logs.String(), driverError) {
				t.Errorf("%s swallowed the error: nothing was logged", c.name)
			}
		})
	}
}

// writeStoreError itself: the three outcomes, each against the mutation that would make it wrong.
func TestWriteStoreErrorChoosesTheStatusFromTheKindOfError(t *testing.T) {
	for _, c := range []struct {
		name       string
		err        error
		status     int
		retryAfter bool
		wantText   string // text the body must contain; "" means the body must carry no text of err
	}{
		{"a domain error keeps its status and its own message", engine.ErrWorkflowNotFound, http.StatusNotFound, false, "workflow not found"},
		{"a wrapped domain error answers the sentinel's text, not the wrap", errors.Join(errors.New(driverError), engine.ErrPromiseNotFound), http.StatusNotFound, false, "promise not found"},
		{"a conflict keeps its status", engine.ErrScheduleExists, http.StatusConflict, false, "schedule already exists"},
		{"a driver error nobody classified is a 500", errors.New(driverError), http.StatusInternalServerError, false, ""},
		{"a deadline is retryable", context.DeadlineExceeded, http.StatusServiceUnavailable, true, ""},
		{"a dropped connection is retryable", fmt.Errorf("write tcp 10.1.2.3:5432: %w", syscall.ECONNRESET), http.StatusServiceUnavailable, true, ""},
		{"a PostgreSQL deadlock is retryable", &pq.Error{Code: "40P01", Message: "deadlock detected " + driverError}, http.StatusServiceUnavailable, true, ""},
		{"a PostgreSQL constraint violation is not", &pq.Error{Code: "23505", Message: "duplicate key " + driverError}, http.StatusInternalServerError, false, ""},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			capturedLog(t)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			newTestAPIServer(&mockStore{}).writeStoreError(rec, req, c.err)
			if rec.Code != c.status {
				t.Errorf("status %d, want %d (%s)", rec.Code, c.status, rec.Body.String())
			}
			if got := rec.Header().Get("Retry-After") != ""; got != c.retryAfter {
				t.Errorf("Retry-After present = %v, want %v", got, c.retryAfter)
			}
			body := rec.Body.String()
			if c.wantText != "" && !strings.Contains(body, c.wantText) {
				t.Errorf("body %q does not contain %q", body, c.wantText)
			}
			if strings.Contains(body, driverError) || strings.Contains(body, "10.1.2.3") {
				t.Errorf("body carries text of the error: %s", body)
			}
		})
	}
}

// routesThatAnswerAStoreError was read off a run of the test above on 2026-11-14 (the routes whose first failing
// store call is answered through writeStoreError); the extras are asserted in the test itself.
var routesThatAnswerAStoreError = []string{
	"instances/instance events",
	"workflows/cancel a run",
	"workflows/list promises",
	"workflows/list routing rules",
	"workflows/list tags",
	"workflows/read allowed signals",
	"workflows/read query state",
	"workflows/read the DAG",
	"workflows/remove a routing rule",
	"workflows/remove a tag",
	"workflows/run history",
	"workflows/set a routing rule",
	"workflows/set a tag",
	"workflows/signal a run",
	"workflows/start a definition",
	"workflows/terminal run",
	"workflows/update a run",
	"workflows/write allowed signals",
}
