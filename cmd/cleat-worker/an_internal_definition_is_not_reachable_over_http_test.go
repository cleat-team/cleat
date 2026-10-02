package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/google/uuid"
)

// cleat#1986 slice 2b.
//
// The claim under test is "an `internal` definition is not reachable from the
// external HTTP surface", and the word carrying the weight is EVERY. A guard
// added to one route proves nothing about the next, so these tests assert three
// things an ordinary test cannot:
//
//  1. REFUSAL, on every arm -- an internal definition is never served.
//
//  2. INDISTINGUISHABILITY, where the route can carry it. For an arm that
//     answers not-found for a target that does not exist, the refusal must be
//     BYTE-IDENTICAL to that miss: a 404 whose body differs is the disclosure
//     the 404 exists to prevent, moved into the body. The routes that cannot
//     carry it are NAMED, with reasons, rather than skipped.
//
//  3. COVERAGE OF THE ROUTE TABLES THEMSELVES. TestTheRouteTablesCoverEveryArm
//     reads each switch's own source and fails if it can reach a handler these
//     tables do not drive, so a route added later without a guard fails here
//     rather than shipping. That is the only instrument that survives the next
//     author; a comment does not.
//
// And one control without which all of it proves nothing:
// TestTheServersCanServeAtAll.
//
// THE TABLES ARE TWO, NOT ONE, AND THAT IS THE POINT. /api/workflows/... is
// dispatched by handleWorkflows in server.go; /api/instances/... by its own
// switch in api_instances.go. An enumeration that read only the first would
// have excluded the very route -- /api/instances/{id}/state -- that resolves its
// own object instead of going through a shared gate, which is how it came to be
// unguarded in the first place. cleat-review caught that exclusion on #3002.

// externalRoute is one route of the external surface that addresses a workflow
// definition or a run of one.
type externalRoute struct {
	handler string // the method the switch calls; asserted against the source
	name    string
	method  string
	path    string
	// collection marks a route that enumerates rather than addresses, so it
	// must OMIT an internal definition rather than refuse the request.
	collection bool
	// body is a request the route ACCEPTS, so the comparison below reaches the
	// existence question rather than stopping at body validation. An empty body
	// makes several routes answer 400 before they ever ask whether the target
	// exists, and the comparison would then be about this fixture rather than
	// about the guard.
	body string
}

// everyWorkflowRoute drives handleWorkflows (server.go).
var everyWorkflowRoute = []externalRoute{
	{"handleGetWorkflow", "get a run", http.MethodGet, "/api/workflows/RUN", false, ""},
	{"handleStartWorkflow", "start a definition", http.MethodPost, "/api/workflows/DEF/start", false, ""},
	{"handleSignal", "signal a run", http.MethodPost, "/api/workflows/RUN/signal", false, `{"signal_name":"s"}`},
	{"handleCancel", "cancel a run", http.MethodPost, "/api/workflows/RUN/cancel", false, ""},
	{"handleWorkflowRetry", "retry a run", http.MethodPost, "/api/workflows/RUN/retry", false, ""},
	{"handleGetTerminalRun", "terminal run", http.MethodGet, "/api/workflows/RUN/terminal", false, ""},
	{"handleGetHistory", "run history", http.MethodGet, "/api/workflows/RUN/history", false, ""},
	{"handleStreamWorkflow", "stream a run", http.MethodGet, "/api/workflows/RUN/stream", false, ""},
	{"handleGetQueryState", "read query state", http.MethodGet, "/api/workflows/RUN/query?key=k", false, ""},
	{"handleGetDAG", "read the DAG", http.MethodGet, "/api/workflows/RUN/dag", false, ""},
	{"handleListPromises", "list promises", http.MethodGet, "/api/workflows/RUN/promises", false, ""},
	{"handleResolvePromise", "resolve a promise", http.MethodPost, "/api/workflows/RUN/promises/P/resolve", false, `{"result":"r"}`},
	{"handleRejectPromise", "reject a promise", http.MethodPost, "/api/workflows/RUN/promises/P/reject", false, `{"reason":"r"}`},
	{"handleListRoutingRules", "list routing rules", http.MethodGet, "/api/workflows/DEF/routing", false, ""},
	{"handleSetRoutingRule", "set a routing rule", http.MethodPost, "/api/workflows/DEF/routing", false, `{"target_version":1,"weight":1}`},
	{"handleRemoveRoutingRule", "remove a routing rule", http.MethodDelete, "/api/workflows/DEF/routing/RID", false, ""},
	{"handleListWorkflowTags", "list tags", http.MethodGet, "/api/workflows/DEF/tags", false, ""},
	{"handleSetWorkflowTag", "set a tag", http.MethodPut, "/api/workflows/DEF/tags", false, `{"tag":"t","version":1}`},
	{"handleRemoveWorkflowTag", "remove a tag", http.MethodDelete, "/api/workflows/DEF/tags/T", false, ""},
	{"handleGetAllowedSignals", "read allowed signals", http.MethodGet, "/api/workflows/RUN/allowed-signals", false, ""},
	{"handleSetAllowedSignals", "write allowed signals", http.MethodPut, "/api/workflows/RUN/allowed-signals", false, `{"allowed_signals":["*"]}`},
	{"handleWorkflowUpdate", "update a run", http.MethodPost, "/api/workflows/RUN/update/U", false, ""},
	// The list is reached through the same entry point: handleWorkflows strips
	// the prefix and delegates an empty remainder here, which is what the mux
	// does for /api/workflows too.
	{"handleWorkflowsList", "list runs", http.MethodGet, "/api/workflows/", true, ""},
}

