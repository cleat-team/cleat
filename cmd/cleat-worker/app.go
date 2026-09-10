package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/cleat-team/cleat/engine"
)

// registerRoutes attaches all API routes to the given mux.
//
// This is the ONLY route table. It used to be one of two: main() registered its
// own set inline and reached this function not at all, so `/api/instances/...`
// and every `/api/admin/instances/...` route -- instance state, event history,
// force-complete, force-fail, re-replay, step resolve -- existed on the table
// the tests drove and on no table the binary served. Seven endpoints, one of
// them documented in CHANGELOG.md as a shipped operator feature, none of them
// reachable since app.go was written (ce48f18). `--enable-admin-api` gated a
// handler nothing routed to.
//
// Nothing reported it because of the SPA fallback below, which answered every
// unmatched path -- `/api/` included -- with 200 and index.html. A JSON client
// asking for a missing endpoint got HTML and a parse error naming nothing,
// rather than a 404 naming the path.
func registerRoutes(mux *http.ServeMux, api *apiServer) *http.ServeMux {
	mux.HandleFunc("/healthz", api.handleHealthz)
	mux.HandleFunc("/metrics", handleMetrics)
	mux.HandleFunc("/api/admin/drain", api.handleDrain)
	mux.HandleFunc("/api/admin/retention/sweep", api.handleRetentionSweep)
	// Schedule routes before workflow routes so /api/schedules is not caught
	// by /api/workflows/.
	mux.HandleFunc("/api/schedules/", api.handleSchedules)
	mux.HandleFunc("/api/schedules", api.handleSchedulesList)
	mux.HandleFunc("/api/workflows/", api.handleWorkflows)
	mux.HandleFunc("/api/workflows", api.handleWorkflowsList)
	mux.HandleFunc("/api/dead-letters/", api.handleDeadLetters)
	mux.HandleFunc("/api/dead-letters", api.handleDeadLettersList)

	// Workflow definitions.
	mux.HandleFunc("GET /api/definitions", api.handleDefinitions)
	mux.HandleFunc("POST /api/definitions", api.handleCreateDefinition)

	// Version management.
	//
	// api.scopedStore, not api.store: store is the process-wide connection
	// opened at boot against the default tenant, and passing it here served
	// every caller's version read and -- worst -- POST
	// /api/versions/<name>/<v>/purge from the default tenant's data regardless
	// of who authenticated.
	engine.RegisterVersionHandler(mux, api.scopedStore)

	// Instance inspection endpoints (always on behind auth).
	mux.HandleFunc("/api/instances/", api.handleInstancesRoutes)

	// Admin API endpoints. Destructive operations are additionally gated
	// behind --enable-admin-api at request time in handleAdminRoutes (see
	// api_admin.go), so the route itself can always be registered.
	mux.HandleFunc("/api/admin/instances/", api.handleAdminRoutes)

	// Plugin discovery, when the binary loaded plugins.
	if api.plugins != nil {
		mux.Handle("/api/plugins", api.plugins)
	}

	// The SPA catch-all, when the binary embeds one.
	if api.spa != nil {
		mux.Handle("/", apiAware404(api.spa))
	}
	return mux
}

// apiAware404 wraps the SPA handler so that an unmatched path under /api/ is a
// JSON 404 rather than the single-page app.
//
// Only UNMATCHED paths reach here: ServeMux prefers the longest matching
// pattern, so every registered /api/ route above still wins. What falls through
// is a path no handler claims -- a typo, a client built against a newer server,
// or a route that was never registered at all, which is the case that hid seven
// endpoints for the life of this file.
func apiAware404(spa http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}
		spa.ServeHTTP(w, r)
	})
}

// ---- Dead Letter Queue handlers ----

func (s *apiServer) handleDeadLettersList(w http.ResponseWriter, r *http.Request) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		s.writeError(w, 405, "method not allowed")
		return
	}
	workflows, err := st.ListWorkflows(r.Context(), engine.WorkflowFilter{Status: "dead_lettered", Limit: 100})
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if workflows == nil {
		workflows = []engine.WorkflowInstance{}
	}
	s.writeJSON(w, 200, workflows)
}

