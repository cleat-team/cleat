package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
)

// handleAdminRoutes routes /api/admin/instances/* requests.
//
// Every route below is gated on callerOwnsTarget: the admin operations are
// destructive and take no tenant parameter, so this layer is the only
// enforcement point there is.
func (s *apiServer) handleAdminRoutes(w http.ResponseWriter, r *http.Request) {
	if !*enableAdminAPI {
		s.writeError(w, 404, "not found")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/admin/instances/")
	if path == "" || path == "/" {
		s.writeError(w, 400, "bad request")
		return
	}

	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
	if len(parts) < 1 {
		s.writeError(w, 400, "bad request")
		return
	}

	id := parts[0]

	var action func(http.ResponseWriter, *http.Request, string, engine.WorkflowStore)
	switch {
	case len(parts) == 2 && parts[1] == "force-complete" && r.Method == http.MethodPost:
		action = s.handleAdminForceComplete
	case len(parts) == 2 && parts[1] == "force-fail" && r.Method == http.MethodPost:
		action = s.handleAdminForceFail
	case len(parts) == 2 && parts[1] == "re-replay" && r.Method == http.MethodPost:
		action = s.handleAdminReReplay
	case len(parts) == 4 && parts[1] == "steps" && parts[3] == "resolve" && r.Method == http.MethodPost:
		// The step travels in the path, and the action signature carries only
		// the workflow ID, so it is bound here rather than re-parsed in the
		// handler. Parsed before the ownership check below runs, so a
		// malformed step is a 400 rather than a 404 that implies the workflow
		// does not exist.
		step, err := strconv.Atoi(parts[2])
		if err != nil || step < 0 {
			s.writeError(w, 400, "step must be a non-negative integer")
			return
		}
		action = func(w http.ResponseWriter, r *http.Request, id string, st engine.WorkflowStore) {
			s.handleAdminResolveStep(w, r, id, step, st)
		}
	default:
		s.writeError(w, 404, "not found")
		return
	}

	// Ownership is checked once, here, rather than in each handler: a handler
	// added later would otherwise inherit the gap by omission.
	//
	// The store it checked against is handed to the handler rather than
	// discarded. The handlers used to check ownership with the caller's
	// tenant-scoped store and then apply the operation to s.store, the
	// process-wide one -- so a force-resolve ran against the default tenant's
	// scope no matter who asked. That was invisible while the store methods
	// were stubs: they returned "not implemented yet" from whichever store
	// reached them.
	st, ok := s.callerOwnsTarget(w, r, id)
	if !ok {
		return
	}
	action(w, r, id, st)
}

// callerOwnsTarget reports whether the caller's tenant owns workflow id, and
// writes the response itself when the answer is no. On success it returns the
// tenant-scoped store the check was made against, which is the store the
// operation must then be applied to.
//
// This is the enforcement point for the admin API. The engine.WorkflowStore
// admin methods (AdminForceComplete / AdminForceFail / AdminReReplay) take no
// tenant parameter, so nothing below this layer can distinguish one tenant's
// workflow from another's. Without this check, any authenticated caller who
// knows or guesses a workflow ID could force-complete, force-fail or re-replay
// a workflow belonging to a different tenant.
//
// It answers 404, never 403: 403 would confirm that the workflow exists, which
// is itself information the caller is not entitled to.
func (s *apiServer) callerOwnsTarget(w http.ResponseWriter, r *http.Request, id string) (engine.WorkflowStore, bool) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return nil, false
	}
	caller, ok := auth.TenantIDFromContext(r.Context())
	if !ok {
		// No tenant on the request means authentication is disabled
		// (--require-auth=false), so there are no tenants to keep apart and
		// nothing to compare against. The operator has chosen to trust the
		// network; this check cannot substitute for that decision.
		return st, true
	}

	// This check was inert on two of three dialects until 3.99: PostgreSQL and
	// SQL Server's GetWorkflowByID did not SELECT tenant_id at all, so
	// wf.TenantID was "" and the comparison below was `"" != "<caller uuid>"`
	// -- true for every request, including the caller's own. Every
	// /api/admin/instances/* route answered 404 whenever --require-auth was on.
	// It failed CLOSED, which is why nothing noticed: the gate was doing its
	// job for attackers and for everybody else equally.
	wf, err := st.GetWorkflowByID(r.Context(), id)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return nil, false
	}
	// EqualFold, not ==. A UUID is case-insensitive by definition, and SQL
	// Server hands one back as whatever CONVERT produced -- uppercase, until
	// GetWorkflowByID started wrapping it in LOWER(). Normalising at the source
	// is the real fix; this is here so that a dialect which stops doing so
	// fails visibly in a test rather than by 404ing every request forever.
	// IMPROVEMENT-PLAN 3.99.
	if wf == nil || !strings.EqualFold(wf.TenantID, caller.String()) {
		// Same response for "does not exist" and "belongs to someone else",
		// deliberately: distinguishing them turns this endpoint into an oracle
		// for which workflow IDs are real.
		s.writeError(w, 404, "not found")
		return nil, false
	}
	return st, true
}

