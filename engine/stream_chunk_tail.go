package engine

import (
	"context"
	"database/sql"
	"fmt"
)

// StreamChunkTailReader reads a run's stream chunks past a cursor, so a reader
// can follow a run from the durable record on any worker. cleat#1639.
//
// # Why this is an optional interface and not a WorkflowStore method
//
// WorkflowStore has four real implementations and ten test doubles: adding one
// method there costs fourteen bodies, thirteen of which would be written to
// keep a compile working rather than because anything calls them. The doc on
// WorkerRegistry makes the same argument for the same reason and resolves it
// the same way.
//
// So this is asserted for at the call site, and a store that does not implement
// it is not a broken store -- it is a store the poll mode is unavailable on,
// which the SSE route reports rather than hides.
//
// TestEveryRealStoreCanTailStreamChunks is what stops that escape hatch from
// silently swallowing a real store. A test double legitimately opts out; a
// dialect never does.
//
// # Why it is not LoadEventHistory
//
// LoadEventHistory reads the WHOLE history -- 31 columns, every event type,
// decrypt and redact per row -- and has no step predicate, so polling it costs
// the whole run on every tick. Measured on postgres 16, three runs, 20 reps,
// against runs of 100 / 1000 / 5000 chunk rows:
//
//	chunks   LoadEventHistory        this, steady state
//	   100   2.46 / 3.25 / 3.06 ms   0.95 / 1.12 / 0.75 ms
//	  1000  12.49 / 9.54 / 10.91 ms  1.91 / 0.68 / 1.19 ms
//	  5000  42.55 / 39.53 / 44.74 ms 1.05 / 0.87 / 1.24 ms
//
// The full read is linear in history size. This one is FLAT: its spread within
// one history size is as large as its spread across all three, which is the
// shape that says history size is not a term in it. At 5000 chunks the ratio is
// 36-45x, and one reader polling the full read at 250ms would spend ~17% of a
// core on a single stream.
//
// It needs no new index. (tenant_id, workflow_id, step) -- idx_event_history_tenant_wf,
// which has existed since the table did -- is an exact prefix match for the
// predicate, and EXPLAIN reports an index scan touching 2-3 buffers against a
// 5000-row history with execution at 0.016-0.018 ms.
//
// That last number is the one to design against rather than the query: a poll
// measured 0.7-1.9 ms in Go against a 0.016 ms query, because on PostgreSQL the
// read is BEGIN + set_config + SELECT + COMMIT and only one of those four round
// trips carries data.
type StreamChunkTailReader interface {
	// LoadStreamChunksAfter returns this run's stream chunk events with a step
	// strictly greater than afterStep, oldest first, at most limit of them.
	//
	// afterStep is the reader's cursor and is exclusive. Pass -1 for "from the
	// beginning", which is distinct from 0 -- step 0 is a real step and a run's
	// first chunk can occupy it.
	//
	// A full page means there may be more: the caller should read again
	// immediately rather than waiting out its poll interval.
	LoadStreamChunksAfter(ctx context.Context, workflowID string, afterStep, limit int) ([]EventRecord, error)
}

// streamChunkTailLimit bounds one page, and bounds it for memory rather than
// for cost -- the predicate is an index range scan either way.
//
// A caller that is keeping up reads nothing or a handful; this matters only for
// one that has been away, and there the alternative to a bound is materialising
// an entire run's chunks in one slice.
const streamChunkTailLimit = 1000

func clampStreamChunkTailLimit(limit int) int {
	if limit <= 0 || limit > streamChunkTailLimit {
		return streamChunkTailLimit
	}
	return limit
}

