package main

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/cleat-team/cleat/engine"
)

// handleInstancesRoutes routes /api/instances/* requests.
func (s *apiServer) handleInstancesRoutes(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/instances/")
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
	case len(parts) == 2 && parts[1] == "events" && r.Method == http.MethodGet:
		s.handleGetInstanceEvents(w, r, id)
	case len(parts) == 2 && parts[1] == "state" && r.Method == http.MethodGet:
		s.handleGetInstanceState(w, r, id)
	case len(parts) == 1 && r.Method == http.MethodGet:
		s.handleGetInstanceState(w, r, id)
	default:
		s.writeError(w, 404, "not found")
	}
}

// handleGetInstanceEvents returns paginated event history for a workflow instance.
func (s *apiServer) handleGetInstanceEvents(w http.ResponseWriter, r *http.Request, id string) {
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	offset := 0
	limit := 1000
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v >= 0 {
		offset = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > 1000 {
		limit = 1000
	}

	total, err := st.CountEventHistory(r.Context(), id)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}

	history, err := st.LoadEventHistoryPaginated(r.Context(), id, offset, limit)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if history == nil {
		history = []engine.EventRecord{}
	}

	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	s.writeJSON(w, 200, history)
}

// handleGetInstanceState returns the full workflow instance state.
func (s *apiServer) handleGetInstanceState(w http.ResponseWriter, r *http.Request, id string) {
	st, ok := s.scopedStore(w, r)
	if !ok {
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
	s.writeJSON(w, 200, wf)
}
