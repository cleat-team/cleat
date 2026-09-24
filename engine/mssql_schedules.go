package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *MSSQLStore) CreateSchedule(ctx context.Context, sch Schedule) error {
	if err := sch.ValidateForCreate(); err != nil {
		return err
	}
	digest := scheduleRequestDigest(sch)

	// See PostgresStore.CreateSchedule for why the key is looked up before the
	// insert rather than only on the way out of a conflict.
	if sch.IdempotencyKey != "" {
		stored, found, lerr := s.lookupScheduleKey(ctx, sch.IdempotencyKey)
		if lerr != nil {
			return fmt.Errorf("CreateSchedule: read idempotency key: %w", lerr)
		}
		if found {
			return scheduleIdempotencyVerdict(stored, digest)
		}
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_schedules (name, def_name, entry_point, cron_expression, input, disabled_at, next_run_at, tenant_id, timezone, misfire_policy, catch_up_limit, overlap_policy, idempotency_key, request_digest)
		VALUES (@p1, @p2, @p3, @p4, CAST(@p5 AS NVARCHAR(MAX)), @p6, @p7, @p8, @p9, @p10, @p11, @p12, @p13, @p14)
	`, sch.Name, sch.DefName, sch.EntryPoint, sch.CronExpression, scheduleInputJSON(sch.Input), sch.DisabledAt, sch.NextRunAt, s.tenantID,
		scheduleTimezoneOrDefault(sch.Timezone), MisfirePolicyOrDefault(sch.MisfirePolicy),
		CatchUpLimitOrDefault(sch.CatchUpLimit), OverlapPolicyOrDefault(sch.OverlapPolicy),
		nullableScheduleKey(sch.IdempotencyKey), digest)
	if err != nil {
		// Two unique constraints now: the name is the PRIMARY KEY and the key
		// has its own filtered index. Detected typed, via isMSSQLDuplicateKey,
		// and then told apart by ASKING whether the key is held rather than by
		// reading an index name out of driver text.
		//
		// No savepoint here, unlike PostgreSQL: this path runs outside a
		// transaction, so the failed INSERT leaves the connection usable.
		if isMSSQLDuplicateKey(err) {
			if sch.IdempotencyKey != "" {
				if stored, found, lerr := s.lookupScheduleKey(ctx, sch.IdempotencyKey); lerr == nil && found {
					return scheduleIdempotencyVerdict(stored, digest)
				}
			}
			return fmt.Errorf("%w: %s", ErrScheduleExists, sch.Name)
		}
		return err
	}
	return nil
}

// lookupScheduleKey reports the stored input digest for a key this tenant holds.
func (s *MSSQLStore) lookupScheduleKey(ctx context.Context, key string) (sql.NullString, bool, error) {
	var stored sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT request_digest FROM workflow_schedules
		WHERE tenant_id = @p1 AND idempotency_key = @p2
	`, s.tenantID, key).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return stored, false, nil
	}
	return stored, err == nil, err
}

func (s *MSSQLStore) ListSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, def_name, entry_point, cron_expression, input, disabled_at, next_run_at, last_run_at, timezone,
		       CONVERT(NVARCHAR(36), tenant_id) AS tenant_id,
		       misfire_policy, catch_up_limit, overlap_policy, ISNULL(last_run_id, '')
		FROM workflow_schedules WHERE tenant_id = @p1 ORDER BY name
	`, s.tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var schedules []Schedule
	for rows.Next() {
		var sch Schedule
		var lastRunAt sql.NullTime
		var inputStr string
		if err := rows.Scan(&sch.Name, &sch.DefName, &sch.EntryPoint, &sch.CronExpression,
			&inputStr, &sch.DisabledAt, &sch.NextRunAt, &lastRunAt, &sch.Timezone, &sch.TenantID,
			&sch.MisfirePolicy, &sch.CatchUpLimit, &sch.OverlapPolicy, &sch.LastRunID); err != nil {
			return nil, err
		}
		sch.Input = json.RawMessage(inputStr)
		if lastRunAt.Valid {
			sch.LastRunAt = &lastRunAt.Time
		}
		schedules = append(schedules, sch)
	}
	return schedules, rows.Err()
}

// DeleteSchedule removes one of this tenant's schedules.
//
// The tenant predicate is not belt-and-braces over dbo.fn_tenant_filter, it is
// the whole of the isolation on the connection this actually runs on. See the
// note above ClaimDueSchedule.
func (s *MSSQLStore) DeleteSchedule(ctx context.Context, name string) error {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM workflow_schedules WHERE name = @p1 AND tenant_id = @p2`,
		name, s.tenantID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrScheduleNotFound
	}

	_, err := s.db.ExecContext(ctx,
		`DELETE FROM workflow_schedules WHERE name = @p1 AND tenant_id = @p2`, name, s.tenantID)
	return err
}

func (s *MSSQLStore) SetScheduleEnabled(ctx context.Context, name string, enabled bool) error {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM workflow_schedules WHERE name = @p1 AND tenant_id = @p2`,
		name, s.tenantID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrScheduleNotFound
	}

	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_schedules
		   SET disabled_at = CASE WHEN @p2 = 1 THEN NULL ELSE COALESCE(disabled_at, SYSUTCDATETIME()) END
		 WHERE name = @p1 AND tenant_id = @p3
	`, name, enabled, s.tenantID)
	return err
}

