package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

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

	wf, err := st.GetWorkflowByID(r.Context(), id)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return nil, false
	}
	if wf == nil || wf.TenantID != caller.String() {
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
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, s.maxBodySize)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeError(w, 400, "invalid JSON: "+err.Error())
			return
		}
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
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, s.maxBodySize)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeError(w, 400, "invalid JSON: "+err.Error())
			return
		}
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
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, s.maxBodySize)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeError(w, 400, "invalid JSON: "+err.Error())
			return
		}
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
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, s.maxBodySize)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeError(w, 400, "invalid JSON: "+err.Error())
			return
		}
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
// 501 is separated from 500 deliberately. Every one of these operations was a
// stub returning "not implemented yet", and the caller was told 500 -- the
// same answer as a database failure, for an operation that had never existed.
//
// All three are real now: re-replay was the last stub, and its body landed
// with IMPROVEMENT-PLAN 3.20. No store in this repo returns
// ErrAdminOpNotImplemented any more, so this branch is unreachable from the
// bundled dialects -- it is kept because WorkflowStore is a public interface
// and an out-of-tree store may implement some of it and not the rest. See that
// error's doc comment.
func (s *apiServer) handleAdminOpError(w http.ResponseWriter, err error) {
	msg := err.Error()
	switch {
	case errors.Is(err, engine.ErrAdminOpNotImplemented):
		s.writeError(w, 501, msg)
	case strings.Contains(msg, "generation mismatch"):
		s.writeJSON(w, 409, map[string]string{"error": msg, "detail": "generation_mismatch"})
	case strings.Contains(msg, "not found"):
		s.writeError(w, 404, msg)
	case strings.Contains(msg, "must be valid JSON"), strings.Contains(msg, "is required"),
		strings.Contains(msg, "must be >= 0"):
		s.writeError(w, 400, msg)
	default:
		s.writeError(w, 500, msg)
	}
}
