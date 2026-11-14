package main

import (
	"context"
	"log/slog"
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
//     MISS -- when it has an answer. When it cannot READ the class it has none,
//     and says so with a 503 instead (cleat#3785; see definitionIsInternal).
//     The owner decision on #1986 is 404 rather than 403 so that an
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
// A run that does not exist (nil, no error) is NOT this function's refusal: it returns false and lets the
// handler produce its own miss, so the two cases cannot drift apart.
//
// A run that cannot be READ is refused with a retryable 503, as refuseIfAbsentOrInternalRun does and as an
// unreadable definition class already is (cleat#3785): a check that cannot find out must not serve. This used
// to return false for an error too, on the reasoning that the handler reads the run again and produces its own
// miss. That holds for a handler that does. Signal and cancel do not, and with no tenant in the request context
// (callerOwnsTarget then skips its own read of the run) the guard's read was the only one, so an unreadable run
// was signalled or cancelled whatever its definition's class (cleat#3799). 404 would be the wrong answer for
// the error, for the reason refuseIfAbsentOrInternalRun says.
func (s *apiServer) refuseIfInternalRun(w http.ResponseWriter, r *http.Request, st engine.WorkflowStore, runID, notFound string) bool {
	wf, err := st.GetWorkflowByID(r.Context(), runID)
	if err != nil {
		s.writeAccessCheckUnavailable(w, r, "the run", err)
		return true
	}
	if wf == nil {
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
	internal, err := s.definitionIsInternal(r.Context(), st, name, version)
	if err != nil {
		s.writeAccessCheckUnavailable(w, r, "the definition's exposure class", err)
		return true
	}
	if internal {
		s.writeError(w, http.StatusNotFound, notFound)
		return true
	}
	return false
}

// definitionIsInternal is the single predicate all of the above reduce to. It returns an error when the
// class could not be READ, and that is a different value from "internal" on purpose.
//
// IT FAILS CLOSED on a read error, and that is the security property: a check that cannot determine the
// class must not serve the request. On the run routes the handler can carry on without the definition, so a
// transient def-read error is otherwise the one path on which an `internal` run is served. The CALLER
// refuses on the error (refuseIfInternalDef) and nothing here decides what to do with it.
//
// WHAT CHANGED, AND WHY (cleat#3785, decided by the coordinator, 2026-11-13). This used to return a bare
// bool with `true` on any error, so a read error and an internal definition were the same value, and the
// caller answered the route's own 404 for both. That cost was written down here as accepted: "a read error
// now answers the route's own not-found rather than a 500". It turned out to be the wrong trade. A 404
// tells a client the workflow does not exist and not to retry, so a database blip on a present, enabled
// definition read as "workflow definition not found" (observed once, under load, starting a 19 MB Python
// module), and the error was logged nowhere, so an operator could not tell a blip from a miss. The
// security property was never in the status. Only the answer changed: a read error is a 503, which tells a
// client to retry, with a log line; 404 is kept for "does not exist" and "internal", which are still the
// same bytes (see the header, property 1).
//
// The class is read by the narrow read where the store has one (engine.DefExposureReader), because the
// whole-row read this used to make selects `wasm_bytes` -- 19 MB for a Python module, and for a library
// member a second read of its bytes -- to learn one short string, and is the read most likely to time out
// under the load that makes any read fail. A store without it (a test double) is asked the wide way.
//
// def == nil / found == false is NOT an error and must stay "not internal": it means no such definition,
// and the handler produces its own miss for that a moment later.
func (s *apiServer) definitionIsInternal(ctx context.Context, st engine.WorkflowStore, name string, version int) (bool, error) {
	if r, ok := st.(engine.DefExposureReader); ok {
		class, found, err := r.GetWorkflowDefExposure(ctx, name, version)
		if err != nil {
			return false, err
		}
		return found && class.OrDefault() == engine.ExposureInternal, nil
	}
	def, err := st.GetWorkflowDef(ctx, name, version)
	if err != nil {
		return false, err
	}
	return rowIsInternal(def), nil
}

// writeAccessCheckUnavailable answers a request whose access check could not READ what it needed: 503, so
// the client retries, with a Retry-After, and a log line that carries the error. The request is refused --
// this is the fail-closed half, and a caller returns true on it -- but it is not a not-found, because the
// workflow may well exist (cleat#3785).
//
// The driver's error is logged and NOT sent: it can carry hostnames, statement text and role names, and the
// caller cannot act on any of it. The operator can, from the log.
func (s *apiServer) writeAccessCheckUnavailable(w http.ResponseWriter, r *http.Request, what string, err error) {
	slog.WarnContext(r.Context(), "could not read "+what+" to decide whether this request may proceed; refusing it with 503 (fail closed)",
		"method", r.Method, "path", r.URL.Path, "error", err)
	w.Header().Set("Retry-After", "1")
	s.writeError(w, http.StatusServiceUnavailable, "the server could not check access to this workflow just now; retry shortly")
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
// not serve the request. It answers 503 and logs, not the route's not-found,
// for the reason definitionIsInternal gives (cleat#3785): a store blip is not a
// miss, and a client told it was will not retry.
func (s *apiServer) refuseIfAbsentOrInternalDef(w http.ResponseWriter, r *http.Request, st engine.WorkflowStore, name, notFound string) bool {
	defs, err := st.ListWorkflowDefs(r.Context(), name)
	if err != nil {
		s.writeAccessCheckUnavailable(w, r, "the definition's versions", err)
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
	if err != nil {
		// A run that cannot be READ is not a run that does not exist (cleat#3785).
		s.writeAccessCheckUnavailable(w, r, "the run", err)
		return true
	}
	if wf == nil {
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
// Hiding the NAME still discloses the CLASS, and that is deliberate rather than
// an oversight: handleCreateSchedule rejects an empty def_name, so a `""` here
// can only mean "hidden" -- a reader can tell a schedule targets an internal
// definition, just not which. That matches the sibling surfaces, which also say
// something internal exists without naming it (the run list omits the row;
// /metrics drops the series). Do not "fix" it by trying to make the two cases
// indistinguishable; there is nothing to distinguish them with.
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
