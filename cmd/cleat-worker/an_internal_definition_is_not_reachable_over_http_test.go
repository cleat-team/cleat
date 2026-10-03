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
// IT IS EMPTY AS OF cleat#3003, AND EMPTY IS THE GOAL. The eight that were
// listed here were fixed, so the loop below now asserts indistinguishability for
// them exactly as it does for every other route -- which is the property this
// file exists for, finally holding on all of them.
//
// The map is KEPT while empty, because it is the mechanism and not the list: the
// test fails on a route whose absent answer is not a 404 unless its handler is
// argued for here, so the day a ninth joins, its author has to write the reason.
//
// The eight, for the record, since a deleted list leaves nothing to read: they
// all shared one cause. They acted on a run or a definition WITHOUT verifying it
// exists, so they answered success, or a complaint about a version, for a target
// that was never deployed -- and that difference from a genuine miss is what told
// a caller "something is here and it is refused". Each now verifies its target
// through refuseIfAbsentOrInternalDef / refuseIfAbsentOrInternalRun, which answer
// the SAME not-found for an absent target as for an internal one.
var routesWithoutANotFound = map[string]string{}

// exposureTestTenant is the caller every request here is scoped to. It matters:
// with no tenant in the context, callerOwnsTarget short-circuits to "trusted",
// and the routes behind it never reach their ownership question at all -- which
// would make the comparison below a fact about this fixture rather than about
// the guard. The stubbed run is owned by this tenant for the same reason.
var exposureTestTenant = uuid.MustParse("00000000-0000-0000-0000-0000000000aa")

// excluded reports whether name is in the exclusion set -- the store-side half
// of the listing contract, modelled here so the fixture cannot pass by ignoring
// the filter.
func excluded(name string, exclude []string) bool {
	for _, e := range exclude {
		if e == name {
			return true
		}
	}
	return false
}

// storeServing builds the store every request in this file is driven against.
// serve=false means it serves NOTHING: every name and every run is unknown.
//
// ONE DELIBERATE BLINDNESS, named because it is the shape that hid cleat#3003:
// getWorkflowDefFn below ignores BOTH of its arguments and answers for any name
// and any version, where a real store is `WHERE name = ? AND version = ?`. That
// is safe for every caller here -- they are name-addressed, or pass the version
// they read off the run -- and it is NOT safe for a new test that asks a
// version-scoped question, which must install its own double that honours the
// version, as TestAnInternalDefinitionIsRefusedAtAVersionItDoesNotHave does.
//
// A double that discards an argument cannot exercise it: every value passes,
// including one no store has. That is how a version-0 lookup that matched
// nothing stayed invisible for two slices.
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
		// The listing honours ExcludeDefNames, because that is the STORE's contract
		// since cleat#3009 -- the handler no longer filters the page it fetched.
		// A double that ignored the field would model a store that leaks, so it is
		// part of the seam this fixture has to get right; the real dialects are
		// covered by engine/workflow_list_query_test.go.
		rows := []engine.WorkflowInstance{{ID: "RUN", DefName: "wfd", DefVersion: 1}}
		ms.listWorkflowsFn = func(_ context.Context, f engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
			var out []engine.WorkflowInstance
			for _, wf := range rows {
				if excluded(wf.DefName, f.ExcludeDefNames) {
					continue
				}
				out = append(out, wf)
			}
			return out, nil
		}
		ms.countWorkflowsFn = func(_ context.Context, f engine.WorkflowFilter) (int, error) {
			n := 0
			for _, wf := range rows {
				if !excluded(wf.DefName, f.ExcludeDefNames) {
					n++
				}
			}
			return n, nil
		}
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
				// A listed route that now answers 404 is a STALE exception, and
				// this is the only place that can be seen: the map is consulted
				// above only when the absent answer is not a 404, so an entry
				// whose route has been fixed would otherwise sit there for ever,
				// granting nothing. That is the shape of cleat#1746 -- a grant
				// covering nothing is invisible -- and it is why the repair is
				// one line here rather than a sweep later.
				if reason, known := routesWithoutANotFound[r.handler]; known {
					t.Errorf("%s is listed in routesWithoutANotFound (%q) but answers 404 for an absent "+
						"target now, so the entry is stale.\n\n"+
						"Remove it: the assertion below then covers this route like every other, and "+
						"leaving it costs the next reader the one signal that says this route is no "+
						"longer an exception.", r.handler, reason)
				}
				if gotCode != absentCode || gotBody != absentBody {
					t.Errorf("%s is distinguishable from a genuine miss:\n  internal: %d %s\n  absent:   %d %s",
						r.handler, gotCode, strings.TrimSpace(gotBody), absentCode, strings.TrimSpace(absentBody))
				}
			})
		}
	}
}

