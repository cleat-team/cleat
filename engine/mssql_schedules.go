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
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_schedules (name, def_name, entry_point, cron_expression, input, enabled, next_run_at, tenant_id, timezone, misfire_policy, catch_up_limit, overlap_policy)
		VALUES (@p1, @p2, @p3, @p4, CAST(@p5 AS NVARCHAR(MAX)), @p6, @p7, @p8, @p9, @p10, @p11, @p12)
	`, sch.Name, sch.DefName, sch.EntryPoint, sch.CronExpression, scheduleInputJSON(sch.Input), sch.Enabled, sch.NextRunAt, s.tenantID,
		scheduleTimezoneOrDefault(sch.Timezone), MisfirePolicyOrDefault(sch.MisfirePolicy),
		CatchUpLimitOrDefault(sch.CatchUpLimit), OverlapPolicyOrDefault(sch.OverlapPolicy))
	if err != nil {
		// The name is the PRIMARY KEY, so a uniqueness violation here is a
		// caller reusing a name. Detected typed, via isMSSQLDuplicateKey, and
		// wrapped so the HTTP layer answers 409 without reading driver text.
		if isMSSQLDuplicateKey(err) {
			return fmt.Errorf("%w: %s", ErrScheduleExists, sch.Name)
		}
		return err
	}
	return nil
}

func (s *MSSQLStore) ListSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, def_name, entry_point, cron_expression, input, enabled, next_run_at, last_run_at, timezone,
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
			&inputStr, &sch.Enabled, &sch.NextRunAt, &lastRunAt, &sch.Timezone, &sch.TenantID,
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
		UPDATE workflow_schedules SET enabled = @p2 WHERE name = @p1 AND tenant_id = @p3
	`, name, enabled, s.tenantID)
	return err
}

func (s *MSSQLStore) GetDueSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, def_name, entry_point, cron_expression, input, enabled, next_run_at, last_run_at, timezone,
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
		WHERE enabled = 1 AND next_run_at <= SYSUTCDATETIME() AND tenant_id = @p1
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
			&inputStr, &sch.Enabled, &sch.NextRunAt, &lastRunAt, &sch.Timezone, &sch.TenantID,
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

