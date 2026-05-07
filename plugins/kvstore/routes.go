package kvstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/auth"
)

func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
	if mux == nil {
		return fmt.Errorf("kvstore: nil mux")
	}
	mux.HandleFunc("GET /kv/{key}", p.handleGet)
	mux.HandleFunc("PUT /kv/{key}", p.handlePut)
	mux.HandleFunc("DELETE /kv/{key}", p.handleDelete)
	mux.HandleFunc("GET /kv", p.handleList)
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

// ---- GET /kv/{key} ----

func (p *Plugin) handleGet(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	key := r.PathValue("key")
	if key == "" {
		p.writeError(w, 400, "key is required")
		return
	}

	var value json.RawMessage
	var version int
	var createdAt, updatedAt time.Time

	err := p.db.QueryRowContext(r.Context(), `
		SELECT value, version, created_at, updated_at
		FROM kv_store
		WHERE tenant_id = $1 AND key = $2
	`, tid, key).Scan(&value, &version, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		p.writeError(w, 404, "key not found")
		return
	}
	if err != nil {
		p.logger.Error("kvstore: get", "key", key, "error", err)
		p.writeError(w, 500, "failed to retrieve value")
		return
	}

	w.Header().Set("ETag", strconv.Itoa(version))
	p.writeJSON(w, 200, map[string]interface{}{
		"key":        key,
		"value":      value,
		"version":    version,
		"created_at": createdAt,
		"updated_at": updatedAt,
	})
}

// ---- PUT /kv/{key} ----

func (p *Plugin) handlePut(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	key := r.PathValue("key")
	if key == "" {
		p.writeError(w, 400, "key is required")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("kvstore: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	if len(body) == 0 {
		p.writeError(w, 400, "empty body")
		return
	}

	if len(body) > p.config.MaxValueSize {
		p.writeError(w, 413, fmt.Sprintf("value exceeds max size of %d bytes", p.config.MaxValueSize))
		return
	}

	// Validate that the body is valid JSON.
	var value json.RawMessage
	if err := json.Unmarshal(body, &value); err != nil {
		p.writeError(w, 400, "invalid JSON value")
		return
	}

	// Optimistic concurrency: If-Match header carries the expected version.
	ifMatch := r.Header.Get("If-Match")
	if ifMatch != "" {
		expectedVersion, err := strconv.Atoi(ifMatch)
		if err != nil {
			p.writeError(w, 400, "invalid If-Match header: expected integer version")
			return
		}

		var newVersion int
		err = p.db.QueryRowContext(r.Context(), `
			UPDATE kv_store
			SET value = $1, version = version + 1, updated_at = now()
			WHERE tenant_id = $2 AND key = $3 AND version = $4
			RETURNING version
		`, value, tid, key, expectedVersion).Scan(&newVersion)
		if err == sql.ErrNoRows {
			p.writeError(w, 409, "conflict: version mismatch")
			return
		}
		if err != nil {
			p.logger.Error("kvstore: put (update)", "key", key, "error", err)
			p.writeError(w, 500, "failed to update value")
			return
		}

		w.Header().Set("ETag", strconv.Itoa(newVersion))
		p.writeJSON(w, 200, map[string]interface{}{
			"key":     key,
			"version": newVersion,
		})
		return
	}

	// No If-Match header: upsert (insert or overwrite unconditionally).
	var newVersion int
	err = p.db.QueryRowContext(r.Context(), `
		INSERT INTO kv_store (tenant_id, key, value)
		VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, key) DO UPDATE
		SET value = EXCLUDED.value,
		    version = kv_store.version + 1,
		    updated_at = now()
		RETURNING version, created_at
	`, tid, key, value).Scan(&newVersion)
	if err != nil {
		p.logger.Error("kvstore: put (upsert)", "key", key, "error", err)
		p.writeError(w, 500, "failed to store value")
		return
	}

	var statusCode int
	if newVersion == 1 {
		statusCode = 201 // created
	} else {
		statusCode = 200 // updated
	}

	w.Header().Set("ETag", strconv.Itoa(newVersion))
	p.writeJSON(w, statusCode, map[string]interface{}{
		"key":     key,
		"version": newVersion,
	})
}

// ---- DELETE /kv/{key} ----

func (p *Plugin) handleDelete(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	key := r.PathValue("key")
	if key == "" {
		p.writeError(w, 400, "key is required")
		return
	}

	result, err := p.db.ExecContext(r.Context(), `
		DELETE FROM kv_store
		WHERE tenant_id = $1 AND key = $2
	`, tid, key)
	if err != nil {
		p.logger.Error("kvstore: delete", "key", key, "error", err)
		p.writeError(w, 500, "failed to delete key")
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		p.writeError(w, 404, "key not found")
		return
	}

	p.logger.Info("kvstore: deleted", "key", key, "tenant", tid)
	w.WriteHeader(http.StatusNoContent)
}

// ---- GET /kv ----

func (p *Plugin) handleList(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	prefix := r.URL.Query().Get("prefix")
	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if v, err := strconv.Atoi(limitStr); err == nil && v > 0 && v <= 1000 {
			limit = v
		}
	}

	query := `
		SELECT key, value, version, created_at, updated_at
		FROM kv_store
		WHERE tenant_id = $1
	`
	args := []interface{}{tid}
	argIdx := 2

	if prefix != "" {
		query += fmt.Sprintf(" AND key LIKE $%d", argIdx)
		args = append(args, prefix+"%")
		argIdx++
	}

	query += " ORDER BY key ASC"
	query += fmt.Sprintf(" LIMIT $%d", argIdx)
	args = append(args, limit)

	rows, err := p.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		p.logger.Error("kvstore: list", "error", err)
		p.writeError(w, 500, "failed to list keys")
		return
	}
	defer rows.Close()

	type kvEntry struct {
		Key       string          `json:"key"`
		Value     json.RawMessage `json:"value"`
		Version   int             `json:"version"`
		CreatedAt time.Time       `json:"created_at"`
		UpdatedAt time.Time       `json:"updated_at"`
	}

	var entries []kvEntry
	for rows.Next() {
		var entry kvEntry
		if err := rows.Scan(
			&entry.Key, &entry.Value, &entry.Version,
			&entry.CreatedAt, &entry.UpdatedAt,
		); err != nil {
			p.logger.Error("kvstore: scan row", "error", err)
			continue
		}
		entries = append(entries, entry)
	}

	if entries == nil {
		entries = []kvEntry{}
	}

	p.writeJSON(w, 200, entries)
}
