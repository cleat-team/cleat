package kvstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
)

func (p *Plugin) RegisterRoutes(mux plugin.Router) error {
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

func (p *Plugin) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (p *Plugin) writeError(w http.ResponseWriter, status int, msg string) {
	p.writeJSON(w, status, map[string]string{"error": msg})
}

// ---- GET /kv/{key} ----

func (p *Plugin) handleGet(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	key := r.PathValue("key")
	if key == "" {
		p.writeError(w, 400, "key is required")
		return
	}

	// plugin.JSONColumn rather than json.RawMessage: SQL Server returns the
	// column as a string and database/sql will not scan that into a named
	// []byte type. See plugin.JSONColumn.
	var value plugin.JSONColumn
	var version int
	var createdAt, updatedAt time.Time

	err := p.db.QueryRow(r.Context(), plugin.Rebind(`
		SELECT value, version, created_at, updated_at
		FROM kv_store
		WHERE tenant_id = $1 AND `+plugin.QuoteIdent("key", p.dialect)+` = $2
	`, p.dialect), tid, key).Scan(&value, &version, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		p.writeError(w, 404, "key not found")
		return
	}
	if err != nil {
		p.logger.Error("kvstore: get", "key", key, "error", err)
		p.writeError(w, 500, "failed to retrieve value")
		return
	}

	w.Header().Set("ETag", strconv.Itoa(version))
	p.writeJSON(w, 200, map[string]any{
		"key":        key,
		"value":      value.Raw,
		"version":    version,
		"created_at": createdAt,
		"updated_at": updatedAt,
	})
}

// ---- PUT /kv/{key} ----

func (p *Plugin) handlePut(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	key := r.PathValue("key")
	if key == "" {
		p.writeError(w, 400, "key is required")
		return
	}

	body, ok := plugin.ReadBody(w, r)
	if !ok {
		return
	}

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
		if p.dialect == plugin.DialectMySQL {
			// MySQL: UPDATE without RETURNING
			rows, execErr := p.db.Exec(r.Context(), plugin.Rebind(updateKVReturning.For(p.dialect), p.dialect),
				plugin.JSONColumn{Raw: value}, tid, key, expectedVersion)
			if execErr != nil {
				p.logger.Error("kvstore: put (update)", "key", key, "error", execErr)
				p.writeError(w, 500, "failed to update value")
				return
			}
			if rows == 0 {
				p.writeError(w, 409, "conflict: version mismatch")
				return
			}
			err = p.db.QueryRow(r.Context(), plugin.Rebind(`SELECT version FROM kv_store WHERE tenant_id = $1 AND `+plugin.QuoteIdent("key", p.dialect)+` = $2`, p.dialect), tid, key).Scan(&newVersion)
		} else {
			err = p.db.QueryRow(r.Context(), plugin.Rebind(updateKVReturning.For(p.dialect), p.dialect),
				plugin.JSONColumn{Raw: value}, tid, key, expectedVersion).Scan(&newVersion)
		}
		if errors.Is(err, sql.ErrNoRows) {
			p.writeError(w, 409, "conflict: version mismatch")
			return
		}
		if err != nil {
			p.logger.Error("kvstore: put (update)", "key", key, "error", err)
			p.writeError(w, 500, "failed to update value")
			return
		}

		w.Header().Set("ETag", strconv.Itoa(newVersion))
		p.writeJSON(w, 200, map[string]any{
			"key":     key,
			"version": newVersion,
		})
		return
	}

	// No If-Match header: upsert (insert or overwrite unconditionally).
	var newVersion int
	var err error
	var statusCode int
	if p.dialect == plugin.DialectMySQL {
		// MySQL: upsert without RETURNING, then select version. Whether
		// THIS call created the row cannot be read back from a separate
		// SELECT after the fact -- under concurrent writers to a brand-new
		// key, other writers' UPDATEs can increment the version between
		// this INSERT committing and this goroutine's own SELECT running,
		// so a literal creator can observe version > 1 and every racer can
		// observe a "stale" value, as cleat#2890's
		// TestConcurrentPutsToABrandNewKeyDoNotRace/mysql demonstrated: 0 of
		// 60 concurrent writers reported 201, though exactly one of them
		// did insert the row.
		//
		// MySQL's own INSERT ... ON DUPLICATE KEY UPDATE reports this
		// atomically via rows-affected, with no extra round trip and no
		// race window: 1 for the literal INSERT, 2 for a DUPLICATE KEY
		// UPDATE that changed a value (ours always does -- it increments
		// version unconditionally), 0 only if CLIENT_FOUND_ROWS were set
		// and the row were set to values identical to its current ones,
		// which cleat enables nowhere (see engine/schedule_errors.go and
		// engine/mysql_ops.go for the same rows-affected caveat elsewhere
		// in this codebase). So rows == 1 is an unambiguous "this call
		// created it".
		rows, execErr := p.db.Exec(r.Context(), plugin.Rebind(upsertKV.For(p.dialect), p.dialect),
			tid, key, plugin.JSONColumn{Raw: value})
		if execErr != nil {
			p.logger.Error("kvstore: put (upsert)", "key", key, "error", execErr)
			p.writeError(w, 500, "failed to store value")
			return
		}
		if rows == 1 {
			statusCode = 201 // created
		} else {
			statusCode = 200 // updated
		}
		err = p.db.QueryRow(r.Context(), plugin.Rebind(`SELECT version FROM kv_store WHERE tenant_id = $1 AND `+plugin.QuoteIdent("key", p.dialect)+` = $2`, p.dialect), tid, key).Scan(&newVersion)
	} else {
		err = p.db.QueryRow(r.Context(), plugin.Rebind(upsertKV.For(p.dialect), p.dialect),
			tid, key, plugin.JSONColumn{Raw: value}).Scan(&newVersion)
		if newVersion == 1 {
			statusCode = 201 // created
		} else {
			statusCode = 200 // updated
		}
	}
	if err != nil {
		p.logger.Error("kvstore: put (upsert)", "key", key, "error", err)
		p.writeError(w, 500, "failed to store value")
		return
	}

	w.Header().Set("ETag", strconv.Itoa(newVersion))
	p.writeJSON(w, statusCode, map[string]any{
		"key":     key,
		"version": newVersion,
	})
}