func (s *MSSQLStore) GetDueSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, def_name, entry_point, cron_expression, input, disabled_at, next_run_at, last_run_at, timezone,
		       -- CONVERT, not the raw column. go-mssqldb scans UNIQUEIDENTIFIER
		       -- into a Go string as its 16 raw storage bytes, not the canonical
		       -- text. The scheduler loop reads Schedule.TenantID and passes it
		       -- straight to StartNewRun, which binds it back to a
		       -- UNIQUEIDENTIFIER parameter -- so the raw form fails the round
		       -- trip with "Conversion failed when converting from a character
		       -- string to uniqueidentifier" and NO schedule ever fires on SQL
		       -- Server. It also lands in the cron:<tenant>:<name>:<instant>
		       -- idempotency key, which is the at-least-once delivery guarantee.
		       CONVERT(NVARCHAR(36), tenant_id) AS tenant_id,
		       misfire_policy, catch_up_limit, overlap_policy, ISNULL(last_run_id, '')
		FROM workflow_schedules WITH (READPAST, UPDLOCK, ROWLOCK)
		WHERE disabled_at IS NULL AND next_run_at <= SYSUTCDATETIME() AND tenant_id = @p1
		ORDER BY next_run_at
	`, s.tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var schedules []Schedule
	for rows.Next() {
		var sch Schedule
		var lastRunAt sql.NullTime
		var inputStr string
		if err := rows.Scan(&sch.Name, &sch.DefName, &sch.EntryPoint, &sch.CronExpression,
			&inputStr, &sch.DisabledAt, &sch.NextRunAt, &lastRunAt, &sch.Timezone, &sch.TenantID,
			&sch.MisfirePolicy, &sch.CatchUpLimit, &sch.OverlapPolicy, &sch.LastRunID); err != nil {
			return nil, err
		}
		sch.Input = json.RawMessage(inputStr)
		if lastRunAt.Valid {
			sch.LastRunAt = &lastRunAt.Time
		}
		schedules = append(schedules, sch)
	}
	return schedules, rows.Err()
}

func (s *MSSQLStore) GetCompactionCandidates(ctx context.Context, threshold int, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT w.id
		FROM workflow_instances w
		-- LEFT, not INNER: see the PostgreSQL half in engine/db.go. A missing
		-- definition row must fall through to the global threshold rather than
		-- removing the workflow from compaction.
		--
		-- d.tenant_id = w.tenant_id IS REQUIRED FOR CORRECTNESS AND DOES NOT
		-- SCOPE ANYTHING. workflow_defs is keyed (tenant_id, name, version), so
		-- joining on name+version alone can match ANOTHER tenant's definition of
		-- the same name and read its max_history_length -- a compaction
		-- threshold silently sourced from someone else's row. That is what this
		-- line prevents.
		--
		-- It is a CORRELATION between two tables, not a restriction to the
		-- caller, and the row scoping is the separate w.tenant_id = @p3 in the
		-- WHERE below. Both are needed and they do different jobs. Said plainly
		-- because the next reader will see tenant_id in an ON clause and
		-- reasonably conclude the query is scoped -- which is exactly what
		-- TestMSSQLTenantScopedTablesAreQueriedWithATenantPredicate concluded
		-- when this join was added without the WHERE, and it was wrong.
		LEFT JOIN workflow_defs d
		       ON d.name = w.def_name AND d.version = w.def_version
		      AND d.tenant_id = w.tenant_id
		WHERE w.tenant_id = @p3
		  AND w.status IN ('ready', 'running')
		  AND (SELECT COUNT(*) FROM event_history e WHERE e.workflow_id = w.id)
		      > COALESCE(NULLIF(d.max_history_length, 0), @p1)
		  AND (w.compaction_step IS NULL OR w.compaction_step < (SELECT MAX(e2.step) FROM event_history e2 WHERE e2.workflow_id = w.id))
		ORDER BY w.created_at
		OFFSET 0 ROWS FETCH NEXT @p2 ROWS ONLY
	`, threshold, limit, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("get compaction candidates: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan compaction candidate: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *MSSQLStore) LoadCompactionState(ctx context.Context, workflowID string) (*CompactionState, error) {
	var stateRaw sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT CAST(compaction_state AS NVARCHAR(MAX)) FROM workflow_instances WHERE id = @p1
	`, workflowID).Scan(&stateRaw)
	if errors.Is(err, sql.ErrNoRows) || !stateRaw.Valid || stateRaw.String == "" {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load compaction state: %w", err)
	}
	var state CompactionState
	if err := json.Unmarshal([]byte(stateRaw.String), &state); err != nil {
		return nil, fmt.Errorf("load compaction state: unmarshal: %w", err)
	}
	return &state, nil
}

func (s *MSSQLStore) CompactHistory(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error {
	return withRollbackGuaranteedRetry(ctx, "compact history", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.compactHistoryOnce(ctx, workflowID, compactionState, compactionStep, keepStep)
	})
}

func (s *MSSQLStore) compactHistoryOnce(ctx context.Context, workflowID string, compactionState []byte, compactionStep int, keepStep int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("compact history: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Read current generation for optimistic locking.
	var gen int64
	err = tx.QueryRowContext(ctx, `SELECT generation FROM workflow_instances WHERE id = @p1`, workflowID).Scan(&gen)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return tx.Commit() // Workflow no longer exists.
		}
		return fmt.Errorf("compact history: get generation: %w", err)
	}

	// Delete events older than keepStep.
	_, err = tx.ExecContext(ctx, `
		DELETE FROM event_history
		WHERE workflow_id = @p1 AND step < @p2
	`, workflowID, keepStep)
	if err != nil {
		return fmt.Errorf("compact history: delete events: %w", err)
	}

	// Persist compaction checkpoint.
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET compaction_state = @p2, compaction_step = @p3, compacted_at = SYSUTCDATETIME()
		WHERE id = @p1 AND generation = @p4
	`, workflowID, string(compactionState), compactionStep, gen)
	if err != nil {
		return fmt.Errorf("compact history: update state: %w", err)
	}

	return tx.Commit()
}

func (s *MSSQLStore) RecordWorkflowMemorySample(ctx context.Context, defName string, sampleBytes int64) error {
	return withRollbackGuaranteedRetry(ctx, "record workflow memory sample", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.recordWorkflowMemorySampleOnce(ctx, defName, sampleBytes)
	})
}

func (s *MSSQLStore) recordWorkflowMemorySampleOnce(ctx context.Context, defName string, sampleBytes int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record memory sample: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO workflow_memory_samples (def_name, sample_bytes, tenant_id) VALUES (@p1, @p2, @p3)`,
		defName, sampleBytes, s.tenantID)
	if err != nil {
		return fmt.Errorf("record memory sample: insert sample: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		MERGE workflow_memory_stats AS target
		USING (SELECT @p1 AS def_name, @p2 AS mean_bytes, @p3 AS tenant_id) AS source
		ON target.def_name = source.def_name AND target.tenant_id = @p3
		WHEN MATCHED THEN UPDATE SET
			mean_bytes  = target.alpha * @p2 + (1 - target.alpha) * target.mean_bytes,
			sample_count = target.sample_count + 1,
			updated_at  = SYSUTCDATETIME()
		WHEN NOT MATCHED THEN INSERT (def_name, mean_bytes, sample_count, updated_at, tenant_id)
			VALUES (@p1, @p2, 1, SYSUTCDATETIME(), @p3);
	`, defName, float64(sampleBytes), s.tenantID)
	if err != nil {
		return fmt.Errorf("record memory sample: upsert stats: %w", err)
	}

	return tx.Commit()
}

