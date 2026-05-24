package clewservice

import (
	"encoding/json"
	"net/http"
)

// handleHealth returns a simple health check.
func (p *Plugin) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleAuthCheck verifies the request is authenticated.
// Uses p.tenantID (set during Init from env.TenantID) rather than
// importing internal/auth to avoid transitive DB dependency.
func (p *Plugin) handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	if p.tenantID == "" {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"tenant":        p.tenantID,
	})
}

// writeJSON sends a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError sends a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg})
}
