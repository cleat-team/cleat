package main

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3775, part 2: GET /api/workflows?state.<key>=<value> on a deployment that seals query_state.
//
// Part 1 answers 422 there, because the database cannot match a key in ciphertext. This answers it in the
// worker, over a WINDOW of the tenant's most recent runs (engine.QueryStateWindowSearcher says why that is
// no new exposure and what the window means), and tells the caller how much it covered in headers, so the
// body stays the bare array every existing caller reads:
//
//	X-Total-Count                     matches INSIDE the window, not in the tenant's history
//	X-Query-State-Window              the window asked for
//	X-Query-State-Scanned             runs actually examined (at most the window)
//	X-Query-State-Window-Exhausted    "true" when the window was used up and OLDER runs were not examined
//	X-Query-State-Unreadable          runs in the window whose published state would not open
//
// "No match" with Exhausted=true means "not among the most recent runs", and nothing about the rest.
// "No match" with Exhausted=false means there is no such run.
const (
	hdrQueryStateWindow     = "X-Query-State-Window"
	hdrQueryStateScanned    = "X-Query-State-Scanned"
	hdrQueryStateExhausted  = "X-Query-State-Window-Exhausted"
	hdrQueryStateUnreadable = "X-Query-State-Unreadable"

	// defaultQueryStateSearchWindow is how many of a tenant's most recent runs a search examines. Measured
	// 2026-11-13 on PostgreSQL 16 over loopback with sealed runs: 5.9 ms at 1,000 runs and 8.7 ms at 2,000,
	// about 4 us per additional run. So 5,000 is on the order of 20 ms; that is an extrapolation past 2,000.
	defaultQueryStateSearchWindow = 5000
	// maxQueryStateSearchWindow bounds the flag, so a typo cannot make one request decrypt a tenant's whole
	// history. On the same measurement it is on the order of 200 ms (extrapolated).
	maxQueryStateSearchWindow = 50000
)

// validateQueryStateSearchWindow checks --query-state-search-window at startup. 0 turns the search off.
func validateQueryStateSearchWindow(n int) error {
	if n < 0 || n > maxQueryStateSearchWindow {
		return fmt.Errorf("--query-state-search-window must be between 0 (off) and %d, got %d", maxQueryStateSearchWindow, n)
	}
	return nil
}

// searchSealedQueryState answers the listing from a window search. It reports whether it handled the
// request: false means this store is not one that needs it (the column is plaintext, or the store cannot),
// and the ordinary path, with its SQL filter or its 422, applies.
func (s *apiServer) searchSealedQueryState(w http.ResponseWriter, r *http.Request, st engine.WorkflowStore, filter engine.WorkflowFilter) bool {
	ws, ok := st.(engine.QueryStateWindowSearcher)
	if !ok || !ws.SealsQueryState() {
		return false
	}
	if s.queryStateSearchWindow <= 0 {
		// Possible here but switched off: say how to switch it on. (A store that cannot do it at all never
		// reaches this branch, and its refusal does not offer a flag that would not help.)
		s.writeError(w, http.StatusUnprocessableEntity, engine.ErrQueryStateFilterUnavailableUnderEncryption.Error()+
			"; start the worker with --query-state-search-window=<N> to search the N most recent runs instead")
		return true
	}

	res, err := ws.SearchQueryStateWindow(r.Context(), filter, s.queryStateSearchWindow)
	switch {
	case errors.Is(err, engine.ErrSearchUnavailableUnderEncryption):
		s.writeError(w, http.StatusUnprocessableEntity, err.Error())
		return true
	case err != nil:
		s.writeListWorkflowsError(w, err)
		return true
	}

	h := w.Header()
	h.Set("X-Total-Count", strconv.Itoa(res.Matched))
	h.Set(hdrQueryStateWindow, strconv.Itoa(res.Window))
	h.Set(hdrQueryStateScanned, strconv.Itoa(res.Scanned))
	h.Set(hdrQueryStateExhausted, strconv.FormatBool(res.Exhausted))
	h.Set(hdrQueryStateUnreadable, strconv.Itoa(res.Unreadable))
	runs := res.Runs
	if runs == nil {
		runs = []engine.WorkflowInstance{}
	}
	s.writeWorkflows(w, r, runs)
	return true
}