func (s *apiServer) handleDeadLetters(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/dead-letters/")
	if path == "" || path == "/" {
		s.handleDeadLettersList(w, r)
		return
	}
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
	if len(parts) < 1 {
		s.writeError(w, 400, "bad request")
		return
	}

	id := parts[0]

	switch {
	case len(parts) == 2 && parts[1] == "reprocess" && r.Method == http.MethodPost:
		s.handleDeadLetterReprocess(w, r, id)
	case len(parts) == 2 && parts[1] == "terminate" && r.Method == http.MethodPost:
		s.handleDeadLetterTerminate(w, r, id)
	default:
		s.writeError(w, 404, "not found")
	}
}

func (s *apiServer) handleDeadLetterReprocess(w http.ResponseWriter, r *http.Request, id string) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	// Fetch the dead-lettered workflow instance.
	wf, err := st.GetWorkflowByID(r.Context(), id)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if wf == nil {
		s.writeError(w, 404, "workflow not found")
		return
	}
	if wf.Status != "dead_lettered" {
		s.writeError(w, 400, "workflow is not dead-lettered, status="+wf.Status)
		return
	}

	// Create a new run from the dead-lettered workflow's definition and input.
	versions, verr := st.ListVersions(r.Context(), wf.DefName)
	if verr != nil {
		s.writeError(w, 500, verr.Error())
		return
	}
	if len(versions) == 0 {
		s.writeError(w, 404, "workflow definition not found")
		return
	}

	// s.tenantFor, not engine.DefaultTenantUUID: reprocessing a dead-lettered
	// workflow re-created it under the default tenant regardless of whose
	// workflow it was, so a tenant's own retry moved its run into another
	// tenant's scope.
	runID, alreadyExisted, serr := st.StartNewRun(r.Context(), "", wf.DefName, versions[0], wf.Input, "", s.tenantFor(r), 0)
	if serr != nil {
		s.writeError(w, 500, serr.Error())
		return
	}
	if alreadyExisted {
		s.writeJSON(w, 200, map[string]string{"workflow_id": runID, "already_started": "true"})
		return
	}

	s.writeJSON(w, 201, map[string]string{"id": runID})
}

func (s *apiServer) handleDeadLetterTerminate(w http.ResponseWriter, r *http.Request, id string) {
	// callerOwnsTarget, not scopedStore. TerminateWorkflow's UPDATE carries
	// `AND tenant_id` since 3.86, so a foreign id already changed nothing --
	// but it changed nothing and returned 200, which reads to the caller as
	// "terminated" and to an operator as a workflow that ignored a terminate.
	// This answers 404 instead, and answers the SAME 404 for an id that does
	// not exist anywhere, which is the point: distinguishing the two would
	// turn this route into an oracle for which workflow ids are real.
	st, ok := s.callerOwnsTarget(w, r, id)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, int64(1<<10)) // 1 KB
		json.NewDecoder(r.Body).Decode(&req)
	}
	if err := st.TerminateWorkflow(r.Context(), id, req.Reason); err != nil {
		// 3.92: the store now reports a terminate that matched nothing rather
		// than returning nil. callerOwnsTarget above has already answered 404
		// for an id this tenant does not own, so reaching here means the row
		// went away between the two -- still a 404, and the same one, because
		// ErrWorkflowNotFound does not distinguish "gone" from "not yours".
		if errors.Is(err, engine.ErrWorkflowNotFound) {
			s.writeError(w, 404, "not found")
			return
		}
		s.writeError(w, 500, err.Error())
		return
	}
	s.writeJSON(w, 200, map[string]string{"status": "terminated"})
}

func (s *apiServer) handleWorkflowRetry(w http.ResponseWriter, r *http.Request, id string) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "method not allowed")
		return
	}
	wf, err := st.GetWorkflowByID(r.Context(), id)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if wf == nil {
		s.writeError(w, 404, "workflow not found")
		return
	}
	if wf.Status != "dead_lettered" {
		s.writeError(w, 400, "workflow is not dead-lettered, status="+wf.Status)
		return
	}
	if err := st.RetryWorkflow(r.Context(), id); err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	s.writeJSON(w, 200, map[string]string{"id": id, "status": "retried"})
}
