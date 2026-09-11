package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// The dead-letter listing is the same defect as cleat#1182, one endpoint over.
// cleat#1166.
//
// workflows_list_paging_test.go's own comment describes what /api/workflows did
// before #1182 fixed it: "the handler hard-coded Limit: 100, never read offset,
// and returned a bare array -- so a tenant with 500 workflows saw 100 and
// nothing in the response distinguished that from having exactly 100". That
// sentence described handleDeadLettersList verbatim until this change.
//
// It matters more here than there, and the issue is right about why: the
// retention sweep deliberately never deletes a dead-lettered run, so this is
// the ONE terminal status whose population only grows. An endpoint whose cost
// rises monotonically with the lifetime of the deployment, and no supported way
// to look at part of it.
//
// WHAT THE ISSUE'S MEASUREMENT COULD NOT SEE, because it had 40 rows: the cap
// is 100, so the endpoint does not "return everything" -- past 100 it returns a
// prefix and says nothing. A bare array of exactly 100 is indistinguishable
// from a store holding exactly 100, which is #1182's sentence again.
//
// The spy records the FILTER rather than asserting on the body, and that is
// load bearing: the mock returns the same rows whatever it is handed, so a body
// assertion would pass against a handler that ignored the parameter entirely.
// It could not disagree.
func TestTheDeadLetterListingPagesAndReportsItsTotal(t *testing.T) {
	rows := []engine.WorkflowInstance{{ID: "dl-1", Status: "dead_lettered"}}

	t.Run("it reports how many rows it is paging over", func(t *testing.T) {
		sp := newListSpy(rows, 512)
		resp := getDeadLetters(t, sp, "/api/dead-letters")

		if got := resp.Header.Get("X-Total-Count"); got != "512" {
			t.Errorf("X-Total-Count = %q, want \"512\".\n\n"+
				"Without it a full page and the end of the data are the same response -- "+
				"and this is the one status retention never deletes, so the population "+
				"only grows.", got)
		}
	})

	t.Run("limit and offset are read rather than assumed", func(t *testing.T) {
		sp := newListSpy(rows, 512)
		getDeadLetters(t, sp, "/api/dead-letters?limit=25&offset=75")

		if sp.got.Limit != 25 {
			t.Errorf("filter.Limit = %d, want 25 -- the handler hard-coded 100 and never "+
				"read the parameter", sp.got.Limit)
		}
		if sp.got.Offset != 75 {
			t.Errorf("filter.Offset = %d, want 75 -- offset was never read at all, so every "+
				"request returned the same first page", sp.got.Offset)
		}
	})

	t.Run("it still asks only for dead-lettered runs", func(t *testing.T) {
		// The assertion that keeps the others honest. A handler that dropped
		// the status filter while gaining paging would satisfy every check
		// above and turn this endpoint into a listing of every workflow.
		sp := newListSpy(rows, 512)
		getDeadLetters(t, sp, "/api/dead-letters?limit=25")

		if sp.got.Status != "dead_lettered" {
			t.Errorf("filter.Status = %q, want \"dead_lettered\" -- paging must not cost "+
				"the endpoint its subject", sp.got.Status)
		}
	})

	t.Run("the body stays a bare array", func(t *testing.T) {
		sp := newListSpy(rows, 512)
		resp := getDeadLetters(t, sp, "/api/dead-letters")

		var body []engine.WorkflowInstance
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("the body is no longer a bare array, which breaks every existing "+
				"caller: %v\n\nThe total goes in a header for exactly this reason -- "+
				"handleGetInstanceEvents established the shape and #1182 followed it.", err)
		}
	})

	t.Run("an empty page is [] and not null", func(t *testing.T) {
		// nil and empty are not interchangeable at this boundary: one marshals
		// to `null` and the other to `[]`, and every store in engine returns a
		// nil slice for an empty listing. A caller iterating the response
		// should not have to special-case the far end of the pagination.
		sp := newListSpy(nil, 512)
		resp := getDeadLetters(t, sp, "/api/dead-letters?offset=100000")

		var raw json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if string(raw) != "[]" {
			t.Errorf("an empty page returned %s, want []", raw)
		}
	})
}

func getDeadLetters(t *testing.T, sp *listSpy, url string) *http.Response {
	t.Helper()
	api := newTestAPIServer(sp.store)
	w := httptest.NewRecorder()
	api.handleDeadLettersList(w, httptest.NewRequest(http.MethodGet, url, nil))
	return w.Result()
}