func (s *MSSQLStore) LoadMemoryEstimates(ctx context.Context) (map[string]float64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT def_name, mean_bytes FROM workflow_memory_stats WHERE tenant_id = @p1
	`, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("load memory estimates: %w", err)
	}
	defer rows.Close()

	estimates := make(map[string]float64)
	for rows.Next() {
		var name string
		var mean float64
		if err := rows.Scan(&name, &mean); err != nil {
			return nil, fmt.Errorf("load memory estimates: scan: %w", err)
		}
		estimates[name] = mean
	}
	return estimates, rows.Err()
}

func (s *MSSQLStore) LoadMemoryStats(ctx context.Context) ([]WorkflowMemoryStats, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT def_name,
			MIN(sample_bytes) OVER (PARTITION BY def_name),
			AVG(CAST(sample_bytes AS FLOAT)) OVER (PARTITION BY def_name),
			MAX(sample_bytes) OVER (PARTITION BY def_name),
			CAST(PERCENTILE_CONT(0.10) WITHIN GROUP (ORDER BY CAST(sample_bytes AS FLOAT)) OVER (PARTITION BY def_name) AS BIGINT),
			CAST(PERCENTILE_CONT(0.25) WITHIN GROUP (ORDER BY CAST(sample_bytes AS FLOAT)) OVER (PARTITION BY def_name) AS BIGINT),
			CAST(PERCENTILE_CONT(0.50) WITHIN GROUP (ORDER BY CAST(sample_bytes AS FLOAT)) OVER (PARTITION BY def_name) AS BIGINT),
			CAST(PERCENTILE_CONT(0.75) WITHIN GROUP (ORDER BY CAST(sample_bytes AS FLOAT)) OVER (PARTITION BY def_name) AS BIGINT),
			CAST(PERCENTILE_CONT(0.90) WITHIN GROUP (ORDER BY CAST(sample_bytes AS FLOAT)) OVER (PARTITION BY def_name) AS BIGINT),
			CAST(PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY CAST(sample_bytes AS FLOAT)) OVER (PARTITION BY def_name) AS BIGINT),
			COUNT(*) OVER (PARTITION BY def_name)
		FROM workflow_memory_samples
		WHERE tenant_id = @p1
		ORDER BY def_name
	`, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("load memory stats: %w", err)
	}
	defer rows.Close()

	var stats []WorkflowMemoryStats
	for rows.Next() {
		var st WorkflowMemoryStats
		if err := rows.Scan(&st.DefName, &st.MinBytes, &st.AvgBytes, &st.MaxBytes,
			&st.P10, &st.P25, &st.P50, &st.P75, &st.P90, &st.P99, &st.SampleCount); err != nil {
			return nil, fmt.Errorf("load memory stats: scan: %w", err)
		}
		stats = append(stats, st)
	}
	return stats, rows.Err()
}

