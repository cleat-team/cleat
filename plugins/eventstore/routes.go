package eventstore

import (
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
		return fmt.Errorf("eventstore: nil mux")
	}
	mux.HandleFunc("POST /events/{stream_id}", p.handleAppend)
	mux.HandleFunc("GET /events/{stream_id}", p.handleRead)
	mux.HandleFunc("GET /events/{stream_id}/stream", p.handleSSE)
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

// isPKConflict returns true if the error is a primary key or unique constraint
// violation. These occur when two concurrent appends compute the same next
// sequence number — a safe retry scenario.
func isPKConflict(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "duplicate") ||
		strings.Contains(s, "unique") ||
		strings.Contains(s, "PRIMARY KEY") ||
		strings.Contains(s, "2627") || // MSSQL
		strings.Contains(s, "1062") || // MySQL
		strings.Contains(s, "23505") // PostgreSQL
}

// tenantID extracts the tenant UUID from the request context. Returns the
// zero UUID if no tenant is set.
func (p *Plugin) tenantID(r *http.Request) uuid.UUID {
	tid, _ := auth.TenantIDFromContext(r.Context())
	return tid
}

// ---- POST /events/{stream_id} ----

func (p *Plugin) handleAppend(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	streamID := r.PathValue("stream_id")
	if streamID == "" {
		p.writeError(w, 400, "stream_id is required")
		return
	}

	// Read body.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.Error("eventstore: read body", "error", err)
		p.writeError(w, 500, "failed to read body")
		return
	}
	defer r.Body.Close()

	if len(body) == 0 {
		p.writeError(w, 400, "empty body")
		return
	}

	if len(body) > p.config.MaxEventSize {
		p.writeError(w, 413, fmt.Sprintf("event too large (max %d bytes)", p.config.MaxEventSize))
		return
	}

	// Validate that body is valid JSON.
	if !json.Valid(body) {
		p.writeError(w, 400, "body must be valid JSON")
		return
	}

	// Insert event with auto-incrementing sequence.
	// Retry loop handles PK conflicts from concurrent appends.
	var sequence int64
	const maxAppendAttempts = 3
	backoff := 10 * time.Millisecond
	for attempt := 1; attempt <= maxAppendAttempts; attempt++ {
		err = p.db.QueryRow(r.Context(), plugin.Rebind(insertEventReturning.For(p.dialect), p.dialect),
			tid, streamID, string(body)).Scan(&sequence)
		if err == nil {
			break
		}
		if !isPKConflict(err) {
			break
		}
		if attempt < maxAppendAttempts {
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	if err != nil {
		p.logger.Error("eventstore: append", "stream", streamID, "error", err)
		p.writeError(w, 500, "failed to append event")
		return
	}

	p.logger.Info("eventstore: appended",
		"stream", streamID,
		"tenant", tid,
		"sequence", sequence,
	)

	p.writeJSON(w, 201, map[string]interface{}{
		"stream_id": streamID,
		"sequence":  sequence,
	})
}

// ---- GET /events/{stream_id} ----

func (p *Plugin) handleRead(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	streamID := r.PathValue("stream_id")
	if streamID == "" {
		p.writeError(w, 400, "stream_id is required")
		return
	}

	fromSeq := int64(0)
	if s := r.URL.Query().Get("from_sequence"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err == nil && v > 0 {
			fromSeq = v
		}
	}

	limit := 100
	if s := r.URL.Query().Get("limit"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 && v <= 1000 {
			limit = v
		}
	}

	rows, err := p.db.Query(r.Context(), plugin.Rebind(`
		SELECT sequence, event, created_at
		FROM event_stream
		WHERE tenant_id = $1 AND stream_id = $2 AND sequence > $3
		ORDER BY sequence ASC
		LIMIT $4
	`, p.dialect), tid, streamID, fromSeq, limit)
	if err != nil {
		p.logger.Error("eventstore: read", "stream", streamID, "error", err)
		p.writeError(w, 500, "failed to read events")
		return
	}
	defer rows.Close()

	type eventEntry struct {
		Sequence  int64           `json:"sequence"`
		Event     json.RawMessage `json:"event"`
		CreatedAt time.Time       `json:"created_at"`
	}

	events := []eventEntry{}
	for rows.Next() {
		var e eventEntry
		if err := rows.Scan(&e.Sequence, &e.Event, &e.CreatedAt); err != nil {
			p.logger.Error("eventstore: scan row", "error", err)
			continue
		}
		events = append(events, e)
	}

	if err := rows.Err(); err != nil {
		p.logger.Error("eventstore: rows iteration", "error", err)
		p.writeError(w, 500, "failed to read events")
		return
	}

	p.writeJSON(w, 200, events)
}

// ---- GET /events/{stream_id}/stream (SSE) ----

func (p *Plugin) handleSSE(w http.ResponseWriter, r *http.Request) {
	tid := p.tenantID(r)
	if tid == uuid.Nil {
		p.writeError(w, 401, "tenant required")
		return
	}

	streamID := r.PathValue("stream_id")
	if streamID == "" {
		p.writeError(w, 400, "stream_id is required")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		p.writeError(w, 500, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Get the latest sequence so far so we only stream new events.
	var lastSeq int64
	err := p.db.QueryRow(r.Context(), plugin.Rebind(`
		SELECT COALESCE(MAX(sequence), 0)
		FROM event_stream
		WHERE tenant_id = $1 AND stream_id = $2
	`, p.dialect), tid, streamID).Scan(&lastSeq)
	if err != nil {
		p.logger.Error("eventstore: sse initial seq", "stream", streamID, "error", err)
		return
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			rows, err := p.db.Query(ctx, plugin.Rebind(`
				SELECT sequence, event
				FROM event_stream
				WHERE tenant_id = $1 AND stream_id = $2 AND sequence > $3
				ORDER BY sequence ASC
			`, p.dialect), tid, streamID, lastSeq)
			if err != nil {
				p.logger.Error("eventstore: sse poll", "stream", streamID, "error", err)
				continue
			}

			for rows.Next() {
				var seq int64
				var event json.RawMessage
				if err := rows.Scan(&seq, &event); err != nil {
					p.logger.Error("eventstore: sse scan", "error", err)
					continue
				}

				payload, _ := json.Marshal(map[string]interface{}{
					"sequence": seq,
					"event":    event,
				})
				fmt.Fprintf(w, "data: %s\n\n", payload)
				flusher.Flush()

				lastSeq = seq
			}
			rows.Close()

			if err := rows.Err(); err != nil {
				p.logger.Error("eventstore: sse rows", "error", err)
			}
		}
	}
}
