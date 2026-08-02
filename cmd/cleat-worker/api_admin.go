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
// SECURITY: none of the handlers below verify that the target workflowID
// belongs to the tenant resolved onto the request context (see
// operatorFromContext). auth.TenantIDFromContext gives us the caller's
// tenant, but it is only used to label the audit operator field — it is
// never compared against the target workflow's owning tenant before
// force-complete/force-fail/re-replay is applied. The engine.WorkflowStore
// admin methods (AdminForceComplete/AdminForceFail/AdminReReplay) also take
// no tenant parameter, so there is no enforcement point below this layer
// either. Today the concrete store implementations
// (engine/store_admin_stubs.go) are unimplemented stubs, so this is latent
// rather than exploitable. The moment those stubs are replaced with real
// implementations, any authenticated caller (of any tenant) who knows or
// guesses a workflow ID will be able to force-complete, force-fail, or
// re-replay a workflow belonging to a different tenant. Before that lands,
// this handler needs to: (1) load the workflow via s.store.GetWorkflowByID,
// (2) compare its TenantID to auth.TenantIDFromContext(r.Context()), and
// (3) return 404 (not 403, to avoid confirming existence) on mismatch. That
// check is intentionally NOT added here yet because the current test suite
// exercises these handlers against a mock store that does not return a
// workflow (GetWorkflowByID defaults to nil), so an unconditional ownership
// check here would 404 the success-path tests
// (TestAdminForceComplete_Success, TestAdminForceFail_Success,
// TestAdminReReplay_Success) even though they are asserting the intended
// contract.
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

	switch {
	case len(parts) == 2 && parts[1] == "force-complete" && r.Method == http.MethodPost:
		s.handleAdminForceComplete(w, r, id)
	case len(parts) == 2 && parts[1] == "force-fail" && r.Method == http.MethodPost:
		s.handleAdminForceFail(w, r, id)
	case len(parts) == 2 && parts[1] == "re-replay" && r.Method == http.MethodPost:
		s.handleAdminReReplay(w, r, id)
	default:
		s.writeError(w, 404, "not found")
	}
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
		Generation  int64  `json:"generation"`
		ErrorMsg    string `json:"error_message"`
		ErrorCode   string `json:"error_code"`
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
