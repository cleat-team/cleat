package main

import (
	"context"
	"net/http"
	"sort"

	"github.com/cleat-team/cleat/engine"
)

// cleat#1986 slice 2b: an `internal` workflow definition is not reachable from
// the EXTERNAL HTTP surface, on any route that addresses it or a run of it.
//
// The class is per definition VERSION (it is written at deploy). The run routes
// resolve the pair from the run they read. The routes that address a NAME cannot
// -- two of them carry no version at all -- so they ask the name-level question
// instead, treating a name as internal if ANY of its versions is; see
// refuseIfAbsentOrInternalDef. A guard that passed 0 as "the definition" would
// match no row and refuse nothing, which is what this file did until cleat#3003.
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
// VERSION version is internal, for the routes that address one definition
// VERSION -- `start`, and the run routes, which learn the pair from the run.
//
// version is the CONCRETE version those callers have already resolved. There is
// no "version 0 means the latest" convention here and there never was:
// every store's GetWorkflowDef is `WHERE name = ? AND version = ?`, so a 0
// matches no row and would report "not internal" for a definition that is. The
// version-free question -- "does this NAME exist, and is any version of it
// internal" -- is a different one, answered by refuseIfAbsentOrInternalDef.
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
	return rowIsInternal(def)
}

// rowIsInternal is the single definition of "this row is internal", applied to a
// row the caller already holds.
//
// It exists so that the two callers below cannot disagree about what internal
// MEANS -- one of them wants `nil` to be an ordinary "no such definition" and
// the other wants it refused, but the class comparison is the same question in
// both, and a second copy of it is a copy that drifts.
//
// OrDefault rather than a bare comparison: a row predating the column reads back
// as the empty string on a store that does not normalise it, and "" must mean
// `auth` here for the same reason it means `auth` at the write (cleat#1986
// slice 2a). `nil` is NOT internal -- see the callers for why they differ about
// what to do with it.
func rowIsInternal(def *engine.WorkflowDef) bool {
	return def != nil && def.Exposure.OrDefault() == engine.ExposureInternal
}

// refuseIfAbsentOrInternalDef refuses with notFound when the definition named
// name is INTERNAL, and ALSO when no such definition exists at all (cleat#3003).
//
// # Why the two cases are one line
//
// These routes act on a definition WITHOUT verifying it is there: they answer
// success, or a complaint about a version, for a target that was never deployed.
// So a caller sending a request for an absent name and one for an internal name
// got two different answers, and the difference told them "something is here and
// it is refused" rather than "nothing is here" -- which defeats the reason
// internal answers 404 rather than 403, without defeating reachability.
//
// Answering both from this one call is what makes them indistinguishable BY
// CONSTRUCTION rather than by two behaviours happening to agree today. The
// per-route `notFound` message is the route's own, so the absent answer and the
// internal answer are the same bytes.
//
// # It asks the NAME, and that is forced rather than chosen
//
// This is the version-free question, and it takes a version-free read:
// ListWorkflowDefs(ctx, name) is `WHERE name = ? ORDER BY version DESC`, so an
// empty result means the name was never deployed. GetWorkflowDef cannot answer
// it -- it is `WHERE name = ? AND version = ?`, so the only version a
// name-addressed route could pass is 0, and no store has a version 0 row. A
// guard built on that would report "not internal" for every definition that is,
// and 404 for every definition that is not: it fails OPEN on exactly the case it
// was written for while appearing, to a mock that ignores its version argument,
// to be working.
//
// Two of the four callers -- handleRemoveWorkflowTag and
// handleRemoveRoutingRule -- address a name with no version in the request at
// all, so a version-scoped answer is not available to them even in principle.
// The other two carry a version, and this deliberately does NOT use it: the name
// is answered as internal if ANY of its versions is, which is coarser than the
// per-version truth and matches internalDefinitionNames, the other bulk reader
// of this class. Coarse here is the conservative direction -- it can refuse a
// request for a public version of a name that also has an internal one, and that
// is a refusal, not a disclosure.
//
// # Why this is here and not inside definitionIsInternal
//
// That predicate deliberately keeps a missing row as "not internal", because
// most routes produce their own miss a moment later and widening it would change
// them. Which routes need the miss to happen HERE is a property of the route,
// not of the predicate -- and the eight that do are exactly the ones whose own
// answer for an absent target is not a not-found.
//
// # It fails closed, and that is the deliberate direction
//
// A read error refuses rather than letting the handler proceed, for the reason
// definitionIsInternal documents: a check that cannot determine the class must
// not serve the request. The cost is the one already accepted there -- a store
// blip answers not-found rather than 500 -- and it discloses nothing, because
// not-found is already this family's designed answer for "not yours to see".
func (s *apiServer) refuseIfAbsentOrInternalDef(w http.ResponseWriter, r *http.Request, st engine.WorkflowStore, name, notFound string) bool {
	defs, err := st.ListWorkflowDefs(r.Context(), name)
	if err != nil {
		s.writeError(w, http.StatusNotFound, notFound)
		return true
	}
	// Both arms answer with this route's own notFound, byte for byte, so the
	// absent answer and the internal answer are the same answer.
	if len(defs) == 0 {
		s.writeError(w, http.StatusNotFound, notFound)
		return true
	}
	for i := range defs {
		if rowIsInternal(&defs[i]) {
			s.writeError(w, http.StatusNotFound, notFound)
			return true
		}
	}
	return false
}

