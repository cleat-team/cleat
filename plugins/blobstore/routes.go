package blobstore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/auth"
	"github.com/rcownie/cleat/internal/plugin"

)

func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
	if mux == nil {
		return fmt.Errorf("blobstore: nil mux")
	}
	mux.HandleFunc("PUT /blobs/{key...}", p.handlePut)
	mux.HandleFunc("GET /blobs/{key...}", p.handleGet)
	mux.HandleFunc("HEAD /blobs/{key...}", p.handleHead)
	mux.HandleFunc("DELETE /blobs/{key...}", p.handleDelete)
	mux.HandleFunc("GET /blobs", p.handleList)
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

// ---- PUT /blobs/{key} ----

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

	// Read the entire body.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("blobstore: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	if len(body) == 0 {
		p.writeError(w, 400, "empty body")
		return
	}

	// Compute content hash (SHA-256) for content-addressing.
	hash := sha256.Sum256(body)
	sha256Hex := fmt.Sprintf("%x", hash)
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// Parse optional tags and TTL from query params.
	tags := make(map[string]string)
	if tagParam := r.URL.Query().Get("tag"); tagParam != "" {
		// Accept multiple ?tag=key:value query params.
		// The first occurrence is consumed above; Handlers must
		// iterate explicitly via r.URL.Query()["tag"].
		for _, t := range r.URL.Query()["tag"] {
			if parts := splitTag(t); parts != nil {
				tags[parts[0]] = parts[1]
			}
		}
	}

	var expiresAt *time.Time
	if ttlStr := r.URL.Query().Get("ttl"); ttlStr != "" {
		d, err := time.ParseDuration(ttlStr)
		if err == nil {
			t := time.Now().Add(d)
			expiresAt = &t
		}
	}

	// Store bytes via the configured backend.
	if err := p.backend.Put(r.Context(), sha256Hex, body, contentType); err != nil {
		p.logger.Error("blobstore: backend put", "key", key, "error", err)
		p.writeError(w, 500, "failed to store content")
		return
	}

	// Insert or increment blob_content metadata.
	storageBackend := p.config.Backend
	var s3Key *string
	if storageBackend == "s3" {
		s3Key = &sha256Hex
	}
	_, err = p.db.Exec(r.Context(), plugin.Rebind(upsertBlobContent.For(p.dialect), p.dialect),
		hash[:], len(body), storageBackend, s3Key)
	if err != nil {
		p.logger.Error("blobstore: store content", "key", key, "error", err)
		p.writeError(w, 500, "failed to store content")
		return
	}

	// Encode tags as JSON for the JSONB column.
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		p.logger.Error("blobstore: marshal tags", "key", key, "error", err)
		p.writeError(w, 500, "failed to encode tags")
		return
	}

	// Insert or update blob_index.
	if expiresAt != nil {
		_, err = p.db.Exec(r.Context(), plugin.Rebind(upsertBlobIndexWithTTL.For(p.dialect), p.dialect),
			key, tid, hash[:], len(body), contentType, tagsJSON, *expiresAt)
	} else {
		_, err = p.db.Exec(r.Context(), plugin.Rebind(upsertBlobIndex.For(p.dialect), p.dialect),
			key, tid, hash[:], len(body), contentType, tagsJSON)
	}
	if err != nil {
		p.logger.Error("blobstore: store index", "key", key, "error", err)
		p.writeError(w, 500, "failed to store metadata")
		return
	}

	p.logger.Info("blobstore: stored",
		"key", key,
		"tenant", tid,
		"size", len(body),
		"content_type", contentType,
	)

	p.writeJSON(w, 201, map[string]interface{}{
		"key":    key,
		"sha256": sha256Hex,
		"size":   len(body),
	})
}

