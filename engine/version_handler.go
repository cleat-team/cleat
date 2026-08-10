package engine

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// VersionStoreResolver resolves the WorkflowStore scoped to an HTTP
// request's authenticated tenant. Implementations write the error response
// themselves and return ok=false when no store can be resolved (no
// authenticated tenant, or a tenant whose store cannot be opened), mirroring
// cmd/cleat-worker's apiServer.scopedStore -- which is exactly what
// production wiring passes here (see the call in cmd/cleat-worker/main.go).
//
// This type exists because RegisterVersionHandler used to take a single
// process-wide WorkflowStore, opened once at boot against the default
// tenant. Every version endpoint -- including
// POST /api/versions/<name>/<v>/purge, which permanently deletes workflow
// definitions -- then served every caller from that one tenant's data
// regardless of who authenticated, and any caller could purge the default
// tenant's definitions. Routing each request through a resolver is the same
// fix cmd/cleat-worker/server.go applies to every other handler; this
// package cannot import cmd/cleat-worker's apiServer or the auth package
// directly (auth already imports engine), so the resolver is the seam
// between them.
type VersionStoreResolver func(w http.ResponseWriter, r *http.Request) (store WorkflowStore, ok bool)

// StaticVersionStore returns a VersionStoreResolver that always resolves to
// store, regardless of the request. It exists for callers that have no
// per-request tenant to scope to -- tests, and an embedded/local-dev setup
// that only ever has one tenant. Production HTTP wiring must not use this:
// it recreates exactly the defect VersionStoreResolver exists to close.
func StaticVersionStore(store WorkflowStore) VersionStoreResolver {
	return func(http.ResponseWriter, *http.Request) (WorkflowStore, bool) {
		return store, true
	}
}

// RegisterVersionHandler adds version management HTTP endpoints to the given
// ServeMux. All routes are prefixed with /api/versions. resolve is called
// once per request to obtain the tenant-scoped store; see
// VersionStoreResolver.
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
func RegisterVersionHandler(mux *http.ServeMux, resolve VersionStoreResolver) {
	h := &versionHandler{resolve: resolve}
	mux.HandleFunc("/api/versions", h.handleVersions)
	mux.HandleFunc("/api/versions/", h.handleVersions)
}

type versionHandler struct {
	resolve VersionStoreResolver
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
	store, ok := h.resolve(w, r)
	if !ok {
		return
	}
	summary, err := CollectVersionMetrics(r.Context(), store)
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
	store, ok := h.resolve(w, r)
	if !ok {
		return
	}
	defs, err := store.ListWorkflowDefs(r.Context(), name)
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
	store, ok := h.resolve(w, r)
	if !ok {
		return
	}
	alerts, err := CheckStaleVersions(r.Context(), store, defaultStaleThreshold, defaultPurgeThreshold)
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
	store, ok := h.resolve(w, r)
	if !ok {
		return
	}
	opts := DefaultGCOptions()
	// Support dry_run=true query parameter.
	if r.URL.Query().Get("dry_run") == "true" {
		opts.DryRun = true
	}
	result, err := GarbageCollectVersions(r.Context(), store, opts)
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
	store, ok := h.resolve(w, r)
	if !ok {
		return
	}
	if err := store.MarkVersionDeprecated(r.Context(), name, version, deprecated); err != nil {
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
	store, ok := h.resolve(w, r)
	if !ok {
		return
	}
	if err := store.PurgeWorkflowDef(r.Context(), name, version); err != nil {
		writeVersionError(w, 500, err.Error())
		return
	}
	writeVersionJSON(w, 200, map[string]string{"status": "purged"})
}

// writeVersionJSON is a standalone JSON response writer for this package.
func writeVersionJSON(w http.ResponseWriter, status int, v any) {
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
	defaultStaleThreshold = 7 * 24 * time.Hour  // 7 days
	defaultPurgeThreshold = 30 * 24 * time.Hour // 30 days
)
