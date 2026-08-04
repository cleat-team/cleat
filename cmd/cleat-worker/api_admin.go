package main

import (
	"encoding/json"
	"net/http"
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

	var action func(http.ResponseWriter, *http.Request, string)
	switch {
	case len(parts) == 2 && parts[1] == "force-complete" && r.Method == http.MethodPost:
		action = s.handleAdminForceComplete
	case len(parts) == 2 && parts[1] == "force-fail" && r.Method == http.MethodPost:
		action = s.handleAdminForceFail
	case len(parts) == 2 && parts[1] == "re-replay" && r.Method == http.MethodPost:
		action = s.handleAdminReReplay
	default:
		s.writeError(w, 404, "not found")
		return
	}

	// Ownership is checked once, here, rather than in each handler: a handler
	// added later would otherwise inherit the gap by omission.
	if !s.callerOwnsTarget(w, r, id) {
		return
	}
	action(w, r, id)
}

// callerOwnsTarget reports whether the caller's tenant owns workflow id, and
// writes the response itself when the answer is no.
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
func (s *apiServer) callerOwnsTarget(w http.ResponseWriter, r *http.Request, id string) bool {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return false
	}
	caller, ok := auth.TenantIDFromContext(r.Context())
	if !ok {
		// No tenant on the request means authentication is disabled
		// (--require-auth=false), so there are no tenants to keep apart and
		// nothing to compare against. The operator has chosen to trust the
		// network; this check cannot substitute for that decision.
		return true
	}

	wf, err := st.GetWorkflowByID(r.Context(), id)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return false
	}
	if wf == nil || wf.TenantID != caller.String() {
		// Same response for "does not exist" and "belongs to someone else",
		// deliberately: distinguishing them turns this endpoint into an oracle
		// for which workflow IDs are real.
		s.writeError(w, 404, "not found")
		return false
	}
	return true
}

// operatorFromContext extracts the operator identity from the request context.
func operatorFromContext(r *http.Request) string {
	if tid, ok := auth.TenantIDFromContext(r.Context()); ok {
		return tid.String()
	}
	return "unknown"
}

func (s *apiServer) handleAdminForceComplete(w http.ResponseWriter, r *http.Request, id string) {
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
	if err := engine.ForceComplete(r.Context(), s.store, id, req.Generation, op, req.Result); err != nil {
		s.handleAdminOpError(w, err)
		return
	}

	s.writeJSON(w, 200, map[string]string{"status": "completed"})
}

func (s *apiServer) handleAdminForceFail(w http.ResponseWriter, r *http.Request, id string) {
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
	if err := engine.ForceFail(r.Context(), s.store, id, req.Generation, op, req.ErrorMsg, req.ErrorCode); err != nil {
		s.handleAdminOpError(w, err)
		return
	}

	s.writeJSON(w, 200, map[string]string{"status": "failed"})
}

func (s *apiServer) handleAdminReReplay(w http.ResponseWriter, r *http.Request, id string) {
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
	if err := engine.ReReplay(r.Context(), s.store, id, req.Generation, op); err != nil {
		s.handleAdminOpError(w, err)
		return
	}

	s.writeJSON(w, 200, map[string]string{"status": "queued_for_replay"})
}

// handleAdminOpError maps engine admin operation errors to HTTP status codes.
func (s *apiServer) handleAdminOpError(w http.ResponseWriter, err error) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "generation mismatch"):
		s.writeJSON(w, 409, map[string]string{"error": msg, "detail": "generation_mismatch"})
	case strings.Contains(msg, "not found"):
		s.writeError(w, 404, msg)
	default:
		s.writeError(w, 500, msg)
	}
}