// ---- DELETE /kv/{key} ----

func (p *Plugin) handleDelete(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	key := r.PathValue("key")
	if key == "" {
		p.writeError(w, 400, "key is required")
		return
	}

	rows, err := p.db.Exec(r.Context(), plugin.Rebind(`
		DELETE FROM kv_store
		WHERE tenant_id = $1 AND `+plugin.QuoteIdent("key", p.dialect)+` = $2
	`, p.dialect), tid, key)
	if err != nil {
		p.logger.Error("kvstore: delete", "key", key, "error", err)
		p.writeError(w, 500, "failed to delete key")
		return
	}
	if rows == 0 {
		p.writeError(w, 404, "key not found")
		return
	}

	p.logger.Info("kvstore: deleted", "key", key, "tenant", tid)
	w.WriteHeader(http.StatusNoContent)
}

// ---- GET /kv ----

func (p *Plugin) handleList(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
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
		SELECT ` + plugin.QuoteIdent("key", p.dialect) + `, value, version, created_at, updated_at
		FROM kv_store
		WHERE tenant_id = $1
		`
	args := []any{tid}
	argIdx := 2

	if prefix != "" {
		query += " AND " + plugin.QuoteIdent("key", p.dialect) + " LIKE " + fmt.Sprintf("$%d", argIdx)
		args = append(args, prefix+"%")
		argIdx++
	}

	query += " ORDER BY " + plugin.QuoteIdent("key", p.dialect) + " ASC"
	query += " " + plugin.LimitClause(fmt.Sprintf("$%d", argIdx), p.dialect)
	args = append(args, limit)

	rows, err := p.db.Query(r.Context(), plugin.Rebind(query, p.dialect), args...)
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
		// See plugin.JSONColumn: SQL Server hands JSON back as a string.
		var value plugin.JSONColumn
		if err := rows.Scan(
			&entry.Key, &value, &entry.Version,
			&entry.CreatedAt, &entry.UpdatedAt,
		); err != nil {
			// Not a continue: skipping the row turned a driver-level type
			// mismatch into a silently short list, which is how this went
			// unnoticed on SQL Server.
			p.logger.Error("kvstore: scan row", "error", err)
			p.writeError(w, 500, "failed to list keys")
			return
		}
		entry.Value = value.Raw
		entries = append(entries, entry)
	}

	if entries == nil {
		entries = []kvEntry{}
	}

	p.writeJSON(w, 200, entries)
}
