package engine

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RegisterVersionHandler adds version management HTTP endpoints to the given
// ServeMux. All routes are prefixed with /api/versions.
//
// Endpoints:
//
//	GET    /api/versions              — list all versions with metrics
//	GET    /api/versions/<name>       — list versions for a specific workflow
//	POST   /api/versions/<name>/<v>/deprecate     — mark version deprecated
//	POST   /api/versions/<name>/<v>/restore       — mark version active
//	POST   /api/versions/<name>/<v>/purge         — delete version permanently
//	GET    /api/versions/stale        — list stale version alerts
//	POST   /api/versions/gc           — run garbage collection
func RegisterVersionHandler(mux *http.ServeMux, store WorkflowStore) {
	h := &versionHandler{store: store}
	mux.HandleFunc("/api/versions", h.handleVersions)
	mux.HandleFunc("/api/versions/", h.handleVersions)
}

type versionHandler struct {
	store WorkflowStore
}

func (h *versionHandler) handleVersions(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/versions")
	path = strings.TrimPrefix(path, "/")

	// Exact match: /api/versions
	if path == "" {
		h.listAllVersions(w, r)
		return
	}

	// Stale alerts: /api/versions/stale
	if path == "stale" {
		h.listStaleAlerts(w, r)
		return
	}

	// Garbage collection: /api/versions/gc
	if path == "gc" && r.Method == http.MethodPost {
		h.runGC(w, r)
		return
	}

	// /api/versions/<name> or /api/versions/<name>/<v>/<action>
	parts := strings.Split(path, "/")
	if len(parts) == 0 {
		writeVersionError(w, 400, "bad request")
		return
	}

	name := parts[0]

	if len(parts) == 1 {
		h.listWorkflowVersions(w, r, name)
		return
	}

	if len(parts) < 3 {
		writeVersionError(w, 400, "bad request")
		return
	}

	version, err := strconv.Atoi(parts[1])
	if err != nil {
		writeVersionError(w, 400, "invalid version: "+parts[1])
		return
	}

	action := parts[2]
	switch action {
	case "deprecate":
		h.markDeprecated(w, r, name, version, true)
	case "restore":
		h.markDeprecated(w, r, name, version, false)
	case "purge":
		h.purgeVersion(w, r, name, version)
	default:
		writeVersionError(w, 404, "unknown action: "+action)
	}
}

func (h *versionHandler) listAllVersions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeVersionError(w, 405, "method not allowed")
		return
	}
	summary, err := CollectVersionMetrics(r.Context(), h.store)
	if err != nil {
		writeVersionError(w, 500, err.Error())
		return
	}
	writeVersionJSON(w, 200, summary)
}

func (h *versionHandler) listWorkflowVersions(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeVersionError(w, 405, "method not allowed")
		return
	}
	defs, err := h.store.ListWorkflowDefs(r.Context(), name)
	if err != nil {
		writeVersionError(w, 500, err.Error())
		return
	}
	if defs == nil {
		defs = []WorkflowDef{}
	}
	writeVersionJSON(w, 200, defs)
}

func (h *versionHandler) listStaleAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeVersionError(w, 405, "method not allowed")
		return
	}
	alerts, err := CheckStaleVersions(r.Context(), h.store, defaultStaleThreshold, defaultPurgeThreshold)
	if err != nil {
		writeVersionError(w, 500, err.Error())
		return
	}
	if alerts == nil {
		alerts = []StaleVersionAlert{}
	}
	writeVersionJSON(w, 200, alerts)
}

func (h *versionHandler) runGC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeVersionError(w, 405, "method not allowed")
		return
	}
	opts := DefaultGCOptions()
	// Support dry_run=true query parameter.
	if r.URL.Query().Get("dry_run") == "true" {
		opts.DryRun = true
	}
	result, err := GarbageCollectVersions(r.Context(), h.store, opts)
	if err != nil {
		writeVersionError(w, 500, err.Error())
		return
	}
	writeVersionJSON(w, 200, result)
}

func (h *versionHandler) markDeprecated(w http.ResponseWriter, r *http.Request, name string, version int, deprecated bool) {
	if r.Method != http.MethodPost {
		writeVersionError(w, 405, "method not allowed")
		return
	}
	if err := h.store.MarkVersionDeprecated(r.Context(), name, version, deprecated); err != nil {
		writeVersionError(w, 500, err.Error())
		return
	}
	status := "deprecated"
	if !deprecated {
		status = "restored"
	}
	writeVersionJSON(w, 200, map[string]string{"status": status})
}

func (h *versionHandler) purgeVersion(w http.ResponseWriter, r *http.Request, name string, version int) {
	if r.Method != http.MethodPost {
		writeVersionError(w, 405, "method not allowed")
		return
	}
	if err := h.store.PurgeWorkflowDef(r.Context(), name, version); err != nil {
		writeVersionError(w, 500, err.Error())
		return
	}
	writeVersionJSON(w, 200, map[string]string{"status": "purged"})
}

// writeVersionJSON is a standalone JSON response writer for this package.
func writeVersionJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeVersionError writes a JSON error response.
func writeVersionError(w http.ResponseWriter, status int, msg string) {
	writeVersionJSON(w, status, map[string]string{"error": msg})
}

// Default thresholds for stale version checking.
const (
	defaultStaleThreshold  = 7 * 24 * time.Hour  // 7 days
	defaultPurgeThreshold  = 30 * 24 * time.Hour // 30 days
)