// everyInstanceRoute drives handleInstancesRoutes (api_instances.go) -- the
// switch OUTSIDE server.go, and the reason these tables are two.
var everyInstanceRoute = []externalRoute{
	{"handleGetInstanceEvents", "instance events", http.MethodGet, "/api/instances/RUN/events", false, ""},
	{"handleGetInstanceState", "instance state", http.MethodGet, "/api/instances/RUN/state", false, ""},
	{"handleGetInstanceState", "instance state by id", http.MethodGet, "/api/instances/RUN", false, ""},
}

// routesWithoutANotFound names the routes whose behaviour for a target that does
// not exist is NOT a not-found, and which therefore cannot be matched by this
// slice's refusal.
//
// It is a list with reasons rather than a skip, because a skip would hide the day
// a ninth route joins them: the test FAILS on an unlisted one, so an author has
// to argue with the reason before adding it.
//
// All eight share one cause, and it is pre-existing: they act on a run or a
// definition WITHOUT verifying that it exists, so they answer success, or a
// complaint about a version, for a target that is not there. That is why a 404
// refusal is observable on them. Making them indistinguishable means making them
// verify their target first -- a behaviour change for absent targets, which
// belongs in its own diff and not in this one. Filed as cleat#3003.
var routesWithoutANotFound = map[string]string{
	"handleSetRoutingRule":    "answers 409 about the VERSION before asking whether the name is deployed",
	"handleSetWorkflowTag":    "same: 409 on the version before the name is considered",
	"handleRemoveRoutingRule": "answers 200 removed for a rule on a definition that is not deployed",
	"handleRemoveWorkflowTag": "answers 200 removed for a tag on a definition that is not deployed",
	"handleResolvePromise":    "answers 200 resolved for a promise on a run that does not exist",
	"handleRejectPromise":     "answers 200 rejected for a promise on a run that does not exist",
	"handleGetAllowedSignals": "answers 200 with an empty list for a run that does not exist",
	"handleSetAllowedSignals": "answers 200 and writes for a run that does not exist",
}

// exposureTestTenant is the caller every request here is scoped to. It matters:
// with no tenant in the context, callerOwnsTarget short-circuits to "trusted",
// and the routes behind it never reach their ownership question at all -- which
// would make the comparison below a fact about this fixture rather than about
// the guard. The stubbed run is owned by this tenant for the same reason.
var exposureTestTenant = uuid.MustParse("00000000-0000-0000-0000-0000000000aa")

