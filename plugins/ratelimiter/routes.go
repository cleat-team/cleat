package ratelimiter

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
)

// upsertQuery provides dialect-specific upsert for rate limits. The MSSQL
// variant's WITH (HOLDLOCK) (cleat#2904/#2915) closes the window where two
// concurrent PUTs establishing a limit under the same new (tenant_id,
// limit_key) -- plain HTTP request concurrency, handlePut has no
// serialization of its own -- both evaluate WHEN NOT MATCHED true and one
// takes a duplicate-key error instead of the UPDATE branch.
var upsertQuery = plugin.Query{
	Default: `
		INSERT INTO rate_limits (tenant_id, limit_key, max_requests, window_seconds)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, limit_key) DO UPDATE
		SET max_requests = EXCLUDED.max_requests,
		    window_seconds = EXCLUDED.window_seconds,
		    updated_at = now()`,
	MySQL: `
		INSERT INTO rate_limits (tenant_id, limit_key, max_requests, window_seconds)
		VALUES ($1, $2, $3, $4)
		ON DUPLICATE KEY UPDATE
		max_requests = VALUES(max_requests),
		window_seconds = VALUES(window_seconds),
		updated_at = now()`,
	MSSQL: `
		MERGE INTO rate_limits WITH (HOLDLOCK) AS target
		USING (VALUES ($1, $2, $3, $4)) AS source (tenant_id, limit_key, max_requests, window_seconds)
		ON target.tenant_id = source.tenant_id AND target.limit_key = source.limit_key
		WHEN MATCHED THEN
		    UPDATE SET max_requests = source.max_requests,
		               window_seconds = source.window_seconds,
		               updated_at = now()
		WHEN NOT MATCHED THEN
		    INSERT (tenant_id, limit_key, max_requests, window_seconds, created_at, updated_at)
		    VALUES (source.tenant_id, source.limit_key, source.max_requests, source.window_seconds, now(), now());`,
}

// rateLimitPut is the JSON body for PUT /rate-limits/{key}.
type rateLimitPut struct {
	MaxRequests   int `json:"max_requests"`
	WindowSeconds int `json:"window_seconds"`
}