// ---- GET /blobs/{key} ----

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

	var sha256Bytes []byte
	var contentType string
	var size int64
	var expiresAt sql.NullTime

	err := p.db.QueryRow(r.Context(), plugin.Rebind(`
		SELECT c.sha256, i.content_type, i.size, i.expires_at
		FROM blob_index i
		JOIN blob_content c ON i.sha256 = c.sha256
		WHERE i.key = $1 AND i.tenant_id = $2 AND i.deleted_at IS NULL
	`, p.dialect), key, tid).Scan(&sha256Bytes, &contentType, &size, &expiresAt)
	if err == sql.ErrNoRows {
		p.writeError(w, 404, "blob not found")
		return
	}
	if err != nil {
		p.logger.Error("blobstore: get", "key", key, "error", err)
		p.writeError(w, 500, "failed to retrieve blob")
		return
	}

	// Check TTL expiry. Return 404 for expired blobs.
	if expiresAt.Valid && expiresAt.Time.Before(time.Now()) {
		p.writeError(w, 404, "blob expired")
		return
	}

	// Retrieve blob data from the configured backend.
	sha256Hex := fmt.Sprintf("%x", sha256Bytes)
	data, err := p.backend.Get(r.Context(), sha256Hex)
	if err != nil {
		p.logger.Error("blobstore: get data", "key", key, "error", err)
		p.writeError(w, 500, "failed to retrieve blob data")
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("X-Blob-SHA256", sha256Hex)
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// ---- HEAD /blobs/{key} ----

func (p *Plugin) handleHead(w http.ResponseWriter, r *http.Request) {
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

	var sha256Bytes []byte
	var contentType string
	var size int64
	var expiresAt sql.NullTime

	err := p.db.QueryRow(r.Context(), plugin.Rebind(`
		SELECT c.sha256, i.content_type, i.size, i.expires_at
		FROM blob_index i
		JOIN blob_content c ON i.sha256 = c.sha256
		WHERE i.key = $1 AND i.tenant_id = $2 AND i.deleted_at IS NULL
	`, p.dialect), key, tid).Scan(&sha256Bytes, &contentType, &size, &expiresAt)
	if err == sql.ErrNoRows {
		p.writeError(w, 404, "blob not found")
		return
	}
	if err != nil {
		p.logger.Error("blobstore: head", "key", key, "error", err)
		p.writeError(w, 500, "failed to retrieve metadata")
		return
	}

	if expiresAt.Valid && expiresAt.Time.Before(time.Now()) {
		p.writeError(w, 404, "blob expired")
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("X-Blob-SHA256", fmt.Sprintf("%x", sha256Bytes))
	w.WriteHeader(http.StatusOK)
}

// ---- DELETE /blobs/{key} ----

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

	// Soft delete: set deleted_at timestamp. Physical deletion is deferred
	// to the TTL cleanup loop, which only removes bytes from S3 when no
	// in-flight workflow references the blob.
	rows, err := p.db.Exec(r.Context(), plugin.Rebind(`
		UPDATE blob_index SET deleted_at = now()
		WHERE key = $1 AND tenant_id = $2 AND deleted_at IS NULL
	`, p.dialect), key, tid)
	if err != nil {
		p.logger.Error("blobstore: soft delete", "key", key, "error", err)
		p.writeError(w, 500, "failed to delete blob")
		return
	}
	if rows == 0 {
		p.writeError(w, 404, "blob not found")
		return
	}

	p.logger.Info("blobstore: deleted (soft)", "key", key, "tenant", tid)
	w.WriteHeader(http.StatusNoContent)
}

// ---- GET /blobs ----

func (p *Plugin) handleList(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	prefix := r.URL.Query().Get("prefix")
	tagFilter := r.URL.Query().Get("tag") // ?tag=key:value
	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if v, err := strconv.Atoi(limitStr); err == nil && v > 0 && v <= 1000 {
			limit = v
		}
	}

	query := `
		SELECT i.key, i.sha256, i.size, i.content_type, i.tags, i.created_at, i.expires_at
		FROM blob_index i
		WHERE i.tenant_id = $1 AND i.deleted_at IS NULL
		`
	args := []interface{}{tid}
	argIdx := 2

	if prefix != "" {
		query += fmt.Sprintf(" AND i.key LIKE $%d", argIdx)
		args = append(args, prefix+"%")
		argIdx++
	}

	if tagFilter != "" {
		if parts := splitTag(tagFilter); parts != nil {
			switch p.dialect {
			case plugin.DialectMySQL:
				query += fmt.Sprintf(" AND JSON_CONTAINS(i.tags, CAST($%d AS JSON))", argIdx)
			case plugin.DialectMSSQL:
				query += fmt.Sprintf(" AND EXISTS (SELECT 1 FROM OPENJSON(i.tags) AS t1 INNER JOIN OPENJSON($%d) AS t2 ON t1.[key] = t2.[key] AND t1.value = t2.value)", argIdx)
			default:
				query += fmt.Sprintf(" AND i.tags @> $%d", argIdx)
			}
			tagJSON, _ := json.Marshal(map[string]string{parts[0]: parts[1]})
			args = append(args, string(tagJSON))
			argIdx++
		}
	}

	query += " ORDER BY i.created_at DESC"
	query += " LIMIT " + fmt.Sprintf("$%d", argIdx)
	args = append(args, limit)

	rows, err := p.db.Query(r.Context(), plugin.Rebind(query, p.dialect), args...)
	if err != nil {
		p.logger.Error("blobstore: list", "error", err)
		p.writeError(w, 500, "failed to list blobs")
		return
	}
	defer rows.Close()

	type blobEntry struct {
		Key         string            `json:"key"`
		SHA256      string            `json:"sha256"`
		Size        int64             `json:"size"`
		ContentType string            `json:"content_type"`
		Tags        map[string]string `json:"tags"`
		CreatedAt   time.Time         `json:"created_at"`
		ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
	}

	var blobs []blobEntry
	for rows.Next() {
		var (
			entry       blobEntry
			sha256Bytes []byte
			tagsJSON    []byte
			expiresAt   sql.NullTime
		)
		if err := rows.Scan(
			&entry.Key, &sha256Bytes, &entry.Size,
			&entry.ContentType, &tagsJSON, &entry.CreatedAt, &expiresAt,
		); err != nil {
			p.logger.Error("blobstore: scan row", "error", err)
			continue
		}
		entry.SHA256 = fmt.Sprintf("%x", sha256Bytes)
		json.Unmarshal(tagsJSON, &entry.Tags)
		if expiresAt.Valid {
			entry.ExpiresAt = &expiresAt.Time
		}
		blobs = append(blobs, entry)
	}

	if blobs == nil {
		blobs = []blobEntry{}
	}

	p.writeJSON(w, 200, blobs)
}

// splitTag parses a "key:value" tag string into [key, value] or returns nil.
func splitTag(t string) []string {
	t = strings.TrimSpace(t)
	if idx := strings.IndexByte(t, ':'); idx > 0 && idx < len(t)-1 {
		return []string{t[:idx], t[idx+1:]}
	}
	return nil
}
