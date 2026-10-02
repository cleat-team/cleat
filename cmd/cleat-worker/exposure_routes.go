package main

import (
	"context"
	"net/http"

	"github.com/cleat-team/cleat/engine"
)

// cleat#1986 slice 2b: an `internal` workflow definition is not reachable from
// the EXTERNAL HTTP surface, on any route that addresses it or a run of it.
//
// The class is per definition VERSION (it is written at deploy), so every
// question here resolves to a (name, version) pair and not to a name.
//
// Two properties this file exists to get right, both of which are easy to get
// wrong in a way no test would notice:
//
//  1. IT ANSWERS **404**, WITH THE SAME BODY THE ROUTE GIVES FOR A GENUINE
//     MISS. The owner decision on #1986 is 404 rather than 403 so that an
//     internal workflow's existence is not disclosed -- but a 404 whose body
//     differs from the route's ordinary miss is exactly that disclosure, moved
//     into the body. So callers pass the route's own not-found string rather
//     than a shared one; there is deliberately no default.
//
//  2. IT RUNS *AFTER* AUTHENTICATION, ALWAYS. Every call site sits below its
//     handler's scopedStore/callerOwnsTarget, never above. An unauthenticated
//     caller therefore gets the 401 it gets today for every class, and cannot
//     use the 404 to probe which definitions exist.
//
// It is deliberately NOT applied to the admin API or the dead-letter routes.
// Those are operator surfaces: an operator must still read an internal run's
// history, see its DLQ entries and cancel it. Exposure governs ingress, not
// administration, and a guard there would strand exactly the runs an operator
// needs to reach. This is a decision, not an omission -- see the note beside
// the /api/workflows switch in app.go.

// refuseIfInternalRun resolves runID to the definition version that created it
// and refuses with notFound when that version is internal. It reports whether
// it refused, so a handler returns immediately on true.
//
// A run or definition that cannot be read is NOT this function's refusal: it
// returns false and lets the handler produce its own miss, so the two cases
// cannot drift apart. Answering 404 here for a read error would be the right
// status for the wrong reason, and would hide a store failure.
func (s *apiServer) refuseIfInternalRun(w http.ResponseWriter, r *http.Request, st engine.WorkflowStore, runID, notFound string) bool {
	wf, err := st.GetWorkflowByID(r.Context(), runID)
	if err != nil || wf == nil {
		return false
	}
	return s.refuseIfInternalDef(w, r, st, wf.DefName, wf.DefVersion, notFound)
}

// refuseIfInternalRunLoaded is refuseIfInternalRun for a handler that has
// already fetched the run. Reusing the row the handler is about to act on is
// what keeps this from costing a second read on every request.
func (s *apiServer) refuseIfInternalRunLoaded(w http.ResponseWriter, r *http.Request, st engine.WorkflowStore, wf *engine.WorkflowInstance, notFound string) bool {
	if wf == nil {
		return false
	}
	return s.refuseIfInternalDef(w, r, st, wf.DefName, wf.DefVersion, notFound)
}

// refuseIfInternalDef refuses with notFound when the definition NAMED name at
// VERSION version is internal. It is for the routes that address a definition
// rather than a run -- `start`, and the routing/tag routes.
//
// version 0 means "the definition" rather than a version of it; resolveDef
// answers the same thing the routes themselves resolve to, so a name-addressed
// route and the run it would create can never disagree.
func (s *apiServer) refuseIfInternalDef(w http.ResponseWriter, r *http.Request, st engine.WorkflowStore, name string, version int, notFound string) bool {
	if !s.definitionIsInternal(r.Context(), st, name, version) {
		return false
	}
	s.writeError(w, http.StatusNotFound, notFound)
	return true
}

// definitionIsInternal is the single predicate all of the above reduce to.
//
// It FAILS CLOSED on a read error. This is an access check, and a check that
// cannot determine the class must not serve the request -- on the run routes
// the handler can carry on without the definition, so a transient def-read
// error is otherwise the one path on which an `internal` run is served.
//
// The cost is real and accepted: a read error now answers the route's own
// not-found rather than a 500, so an operator sees "not found" during a store
// blip instead of a server error. That is the safe direction for an access
// check, and it discloses nothing new -- not-found is already this route's
// designed answer for "not yours to see".
//
// def == nil is NOT an error and must stay false: it means no such definition,
// and the handler produces its own miss for that a moment later.
func (s *apiServer) definitionIsInternal(ctx context.Context, st engine.WorkflowStore, name string, version int) bool {
	def, err := st.GetWorkflowDef(ctx, name, version)
	if err != nil {
		return true
	}
	if def == nil {
		return false
	}
	// OrDefault rather than a bare comparison: a row predating the column
	// reads back as the empty string on a store that does not normalise it,
	// and "" must mean `auth` here for the same reason it means `auth` at the
	// write (cleat#1986 slice 2a).
	return def.Exposure.OrDefault() == engine.ExposureInternal
}

// withoutInternalDefs drops every internal definition from defs, for the routes
// that PUBLISH definitions in bulk rather than addressing one.
//
// This is the same disclosure class as the run list: `/api/openapi.json`
// enumerates every definition and publishes each one's entry-point schemas, so
// an internal definition disclosed there would defeat the 404 on every route
// that addresses it -- and a client generated from that document would carry
// typed calls for it. It reads the class straight off the row it already has,
// so it costs no extra query.
//
// The filter is by definition, not by version: the document is keyed by name,
// so a name with any internal version is omitted, matching defExists.
func withoutInternalDefs(defs []engine.WorkflowDef) []engine.WorkflowDef {
	kept := defs[:0]
	for _, d := range defs {
		if d.Exposure.OrDefault() != engine.ExposureInternal {
			kept = append(kept, d)
		}
	}
	return kept
}

// internalDefinitionNames returns the set of definition names that have an
// internal definition NOW, for the list route, which addresses runs in bulk
// rather than one at a time.
//
// A list cannot answer 404 -- it is not addressing one workflow -- so it must
// OMIT internal runs instead, and omitting them is what keeps the 404 on the
// individual routes from being undone by a list that still shows the run.
//
// This is a set of NAMES, which is coarser than the per-version truth the
// other helpers use: a name with one internal version is treated as internal
// for the whole list. That is the conservative direction, and the alternative
// -- resolving every listed run's definition version -- is a query per row.
func (s *apiServer) internalDefinitionNames(ctx context.Context, st engine.WorkflowStore) (map[string]bool, error) {
	defs, err := st.ListWorkflowDefs(ctx, "")
	if err != nil {
		// NOT nil-and-carry-on: an unreadable definition set would silently
		// filter nothing, which discloses every internal run in the list. Fail
		// closed and let the caller refuse the whole page.
		return nil, err
	}
	internal := make(map[string]bool)
	for _, d := range defs {
		if d.Exposure.OrDefault() == engine.ExposureInternal {
			internal[d.Name] = true
		}
	}
	return internal, nil
}
