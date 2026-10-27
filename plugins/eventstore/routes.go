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

// isPKConflict returns true if the error is a primary key or unique
// constraint violation. appendOnce's resync path uses this to recognise the
// one case it needs to react to within one transaction: a sequence number
// event_stream_head itself never produced, claimed by a writer that
// bypasses it entirely. handleAppend's outer retry loop also treats it as
// retryable, for the case ONE resync cannot fix -- see
// isRetryableAppendError.
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

// isRetryableAppendError reports whether appendOnce's failure is worth a
// fresh attempt in a new transaction, from handleAppend's outer loop.
//
// Two cases, both measured by cleat-review on a mixed workload -- real
// old-binary writers (develop's own read-MAX-then-insert, 32-attempt retry)
// racing real new-code appenders against ONE stream, cleat#2268 round 2
// (G4):
//
//   - isPKConflict, for a collision ONE resync inside appendOnce cannot
//     clear: the resync reads MAX(sequence) and inserts at MAX+1, and if a
//     DIFFERENT writer -- old or new -- claims that exact value in the gap
//     between that read and that insert, the resync's own insert collides
//     too. appendOnce does not loop on this itself (see its resync
//     comment); a second attempt gets a fresh MAX read, which is enough in
//     practice because the pattern that enables indefinite recollision
//     would need a writer winning that same narrow gap every single time.
//   - a deadlock or serialization failure, which carries no information
//     about which side "deserves" to win and aborts the WHOLE transaction
//     before resync logic even runs -- so there is nothing for appendOnce
//     itself to react to; only a fresh attempt helps. Measured on the mixed
//     workload: MySQL's upsertStreamHead step hit Error 1213 on every one
//     of 10 concurrent new-code appends across three rounds, with zero
//     retries before this fix (new-code-only load never triggers it, so
//     cleat#2268 round 2's own concurrency test did not catch this).
//
// Substring matching on the error TEXT, same style as isPKConflict above,
// rather than errors.As against each driver's own error type (cleat-review,
// cleat#2268 round 2). The known cost: an unrelated error whose message
// happens to contain "1205" or "1213" -- a stream id embedded in a
// constraint-violation message, say -- gets retried for a few seconds
// before surfacing as the same final error, not an incorrect one. A
// driver-typed check would close that gap at the cost of importing
// lib/pq, go-sql-driver/mysql and the mssql driver's error types directly
// into this plugin for three string comparisons' worth of precision.
func isRetryableAppendError(err error) bool {
	if isPKConflict(err) {
		return true
	}
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "1213") || // MySQL: deadlock found
		strings.Contains(s, "40001") || // Postgres/MSSQL: serialization failure
		strings.Contains(s, "40P01") || // Postgres: deadlock detected
		strings.Contains(s, "1205") // MSSQL: transaction was deadlocked
}

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

	// Insert event with a sequence claimed from event_stream_head. cleat#2260
	// split this into two statements because a same-table subquery INSERT
	// fails outright on MySQL; cleat#2268 replaced the read-MAX-then-retry
	// shape that fix originally used with real serialization -- see
	// upsertStreamHead's comment in queries.go for the mechanism. Two
	// new-code transactions can never compute the same sequence for the same
	// stream; appendOnce's own one-shot resync exists for a DIFFERENT writer
	// entirely -- see its resync comment.
	//
	// The loop below is for what ONE resync cannot clear: a mixed workload
	// of old-binary and new-code writers hitting one stream at once
	// (cleat-review + coordinator, cleat#2268 round 2, G4) produces both a
	// second collision on the resync's own insert and outright
	// deadlocks/serialization failures that abort the transaction before
	// resync logic runs at all -- see isRetryableAppendError. New-code-only
	// load never triggers either, so this never fires outside a rolling
	// upgrade; the attempt count and backoff shape are the ones
	// cleat#2260's own n=20 concurrent-append test tuned before cleat#2268
	// replaced their original use (see git history for the measurement).
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
		if !isRetryableAppendError(err) {
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

// appendOnce claims the next sequence for streamID from event_stream_head
// and inserts the event, in a single transaction, writing the sequence it
// used into *sequence. See upsertStreamHead's comment in queries.go for why
// two NEW-code transactions can never collide here: the row lock it takes on
// event_stream_head is held for this transaction's lifetime, so a second
// concurrent appender to the SAME stream blocks on that statement rather
// than computing the same sequence.
//
// RESYNC: A DIFFERENT writer can still collide with it. verifySchema
// (cmd/cleat-worker/schema_verify.go) warns rather than refuses when the
// schema is AHEAD of the binary, on purpose -- a rolling upgrade's deploy
// step migrates the schema before every old-binary worker has stopped, and
// refusing there would wedge the rollout on the workers it is replacing.
// Until the last old-binary worker exits, it keeps computing
// MAX(sequence)+1 and inserting directly into event_stream; it never writes
// event_stream_head, so the head this transaction claims can be stale-LOW
// and insertEvent's INSERT collides on the primary key an old worker
// already committed (cleat-review + coordinator, cleat#2268 round 1).
//
// That collision is this function's ONLY in-transaction retry trigger,
// detected by isPKConflict, and the retry is bounded at ONE attempt PER
// CALL: on collision, read event_stream's actual MAX(sequence) for this
// stream (ground truth, independent of what the head row believed), raise
// the head row to MAX+1 via resyncStreamHead, and insert again at that
// value. The row lock this transaction already holds on event_stream_head
// (taken above, by upsertStreamHead) makes the raise race-free -- no other
// transaction can be mid-upsert against the same stream while this one
// holds it. Once every old-binary worker has exited, a stream stays
// resynced permanently: nothing commits to event_stream a new-code
// transaction didn't account for in event_stream_head's lock.
//
// One attempt is not enough for a BUSY mix of old and new writers, though:
// a different writer can claim the resynced value in the gap between this
// transaction's own MAX read and its insert, and a deadlock or
// serialization failure can abort the transaction before any of this logic
// runs at all. Neither is this function's job to retry -- it returns the
// error, and handleAppend's outer loop (isRetryableAppendError) is what
// gives a mixed workload enough attempts to make progress. See that loop's
// comment for the measurement (cleat-review + coordinator, cleat#2268
// round 2, G4).
//
// This does NOT fold the floor into upsertStreamHead's own statement --
// round 1 did that, computing MAX(sequence) on every call, and deadlocked
// MySQL under concurrency (see upsertStreamHead's comment). Reading
// event_stream only on an actual collision keeps the fast, lock-minimal
// path lock-identical to pre-round-2 for the overwhelmingly common case
// (no old-binary writer racing), and only pays the extra read when there is
// something to reconcile.
//
// MySQL has no RETURNING, so its branch does the upsert with Exec and reads
// the result back with a plain SELECT in the same transaction -- see
// selectStreamHead's comment for why that read is safe. Postgres and MSSQL
// get the new value directly from the upsert statement.
func (p *Plugin) appendOnce(ctx context.Context, tenantID uuid.UUID, streamID string, body []byte, sequence *int64) error {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}

	var head int64
	if p.dialect == plugin.DialectMySQL {
		if _, err := tx.Exec(ctx, upsertStreamHead.For(p.dialect),
			tenantID, streamID); err != nil {
			tx.Rollback()
			return fmt.Errorf("upsert stream head: %w", err)
		}
		if err := tx.QueryRow(ctx, selectStreamHead.For(p.dialect),
			tenantID, streamID).Scan(&head); err != nil {
			tx.Rollback()
			return fmt.Errorf("read stream head: %w", err)
		}
	} else {
		if err := tx.QueryRow(ctx, upsertStreamHead.For(p.dialect),
			tenantID, streamID).Scan(&head); err != nil {
			tx.Rollback()
			return fmt.Errorf("upsert stream head: %w", err)
		}
	}
	*sequence = head

	// Postgres aborts the whole transaction on a statement error -- every
	// later statement on the same tx fails closed with 25P02 ("current
	// transaction is aborted") until a ROLLBACK, which would also discard
	// the event_stream_head row lock this function depends on. A SAVEPOINT
	// scopes that abort to just the optimistic insert, so the resync path
	// below can keep using this transaction. MySQL and MSSQL don't poison
	// the transaction on a duplicate-key error, so this is a no-op there --
	// confirmed by this function's falsification: without it, only the
	// postgres leg of TestOldStyleWriterBetweenAppendsIsAbsorbedByResync
	// failed (25P02 on the resync read), mysql and mssql already passed.
	usingSavepoint := p.dialect == plugin.DialectPostgres
	if usingSavepoint {
		if _, err := tx.Exec(ctx, "SAVEPOINT eventstore_append_attempt"); err != nil {
			tx.Rollback()
			return fmt.Errorf("savepoint: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, insertEvent.For(p.dialect),
		tenantID, streamID, *sequence, string(body)); err != nil {
		if !isPKConflict(err) {
			tx.Rollback()
			return err
		}

		if usingSavepoint {
			if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT eventstore_append_attempt"); err != nil {
				tx.Rollback()
				return fmt.Errorf("resync: rollback to savepoint: %w", err)
			}
		}

		// Resync: see this function's doc comment. A writer outside
		// event_stream_head's lock already used *sequence.
		var floor int64
		if err := tx.QueryRow(ctx, selectMaxSequence.For(p.dialect),
			tenantID, streamID).Scan(&floor); err != nil {
			tx.Rollback()
			return fmt.Errorf("resync: read max sequence: %w", err)
		}
		floor++
		if _, err := tx.Exec(ctx, resyncStreamHead.For(p.dialect),
			tenantID, streamID, floor); err != nil {
			tx.Rollback()
			return fmt.Errorf("resync: raise stream head: %w", err)
		}
		*sequence = floor

		if _, err := tx.Exec(ctx, insertEvent.For(p.dialect),
			tenantID, streamID, *sequence, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("resync: insert after resync: %w", err)
		}
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
	err := p.db.QueryRow(r.Context(), `
		SELECT COALESCE(MAX(sequence), 0)
		FROM event_stream
		WHERE tenant_id = $1 AND stream_id = $2
	`, tid, streamID).Scan(&lastSeq)
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
			rows, err := p.db.Query(ctx, `
				SELECT sequence, event
				FROM event_stream
				WHERE tenant_id = $1 AND stream_id = $2 AND sequence > $3
				ORDER BY sequence ASC
			`, tid, streamID, lastSeq)
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
