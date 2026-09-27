package eventstore

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
)

func (p *Plugin) RegisterRoutes(mux plugin.Router) error {
	if mux == nil {
		return fmt.Errorf("eventstore: nil mux")
	}
	mux.HandleFunc("POST /events/{stream_id}", p.handleAppend)
	mux.HandleFunc("GET /events/{stream_id}", p.handleRead)
	mux.HandleFunc("GET /events/{stream_id}/stream", p.handleSSE)
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

// ---- POST /events/{stream_id} ----

func (p *Plugin) handleAppend(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
		p.writeError(w, 401, "tenant required")
		return
	}

	streamID := r.PathValue("stream_id")
	if streamID == "" {
		p.writeError(w, 400, "stream_id is required")
		return
	}

	// Read body.
	body, ok := plugin.ReadBody(w, r)
	if !ok {
		return
	}

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

	// Insert event with auto-incrementing sequence. The next sequence is
	// read in its own statement, not a subquery of the INSERT (cleat#2260 --
	// see nextSequenceForStream's comment in queries.go for why). Retry loop
	// handles PK conflicts from concurrent appends racing on the same
	// read-then-write.
	//
	// maxAppendAttempts was 3 until cleat#2260's own concurrent-append test
	// (n=20 appenders against one stream) exhausted it for real: every
	// loser's retry reads the now-current MAX and can still collide with
	// another concurrent loser, and a fixed 10/20ms backoff retries every
	// loser in the same round in lockstep, which does not thin the herd.
	// 32 attempts with jittered backoff (so losers spread out instead of
	// re-colliding together) clears n=20 reliably; see that test for the
	// measurement this is tuned against.
	var sequence int64
	var err error
	const maxAppendAttempts = 32
	backoff := 5 * time.Millisecond
	const maxBackoff = 100 * time.Millisecond
	for attempt := 1; attempt <= maxAppendAttempts; attempt++ {
		err = p.appendOnce(r.Context(), tid, streamID, body, &sequence)
		if err == nil {
			break
		}
		if !isPKConflict(err) {
			break
		}
		if attempt < maxAppendAttempts {
			//nolint:gosec // G404: retry-backoff jitter, not a security context -- spreads concurrent losers apart so they don't retry in lockstep. No secret or token derives from it.
			jitter := time.Duration(rand.Int64N(int64(backoff)))
			time.Sleep(backoff/2 + jitter)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
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

	p.writeJSON(w, 201, map[string]any{
		"stream_id": streamID,
		"sequence":  sequence,
	})
}

// appendOnce runs one attempt of the read-next-sequence-then-insert pair in
// a single transaction, writing the sequence it used into *sequence. A
// caller retries on a duplicate-key error (isPKConflict); see the comment on
// nextSequenceForStream in queries.go for why this is two statements and
// why the retry, not a lock, is what makes it safe under concurrency.
func (p *Plugin) appendOnce(ctx context.Context, tenantID uuid.UUID, streamID string, body []byte, sequence *int64) error {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}

	var maxSeq int64
	if err := tx.QueryRow(ctx, plugin.Rebind(nextSequenceForStream.For(p.dialect), p.dialect),
		tenantID, streamID).Scan(&maxSeq); err != nil {
		tx.Rollback()
		return fmt.Errorf("read next sequence: %w", err)
	}
	*sequence = maxSeq + 1

	if _, err := tx.Exec(ctx, plugin.Rebind(insertEvent.For(p.dialect), p.dialect),
		tenantID, streamID, *sequence, string(body)); err != nil {
		tx.Rollback()
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ---- GET /events/{stream_id} ----

func (p *Plugin) handleRead(w http.ResponseWriter, r *http.Request) {
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
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

	rows, err := p.db.Query(r.Context(), queryStreamPage.For(p.dialect),
		tid, streamID, fromSeq, limit)
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
		// plugin.JSONColumn, not &e.Event directly: json.RawMessage is a
		// named []byte type, and database/sql's convertAssign fast path
		// doesn't convert a driver string into one -- go-mssqldb returns
		// NVARCHAR as string, so every row failed to scan and this endpoint
		// returned an empty list on SQL Server. See plugin.JSONColumn.
		// cleat#2257.
		var eventCol plugin.JSONColumn
		if err := rows.Scan(&e.Sequence, &eventCol, &e.CreatedAt); err != nil {
			p.logger.Error("eventstore: scan row", "error", err)
			continue
		}
		e.Event = eventCol.Raw
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
	tid, ok := auth.TenantIDFromRequest(r)
	if !ok {
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
				// plugin.JSONColumn: see handleGet's identical comment above.
				// cleat#2257.
				var eventCol plugin.JSONColumn
				if err := rows.Scan(&seq, &eventCol); err != nil {
					p.logger.Error("eventstore: sse scan", "error", err)
					continue
				}
				event := eventCol.Raw

				payload, _ := json.Marshal(map[string]any{
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