func (s *MSSQLStore) CleanupMemorySamples(ctx context.Context, maxSamplesPerDef int) (int64, error) {
	defRows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT def_name FROM workflow_memory_samples WHERE tenant_id = @p1`, s.tenantID)
	if err != nil {
		return 0, fmt.Errorf("cleanup memory samples: list defs: %w", err)
	}
	defer defRows.Close()

	var defNames []string
	for defRows.Next() {
		var name string
		if err := defRows.Scan(&name); err != nil {
			return 0, fmt.Errorf("cleanup memory samples: scan def: %w", err)
		}
		defNames = append(defNames, name)
	}
	if err := defRows.Err(); err != nil {
		return 0, err
	}

	var totalDeleted int64
	for _, defName := range defNames {
		result, err := s.db.ExecContext(ctx, `
			DELETE FROM workflow_memory_samples
			WHERE def_name = @p1
			  AND tenant_id = @p3
			  AND id NOT IN (
			      SELECT id FROM (
				  SELECT id, ROW_NUMBER() OVER (ORDER BY recorded_at DESC) AS rn
				  FROM workflow_memory_samples
				  WHERE def_name = @p1
				    AND tenant_id = @p3
			      ) AS ranked
			      WHERE ranked.rn <= @p2
			  )
		`, defName, maxSamplesPerDef, s.tenantID)
		if err != nil {
			return totalDeleted, fmt.Errorf("cleanup memory samples: delete %s: %w", defName, err)
		}
		n, _ := result.RowsAffected()
		totalDeleted += n
	}
	return totalDeleted, nil
}

// mssqlIDChunk is the id-list chunk size deleteByWorkflowIDs and
// chunkedMarkHistorySwept use, kept under SQL Server's 2100-parameter cap.
//
// mssqlEventRowChunk bounds how many event_history rows a single DELETE may
// remove -- cleat#2060. SQL Server escalates row/page locks to a table lock
// once a statement holds ~5000 locks on one object; event_history is the one
// table in this file where a workflow can own an unbounded number of rows, so
// it is the only delete bounded by ROW count rather than workflow count. Set
// well under that threshold, and equal to mssqlIDChunk so a statement that
// happens to delete one event per workflow still cannot mark more than
// mssqlIDChunk workflows swept -- see chunkedMarkHistorySwept, which chunks
// regardless rather than relying on that coincidence.
const (
	mssqlIDChunk       = 2000
	mssqlEventRowChunk = 2000

	// mssqlInterleaveChunk bounds how many workflow ids deleteWorkflowsBatchOnce
	// carries through event_history's delete AND workflow_instances's delete
	// together before moving to the next chunk. cleat#2060, and this is the
	// part row-bounding event_history's OWN delete does not cover.
	//
	// event_history's FK to workflow_instances is ON DELETE CASCADE (see
	// mssqlWorkflowChildTables's comment), so deleting a workflow_instances
	// row makes SQL Server verify no event_history row still references it.
	// That check still takes locks on event_history even when it finds
	// nothing to cascade -- ghost records from event_history's OWN
	// just-completed delete are still physically present until a background
	// task reclaims them, and the RI check locks its way past them to
	// confirm there is no LIVE row left.
	//
	// Bisected directly against TestMSSQLRetentionSweepsCauseNoLockEscalation's
	// scenario (6,000 workflows, 5 events each, deleted then their parents
	// deleted): chunks of 100 and 50 still intermittently escalated by 1;
	// 25 and 10 held at 0 across repeated trials. Set to 20 for headroom
	// against workflows that accumulate more events than that test's scale
	// before compaction catches up, at the cost of more, smaller
	// transactions -- an acceptable trade for a background sweep, and the
	// one deleteWorkflowsBatchOnce's interleaved loop is built around. See
	// its comment for the two designs that were tried and measured first.
	mssqlInterleaveChunk = 20
)

// mssqlDeleteExpiredEventsQuery bounds event_history rows removed per
// statement to mssqlEventRowChunk (cleat#2060) rather than the number of
// workflows scanned to find them -- a workflow batch of up to 10000 candidate
// ids can own far more than mssqlEventRowChunk events between them. Built
// once at package init since msExpiredEventsWorkflows is itself a constant.
var mssqlDeleteExpiredEventsQuery = fmt.Sprintf(`
			DELETE TOP (%d) FROM event_history
			OUTPUT deleted.workflow_id
			WHERE workflow_id IN (
				SELECT id`+msExpiredEventsWorkflows+`
				  AND tenant_id = @p2
				ORDER BY id
				OFFSET 0 ROWS FETCH NEXT 10000 ROWS ONLY
			)
		`, mssqlEventRowChunk)

func (s *MSSQLStore) DeleteExpiredEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	var out int64
	err := withRollbackGuaranteedRetry(ctx, "delete expired events", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		var err error
		out, err = s.deleteExpiredEventsOnce(ctx, olderThan)
		return err
	})
	if err != nil {
		return 0, err
	}
	return out, nil
}

// deleteExpiredEventsOnce also marks history_swept_at on every workflow it
// actually swept, so ReReplay's pending-intent guard (engine/admin_ops.go)
// can tell "never attempted" from "swept, outcome unknown" -- see
// PostgresStore.DeleteExpiredEvents in engine/db.go for the full cleat#2038
// reasoning; this is the MSSQL implementation of the same fix, using OUTPUT
// deleted.workflow_id as the RETURNING-equivalent.
func (s *MSSQLStore) deleteExpiredEventsOnce(ctx context.Context, olderThan time.Time) (int64, error) {
	var totalDeleted int64
	for {
		tx, err := s.beginTxWithContext(ctx)
		if err != nil {
			return totalDeleted, fmt.Errorf("delete expired events: begin: %w", err)
		}
		// OUTPUT deleted.workflow_id, not a separate re-run of the same
		// predicate: ties history_swept_at to the workflows this statement
		// actually removed rows for. A workflow_id can repeat (multiple
		// event_history rows); deduplicated below before the UPDATE.
		rows, err := tx.QueryContext(ctx, mssqlDeleteExpiredEventsQuery, olderThan, s.tenantID)
		if err != nil {
			tx.Rollback()
			return totalDeleted, fmt.Errorf("delete expired events: %w", err)
		}
		// rowsDeleted counts EVENT ROWS (what totalDeleted has always
		// counted); swept collects the DISTINCT workflow ids, for the
		// UPDATE below.
		var rowsDeleted int64
		seen := make(map[string]struct{})
		var swept []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				tx.Rollback()
				return totalDeleted, fmt.Errorf("delete expired events: scan: %w", err)
			}
			rowsDeleted++
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				swept = append(swept, id)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			tx.Rollback()
			return totalDeleted, fmt.Errorf("delete expired events: rows: %w", err)
		}
		rows.Close()

		// cleat#2038: mark, don't just delete -- ReReplay's pending-intent
		// guard needs to tell "never attempted" from "swept, outcome
		// unknown" apart, and both currently read as empty history.
		//
		// No tenant_id predicate inside chunkedMarkHistorySwept: swept is
		// scopedByCaller, not missing a check. Every id in it came from
		// THIS function's own SELECT above (msExpiredEventsWorkflows AND
		// tenant_id = @p2), so it cannot name another tenant's workflow --
		// see the tenantPredicateAllowlist entry below.
		//
		// cleat#2103: chunked rather than one IN (...) of arbitrary size --
		// mssqlEventRowChunk bounds this statement's own DELETE to at most
		// mssqlEventRowChunk distinct workflows when every swept row is a
		// different workflow, which already keeps swept under SQL Server's
		// 2100-parameter cap today. Chunking here anyway, rather than
		// relying on that, is what keeps it true if mssqlEventRowChunk is
		// ever raised independently of this UPDATE.
		if err := s.chunkedMarkHistorySwept(ctx, tx, swept); err != nil {
			tx.Rollback()
			return totalDeleted, fmt.Errorf("delete expired events: mark swept: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return totalDeleted, fmt.Errorf("delete expired events: commit: %w", err)
		}
		totalDeleted += rowsDeleted
		if rowsDeleted == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	return totalDeleted, nil
}

// ClearExpiredCompactionState clears compaction bookkeeping -- compaction_state,
// compaction_step, compacted_at -- on terminal workflows older than the cutoff.
//
// SPLIT OUT OF DeleteExpiredEvents, and the reason is a metric rather than
// tidiness. It used to be a second loop inside that function whose RowsAffected
// was discarded, so the sweep reported "deleted 0 rows" on runs where it had
// done real work.
//
// THIS COMMENT USED TO SAY THE FIRST LOOP "CAN NEVER MATCH" -- that finalize
// already purges those events -- citing cleat#1016. That was wrong about which
// code path a 'failed' workflow takes: finalize_workflow_status purges a
// 'done' workflow's events, not a 'failed' one's, and DeleteExpiredEvents is
// what removes a 'failed' workflow's events, --retention-days days later.
// See engine/db.go's PostgresStore.DeleteExpiredEvents doc comment and
// engine/retention_predicates.go for the full correction, found via cleat#2038
// while grounding cleat#1999's TLA+ model in source.
//
// Summing the two into one return was the obvious fix and the wrong one. They
// are different tables, different operations and different units -- deleted
// event_history rows against updated workflow_instances rows -- under a counter
// documented as "expired event history rows deleted". cleat#1024.
func (s *MSSQLStore) ClearExpiredCompactionState(ctx context.Context, olderThan time.Time) (int64, error) {
	var out int64
	err := withRollbackGuaranteedRetry(ctx, "clear expired compaction state", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		var err error
		out, err = s.clearExpiredCompactionStateOnce(ctx, olderThan)
		return err
	})
	if err != nil {
		return 0, err
	}
	return out, nil
}

func (s *MSSQLStore) clearExpiredCompactionStateOnce(ctx context.Context, olderThan time.Time) (int64, error) {
	var totalCleared int64
	// Batched, so a large backlog does not hold one transaction open.
	for {
		tx, err := s.beginTxWithContext(ctx)
		if err != nil {
			return totalCleared, fmt.Errorf("delete expired events: begin compaction: %w", err)
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET compaction_state = NULL, compaction_step = NULL, compacted_at = NULL
			WHERE id IN (
				SELECT id`+msExpiredCompactionState+`
				  AND tenant_id = @p2
				ORDER BY id
				OFFSET 0 ROWS FETCH NEXT 10000 ROWS ONLY
			)
		`, olderThan, s.tenantID)
		if err != nil {
			tx.Rollback()
			break
		}
		if err := tx.Commit(); err != nil {
			break
		}
		n, _ := result.RowsAffected()
		totalCleared += n
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return totalCleared, nil
}

