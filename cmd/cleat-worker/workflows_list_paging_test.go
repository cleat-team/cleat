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