// storeServing builds the store every request in this file is driven against.
// serve=false means it serves NOTHING: every name and every run is unknown.
func storeServing(class engine.ExposureClass, serve bool) *mockStore {
	ms := &mockStore{}
	if serve {
		ms.getWorkflowDefFn = func(_ context.Context, _ string, _ int) (*engine.WorkflowDef, error) {
			return &engine.WorkflowDef{
				Name: "wfd", Version: 1, ABIVersion: 1, MinVersion: 1,
				WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, Exposure: class,
			}, nil
		}
		ms.listWorkflowDefsFn = func(_ context.Context, _ string) ([]engine.WorkflowDef, error) {
			return []engine.WorkflowDef{{Name: "wfd", Version: 1, Exposure: class}}, nil
		}
		ms.getWorkflowByIDFn = func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
			return &engine.WorkflowInstance{
				ID: id, DefName: "wfd", DefVersion: 1, Status: "ready",
				TenantID: exposureTestTenant.String(),
			}, nil
		}
		ms.listVersionsFn = func(_ context.Context, _ string) ([]int, error) { return []int{1}, nil }
		ms.validateVersionFn = func(_ context.Context, _ string, _ int) (bool, error) { return true, nil }
		ms.listWorkflowsFn = func(_ context.Context, _ engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
			return []engine.WorkflowInstance{{ID: "RUN", DefName: "wfd", DefVersion: 1}}, nil
		}
		ms.countWorkflowsFn = func(_ context.Context, _ engine.WorkflowFilter) (int, error) { return 1, nil }
	} else {
		ms.getWorkflowDefFn = func(_ context.Context, _ string, _ int) (*engine.WorkflowDef, error) { return nil, nil }
		ms.listWorkflowDefsFn = func(_ context.Context, _ string) ([]engine.WorkflowDef, error) { return nil, nil }
		ms.getWorkflowByIDFn = func(_ context.Context, _ string) (*engine.WorkflowInstance, error) { return nil, nil }
		ms.listVersionsFn = func(_ context.Context, _ string) ([]int, error) { return nil, nil }
		ms.validateVersionFn = func(_ context.Context, _ string, _ int) (bool, error) { return false, nil }
		ms.listWorkflowsFn = func(_ context.Context, _ engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
			return nil, nil
		}
		ms.countWorkflowsFn = func(_ context.Context, _ engine.WorkflowFilter) (int, error) { return 0, nil }
	}
	return ms
}

// apiEntry is a method EXPRESSION on *apiServer -- (*apiServer).handleWorkflows
// rather than api.handleWorkflows. The difference matters: the latter is a
// method VALUE bound to one receiver, and binding it to nil would compile and
// panic on call.
type apiEntry func(*apiServer, http.ResponseWriter, *http.Request)

