package ratelimiter

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/durable/internal/auth"
)

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

// RegisterRoutes registers the rate limit management HTTP routes on the
// given mux. All routes require a tenant context set by the auth middleware.
//
//	GET    /rate-limits        — list rate limits for the tenant
//	PUT    /rate-limits/{key}  — create or update a rate limit
//	DELETE /rate-limits/{key}  — remove a rate limit
func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
	if mux == nil {
		return nil
	}
	mux.HandleFunc("GET /rate-limits", p.handleList)
	mux.HandleFunc("PUT /rate-limits/{key}", p.handlePut)
	mux.HandleFunc("DELETE /rate-limits/{key}", p.handleDelete)
	return nil
}

// ---- helpers ----

func (p *Plugin) writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (p *Plugin) writeError(w http.ResponseWriter, status int, msg string) {
	p.writeJSON(w, status, map[string]string{"error": msg})
}

// tenantID extracts the tenant UUID from the request context. Returns the
// zero UUID if no tenant is set.
func (p *Plugin) tenantID(r *http.Request) uuid.UUID {
	tid, _ := auth.TenantIDFromContext(r.Context())
	return tid
}

// ---- GET /rate-limits ----

func (p *Plugin) handleList(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, http.StatusUnauthorized, "tenant required")
		return
	}

	rows, err := p.db.QueryContext(r.Context(), `
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
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, http.StatusUnauthorized, "tenant required")
		return
	}

	key := r.PathValue("key")
	if key == "" {
		p.writeError(w, http.StatusBadRequest, "key is required")
		return
	}

	var req rateLimitPut
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		p.writeError(w, http.StatusBadRequest, "invalid JSON body")
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

	_, err := p.db.ExecContext(r.Context(), `
		INSERT INTO rate_limits (tenant_id, limit_key, max_requests, window_seconds)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, limit_key) DO UPDATE
		SET max_requests = EXCLUDED.max_requests,
		    window_seconds = EXCLUDED.window_seconds,
		    updated_at = now()
	`, tid, key, req.MaxRequests, req.WindowSeconds)
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

	p.writeJSON(w, http.StatusOK, map[string]interface{}{
		"limit_key":      key,
		"max_requests":   req.MaxRequests,
		"window_seconds": req.WindowSeconds,
	})
}

// ---- DELETE /rate-limits/{key} ----

func (p *Plugin) handleDelete(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, http.StatusUnauthorized, "tenant required")
		return
	}

	key := r.PathValue("key")
	if key == "" {
		p.writeError(w, http.StatusBadRequest, "key is required")
		return
	}

	result, err := p.db.ExecContext(r.Context(), `
		DELETE FROM rate_limits
		WHERE tenant_id = $1 AND limit_key = $2
	`, tid, key)
	if err != nil {
		p.logger.Error("rate-limiter: delete", "key", key, "tenant", tid, "error", err)
		p.writeError(w, http.StatusInternalServerError, "failed to delete rate limit")
		return
	}
	rows, _ := result.RowsAffected()
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