func (s *MSSQLStore) DeleteDeadLetteredWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	var totalDeleted int64
	for {
		n, err := s.deleteWorkflowsBatch(ctx, mssqlSelectDeadLetteredBatch, "delete dead-lettered workflows", olderThan)
		if err != nil {
			return totalDeleted, err
		}
		totalDeleted += n
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return totalDeleted, nil
}

// DeleteCompletedWorkflows permanently deletes workflow_instances rows in a
// terminal, no-further-action status ('done', 'failed', 'terminated') whose
// completed_at is older than the cutoff. 'dead_lettered' is deliberately
// excluded -- see the interface doc (store_interface.go) and
// DeleteDeadLetteredWorkflows above.
//
// migrations/mssql/001_schema.sql declares event_history's FK to
// workflow_instances ON DELETE CASCADE, and SQL Server never dropped it
// (only PostgreSQL did, deliberately) -- so deleting the workflow_instances
// row below WOULD cascade event_history (and workflow_signals,
// workflow_promises, concurrency_keys, workflow_update_requests)
// automatically, correctness-wise. This still runs an explicit,
// row-bounded event_history delete first (deleteWorkflowsBatchOnce, via
// deleteEventHistoryRowBoundedCommitting) rather than relying on that --
// an uncontrolled cascade over a workflow that has accumulated years of
// events is exactly the unbounded-statement shape cleat#2060 exists to
// avoid, and cascade gives the caller no way to bound it. See
// mssqlWorkflowChildTables's comment for the fuller FK picture, confirmed
// live against a real SQL Server built from these migrations (cleat#2060).
func (s *MSSQLStore) DeleteCompletedWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	var totalDeleted int64
	for {
		n, err := s.deleteWorkflowsBatch(ctx, mssqlSelectCompletedBatch, "delete completed workflows", olderThan)
		if err != nil {
			return totalDeleted, err
		}
		totalDeleted += n
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return totalDeleted, nil
}

// mssqlWorkflowChildTables are the tables a workflow_instances row owns, in
// deletion order.
//
// THIS COMMENT USED TO SAY SQL SERVER DECLARES NO FOREIGN KEYS TO
// workflow_instances, citing a 0 from `grep -c "REFERENCES workflow_instances"
// migrations/mssql/*.sql` against cleat#1265's original audit. That grep
// pattern is missing the schema qualifier the migrations actually use --
// `grep -o "REFERENCES dbo.workflow_instances" migrations/mssql/*.sql | wc -l`
// is 6, not 0 (five in the tables below, one more on queue_holders, which
// isn't), and confirmed live against a real SQL Server built from these
// migrations (cleat#2060):
//
//	SELECT OBJECT_NAME(parent_object_id), delete_referential_action_desc
//	FROM sys.foreign_keys WHERE referenced_object_id = OBJECT_ID('dbo.workflow_instances')
//
// returns event_history, workflow_signals, workflow_promises, concurrency_keys
// and workflow_update_requests, every one ON DELETE CASCADE -- five of the six
// tables below. Only idempotency_keys has no FK to workflow_instances at all.
//
// So deleting workflow_instances DOES cascade-remove five of these six tables
// on its own, and the explicit deletes below are not what cleat#1265's fix
// assumed. They still matter for a different reason: cascade gives no control
// over HOW the removal happens, and for event_history specifically that
// matters a great deal -- see deleteEventHistoryRowBoundedCommitting and
// cleat#2060, where an unbounded delete (which is what an uncontrolled
// cascade would run) is the exact failure this file exists to avoid. The
// explicit deletes below run first so each table's own delete is the one
// that actually removes its rows; the cascade that follows when
// workflow_instances is deleted finds nothing left to touch.
//
// Not event_awaiters or workflow_blob_refs: those carry a workflow_id but are
// owned by the eventtriggers and blobstore plugins, each with its own
// migrations.go. Whether plugin-owned rows should follow the run is a real
// question and a different one.
var mssqlWorkflowChildTables = []string{
	"event_history",
	"idempotency_keys",
	"concurrency_keys",
	"workflow_signals",
	"workflow_promises",
	"workflow_update_requests",
}

// mssqlDeleteByWorkflowPrefix holds the complete, constant head of each delete.
//
// The table and column are NOT formatted into the statement at run time. Every
// candidate is spelled out here, so the only text ever appended is the generated
// placeholder list (@id0, @id1, ...) -- which also keeps gosec's G201 satisfied
// without a #nosec, since there is no SQL string formatting left to audit.
var mssqlDeleteByWorkflowPrefix = map[string]string{
	"event_history":            "DELETE FROM event_history WHERE workflow_id IN (",
	"idempotency_keys":         "DELETE FROM idempotency_keys WHERE workflow_id IN (",
	"concurrency_keys":         "DELETE FROM concurrency_keys WHERE workflow_id IN (",
	"workflow_signals":         "DELETE FROM workflow_signals WHERE workflow_id IN (",
	"workflow_promises":        "DELETE FROM workflow_promises WHERE workflow_id IN (",
	"workflow_update_requests": "DELETE FROM workflow_update_requests WHERE workflow_id IN (",
	"workflow_instances":       "DELETE FROM workflow_instances WHERE id IN (",
}