// TestAnInternalDefinitionIsRefusedAtAVersionItDoesNotHave pins the refusal as a
// property of the NAME, and it is the regression test for the shape slice 2b
// shipped with.
//
// A version-scoped answer is only right for the one version it names. Slice 2b's
// guard asked the store for a concrete version -- a 0, which matches no row in
// any dialect -- so it refused nothing at all; a guard that passed the request's
// version instead would refuse only that version and carry on at the next one.
// Either way a caller can find the version at which an internal definition stops
// being refused, and on the two writers "carrying on" means WRITING a routing
// rule or a tag for a definition that is supposed to be unreachable.
//
// THE FIXTURE HAS TO HONOUR THE VERSION ARGUMENT, and that is the whole reason
// this test exists rather than being covered above. storeServing answers for any
// version, which is precisely how the defect stayed invisible: a store double
// that ignores the argument supplies the row the real store never returns. Here
// it answers only for the version it holds, as `WHERE name = ? AND version = ?`
// does on every dialect.
func TestAnInternalDefinitionIsRefusedAtAVersionItDoesNotHave(t *testing.T) {
	// 99 is the version the definition below does not have, so a version-scoped
	// check of any kind finds nothing there.
	serving := func(class engine.ExposureClass) *mockStore {
		ms := storeServing(class, true)
		ms.getWorkflowDefFn = func(_ context.Context, _ string, version int) (*engine.WorkflowDef, error) {
			if version != 1 {
				return nil, nil
			}
			return &engine.WorkflowDef{
				Name: "wfd", Version: 1, ABIVersion: 1, MinVersion: 1,
				WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, Exposure: class,
			}, nil
		}
		return ms
	}

	for _, c := range []struct {
		handler, body string
	}{
		{"handleSetRoutingRule", `{"target_version":99,"weight":1}`},
		{"handleSetWorkflowTag", `{"tag":"t","version":99}`},
	} {
		c := c
		t.Run(c.handler, func(t *testing.T) {
			var route externalRoute
			for _, r := range everyWorkflowRoute {
				if r.handler == c.handler {
					route = r
				}
			}
			if route.handler == "" {
				t.Fatalf("%s is not in everyWorkflowRoute, so this test is driving nothing", c.handler)
			}
			entry := entryFor(t, everyWorkflowRoute)

			absentCode, absentBody := serve(t, entry, newTestAPIServer(storeServing("", false)),
				route.method, route.path, c.body)
			gotCode, gotBody := serve(t, entry, newTestAPIServer(serving(engine.ExposureInternal)),
				route.method, route.path, c.body)

			if gotCode != http.StatusNotFound || gotBody != absentBody {
				t.Errorf("%s at version 99 -- a version the definition does not have -- answered %d %s.\n"+
					"An absent target answers %d %s, so this is distinguishable from a genuine miss, and the "+
					"caller can find the version at which an internal definition stops being refused.\n"+
					"The refusal is a property of the NAME; every version of an internal definition is 404.",
					c.handler, gotCode, strings.TrimSpace(gotBody),
					absentCode, strings.TrimSpace(absentBody))
			}
		})
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
