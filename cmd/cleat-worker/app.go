package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/rcownie/cleat/internal/host"
)

// StartAPIServer creates and starts the HTTP API server with the given
// configuration, worker, and plugin chain. It runs in a background goroutine
// and shuts down when ctx is cancelled.
func StartAPIServer(cfg *Config, w *Worker, plugMux, plugHandler http.Handler, plugList interface{}, db *sql.DB) {
	if cfg.APIAddr == "" {
		return
	}

	api := &apiServer{
		store:       w.store,
		worker:      w,
		maxBodySize: cfg.MaxBodySize,
		db:          db,
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
		log.Printf("[worker %s] HTTP API listening on %s", w.id, cfg.APIAddr)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("[worker %s] HTTP server error: %v", w.id, err)
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
	return mux
}

// ---- Dead Letter Queue handlers ----

func (s *apiServer) handleDeadLettersList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, 405, "method not allowed")
		return
	}
	workflows, err := s.store.ListWorkflows(r.Context(), host.WorkflowFilter{Status: "dead_lettered", Limit: 100})
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if workflows == nil {
		workflows = []host.WorkflowInstance{}
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
	// Fetch the dead-lettered workflow instance.
	wf, err := s.store.GetWorkflowByID(r.Context(), id)
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
	versions, verr := s.store.ListVersions(r.Context(), wf.DefName)
	if verr != nil {
		s.writeError(w, 500, verr.Error())
		return
	}
	if len(versions) == 0 {
		s.writeError(w, 404, "workflow definition not found")
		return
	}

	runID, alreadyExisted, serr := s.store.StartNewRun(r.Context(), "", wf.DefName, versions[0], wf.Input, "")
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
	var req struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, int64(1<<10)) // 1 KB
		json.NewDecoder(r.Body).Decode(&req)
	}
	if err := s.store.TerminateWorkflow(r.Context(), id, req.Reason); err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	s.writeJSON(w, 200, map[string]string{"status": "terminated"})
}

func (s *apiServer) handleWorkflowRetry(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		s.writeError(w, 405, "method not allowed")
		return
	}
	wf, err := s.store.GetWorkflowByID(r.Context(), id)
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
	if err := s.store.RetryWorkflow(r.Context(), id); err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	s.writeJSON(w, 200, map[string]string{"id": id, "status": "retried"})
}
