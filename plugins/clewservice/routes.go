package clewservice

import (
	"net/http"
)

// RegisterRoutes registers all HTTP endpoints on the cleat worker's API mux.
func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
	mux.HandleFunc("GET /healthz", p.handleHealth)

	mux.HandleFunc("GET /api/auth/check", p.handleAuthCheck)

	mux.HandleFunc("GET /api/projects", p.handleProjectsGet)
	mux.HandleFunc("POST /api/projects", p.handleProjectsPost)

	mux.HandleFunc("GET /api/tasks", p.handleTasksGet)
	mux.HandleFunc("POST /api/tasks", p.handleTasksPost)
	mux.HandleFunc("GET /api/tasks/{id}", p.handleTaskGet)
	mux.HandleFunc("GET /api/tasks/{id}/content", p.handleTaskContentGet)
	mux.HandleFunc("POST /api/tasks/{id}/result", p.handleTaskResultPost)
	mux.HandleFunc("POST /api/tasks/{id}/dispatch", p.handleTaskDispatchPost)
	mux.HandleFunc("POST /api/tasks/{id}/cancel", p.handleTaskCancelPost)
	mux.HandleFunc("GET /api/tasks/{id}/logs", p.handleTaskLogsGet)
	mux.HandleFunc("GET /api/tasks/{id}/artifacts", p.handleTaskArtifactsGet)

	mux.HandleFunc("GET /api/agent/poll", p.handleAgentPoll)
	mux.HandleFunc("POST /api/agent/heartbeat", p.handleAgentHeartbeat)

	mux.HandleFunc("GET /api/dashboard/summary", p.handleDashboardSummary)
	mux.HandleFunc("GET /api/lessons", p.handleLessonsGet)
	mux.HandleFunc("GET /api/content/{path...}", p.handleContentGet)

	return nil
}
