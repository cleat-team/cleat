package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (s *MSSQLStore) CreateSchedule(ctx context.Context, sch Schedule) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_schedules (name, def_name, entry_point, cron_expression, input, enabled, next_run_at, tenant_id, timezone, misfire_policy, catch_up_limit, overlap_policy)
		VALUES (@p1, @p2, @p3, @p4, CAST(@p5 AS NVARCHAR(MAX)), @p6, @p7, @p8, @p9, @p10, @p11, @p12)
	`, sch.Name, sch.DefName, sch.EntryPoint, sch.CronExpression, scheduleInputJSON(sch.Input), sch.Enabled, sch.NextRunAt, s.tenantID,
		scheduleTimezoneOrDefault(sch.Timezone), MisfirePolicyOrDefault(sch.MisfirePolicy),
		CatchUpLimitOrDefault(sch.CatchUpLimit), OverlapPolicyOrDefault(sch.OverlapPolicy))
	return err
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

func (s *MSSQLStore) DeleteSchedule(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM workflow_schedules WHERE name = @p1`, name)
	return err
}

func (s *MSSQLStore) SetScheduleEnabled(ctx context.Context, name string, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_schedules SET enabled = @p2 WHERE name = @p1
	`, name, enabled)
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
		WHERE enabled = 1 AND next_run_at <= SYSUTCDATETIME()
		ORDER BY next_run_at
	`)
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

func (s *MSSQLStore) UpdateScheduleNextRun(ctx context.Context, name string, nextRun time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_schedules SET last_run_at = SYSUTCDATETIME(), next_run_at = @p2 WHERE name = @p1
	`, name, nextRun)
	return err
}

func (s *MSSQLStore) GetCompactionCandidates(ctx context.Context, threshold int, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT w.id
		FROM workflow_instances w
		WHERE w.status IN ('ready', 'running')
		  AND (SELECT COUNT(*) FROM event_history e WHERE e.workflow_id = w.id) > @p1
		  AND (w.compaction_step IS NULL OR w.compaction_step < (SELECT MAX(e2.step) FROM event_history e2 WHERE e2.workflow_id = w.id))
		ORDER BY w.created_at
		OFFSET 0 ROWS FETCH NEXT @p2 ROWS ONLY
	`, threshold, limit)
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
		`INSERT INTO workflow_memory_samples (def_name, sample_bytes) VALUES (@p1, @p2)`,
		defName, sampleBytes)
	if err != nil {
		return fmt.Errorf("record memory sample: insert sample: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		MERGE workflow_memory_stats AS target
		USING (SELECT @p1 AS def_name, @p2 AS mean_bytes) AS source
		ON target.def_name = source.def_name
		WHEN MATCHED THEN UPDATE SET
			mean_bytes  = target.alpha * @p2 + (1 - target.alpha) * target.mean_bytes,
			sample_count = target.sample_count + 1,
			updated_at  = SYSUTCDATETIME()
		WHEN NOT MATCHED THEN INSERT (def_name, mean_bytes, sample_count, updated_at)
			VALUES (@p1, @p2, 1, SYSUTCDATETIME());
	`, defName, float64(sampleBytes))
	if err != nil {
		return fmt.Errorf("record memory sample: upsert stats: %w", err)
	}

	return tx.Commit()
}

func (s *MSSQLStore) LoadMemoryEstimates(ctx context.Context) (map[string]float64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT def_name, mean_bytes FROM workflow_memory_stats
	`)
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
		ORDER BY def_name
	`)
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
		`SELECT DISTINCT def_name FROM workflow_memory_samples`)
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
			  AND id NOT IN (
			      SELECT id FROM (
				  SELECT id, ROW_NUMBER() OVER (ORDER BY recorded_at DESC) AS rn
				  FROM workflow_memory_samples
				  WHERE def_name = @p1
			      ) AS ranked
			      WHERE ranked.rn <= @p2
			  )
		`, defName, maxSamplesPerDef)
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

	// Also batch cleanup compaction states.
	for {
		tx, err := s.beginTxWithContext(ctx)
		if err != nil {
			return totalDeleted, fmt.Errorf("delete expired events: begin compaction: %w", err)
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
				ORDER BY id
				OFFSET 0 ROWS FETCH NEXT 10000 ROWS ONLY
			)
		`, olderThan)
		if err != nil {
			tx.Rollback()
			break
		}
		if err := tx.Commit(); err != nil {
			break
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	return totalDeleted, nil
}

func (s *MSSQLStore) DeleteDeadLetteredWorkflows(ctx context.Context, olderThan time.Time) (int64, error) {
	var totalDeleted int64
	for {
		result, err := s.db.ExecContext(ctx, `
			DELETE FROM workflow_instances
			WHERE id IN (
				SELECT id FROM workflow_instances
				WHERE status = 'dead_lettered'
				  AND completed_at IS NOT NULL
				  AND completed_at < @p1
				  AND tenant_id = @p2
				ORDER BY id
				OFFSET 0 ROWS FETCH NEXT 10000 ROWS ONLY
			)
		`, sql.Named("p1", olderThan), sql.Named("p2", s.tenantID))
		if err != nil {
			return totalDeleted, fmt.Errorf("delete dead-lettered workflows: %w", err)
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
// An empty input becomes "{}": the column is NOT NULL with a '{}' default in
// the shipped schema, and an empty string is not valid JSON either.
func scheduleInputJSON(input json.RawMessage) string {
	if len(input) == 0 {
		return "{}"
	}
	return string(input)
}

// ClaimDueSchedule advances a schedule's next_run_at, but only if it still
// holds expectedNextRun. See the interface doc for why this is a CAS.
func (s *MSSQLStore) ClaimDueSchedule(ctx context.Context, name string, expectedNextRun, newNextRun time.Time, runID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE workflow_schedules
		SET next_run_at = @p2, last_run_at = SYSUTCDATETIME(),
		    last_run_id = CASE WHEN @p4 = '' THEN last_run_id ELSE @p4 END
		WHERE name = @p1 AND next_run_at = @p3
	`, name, newNextRun, expectedNextRun, runID)
	if err != nil {
		return false, fmt.Errorf("ClaimDueSchedule: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ClaimDueSchedule: rows affected: %w", err)
	}
	return n == 1, nil
}