// scanStreamChunkRows turns the five columns every dialect selects into records.
//
// decryptAndRedactEventRecord is not optional and is the reason this is shared
// rather than written three times: plugin_output is an ENCRYPTED and REDACTED
// field on the ordinary read path, so a tail that skipped it would serve
// ciphertext to a client that had been getting plaintext -- or worse, serve a
// secret that RedactOnRead exists to remove. The chunk's index and finish flag
// live in the payload JSON, which is itself encrypted, so populateFromPayload
// runs on the decrypted text exactly as it does in LoadEventHistory.
//
// The two closures are what differ per dialect and the reason this takes them
// rather than a store: PostgreSQL encrypts these fields and its payload column,
// MySQL and SQL Server encrypt neither and only redact. Passing the store would
// hide that difference behind an interface; passing the two steps makes each
// call site state which treatment its rows get.
func scanStreamChunkRows(
	rows *sql.Rows,
	workflowID string,
	decrypt func(rec *EventRecord, workflowID string),
	decryptPayload func(string) string,
) ([]EventRecord, error) {
	var out []EventRecord
	for rows.Next() {
		var rec EventRecord
		var pluginName, pluginFunc, pluginOutput, payload sql.NullString

		if err := rows.Scan(&rec.Step, &pluginName, &pluginFunc, &pluginOutput, &payload); err != nil {
			return nil, fmt.Errorf("scan stream chunk: %w", err)
		}
		rec.EventType = EventTypePluginCallStreamChunk
		rec.PluginName = pluginName.String
		rec.PluginFunc = pluginFunc.String
		rec.PluginOutput = pluginOutput.String

		decrypt(&rec, workflowID)

		if payload.Valid {
			populateFromPayload(&rec, []byte(decryptPayload(payload.String)))
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// LoadStreamChunksAfter implements StreamChunkTailReader.
//
// On an RLS transaction rather than s.db, for the ordinary reason: event_history
// carries ENABLE + FORCE ROW LEVEL SECURITY and a policy that RAISES the moment
// a candidate row is examined, so a statement issued outside a transaction that
// has set cleat.tenant_id fails for any run that has events.
func (s *PostgresStore) LoadStreamChunksAfter(ctx context.Context, workflowID string, afterStep, limit int) ([]EventRecord, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("load stream chunks: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT step, plugin_name, plugin_func, plugin_output, payload
		FROM event_history
		WHERE workflow_id = $1 AND tenant_id = $2
		  AND event_type = $3
		  AND step > $4
		ORDER BY step
		LIMIT $5
	`, workflowID, s.tenantID, EventTypePluginCallStreamChunk, afterStep, clampStreamChunkTailLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("load stream chunks: %w", err)
	}
	defer rows.Close()

	out, err := scanStreamChunkRows(rows, workflowID, s.decryptAndRedactEventRecord, s.decryptPayloadJSON)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// LoadStreamChunksAfter implements StreamChunkTailReader.
func (s *MySQLStore) LoadStreamChunksAfter(ctx context.Context, workflowID string, afterStep, limit int) ([]EventRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT step, plugin_name, plugin_func, plugin_output, payload
		FROM event_history
		WHERE workflow_id = ? AND tenant_id = ?
		  AND event_type = ?
		  AND step > ?
		ORDER BY step
		LIMIT ?
	`, workflowID, s.tenantID, EventTypePluginCallStreamChunk, afterStep, clampStreamChunkTailLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("load stream chunks: %w", err)
	}
	defer rows.Close()

	return scanStreamChunkRows(rows, workflowID, s.redactStreamChunk, identityPayload)
}

// redactStreamChunk applies this store's read-path treatment to one chunk.
//
// Redaction only: MySQL stores these fields in the clear -- LoadEventHistory
// does the same, and a decrypt step here would be a treatment the write path
// never applied.
func (s *MySQLStore) redactStreamChunk(rec *EventRecord, _ string) {
	if !s.disableReadRedaction {
		rec.PluginOutput = RedactOnRead(rec.PluginOutput)
	}
}

// LoadStreamChunksAfter implements StreamChunkTailReader.
//
// OFFSET 0 ROWS FETCH NEXT ... rather than LIMIT, which SQL Server does not
// have, and it requires the ORDER BY that is already there.
func (s *MSSQLStore) LoadStreamChunksAfter(ctx context.Context, workflowID string, afterStep, limit int) ([]EventRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT step, plugin_name, plugin_func, plugin_output, payload
		FROM event_history
		WHERE workflow_id = @p1 AND tenant_id = @p2
		  AND event_type = @p3
		  AND step > @p4
		ORDER BY step
		OFFSET 0 ROWS FETCH NEXT @p5 ROWS ONLY
	`, workflowID, s.tenantID, EventTypePluginCallStreamChunk, afterStep, clampStreamChunkTailLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("load stream chunks: %w", err)
	}
	defer rows.Close()

	return scanStreamChunkRows(rows, workflowID, s.redactStreamChunk, identityPayload)
}

// redactStreamChunk applies this store's read-path treatment to one chunk.
// Redaction only, for the same reason as MySQL's.
func (s *MSSQLStore) redactStreamChunk(rec *EventRecord, _ string) {
	if !s.disableReadRedaction {
		rec.PluginOutput = RedactOnRead(rec.PluginOutput)
	}
}

// identityPayload is the payload step for a store that does not encrypt it.
func identityPayload(s string) string { return s }

// LoadStreamChunksAfter implements StreamChunkTailReader by delegating to the
// shard holding this run, and reports rather than guesses when the shard cannot
// tail.
func (s *ShardedStore) LoadStreamChunksAfter(ctx context.Context, workflowID string, afterStep, limit int) ([]EventRecord, error) {
	shard := s.getShard(workflowID)
	if shard == nil {
		return nil, fmt.Errorf("load stream chunks: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	tail, ok := shard.Store.(StreamChunkTailReader)
	if !ok {
		return nil, fmt.Errorf("load stream chunks: shard for workflow %s cannot tail stream chunks", workflowID)
	}
	return tail.LoadStreamChunksAfter(ctx, workflowID, afterStep, limit)
}