func (s *MSSQLStore) deleteExpiredEventsOnce(ctx context.Context, olderThan time.Time) (int64, error) {
	var totalDeleted int64
	for {
		tx, err := s.beginTxWithContext(ctx)
		if err != nil {
			return totalDeleted, fmt.Errorf("delete expired events: begin: %w", err)
		}
		result, err := tx.ExecContext(ctx, `
			DELETE FROM event_history
			WHERE workflow_id IN (
				SELECT id FROM workflow_instances
				WHERE status IN ('done', 'failed')
				  AND completed_at IS NOT NULL
				  AND completed_at < @p1
				  AND tenant_id = @p2
				ORDER BY id
				OFFSET 0 ROWS FETCH NEXT 10000 ROWS ONLY
			)
		`, olderThan, s.tenantID)
		if err != nil {
			tx.Rollback()
			return totalDeleted, fmt.Errorf("delete expired events: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return totalDeleted, fmt.Errorf("delete expired events: commit: %w", err)
		}
		n, _ := result.RowsAffected()
		totalDeleted += n
		if n == 0 {
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
// done real work: the first loop can never match (finalize_workflow_status
// already purged those events, cleat#1016) while this one clears up to 10000
// workflow_instances rows a batch.
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
				SELECT id FROM workflow_instances
				WHERE status IN ('done', 'failed')
				  AND completed_at IS NOT NULL
				  AND completed_at < @p1
				  AND compaction_state IS NOT NULL
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
// No explicit event_history delete is needed here: migrations/mssql/001_schema.sql
// declares event_history's FK to workflow_instances ON DELETE CASCADE and SQL
// Server never dropped it (only PostgreSQL did, deliberately). Deleting the
// workflow_instances row below cascades event_history (and workflow_signals,
// workflow_promises, concurrency_keys, workflow_update_requests)
// automatically.
//
// UNVERIFIED: no SQL Server instance was available to run this against; it
// is written to match DeleteDeadLetteredWorkflows immediately above exactly
// (same batching shape, same reliance on cascade), which was itself the
// verified reference for this dialect's FK graph.
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
// SQL Server declares NO foreign keys to workflow_instances -- `grep -c
// "REFERENCES workflow_instances" migrations/mssql/*.sql` is 0, against 5 for
// each of the other dialects -- so nothing cascades here and every one of these
// must be deleted explicitly or it is orphaned permanently (cleat#1265). The
// audit that found this measured 6 of 6 surviving a sweep, including the whole
// of event_history, which is the largest table in the schema: deleting the
// instance row removes what made those rows reachable, so they were not merely
// retained but unreachable AND permanent, while the falling instance count told
// the operator retention was working.
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
		SELECT id FROM workflow_instances
		WHERE status IN ('done', 'failed', 'terminated')
		  AND completed_at IS NOT NULL
		  AND completed_at < @p1
		  AND tenant_id = @p2
		ORDER BY id
		OFFSET 0 ROWS FETCH NEXT 10000 ROWS ONLY`

const mssqlSelectDeadLetteredBatch = `
		SELECT id FROM workflow_instances
		WHERE status = 'dead_lettered'
		  AND completed_at IS NOT NULL
		  AND completed_at < @p1
		  AND tenant_id = @p2
		ORDER BY id
		OFFSET 0 ROWS FETCH NEXT 10000 ROWS ONLY`

func (s *MSSQLStore) deleteWorkflowsBatchOnce(ctx context.Context, selectSQL, label string, olderThan time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("%s: begin: %w", label, err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, selectSQL,
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
		return 0, tx.Commit()
	}

	// SQL Server has no array parameter, so the id list is expanded into named
	// placeholders. Never interpolated: these ids come from the database, but a
	// value that round-trips is still a value.
	for _, table := range mssqlWorkflowChildTables {
		if err := s.deleteByWorkflowIDs(ctx, tx, table, ids); err != nil {
			return 0, err
		}
	}
	if err := s.deleteByWorkflowIDs(ctx, tx, "workflow_instances", ids); err != nil {
		return 0, err
	}
	return int64(len(ids)), tx.Commit()
}

// deleteByWorkflowIDs deletes rows keyed to the given workflow ids, in chunks.
//
// SQL Server caps a statement at 2100 parameters, and the batch above is 10000
// ids, so a single IN (...) would fail with "too many parameters" -- on the
// large batches only, which is the shape that would have passed every test and
// failed on the first real retention run.
func (s *MSSQLStore) deleteByWorkflowIDs(ctx context.Context, tx *sql.Tx, table string, ids []string) error {
	const chunk = 2000
	prefix, ok := mssqlDeleteByWorkflowPrefix[table]
	if !ok {
		return fmt.Errorf("delete completed workflows: no delete defined for table %q", table)
	}
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
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

// GetDueSchedulesAcrossTenants returns every tenant's due schedules.
//
// Same mechanism as ClaimWorkflowsAcrossTenants and the same reasoning: SQL
// Server's isolation here is dbo.fn_tenant_filter, and that predicate already
// admits a connection whose login is a member of dbo.cleat_admin regardless of
// SESSION_CONTEXT (migrations/mssql/012_admin_role.sql). So there is no second
// query to write -- there is the same query, on a connection the predicate lets
// through, and a check that this connection is actually one of those.
//
// Unlike PostgreSQL, no migration 024 equivalent is needed: 012 already grants
// across every table fn_tenant_filter is bound to, and workflow_schedules is
// one of them. What a deployment must do is grant the role, which it must
// already have done for the cross-tenant claim.
func (s *MSSQLStore) GetDueSchedulesAcrossTenants(ctx context.Context) ([]Schedule, error) {
	if err := s.requireCleatAdminMembership(ctx); err != nil {
		return nil, err
	}

	// No beginTxWithContext: that helper sets SESSION_CONTEXT to this store's
	// tenantID, which would misstate what this call is scoped to and is not
	// load-bearing on an admin connection anyway.
	//
	// No READPAST/UPDLOCK either, matching the PostgreSQL side's decision to
	// drop FOR UPDATE SKIP LOCKED: the row locks are released before the caller
	// acts on the rows, and ClaimDueSchedule's compare-and-swap is what makes
	// delivery at-least-once. See 024_cross_tenant_schedules.sql.
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, def_name, entry_point, cron_expression, input, enabled, next_run_at, last_run_at, timezone,
		       -- CONVERT, not the raw column: go-mssqldb scans UNIQUEIDENTIFIER
		       -- into a Go string as 16 raw bytes. This value is load-bearing
		       -- here in a way it is nowhere else -- the caller re-scopes the
		       -- whole firing on it -- and a raw one fails the round trip back
		       -- into StartNewRun's UNIQUEIDENTIFIER parameter.
		       CONVERT(NVARCHAR(36), tenant_id) AS tenant_id,
		       misfire_policy, catch_up_limit, overlap_policy, ISNULL(last_run_id, '')
		FROM workflow_schedules
		WHERE enabled = 1 AND next_run_at <= SYSUTCDATETIME()
		ORDER BY next_run_at
	`)
	if err != nil {
		return nil, fmt.Errorf("get due schedules across tenants: %w", err)
	}
	defer rows.Close()

	var schedules []Schedule
	for rows.Next() {
		var sch Schedule
		var lastRunAt sql.NullTime
		var inputStr string
		if err := rows.Scan(&sch.Name, &sch.DefName, &sch.EntryPoint, &sch.CronExpression,
			&inputStr, &sch.Enabled, &sch.NextRunAt, &lastRunAt, &sch.Timezone, &sch.TenantID,
			&sch.MisfirePolicy, &sch.CatchUpLimit, &sch.OverlapPolicy, &sch.LastRunID); err != nil {
			return nil, fmt.Errorf("get due schedules across tenants scan: %w", err)
		}
		sch.Input = json.RawMessage(inputStr)
		if lastRunAt.Valid {
			sch.LastRunAt = &lastRunAt.Time
		}
		schedules = append(schedules, sch)
	}
	return schedules, rows.Err()
}

// CheckCrossTenantCapability answers from SQL Server's role membership.
//
// One question covers both paths here, unlike PostgreSQL's two functions and
// two grants: dbo.fn_tenant_filter admits on IS_ROLEMEMBER(N'cleat_admin') and
// is bound to every tenant-scoped table, workflow_instances and
// workflow_schedules included. So a connection either sees across tenants for
// both or for neither.
//
// There is no BYPASSRLS analogue to lose. The exemption lives in the predicate
// itself rather than in a role attribute, so the silent-degradation failure the
// PostgreSQL check exists for cannot arise the same way: dropping the role
// membership shows up here, and altering the predicate is a schema change.
func (s *MSSQLStore) CheckCrossTenantCapability(ctx context.Context) CrossTenantCapability {
	var isMember sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT IS_ROLEMEMBER(N'cleat_admin')`).Scan(&isMember); err != nil {
		reason := fmt.Sprintf("could not check dbo.cleat_admin membership: %v", err)
		return CrossTenantCapability{ClaimReason: reason, SchedulesReason: reason}
	}
	if isMember.Int64 == 1 {
		return CrossTenantCapability{Claim: true, Schedules: true}
	}
	// NULL means the role does not exist -- migration 012 was never applied --
	// and 0 means it exists and this login is not in it. Both reach here, and
	// applying the migration is step one of granting membership either way.
	const reason = "this connection is not a member of dbo.cleat_admin, so dbo.fn_tenant_filter " +
		"admits only rows matching this connection's SESSION_CONTEXT. Grant it as " +
		"migrations/mssql/012_admin_role.sql documents: CREATE LOGIN, CREATE USER, then " +
		"ALTER ROLE cleat_admin ADD MEMBER"
	return CrossTenantCapability{ClaimReason: reason, SchedulesReason: reason}
}
