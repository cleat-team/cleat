package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// StartAPIServer creates and starts the HTTP API server with the given
// configuration, worker, and plugin chain. It runs in a background goroutine
// and shuts down when ctx is cancelled.
//
// factory is what lets handlers scope each request to the tenant that
// authenticated it; without it an authenticated request cannot be served at all
// (storeFor refuses rather than falling back to the default tenant). It is a
// parameter rather than a Config field because Config holds flag-derived values
// and this is a live dependency.
func StartAPIServer(cfg *Config, w *Worker, plugMux, plugHandler http.Handler, plugList any, db *sql.DB, factory engine.StoreFactory) {
	if cfg.APIAddr == "" {
		return
	}

	api := &apiServer{
		store:       w.store,
		worker:      w,
		maxBodySize: cfg.MaxBodySize,
		db:          db,
		factory:     factory,
		taskQueues:  cfg.TaskQueues,
		requireAuth: cfg.RequireAuth,
	}

	mux := plugMux
	if mux == nil {
		mux = http.NewServeMux()
	}

	sm, ok := mux.(*http.ServeMux)
	if !ok {
		sm = http.NewServeMux()
	}
	mux = registerRoutes(sm, api)

	handler := plugHandler
	if handler == nil {
		handler = mux
	}

	srv := &http.Server{
		Addr:         cfg.APIAddr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		w.logger.InfoContext(context.Background(), "HTTP API listening", "worker_id", w.id, "addr", cfg.APIAddr)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			w.logger.ErrorContext(context.Background(), "HTTP server error", "worker_id", w.id, "error", err)
		}
	}()

	go func() {
		<-w.ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
}

// registerRoutes attaches all API routes to the given mux.
func registerRoutes(mux *http.ServeMux, api *apiServer) *http.ServeMux {
	mux.HandleFunc("/healthz", api.handleHealthz)
	mux.HandleFunc("/metrics", handleMetrics)
	mux.HandleFunc("/api/schedules/", api.handleSchedules)
	mux.HandleFunc("/api/schedules", api.handleSchedulesList)
	mux.HandleFunc("/api/workflows/", api.handleWorkflows)
	mux.HandleFunc("/api/workflows", api.handleWorkflowsList)
	mux.HandleFunc("/api/dead-letters/", api.handleDeadLetters)
	mux.HandleFunc("/api/dead-letters", api.handleDeadLettersList)

	// Instance inspection endpoints (always on behind auth).
	mux.HandleFunc("/api/instances/", api.handleInstancesRoutes)

	// Admin API endpoints. Destructive operations are additionally gated
	// behind --enable-admin-api at request time in handleAdminRoutes (see
	// api_admin.go), so the route itself can always be registered.
	mux.HandleFunc("/api/admin/instances/", api.handleAdminRoutes)
	return mux
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
