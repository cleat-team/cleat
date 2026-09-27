package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// The filter the handler builds is recorded, because it is the only thing that
// can show a query parameter was READ. Asserting on the response body would
// pass for a handler that ignored the parameter entirely -- the mock returns
// the same rows either way, so the check could not disagree.
type listSpy struct {
	store *mockStore
	got   *engine.WorkflowFilter
}

func newListSpy(rows []engine.WorkflowInstance, total int) *listSpy {
	sp := &listSpy{store: &mockStore{}, got: new(engine.WorkflowFilter)}
	sp.store.listWorkflowsFn = func(_ context.Context, f engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
		*sp.got = f
		return rows, nil
	}
	sp.store.countWorkflowsFn = func(_ context.Context, f engine.WorkflowFilter) (int, error) {
		*sp.got = f
		return total, nil
	}
	return sp
}

func get(t *testing.T, sp *listSpy, url string) *http.Response {
	t.Helper()
	api := newTestAPIServer(sp.store)
	w := httptest.NewRecorder()
	api.handleWorkflowsList(w, httptest.NewRequest(http.MethodGet, url, nil))
	return w.Result()
}

// TestTheListingReportsHowManyRowsItIsPagingOver.
//
// Before this the handler hard-coded Limit: 100, never read offset, and
// returned a bare array -- so a tenant with 500 workflows saw 100 and nothing
// in the response distinguished that from having exactly 100 (cleat#1182).
func TestTheListingReportsHowManyRowsItIsPagingOver(t *testing.T) {
	sp := newListSpy([]engine.WorkflowInstance{{ID: "wf-1"}}, 512)
	resp := get(t, sp, "/api/workflows")

	if got := resp.Header.Get("X-Total-Count"); got != "512" {
		t.Errorf("X-Total-Count = %q, want \"512\". Without it a full page and "+
			"the end of the data are the same response.", got)
	}

	// The body stays a bare array: an envelope would have been a breaking
	// change for every existing caller, and the events endpoint already
	// established the header as this API's answer.
	var body []engine.WorkflowInstance
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("the body is no longer a bare array, which breaks existing callers: %v", err)
	}
}

func TestPagingParametersAreReadRatherThanAssumed(t *testing.T) {
	sp := newListSpy(nil, 0)
	get(t, sp, "/api/workflows?limit=25&offset=75")
	if sp.got.Limit != 25 {
		t.Errorf("limit = %d, want 25 -- the handler ignored the parameter", sp.got.Limit)
	}
	if sp.got.Offset != 75 {
		t.Errorf("offset = %d, want 75 -- the handler ignored the parameter", sp.got.Offset)
	}
}

func TestTheServerCeilingOutranksTheCaller(t *testing.T) {
	sp := newListSpy(nil, 0)
	get(t, sp, "/api/workflows?limit=99999")
	if sp.got.Limit != 1000 {
		t.Errorf("limit = %d, want the 1000 ceiling. A caller must not be able "+
			"to raise it.", sp.got.Limit)
	}
}

func TestTheTargetedFiltersReachTheStore(t *testing.T) {
	sp := newListSpy(nil, 0)
	get(t, sp, "/api/workflows?def_name=nightly&error_code=cancelled&id_prefix=0f74")
	if sp.got.DefName != "nightly" {
		t.Errorf("def_name = %q, want \"nightly\"", sp.got.DefName)
	}
	if sp.got.ErrorCode != "cancelled" {
		t.Errorf("error_code = %q, want \"cancelled\"", sp.got.ErrorCode)
	}
	if sp.got.IDPrefix != "0f74" {
		t.Errorf("id_prefix = %q, want \"0f74\"", sp.got.IDPrefix)
	}
}

// TestEveryListFilterReachesTheStore covers the four the test above does not
// (cleat#1248). Measured by sabotage: replacing each read in handleListWorkflows
// with the empty string and running the whole package, `status`,
// `input_contains`, `error_contains` and `search` were noticed by nothing, while
// `def_name`, `error_code` and `id_prefix` were caught by the test above. All
// four are live -- applyWorkflowFilters turns them into `status = %s` and three
// LIKE conditions -- so they work today and nothing would report the day they
// stop.
//
// The direction of the failure is why this matters. A dropped filter does not
// error, it WIDENS the result: a caller asking for status=failed is handed every
// run and a UI renders it without complaint. TestATimeWindowIsParsedAndABadOneIsRefused
// already makes this argument for the time bounds -- "the failure would be extra
// data, which is the direction nobody checks" -- and it covers these equally.
//
// Asserting every field in one request also keeps the guard honest as filters
// are added: a new one is unprotected until it appears here, and this test is
// where a reader looks to find out which are covered.
func TestEveryListFilterReachesTheStore(t *testing.T) {
	sp := newListSpy(nil, 0)
	get(t, sp, "/api/workflows?"+
		"status=failed&input_contains=order-42&error_contains=timed+out&search=nightly-rollup&"+
		"def_name=nightly&error_code=cancelled&id_prefix=0f74")

	for _, c := range []struct {
		param string
		got   string
		want  string
	}{
		{"status", sp.got.Status, "failed"},
		{"input_contains", sp.got.InputContains, "order-42"},
		{"error_contains", sp.got.ErrorContains, "timed out"},
		{"search", sp.got.Search, "nightly-rollup"},
		{"def_name", sp.got.DefName, "nightly"},
		{"error_code", sp.got.ErrorCode, "cancelled"},
		{"id_prefix", sp.got.IDPrefix, "0f74"},
	} {
		if c.got != c.want {
			t.Errorf("%s reached the store as %q, want %q -- an ignored filter "+
				"returns MORE rows than the caller asked for, silently", c.param, c.got, c.want)
		}
	}
}

func TestATimeWindowIsParsedAndABadOneIsRefused(t *testing.T) {
	sp := newListSpy(nil, 0)
	get(t, sp, "/api/workflows?started_after=2026-09-10T14:00:00Z")
	want := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)
	if !sp.got.StartedAfter.Equal(want) {
		t.Errorf("started_after = %v, want %v", sp.got.StartedAfter, want)
	}

	// Refused, not ignored. Dropping an unparseable bound would WIDEN the
	// window silently, returning rows the caller asked not to see -- the
	// failure would be extra data, which is the direction nobody checks.
	resp := get(t, newListSpy(nil, 0), "/api/workflows?started_before=yesterday")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a malformed started_before returned %d, want 400", resp.StatusCode)
	}
}