// serve drives one request through a route entry point.
func serve(t *testing.T, entry apiEntry, api *apiServer, method, path, body string) (int, string) {
	t.Helper()
	if body == "" && (method == http.MethodPost || method == http.MethodPut) {
		body = `{}`
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(auth.WithTenantID(req.Context(), exposureTestTenant))
	rec := httptest.NewRecorder()
	entry(api, rec, req)
	return rec.Code, rec.Body.String()
}

// TestTheServersCanServeAtAll is the POSITIVE CONTROL for everything below.
//
// "An internal definition is never served" is satisfied by a harness that serves
// nothing, so the control proves the difference: an `auth` definition -- the
// class that means "what the server already does" -- IS served, through BOTH
// entry points. Without it, every assertion in this file would pass against a
// broken fixture, which is the failure mode the whole file exists to avoid.
func TestTheServersCanServeAtAll(t *testing.T) {
	for _, c := range []struct {
		name  string
		entry apiEntry
		path  string
	}{
		{"handleWorkflows", (*apiServer).handleWorkflows, "/api/workflows/RUN"},
		{"handleInstancesRoutes", (*apiServer).handleInstancesRoutes, "/api/instances/RUN/state"},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			api := newTestAPIServer(storeServing(engine.ExposureAuth, true))
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, c.path, nil)
			req = req.WithContext(auth.WithTenantID(req.Context(), exposureTestTenant))
			c.entry(api, rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("CONTROL FAILED: an `auth` definition was not served through %s (got %d %s), so \"an "+
					"internal one is not served\" below would hold for a fixture that serves nothing at all",
					c.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// entryFor is the route table's entry point, so each table drives its own switch.
func entryFor(t *testing.T, rows []externalRoute) apiEntry {
	t.Helper()
	if len(rows) == 0 {
		t.Fatal("empty route table")
	}
	switch rows[0].handler {
	case "handleGetInstanceEvents", "handleGetInstanceState":
		return (*apiServer).handleInstancesRoutes
	}
	return (*apiServer).handleWorkflows
}

// TestEveryExternalRouteRefusesAnInternalDefinition is the primary property: an
// internal definition is never SERVED on any arm of either switch.
func TestEveryExternalRouteRefusesAnInternalDefinition(t *testing.T) {
	for _, table := range []struct {
		name string
		rows []externalRoute
	}{{"workflows", everyWorkflowRoute}, {"instances", everyInstanceRoute}} {
		table := table
		entry := entryFor(t, table.rows)
		for _, r := range table.rows {
			r := r
			t.Run(table.name+"/"+r.name, func(t *testing.T) {
				api := newTestAPIServer(storeServing(engine.ExposureInternal, true))
				code, body := serve(t, entry, api, r.method, r.path, r.body)
				if r.collection {
					// A collection cannot refuse; it must OMIT. Its own test asserts that.
					if strings.Contains(body, "wfd") {
						t.Errorf("%s disclosed an `internal` definition by name:\n%s", r.handler, body)
					}
					return
				}
				if code < 400 {
					t.Errorf("%s SERVED an `internal` definition (got %d):\n%s", r.handler, code, body)
				}
			})
		}
	}
}

// TestTheRefusalIsIndistinguishableWhereTheRouteHasAMiss pins the stronger
// property where the route supplies a miss to match, and NAMES the exceptions
// rather than skipping them.
func TestTheRefusalIsIndistinguishableWhereTheRouteHasAMiss(t *testing.T) {
	for _, table := range []struct {
		name string
		rows []externalRoute
	}{{"workflows", everyWorkflowRoute}, {"instances", everyInstanceRoute}} {
		table := table
		entry := entryFor(t, table.rows)
		for _, r := range table.rows {
			if r.collection {
				continue
			}
			r := r
			t.Run(table.name+"/"+r.name, func(t *testing.T) {
				driven := func(class engine.ExposureClass, present bool) (int, string) {
					api := newTestAPIServer(storeServing(class, present))
					return serve(t, entry, api, r.method, r.path, r.body)
				}
				absentCode, absentBody := driven("", false)
				gotCode, gotBody := driven(engine.ExposureInternal, true)

				if absentCode != http.StatusNotFound {
					reason, known := routesWithoutANotFound[r.handler]
					if !known {
						t.Errorf("%s answers %d (not a 404) for a target that does not exist, so its refusal for an "+
							"internal one is distinguishable. If that is the route's own pre-existing behaviour, add "+
							"it to routesWithoutANotFound with the reason; if not, this is a defect in the guard:\n"+
							"  absent: %d %s", r.handler, absentCode, absentCode, strings.TrimSpace(absentBody))
						return
					}
					t.Logf("recorded exception: %s -- %s", r.handler, reason)
					if gotCode < 400 {
						t.Errorf("%s SERVED an internal definition (%d): %s", r.handler, gotCode, gotBody)
					}
					return
				}
				if gotCode != absentCode || gotBody != absentBody {
					t.Errorf("%s is distinguishable from a genuine miss:\n  internal: %d %s\n  absent:   %d %s",
						r.handler, gotCode, strings.TrimSpace(gotBody), absentCode, strings.TrimSpace(absentBody))
				}
			})
		}
	}
}

// TestTheRouteTablesCoverEveryArm reads EACH switch's own source and fails if it
// can reach a handler these tables do not drive.
//
// This is the instrument that carries "every". A guard added to one family of
// routes proves nothing about the other -- runExists is id-scoped and defExists
// is the name-scoped twin -- and MOST arms resolve their own object rather than
// using either gate, which is how /api/instances/{id}/state went unguarded until
// cleat-review found it. Only an enumeration that reads the source can fail when
// the next author adds an arm.
//
// TWO switches, because there are two: reading only server.go's would exclude
// the instances route, which is the exclusion this test now exists to prevent.
func TestTheRouteTablesCoverEveryArm(t *testing.T) {
	for _, c := range []struct {
		file    string
		fn      string
		rows    []externalRoute
		aliases []string // reached by delegation rather than by an arm
	}{
		{"server.go", "handleWorkflows", everyWorkflowRoute, []string{"handleWorkflowsList"}},
		{"api_instances.go", "handleInstancesRoutes", everyInstanceRoute, nil},
	} {
		c := c
		t.Run(c.fn, func(t *testing.T) {
			src, err := os.ReadFile(c.file)
			if err != nil {
				t.Fatalf("read %s: %v", c.file, err)
			}
			body, err := switchBody(string(src), c.fn)
			if err != nil {
				t.Fatalf("locate %s in %s: %v", c.fn, c.file, err)
			}

			inSource := map[string]bool{}
			for _, m := range regexp.MustCompile(`s\.(handle[A-Za-z]+)\(`).FindAllStringSubmatch(body, -1) {
				inSource[m[1]] = true
			}
			for _, a := range c.aliases {
				inSource[a] = true
			}

			inTable := map[string]bool{}
			for _, r := range c.rows {
				inTable[r.handler] = true
			}

			if uncovered := diffKeys(inSource, inTable); len(uncovered) > 0 {
				t.Errorf("%s can reach %d handler(s) these tests do not drive, so their exposure behaviour is "+
					"asserted nowhere: %v\nAdd a row for each.", c.fn, len(uncovered), uncovered)
			}
			if stale := diffKeys(inTable, inSource); len(stale) > 0 {
				t.Errorf("these tests drive %d handler(s) %s can no longer reach, so the table has drifted from "+
					"the code it claims to cover: %v", len(stale), c.fn, stale)
			}
		})
	}
}

// TestTheListingRoutesOmitAnInternalDefinition covers the routes that PUBLISH
// definitions or runs in bulk. They cannot answer 404 -- a collection is not
// addressing one member -- so they must omit it, or the 404 on the routes that
// address it individually is undone by a list that still shows it.
func TestTheListingRoutesOmitAnInternalDefinition(t *testing.T) {
	api := newTestAPIServer(storeServing(engine.ExposureInternal, true))
	for _, c := range []struct {
		name   string
		call   func(http.ResponseWriter, *http.Request)
		method string
		path   string
	}{
		{"GET /api/definitions", api.handleDefinitions, http.MethodGet, "/api/definitions"},
		{"GET /api/openapi.json", api.handleOpenAPIDocument, http.MethodGet, "/api/openapi.json"},
		{"GET /api/workflows", api.handleWorkflowsList, http.MethodGet, "/api/workflows"},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, nil)
			req = req.WithContext(auth.WithTenantID(req.Context(), exposureTestTenant))
			rec := httptest.NewRecorder()
			c.call(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s answered %d, expected 200: %s", c.name, rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "wfd") {
				t.Errorf("%s named an `internal` definition, which discloses it on a route the 404 cannot cover:\n%s",
					c.name, rec.Body.String())
			}
		})
	}
}

// TestTheAdminAndDeadLetterRoutesStillSeeAnInternalRun pins the decision this
// slice makes on purpose: exposure governs INGRESS, not administration. An
// operator must still read an internal run's history and reach its DLQ entry --
// a guard there would strand exactly the runs it exists to rescue.
//
// Asserted rather than left implied, so a later change that "helpfully" closes
// these fails here and has to argue with the reason.
func TestTheAdminAndDeadLetterRoutesStillSeeAnInternalRun(t *testing.T) {
	api := newTestAPIServer(storeServing(engine.ExposureInternal, true))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/instances/RUN", nil)
	req = req.WithContext(auth.WithTenantID(req.Context(), exposureTestTenant))

	// callerOwnsTarget is the admin API's enforcement point; it must keep
	// answering true for an internal run.
	st, ok := api.callerOwnsTarget(rec, req, "RUN")
	if !ok || st == nil {
		t.Fatal("callerOwnsTarget refused an `internal` run, so the admin API can no longer manage it. Exposure " +
			"governs ingress, not administration -- see the note beside the /api/workflows mounts.")
	}
}

// switchBody returns the source of the named function, from its declaration to
// the next top-level func.
func switchBody(src, fn string) (string, error) {
	re := regexp.MustCompile(`(?m)^func \(s \*apiServer\) ` + fn + `\(`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		return "", fmt.Errorf("no declaration of %s in this file", fn)
	}
	rest := src[loc[1]:]
	if nxt := regexp.MustCompile(`(?m)^func `).FindStringIndex(rest); nxt != nil {
		return rest[:nxt[0]], nil
	}
	return rest, nil
}

func diffKeys(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