// mssqlDeleteEventHistoryTopPrefix is deleteEventHistoryRowBoundedCommitting's own head,
// separate from mssqlDeleteByWorkflowPrefix["event_history"] because a
// TOP (N) delete is a different statement, not that prefix with text spliced
// in front of it -- "DELETE TOP (%d) " + "DELETE FROM event_history..." is
// two DELETE keywords in one statement and SQL Server rejects it outright
// (Incorrect syntax near the keyword 'DELETE'), caught the first time this
// path ran against a real database rather than in gofmt/go vet, which have
// no way to know either string is SQL.
var mssqlDeleteEventHistoryTopPrefix = fmt.Sprintf(
	"DELETE TOP (%d) FROM event_history WHERE workflow_id IN (", mssqlEventRowChunk)

// deleteCompletedWorkflowsBatch deletes one batch, retrying the whole
// transaction on a rollback-guaranteed failure.
//
// Required on this dialect, and TestEveryMSSQLTransactionBoundaryIsRetried
// caught its absence: SQL Server rolls a deadlock victim back itself, so the
// failure reaches Go as an ordinary error and the batch would simply be lost
// rather than replayed. Every other MSSQL path that opens a transaction is
// wrapped the same way.
func (s *MSSQLStore) deleteWorkflowsBatch(ctx context.Context, selectSQL, label string, olderThan time.Time) (int64, error) {
	var deleted int64
	err := withRollbackGuaranteedRetry(ctx, label, mssqlTxRetries, mssqlTxRetryDelay, func() error {
		n, err := s.deleteWorkflowsBatchOnce(ctx, selectSQL, label, olderThan)
		if err != nil {
			return err
		}
		deleted = n
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// deleteCompletedWorkflowsBatchOnce deletes up to 10000 terminal workflow
// instances and the rows they own, in ONE transaction.
//
// The transaction is the point, and the previous shape had none: children were
// deleted by one statement and parents by another, so a failure between them
// left rows whose owner was gone with nothing to find them by. This mirrors the
// PostgreSQL batch in engine/db.go, which selects the batch first and deletes
// against that fixed id set rather than re-evaluating the predicate per
// statement.
// mssqlSelectCompletedBatch and mssqlSelectDeadLetteredBatch differ in one
// line -- the status predicate -- and everything after the SELECT is shared.
//
// WRITTEN AS TWO CONSTANTS RATHER THAN ONE FORMAT STRING so no SQL is built at
// run time, and passed to one batch function rather than copied into two, which
// is the half of cleat#1265 this closes. The completed sweep's comment used to
// cite the dead-letter sweep as its "verified reference" for the claim that
// SQL Server cascades; neither was verified, and the citation is what made the
// copy look supported. Two functions sharing a premise is how that happened,
// so they now share the code instead.
const mssqlSelectCompletedBatch = `
		SELECT id` + msCompletedWorkflows + `
		  AND tenant_id = @p2
		ORDER BY id
		OFFSET 0 ROWS FETCH NEXT 10000 ROWS ONLY`

const mssqlSelectDeadLetteredBatch = `
		SELECT id` + msDeadLetteredWorkflows + `
		  AND tenant_id = @p2
		ORDER BY id
		OFFSET 0 ROWS FETCH NEXT 10000 ROWS ONLY`

func (s *MSSQLStore) deleteWorkflowsBatchOnce(ctx context.Context, selectSQL, label string, olderThan time.Time) (int64, error) {
	// Plain read, no transaction: RCSI (required in production, asserted by
	// TestMSSQLRetentionSweepsCauseNoLockEscalation in test) gives this a
	// versioned, non-blocking read, and nothing below depends on the
	// candidate set being read inside the same transaction that deletes it.
	rows, err := s.db.QueryContext(ctx, selectSQL,
		sql.Named("p1", olderThan), sql.Named("p2", s.tenantID))
	if err != nil {
		return 0, fmt.Errorf("%s: select batch: %w", label, err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("%s: scan: %w", label, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("%s: rows: %w", label, err)
	}
	rows.Close()

	if len(ids) == 0 {
		return 0, nil
	}

	// INTERLEAVED, small-chunk delete -- cleat#2060, and the shape here is
	// load-bearing, not a style choice. It went through three designs before
	// this one, and TestMSSQLRetentionSweepsCauseNoLockEscalation caught
	// each of the first two failing for a different reason:
	//
	//  1. Delete ALL of event_history for the whole batch first (row-bounded,
	//     committing per statement), THEN delete workflow_instances
	//     afterward, chunked at mssqlIDChunk in one shared transaction.
	//     event_history's OWN delete never escalated. workflow_instances did
	//     -- deleting a workflow_instances row fires a referential-integrity
	//     check against event_history (mssqlWorkflowChildTables's comment
	//     has the FK detail), and that check still takes locks on
	//     event_history even when it finds nothing to cascade, because the
	//     rows it must rule out are GHOST RECORDS: physically still present
	//     after a DELETE until a background task reclaims them. 6,000
	//     already-emptied ids, one shared transaction: +1 escalation.
	//
	//  2. Same split, but commit the workflow_instances delete per chunk
	//     too. This made it WORSE, not better -- 15 chunks of 400 gave +15,
	//     60 chunks of 100 gave +60 -- because the ghost backlog from step 1
	//     is a property of the WHOLE prior event_history delete, not of any
	//     one chunk, so every later transaction's RI check pays the same
	//     tax regardless of its own size, and more transactions means more
	//     chances to cross the threshold.
	//
	// What actually worked: keep the ghost backlog SMALL by never letting it
	// accumulate across the whole batch in the first place. Each small
	// id-chunk deletes its OWN event_history rows and then IMMEDIATELY
	// deletes its OWN workflow_instances (and other child) rows, before the
	// next chunk's event_history delete creates any more ghosts. Measured at
	// mssqlInterleaveChunk=20 over 6,000 ids (300 chunk-pairs): 0 escalations
	// across 3 repeated trials, where 100 and 50 still intermittently gave
	// +1. See mssqlInterleaveChunk's own comment for the full bisection.
	//
	// Losing whole-batch atomicity (previously up to 10000 ids in one
	// transaction) is safe here for the same reason it was in the two
	// designs above: a crash between chunks leaves the remaining ids
	// exactly where the next sweep's SELECT finds them again, and a chunk
	// that has already committed -- events and parent row both -- is simply
	// not reselected.
	var totalDeleted int64
	for start := 0; start < len(ids); start += mssqlInterleaveChunk {
		end := start + mssqlInterleaveChunk
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		if err := s.deleteEventHistoryRowBoundedCommitting(ctx, chunk); err != nil {
			return totalDeleted, err
		}
		if err := s.deleteWorkflowsChunkOnce(ctx, chunk); err != nil {
			return totalDeleted, err
		}
		totalDeleted += int64(len(chunk))
	}
	return totalDeleted, nil
}

// deleteWorkflowsChunkOnce deletes one mssqlInterleaveChunk-sized (or
// smaller) slice of workflow ids from every remaining child table and from
// workflow_instances itself, in one short transaction. Called immediately
// after that same chunk's event_history rows are deleted -- see
// deleteWorkflowsBatchOnce for why the two are interleaved per small chunk
// rather than run as two separate passes over the whole batch, and why
// event_history is not in the loop below (it was already deleted, outside
// any transaction this function opens).
//
// SQL Server has no array parameter, so the id list is expanded into named
// placeholders. Never interpolated: these ids come from the database, but a
// value that round-trips is still a value. The non-event_history tables need
// no row-bounding of their own: a workflow owns at most a handful of rows in
// each of them (a signal, a promise, an idempotency key), so
// deleteByWorkflowIDs's id-chunking -- already satisfied here, since chunk is
// far below mssqlIDChunk -- already bounds rows per statement well under the
// escalation threshold.
func (s *MSSQLStore) deleteWorkflowsChunkOnce(ctx context.Context, chunk []string) error {
	return withRollbackGuaranteedRetry(ctx, "delete workflows chunk", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.deleteWorkflowsChunkOnceOnce(ctx, chunk)
	})
}

// deleteWorkflowsChunkOnceOnce is deleteWorkflowsChunkOnce's own transaction
// body, split out so TestEveryMSSQLTransactionBoundaryIsRetried can see it is
// reached through withRollbackGuaranteedRetry -- that test keys retry
// coverage on a SEPARATELY NAMED function called by selector from inside the
// retry closure (the same shape CompactHistory/compactHistoryOnce already
// use), not on a BeginTx/Commit pair living directly in the wrapped function.
// The interleaved small chunks deleteWorkflowsBatchOnce drives this from are
// exactly the shape most likely to collide with a concurrent writer and be
// chosen as a deadlock victim, which SQL Server rolls back itself -- reaching
// the caller as an ordinary error the retry wrapper needs to see, not one
// this function should ever swallow unretried.
func (s *MSSQLStore) deleteWorkflowsChunkOnceOnce(ctx context.Context, chunk []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	for _, table := range mssqlWorkflowChildTables {
		if table == "event_history" {
			continue
		}
		if err := s.deleteByWorkflowIDs(ctx, tx, table, chunk); err != nil {
			return err
		}
	}
	if err := s.deleteByWorkflowIDs(ctx, tx, "workflow_instances", chunk); err != nil {
		return err
	}
	return tx.Commit()
}

// deleteByWorkflowIDs deletes rows keyed to the given workflow ids, in chunks.
//
// SQL Server caps a statement at 2100 parameters, and the batch above is 10000
// ids, so a single IN (...) would fail with "too many parameters" -- on the
// large batches only, which is the shape that would have passed every test and
// failed on the first real retention run.
func (s *MSSQLStore) deleteByWorkflowIDs(ctx context.Context, tx *sql.Tx, table string, ids []string) error {
	prefix, ok := mssqlDeleteByWorkflowPrefix[table]
	if !ok {
		return fmt.Errorf("delete completed workflows: no delete defined for table %q", table)
	}
	for start := 0; start < len(ids); start += mssqlIDChunk {
		end := start + mssqlIDChunk
		if end > len(ids) {
			end = len(ids)
		}
		part := ids[start:end]
		placeholders := make([]string, len(part))
		args := make([]any, 0, len(part))
		for i, id := range part {
			name := fmt.Sprintf("id%d", i)
			placeholders[i] = "@" + name
			args = append(args, sql.Named(name, id))
		}
		stmt := prefix + strings.Join(placeholders, ", ") + ")"
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			return fmt.Errorf("delete completed workflows: delete %s: %w", table, err)
		}
	}
	return nil
}

// deleteEventHistoryRowBoundedCommitting deletes event_history rows for the
// given workflow ids, bounded to mssqlEventRowChunk ROWS per statement AND
// COMMITTING AFTER EVERY STATEMENT -- cleat#2060. ids is first chunked at
// mssqlIDChunk for the 2100-parameter cap (cleat#2103, same as
// deleteByWorkflowIDs), but a chunk of mssqlIDChunk workflows can still own
// far more than mssqlEventRowChunk events between them, so each chunk's
// delete repeats until nothing more matches it -- the same "loop a bounded
// statement to zero" shape deleteExpiredEventsOnce's outer loop already
// uses, one level down.
//
// EACH STATEMENT GETS ITS OWN TRANSACTION, and that is not incidental: a row
// lock is held until its transaction COMMITS, not until the statement that
// took it returns. A first version of this function took one tx from its
// caller and ran every DELETE TOP through it, and locks from every prior
// iteration were still held when the next one started -- so a batch large
// enough to need several iterations could still cross the escalation
// threshold on their SUM, even with every individual statement safely under
// it. TestMSSQLRetentionSweepsCauseNoLockEscalation caught this on the first
// real run, on DeleteDeadLetteredWorkflows: 6,000 workflows x 5 events is 15
// iterations of 2,000 rows each, comfortably over 5,000 well before the
// 15th. Committing here, rather than only in the caller's transaction, is
// what deleteWorkflowsBatchOnce's split relies on -- see its comment for why
// running this ahead of and outside that transaction is safe to redo.
//
// The other tables in mssqlWorkflowChildTables do not need this: a workflow
// owns at most a handful of rows in each of them (a signal, a promise, an
// idempotency key), so id-chunking alone already bounds rows per statement.
// event_history is the one table a workflow can own an unbounded number of
// rows in.
func (s *MSSQLStore) deleteEventHistoryRowBoundedCommitting(ctx context.Context, ids []string) error {
	for start := 0; start < len(ids); start += mssqlIDChunk {
		end := start + mssqlIDChunk
		if end > len(ids) {
			end = len(ids)
		}
		part := ids[start:end]
		placeholders := make([]string, len(part))
		args := make([]any, 0, len(part))
		for i, id := range part {
			name := fmt.Sprintf("id%d", i)
			placeholders[i] = "@" + name
			args = append(args, sql.Named(name, id))
		}
		inClause := strings.Join(placeholders, ", ") + ")"
		stmt := mssqlDeleteEventHistoryTopPrefix + inClause
		for {
			n, err := s.deleteEventHistoryChunkOnce(ctx, stmt, args)
			if err != nil {
				return err
			}
			if n == 0 {
				break
			}
		}
	}
	return nil
}

// deleteEventHistoryChunkOnce runs one bounded DELETE TOP statement in its
// own short transaction and returns the rows it removed. Split out of
// deleteEventHistoryRowBoundedCommitting so the retry-on-rollback-guaranteed
// wrapper every other MSSQL transaction in this file uses (see
// deleteWorkflowsBatch above) covers this one too, rather than leaving a
// deadlock victim here to surface as a bare, unretried error.
func (s *MSSQLStore) deleteEventHistoryChunkOnce(ctx context.Context, stmt string, args []any) (int64, error) {
	var n int64
	err := withRollbackGuaranteedRetry(ctx, "delete event_history chunk", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		var innerErr error
		n, innerErr = s.deleteEventHistoryChunkOnceOnce(ctx, stmt, args)
		return innerErr
	})
	if err != nil {
		return 0, fmt.Errorf("delete completed workflows: %w", err)
	}
	return n, nil
}

// deleteEventHistoryChunkOnceOnce is deleteEventHistoryChunkOnce's own
// transaction body -- named and reached the same way
// deleteWorkflowsChunkOnceOnce is; see that function's comment for why the
// split exists (TestEveryMSSQLTransactionBoundaryIsRetried).
func (s *MSSQLStore) deleteEventHistoryChunkOnceOnce(ctx context.Context, stmt string, args []any) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, stmt, args...)
	if err != nil {
		return 0, fmt.Errorf("delete event_history: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete event_history: rows affected: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// chunkedMarkHistorySwept sets history_swept_at on every id in ids, chunked
// at mssqlIDChunk -- cleat#2103. No tenant_id predicate: every caller passes
// ids sourced from its own already-tenant-scoped SELECT (see the call site
// in deleteExpiredEventsOnce), never from user input -- see the
// tenantPredicateAllowlist entry for this function.
func (s *MSSQLStore) chunkedMarkHistorySwept(ctx context.Context, tx *sql.Tx, ids []string) error {
	for start := 0; start < len(ids); start += mssqlIDChunk {
		end := start + mssqlIDChunk
		if end > len(ids) {
			end = len(ids)
		}
		part := ids[start:end]
		placeholders := make([]string, len(part))
		args := make([]any, len(part))
		for i, id := range part {
			name := fmt.Sprintf("id%d", i)
			placeholders[i] = "@" + name
			args[i] = sql.Named(name, id)
		}
		//nolint:gosec // G202: the only concatenated fragment is placeholders, built above
		// as "@id0", "@id1", ... -- ids are bound as arguments, never interpolated.
		if _, err := tx.ExecContext(ctx, `
			UPDATE workflow_instances
			SET history_swept_at = SYSUTCDATETIME()
			WHERE id IN (`+strings.Join(placeholders, ",")+`)
		`, args...); err != nil {
			return fmt.Errorf("mark swept: %w", err)
		}
	}
	return nil
}

// scheduleInputJSON renders a schedule's input for a SQL Server text column.
//
// json.RawMessage is a []byte, and go-mssqldb binds a []byte as VARBINARY. The
// value that reached workflow_schedules.input was therefore the binary
// rendering of the JSON, not the JSON -- which the shipped schema rejects
// outright:
//
//	The INSERT statement conflicted with the CHECK constraint
//	"ck_workflow_schedules_input" ... column 'input'
//
// so CreateSchedule could not create a schedule on any SQL Server built from
// migrations/mssql/001_schema.sql. Nothing caught it because engine/testutil's
// MSSQL schema declares no CHECK constraint, so the malformed value went in
// and every test passed. IMPROVEMENT-PLAN 3.16.
//
// The empty-input handling this used to do inline now lives in
// scheduleInputOrDefault, which every dialect's CreateSchedule calls: the
// reason it gives is a property of the shipped schemas rather than of SQL
// Server, and Postgres and MySQL went without it until then. What stays here
// is the rendering -- go-mssqldb needs a string for the NVARCHAR cast, which
// is what made this file the one that noticed.
func scheduleInputJSON(input json.RawMessage) string {
	return string(scheduleInputOrDefault(input))
}

// ClaimDueSchedule advances a schedule's next_run_at, but only if it still
// holds expectedNextRun. See the interface doc for why this is a CAS.
//
// `AND tenant_id` on this and the four statements above it is load-bearing
// rather than defensive, and the reason is specific to SQL Server.
// dbo.fn_tenant_filter admits any connection whose login is a member of
// dbo.cleat_admin, regardless of SESSION_CONTEXT (012_admin_role.sql) -- and a
// multi-tenant deployment must grant that role, because
// GetDueSchedulesAcrossTenants and ClaimReadyAcrossTenants require it and
// without them a non-default tenant's workflows never fire at all. WithTenant
// copies the store and shares s.db, so on such a deployment every
// tenant-scoped store is running unfiltered and a name-only predicate reaches
// every tenant's rows.
//
// PostgreSQL does not have this problem because its exemption is a separate
// role owning a SECURITY DEFINER function; the application role keeps
// BYPASSRLS off. Here the exemption is in the predicate, which cannot tell
// which statement is asking, so each statement has to say so itself.
//
// Measured in engine/mssql_admin_login_schedule_tenant_test.go: without these
// predicates one tenant deletes, disables and reschedules another tenant's
// cron schedules through the ordinary HTTP API.
func (s *MSSQLStore) ClaimDueSchedule(ctx context.Context, name string, expectedNextRun, newNextRun time.Time, runID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE workflow_schedules
		SET next_run_at = @p2, last_run_at = SYSUTCDATETIME(),
		    last_run_id = CASE WHEN @p4 = '' THEN last_run_id ELSE @p4 END
		WHERE name = @p1 AND next_run_at = @p3 AND tenant_id = @p5
	`, name, newNextRun, expectedNextRun, runID, s.tenantID)
	if err != nil {
		return false, fmt.Errorf("ClaimDueSchedule: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ClaimDueSchedule: rows affected: %w", err)
	}
	return n == 1, nil
}

// The two values admin.rls_predicate_form can hold. They are spelled here and
// in migrations/mssql/075 and migrations/mssql/optional/cross_tenant_claim.sql,
// and the table's own CHECK constraint refuses anything else -- so a typo in a
// migration fails at apply time rather than reading as "not admin" here.