// operatorFromContext extracts the operator identity from the request context.
func operatorFromContext(r *http.Request) string {
	if tid, ok := auth.TenantIDFromContext(r.Context()); ok {
		return tid.String()
	}
	return "unknown"
}

func (s *apiServer) handleAdminForceComplete(w http.ResponseWriter, r *http.Request, id string, st engine.WorkflowStore) {
	if r.Header.Get("X-Confirm") != "force-complete" {
		s.writeError(w, 400, "X-Confirm header must be 'force-complete'")
		return
	}

	var req struct {
		Generation int64  `json:"generation"`
		Result     string `json:"result"`
	}
	if !s.decodeJSONBody(w, r, s.configuredBodyLimit(), &req) {
		return
	}

	op := operatorFromContext(r)
	if err := engine.ForceComplete(r.Context(), st, id, req.Generation, op, req.Result); err != nil {
		s.handleAdminOpError(w, err)
		return
	}

	s.writeJSON(w, 200, map[string]string{"status": "completed"})
}

func (s *apiServer) handleAdminForceFail(w http.ResponseWriter, r *http.Request, id string, st engine.WorkflowStore) {
	if r.Header.Get("X-Confirm") != "force-fail" {
		s.writeError(w, 400, "X-Confirm header must be 'force-fail'")
		return
	}

	var req struct {
		Generation int64  `json:"generation"`
		ErrorMsg   string `json:"error_message"`
		ErrorCode  string `json:"error_code"`
	}
	if !s.decodeJSONBody(w, r, s.configuredBodyLimit(), &req) {
		return
	}

	op := operatorFromContext(r)
	if err := engine.ForceFail(r.Context(), st, id, req.Generation, op, req.ErrorMsg, req.ErrorCode); err != nil {
		s.handleAdminOpError(w, err)
		return
	}

	s.writeJSON(w, 200, map[string]string{"status": "failed"})
}

// handleAdminResolveStep records an outcome for a call left ambiguous by a
// crash -- IMPROVEMENT-PLAN 1.4 phase F.
//
// The X-Confirm header matches force-complete and force-fail, and for a
// stronger reason than symmetry: this writes an outcome that replay will treat
// as the call's real result for the life of the workflow. An operator who has
// not checked the external service can silently convert "we do not know" into
// "it succeeded", which is the one thing the [AMBIGUOUS] state exists to
// prevent.
func (s *apiServer) handleAdminResolveStep(w http.ResponseWriter, r *http.Request, id string, step int, st engine.WorkflowStore) {
	if r.Header.Get("X-Confirm") != "resolve-step" {
		s.writeError(w, 400, "X-Confirm header must be 'resolve-step'")
		return
	}

	var req struct {
		Response string `json:"response"`
	}
	if !s.decodeJSONBody(w, r, s.configuredBodyLimit(), &req) {
		return
	}

	op := operatorFromContext(r)
	if err := engine.ResolveStep(r.Context(), st, id, step, req.Response, op); err != nil {
		s.handleAdminOpError(w, err)
		return
	}

	s.writeJSON(w, 200, map[string]any{"status": "resolved", "step": step, "resolved_by": op})
}

func (s *apiServer) handleAdminReReplay(w http.ResponseWriter, r *http.Request, id string, st engine.WorkflowStore) {
	if r.Header.Get("X-Confirm") != "re-replay" {
		s.writeError(w, 400, "X-Confirm header must be 're-replay'")
		return
	}

	var req struct {
		Generation int64 `json:"generation"`
	}
	if !s.decodeJSONBody(w, r, s.configuredBodyLimit(), &req) {
		return
	}

	op := operatorFromContext(r)
	if err := engine.ReReplay(r.Context(), st, id, req.Generation, op); err != nil {
		s.handleAdminOpError(w, err)
		return
	}

	s.writeJSON(w, 200, map[string]string{"status": "queued_for_replay"})
}