// rateLimitEntry is the JSON shape returned by GET /rate-limits.
type rateLimitEntry struct {
	LimitKey      string    `json:"limit_key"`
	MaxRequests   int       `json:"max_requests"`
	WindowSeconds int       `json:"window_seconds"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// managementBasePath is the path every route in this plugin's management
// surface lives under, declared ONCE because TWO places have to agree about it:
// RegisterRoutes mounts them, and Middleware EXEMPTS them from the limit they
// manage.
//
// If the two drift, one of two defects appears and neither is visible from the
// other side: a mounted route that is not exempt is refused by the bucket it
// would change (cleat#2551), and an exempt prefix with nothing mounted under it
// is a hole with no handler behind it. Spelling the base path here is what makes
// that a one-line change rather than a search.
const managementBasePath = "/rate-limits"

// isManagementRequest reports whether r is a request for one of this plugin's
// OWN management routes.
//
// THE BOUNDARY IS THE SLASH, and it is asserted by a test that sends
// `/rate-limits-are-not-a-sibling` and requires it to be LIMITED. A bare
// HasPrefix(path, managementBasePath) would exempt every path that merely
// begins with the same characters, which is how a prefix check quietly becomes
// broader than the surface it was written for.
//
// It is deliberately broader than the REGISTERED patterns (`/rate-limits/{key}`
// matches any single segment) and narrower than the character prefix: a path
// like `/rate-limits/a/b` is exempt and serves nothing, so the cost of the
// slack is a 404 that skipped a counter, not a route that escaped it.
func isManagementRequest(r *http.Request) bool {
	path := r.URL.Path
	return path == managementBasePath || strings.HasPrefix(path, managementBasePath+"/")
}

// RegisterRoutes registers the rate limit management HTTP routes on the
// given mux. All routes require a tenant context set by the auth middleware.
//
// THESE ROUTES ARE EXEMPT FROM THIS PLUGIN'S OWN LIMIT — see Middleware. They
// are how an operator raises or removes a limit, so they cannot be behind it.
//
//	GET    /rate-limits        — list rate limits for the tenant
//	PUT    /rate-limits/{key}  — create or update a rate limit
//	DELETE /rate-limits/{key}  — remove a rate limit
func (p *Plugin) RegisterRoutes(mux plugin.Router) error {
	if mux == nil {
		return nil
	}
	mux.HandleFunc("GET "+managementBasePath, p.handleList)
	mux.HandleFunc("PUT "+managementBasePath+"/{key}", p.handlePut)
	mux.HandleFunc("DELETE "+managementBasePath+"/{key}", p.handleDelete)
	return nil
}

// ---- helpers ----

func (p *Plugin) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (p *Plugin) writeError(w http.ResponseWriter, status int, msg string) {
	p.writeJSON(w, status, map[string]string{"error": msg})
}

// ---- GET /rate-limits ----

func (p *Plugin) handleList(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, http.StatusUnauthorized, "tenant required")
		return
	}

	rows, err := p.db.Query(r.Context(), `
		SELECT limit_key, max_requests, window_seconds, created_at, updated_at
		FROM rate_limits
		WHERE tenant_id = $1
		ORDER BY limit_key
	`, tid)
	if err != nil {
		p.logger.Error("rate-limiter: list", "error", err)
		p.writeError(w, http.StatusInternalServerError, "failed to list rate limits")
		return
	}
	defer rows.Close()

	limits := make([]rateLimitEntry, 0)
	for rows.Next() {
		var entry rateLimitEntry
		if err := rows.Scan(&entry.LimitKey, &entry.MaxRequests, &entry.WindowSeconds, &entry.CreatedAt, &entry.UpdatedAt); err != nil {
			p.logger.Error("rate-limiter: scan row", "error", err)
			continue
		}
		limits = append(limits, entry)
	}
	if err := rows.Err(); err != nil {
		p.logger.Error("rate-limiter: rows iteration", "error", err)
		p.writeError(w, http.StatusInternalServerError, "failed to iterate rate limits")
		return
	}

	p.writeJSON(w, http.StatusOK, limits)
}

// ---- PUT /rate-limits/{key} ----

func (p *Plugin) handlePut(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, http.StatusUnauthorized, "tenant required")
		return
	}

	key := r.PathValue("key")
	if key == "" {
		p.writeError(w, http.StatusBadRequest, "key is required")
		return
	}

	var req rateLimitPut
	if !plugin.ReadJSONBody(w, r, &req) {
		return
	}
	if req.MaxRequests <= 0 {
		p.writeError(w, http.StatusBadRequest, "max_requests must be positive")
		return
	}
	if req.WindowSeconds <= 0 {
		p.writeError(w, http.StatusBadRequest, "window_seconds must be positive")
		return
	}

	_, err := p.db.Exec(r.Context(), upsertQuery.For(p.dialect), tid, key, req.MaxRequests, req.WindowSeconds)
	if err != nil {
		p.logger.Error("rate-limiter: upsert", "key", key, "tenant", tid, "error", err)
		p.writeError(w, http.StatusInternalServerError, "failed to set rate limit")
		return
	}

	// Update the in-memory token bucket immediately so the change takes
	// effect without waiting for the next background reload cycle.
	p.mu.Lock()
	p.buckets[tid.String()+"/"+key] = newTokenBucket(req.MaxRequests, req.WindowSeconds)
	p.mu.Unlock()

	p.logger.Info("rate-limiter: set rate limit",
		"key", key, "tenant", tid,
		"max_requests", req.MaxRequests,
		"window_seconds", req.WindowSeconds,
	)

	p.writeJSON(w, http.StatusOK, map[string]any{
		"limit_key":      key,
		"max_requests":   req.MaxRequests,
		"window_seconds": req.WindowSeconds,
	})
}

// ---- DELETE /rate-limits/{key} ----

func (p *Plugin) handleDelete(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, http.StatusUnauthorized, "tenant required")
		return
	}

	key := r.PathValue("key")
	if key == "" {
		p.writeError(w, http.StatusBadRequest, "key is required")
		return
	}

	rows, err := p.db.Exec(r.Context(), `
		DELETE FROM rate_limits
		WHERE tenant_id = $1 AND limit_key = $2
	`, tid, key)
	if err != nil {
		p.logger.Error("rate-limiter: delete", "key", key, "tenant", tid, "error", err)
		p.writeError(w, http.StatusInternalServerError, "failed to delete rate limit")
		return
	}
	if rows == 0 {
		p.writeError(w, http.StatusNotFound, "rate limit not found")
		return
	}

	// Remove from in-memory cache.
	p.mu.Lock()
	delete(p.buckets, tid.String()+"/"+key)
	p.mu.Unlock()

	p.logger.Info("rate-limiter: deleted rate limit",
		"key", key, "tenant", tid,
	)

	w.WriteHeader(http.StatusNoContent)
}