// refuseIfAbsentOrInternalRun is the run-addressed twin of the above: it refuses
// with notFound when the run does not exist, and when the definition it belongs
// to is internal (cleat#3003).
//
// One read, reusing the row: the definition lookup then goes through
// refuseIfInternalDef exactly as refuseIfInternalRun does, so the class
// comparison has one home.
func (s *apiServer) refuseIfAbsentOrInternalRun(w http.ResponseWriter, r *http.Request, st engine.WorkflowStore, runID, notFound string) bool {
	wf, err := st.GetWorkflowByID(r.Context(), runID)
	if err != nil || wf == nil {
		s.writeError(w, http.StatusNotFound, notFound)
		return true
	}
	return s.refuseIfInternalDef(w, r, st, wf.DefName, wf.DefVersion, notFound)
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
func (s *apiServer) internalDefinitionNames(ctx context.Context, st engine.WorkflowStore) ([]string, error) {
	defs, err := st.ListWorkflowDefs(ctx, "")
	if err != nil {
		// NOT nil-and-carry-on: an unreadable definition set would silently
		// filter nothing, which discloses every internal run in the list. Fail
		// closed and let the caller refuse the whole page.
		return nil, err
	}
	var internal []string
	for _, d := range defs {
		if d.Exposure.OrDefault() == engine.ExposureInternal {
			internal = append(internal, d.Name)
		}
	}
	// Sorted, so the statement this feeds is deterministic: the exclusion is a
	// list of placeholders, and a map's iteration order would change the SQL text
	// run to run for no benefit.
	sort.Strings(internal)
	return internal, nil
}

// hideInternalScheduleTargets blanks the target's name on every schedule that
// points at an `internal` definition, so GET /api/schedules does not disclose
// the name the 404 on the per-definition routes exists to hide (cleat#3001).
//
// The schedule is KEPT and only its DefName is hidden -- the reverse of the run
// list, which omits internal rows entirely. The difference is the reason: a
// cron-triggered internal workflow is legitimate and its schedule is an
// operational object its owner must still see, so the disclosure is the NAME,
// not the schedule's existence.
//
// The caller resolves the names with internalDefinitionNames, so this surface
// and the run list cannot disagree about which names are internal.
func hideInternalScheduleTargets(internal []string, schedules []engine.Schedule) {
	if len(internal) == 0 {
		return
	}
	hidden := make(map[string]struct{}, len(internal))
	for _, n := range internal {
		hidden[n] = struct{}{}
	}
	for i := range schedules {
		if _, ok := hidden[schedules[i].DefName]; ok {
			schedules[i].DefName = ""
		}
	}
}