// handleAdminOpError maps engine admin operation errors to HTTP status codes.
//
// By CLASS, not by substring. It used to switch on `strings.Contains(msg,
// "not found")` and `strings.Contains(msg, "generation mismatch")`, which made
// an operator-facing sentence part of the API contract -- store_admin.go said
// so: "the wording is load-bearing". Two things followed from that:
//
//   - Every refusal whose wording matched no pattern was a 500. Re-replaying a
//     `done` workflow and re-replaying one with an unresolved ambiguous call
//     are decisions the server makes on purpose, and both were reported as the
//     server having broken. Measured 2026-09-06 against a live worker:
//     POST .../re-replay on a done workflow returned 500 with the correct
//     explanation in the body.
//   - Any error whose text merely contained "not found" was claimed as a 404.
//     A driver reporting a missing relation is a server fault, and it answered
//     as though the workflow did not exist.
//
// 501 is separated from 500 deliberately. Every one of these operations was
// once a stub returning "not implemented yet", and the caller was told 500 --
// the same answer as a database failure, for an operation that had never
// existed. All three are real now, so no store in this repo returns
// ErrAdminOpNotImplemented; the branch is kept because WorkflowStore is a
// public interface and an out-of-tree store may implement some of it and not
// the rest.
//
// The default is still 500, and that is the point of classifying: an
// unclassified error is a genuine server fault, not a refusal nobody got round
// to labelling.
func (s *apiServer) handleAdminOpError(w http.ResponseWriter, err error) {
	msg := err.Error()
	switch {
	case errors.Is(err, engine.ErrAdminOpNotImplemented):
		s.writeError(w, 501, msg)
	case errors.Is(err, engine.ErrAdminGenerationMismatch):
		// detail is a stable machine-readable discriminator, so a client can
		// tell "you raced another writer" from the other 409 without parsing
		// the message. The state conflict below carries its own.
		s.writeJSON(w, 409, map[string]string{"error": msg, "detail": "generation_mismatch"})
	case errors.Is(err, engine.ErrAdminStateConflict):
		s.writeJSON(w, 409, map[string]string{"error": msg, "detail": "state_conflict"})
	case errors.Is(err, engine.ErrAdminNotFound):
		s.writeError(w, 404, msg)
	case errors.Is(err, engine.ErrAdminBadRequest):
		s.writeError(w, 400, msg)
	default:
		s.writeError(w, 500, msg)
	}
}

// handleRetentionSweep handles POST /api/admin/retention/sweep.
//
// cleat#1130. Retention was unobservable from outside the engine: the window is
// integer DAYS with 0 meaning disabled, the predicate is `completed_at <
// cutoff`, and nothing on the HTTP surface started a sweep. An out-of-process
// observer could not produce a swept row without waiting a day or ageing
// `completed_at` in the database directly. It also left operators with no way
// to see a configuration change take effect for up to --retention-interval.
//
// THE WINDOW OVERRIDE IS WHAT MAKES THIS MORE THAN A BUTTON. `{"older_than":
// "5s"}` supplies the cutoff the flags cannot express. Without it, a trigger
// running the configured sweep would match nothing for any run completed today,
// on every call, and report success -- an endpoint that ships as a working
// feature and is provably inert.
//
// It does NOT enable a disabled arm. A flag at 0 is a decision --
// --completed-workflow-retention-days deletes the workflow record itself and is
// off by default for exactly that reason -- and a request body is not where
// that gets reversed. Disabled arms come back in `skipped`, so a zero count is
// never ambiguous between "disabled" and "found nothing".
//
// Gated on *enableAdminAPI like the other destructive admin routes, so it
// inherits that exposure decision rather than making a new one.
func (s *apiServer) handleRetentionSweep(w http.ResponseWriter, r *http.Request) {
	if !*enableAdminAPI {
		s.writeError(w, 404, "not found")
		return
	}
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "method not allowed")
		return
	}

	var req struct {
		OlderThan string `json:"older_than"`
	}
	if r.Body != nil {
		// An absent or empty body means "use the configured windows", which is
		// the operator-facing case: apply my config change now.
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&req); err != nil && err != io.EOF {
			s.writeError(w, 400, "invalid JSON body")
			return
		}
	}

	var window time.Duration
	if req.OlderThan != "" {
		d, err := time.ParseDuration(req.OlderThan)
		if err != nil {
			s.writeError(w, 400, "older_than must be a Go duration such as \"5s\" or \"48h\": "+err.Error())
			return
		}
		if d <= 0 {
			// A non-positive window would make the cutoff now-or-later and
			// sweep live work. The flags cannot express it and neither can
			// this.
			s.writeError(w, 400, "older_than must be positive")
			return
		}
		window = d
	}

	res := s.worker.runRetentionSweepWindow(
		*retentionDays, *completedWorkflowRetentionDays, *deadLetterRetentionDays, window)

	status := 200
	if len(res.Errors) > 0 {
		// Partial failure is reported as such rather than as success: the arms
		// are independent and one failing does not stop the others, so the
		// counts above it are real.
		status = 207
	}
	s.writeJSON(w, status, res)
}
