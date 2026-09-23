package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// ClaimWorkflow
// ---------------------------------------------------------------------------

// ClaimWorkflow atomically dequeues a runnable workflow instance.
// Uses SELECT ... FOR UPDATE SKIP LOCKED. Delegates to ClaimWorkflows.
func (s *MySQLStore) ClaimWorkflow(ctx context.Context, workerID string) (*WorkflowInstance, error) {
	wfs, err := s.ClaimWorkflows(ctx, workerID, 1)
	if err != nil {
		return nil, err
	}
	if len(wfs) == 0 {
		return nil, nil
	}
	return wfs[0], nil
}

// ClaimWorkflows atomically claims up to limit runnable workflow instances.
// Uses SELECT ... FOR UPDATE SKIP LOCKED to avoid contention.
// MySQL does not support UPDATE ... RETURNING, so we use a three-step
// process inside a transaction: SELECT FOR UPDATE, UPDATE, SELECT.
// CountRunnableWorkflows mirrors ClaimWorkflows' candidate predicate exactly,
// minus the lock and the LIMIT.
//
// MySQL carries `AND tenant_id = ?` explicitly, as its claim does -- it has no
// RLS to lean on. The task_queue placeholders are built the same way for the
// same reason: a fixed count would silently truncate a worker serving more
// queues than the literal allows.
func (s *MySQLStore) CountRunnableWorkflows(ctx context.Context) (int, error) {
	if len(s.taskQueues) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(s.taskQueues)), ",")
	args := make([]any, 0, len(s.taskQueues)+1)
	for _, q := range s.taskQueues {
		args = append(args, q)
	}
	args = append(args, s.tenantID)

	var n int
	err := s.db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT count(*) FROM workflow_instances w
		LEFT JOIN queues q ON q.tenant_id = w.tenant_id AND q.name = w.concurrency_key AND q.disabled_at IS NULL
		WHERE w.status IN ('ready', 'terminating')
		  AND w.next_wake_at <= NOW(6)
		  AND w.task_queue IN (%s)
		  AND (
		    (q.name IS NULL AND (
		      w.concurrency_key_hash IS NULL
		      OR NOT EXISTS (SELECT 1 FROM concurrency_keys ck
		                      WHERE (ck.key_hash = w.concurrency_key_hash
		                        AND ck.tenant_id = w.tenant_id
		                        AND ck.workflow_id <> w.id)
		                        AND EXISTS (SELECT 1 FROM workflow_instances wi
		                                     WHERE wi.id = ck.workflow_id AND wi.tenant_id = ck.tenant_id
		                                       AND wi.status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')))
		    ))
		    OR
		    (q.name IS NOT NULL AND (
		      (
		        (SELECT count(*) FROM queue_holders qh
		          WHERE qh.tenant_id = w.tenant_id
		            AND qh.queue_name = q.name
		            AND qh.workflow_id <> w.id
		            AND EXISTS (SELECT 1 FROM workflow_instances wi
		                         WHERE wi.id = qh.workflow_id AND wi.tenant_id = qh.tenant_id
		                           AND wi.status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled'))) < q.concurrency_limit
		      )
		      AND (
		        q.rate_limit IS NULL OR
		        (SELECT count(*) FROM queue_rate_tokens qrt
		          WHERE qrt.tenant_id = w.tenant_id
		            AND qrt.queue_name = q.name
		            AND qrt.expires_at > NOW(6)) < q.rate_limit
		      )
		    ))
		  )
		  AND w.tenant_id = ?
	`, placeholders), args...).Scan(&n)
	return n, err
}

func (s *MySQLStore) ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	tx, err := s.beginTxReadCommitted(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: begin: %w", err)
	}
	defer tx.Rollback()

	tqClause, tqArgs := s.taskQueueClause()

	// Step 1: select candidate rows with SKIP LOCKED and a snapshot predicate.
	// The predicate branches on whether the key names a registered queue; see the
	// same comment on the postgres claim for why this is a snapshot and the
	// acquire below is the guarantee.
	selArgs := make([]any, 0, len(tqArgs)+2)
	selArgs = append(selArgs, tqArgs...)
	selArgs = append(selArgs, s.tenantID, limit)
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT w.id, w.tenant_id, w.concurrency_key, w.concurrency_key_hash,
		       (q.name IS NOT NULL) AS registered
		FROM workflow_instances w
		LEFT JOIN queues q ON q.tenant_id = w.tenant_id AND q.name = w.concurrency_key AND q.disabled_at IS NULL
		WHERE w.status IN ('ready', 'terminating')
		  AND w.next_wake_at <= NOW(6)
		  AND w.task_queue IN (%s)
		  AND (
		    (q.name IS NULL AND (
		      w.concurrency_key_hash IS NULL
		      OR NOT EXISTS (SELECT 1 FROM concurrency_keys ck
		                      WHERE (ck.key_hash = w.concurrency_key_hash
		                        AND ck.tenant_id = w.tenant_id
		                        AND ck.workflow_id <> w.id)
		                        AND EXISTS (SELECT 1 FROM workflow_instances wi
		                                     WHERE wi.id = ck.workflow_id AND wi.tenant_id = ck.tenant_id
		                                       AND wi.status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')))
		    ))
		    OR
		    (q.name IS NOT NULL AND (
		      (
		        (SELECT count(*) FROM queue_holders qh
		          WHERE qh.tenant_id = w.tenant_id
		            AND qh.queue_name = q.name
		            AND qh.workflow_id <> w.id
		            AND EXISTS (SELECT 1 FROM workflow_instances wi
		                         WHERE wi.id = qh.workflow_id AND wi.tenant_id = qh.tenant_id
		                           AND wi.status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled'))) < q.concurrency_limit
		      )
		      AND (
		        q.rate_limit IS NULL OR
		        (SELECT count(*) FROM queue_rate_tokens qrt
		          WHERE qrt.tenant_id = w.tenant_id
		            AND qrt.queue_name = q.name
		            AND qrt.expires_at > NOW(6)) < q.rate_limit
		      )
		    ))
		  )
		  AND w.tenant_id = ?
		ORDER BY w.priority ASC, w.created_at
		LIMIT ?
		FOR UPDATE OF w SKIP LOCKED
	`, tqClause), selArgs...)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: select: %w", err)
	}
	var cands []claimCandidate
	for rows.Next() {
		var c claimCandidate
		if err := rows.Scan(&c.id, &c.tenantID, &c.key, &c.hash, &c.registered); err != nil {
			rows.Close()
			return nil, fmt.Errorf("claim workflows: scan id: %w", err)
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("claim workflows: rows: %w", err)
	}
	rows.Close()

	// Step 2: lock the registered queues among the candidate keys, sorted.
	limits, err := s.lockRegisteredQueueLimits(ctx, tx, cands)
	if err != nil {
		return nil, err
	}

	// Step 3: acquire each candidate's key.
	var ids []string
	for _, c := range cands {
		ok, err := s.acquireCandidateConcurrencyKey(ctx, tx, c, limits, workerID)
		if err != nil {
			return nil, err
		}
		logClaimKeyDecision(s.log(), c, ok)
		if ok {
			ids = append(ids, c.id)
		}
	}
	if len(ids) == 0 {
		tx.Rollback()
		return nil, nil
	}

	// Step 4: update the claimed rows.
	idClause := inClausePlaceholders(len(ids))
	idArgs := make([]any, len(ids))
	for i, id := range ids {
		idArgs[i] = id
	}

	updateArgs := append([]any{workerID}, idArgs...)
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE workflow_instances
		SET status = 'running',
		    signal_seq_at_claim = signal_seq,
		    signal_consumed_at_claim = signal_consumed_seq,
		    assigned_to = ?,
		    heartbeat_at = NOW(6),
		    started_at = COALESCE(started_at, NOW(6)),
		    generation = generation + 1
		WHERE id IN (%s)
	`, idClause), updateArgs...)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: update: %w", err)
	}

	// Step 5: fetch the full rows.
	rows2, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, def_name, def_version, status, input, COALESCE(assigned_to, ''), next_wake_at, tenant_id, created_at, error_code, error_op, generation, COALESCE(priority, 0) AS priority, COALESCE(trace_id, '') AS trace_id, COALESCE(pending_terminal_status, '') AS pending_terminal_status
		FROM workflow_instances
		WHERE id IN (%s)
	`, idClause), idArgs...)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: fetch: %w", err)
	}
	defer rows2.Close()

	var wfs []*WorkflowInstance
	for rows2.Next() {
		var wf WorkflowInstance
		if err := s.dialect.scanWorkflowInstanceExtra(rows2, &wf); err != nil {
			return nil, fmt.Errorf("claim workflows scan: %w", err)
		}
		wfs = append(wfs, &wf)
	}
	if err := rows2.Err(); err != nil {
		return nil, fmt.Errorf("claim workflows rows: %w", err)
	}

	return s.finishClaim(ctx, tx, workerID, limit, wfs)
}

func (s *MySQLStore) lockRegisteredQueueLimits(ctx context.Context, tx *sql.Tx, cands []claimCandidate) (map[string]registeredQueueLimits, error) {
	limits := map[string]registeredQueueLimits{}
	seen := map[string]bool{}
	var keys []string
	for _, c := range cands {
		if c.registered && !seen[*c.key] {
			seen[*c.key] = true
			keys = append(keys, *c.key)
		}
	}
	if len(keys) == 0 {
		return limits, nil
	}
	phs := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, 0, len(keys)+1)
	for _, k := range keys {
		args = append(args, k)
	}
	args = append(args, s.tenantID)
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT name, concurrency_limit, rate_limit, rate_period_seconds, worker_concurrency FROM queues
		WHERE name IN (%s) AND tenant_id = ? AND disabled_at IS NULL
		ORDER BY name
		FOR UPDATE
	`, phs), args...)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: lock registered queues: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var ql registeredQueueLimits
		var rateLimit, ratePeriodSeconds, workerConcurrency sql.NullInt64
		if err := rows.Scan(&name, &ql.concurrencyLimit, &rateLimit, &ratePeriodSeconds, &workerConcurrency); err != nil {
			return nil, fmt.Errorf("claim workflows: scan queue limit: %w", err)
		}
		ql.rateLimit, ql.ratePeriodSeconds = nullInt64Pair(rateLimit, ratePeriodSeconds)
		ql.workerConcurrency = nullableIntFromSQL(workerConcurrency)
		limits[name] = ql
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim workflows: queue limits rows: %w", err)
	}
	return limits, nil
}

func (s *MySQLStore) acquireCandidateConcurrencyKey(ctx context.Context, tx *sql.Tx, c claimCandidate, limits map[string]registeredQueueLimits, workerID string) (bool, error) {
	if !c.registered {
		if c.hash == nil {
			return true, nil // no key at all
		}
		// Bare key: mutex via INSERT IGNORE, exactly the old per-candidate acquire.
		res, err := tx.ExecContext(ctx, `
			INSERT IGNORE INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at, tenant_id)
			VALUES (?, ?, ?, DATE_ADD(NOW(6), INTERVAL ? SECOND), ?)
		`, c.hash, c.key, c.id, int64(claimedKeyTTL.Seconds()), c.tenantID)
		if err != nil {
			return false, fmt.Errorf("claim workflows: acquire concurrency key: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("claim workflows: acquire concurrency key rows: %w", err)
		}
		if n > 0 {
			return true, nil
		}
		// INSERT IGNORE affected nothing: either this run already holds the key
		// (a re-claim after a lost fence) or another run took it.
		var holder string
		err = tx.QueryRowContext(ctx,
			`SELECT workflow_id FROM concurrency_keys WHERE key_hash = ? AND tenant_id = ?`,
			c.hash, c.tenantID).Scan(&holder)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil // released between insert and read; not ours
		}
		if err != nil {
			return false, fmt.Errorf("claim workflows: concurrency key holder: %w", err)
		}
		return holder == c.id, nil
	}

	// Registered queue: semaphore under the queues lock.
	ql, ok := limits[*c.key]
	if !ok {
		// The candidate predicate saw it registered, but the lock step did not
		// (disabled or deleted between the two statements). Not claimable.
		return false, nil
	}
	// Does this workflow already hold a live slot on this queue, and whose
	// worker_id does it carry? cleat#1917: same shape as the postgres claim --
	// see its comment for the full reasoning. A holder owned by the CLAIMING
	// worker is the original re-claim shortcut, unchanged; one that exists but
	// belongs to nobody or to a DIFFERENT worker must pass every gate below and
	// MOVES to this worker if admitted, rather than a second row being inserted.
	var existingWorker sql.NullString
	holderExists := true
	err := tx.QueryRowContext(ctx, `
		SELECT worker_id FROM queue_holders
		WHERE tenant_id = ? AND queue_name = ? AND workflow_id = ?
	`, c.tenantID, *c.key, c.id).Scan(&existingWorker)
	if errors.Is(err, sql.ErrNoRows) {
		holderExists = false
	} else if err != nil {
		return false, fmt.Errorf("claim workflows: queue self-hold check: %w", err)
	}
	if holderExists && existingWorker.Valid && existingWorker.String == workerID {
		return true, nil
	}
	var held int
	err = tx.QueryRowContext(ctx, `
		SELECT count(*) FROM queue_holders qh
		WHERE qh.tenant_id = ? AND qh.queue_name = ? AND qh.workflow_id <> ?
		  AND EXISTS (SELECT 1 FROM workflow_instances wi
		               WHERE wi.id = qh.workflow_id AND wi.tenant_id = qh.tenant_id
		                 AND wi.status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled'))
	`, c.tenantID, *c.key, c.id).Scan(&held)
	if err != nil {
		return false, fmt.Errorf("claim workflows: count queue holders: %w", err)
	}
	if held >= ql.concurrencyLimit {
		return false, nil // at capacity
	}
	// cleat#1917: the per-worker cap, if declared, is a second independent gate.
	if ql.workerConcurrency != nil {
		var workerHeld int
		err = tx.QueryRowContext(ctx, `
			SELECT count(*) FROM queue_holders qh
			WHERE qh.tenant_id = ? AND qh.queue_name = ? AND qh.worker_id = ?
			  AND EXISTS (SELECT 1 FROM workflow_instances wi
			               WHERE wi.id = qh.workflow_id AND wi.tenant_id = qh.tenant_id
			                 AND wi.status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled'))
		`, c.tenantID, *c.key, workerID).Scan(&workerHeld)
		if err != nil {
			return false, fmt.Errorf("claim workflows: count worker queue holders: %w", err)
		}
		if workerHeld >= *ql.workerConcurrency {
			return false, nil // this worker is at its own cap
		}
	}
	// cleat#1918: the rate limit, if declared, is a third independent gate,
	// applying only to a genuinely fresh admission -- see the postgres claim's
	// comment for why a moved holder takes no new token.
	if !holderExists && ql.rateLimit != nil {
		var rateHeld int
		err = tx.QueryRowContext(ctx, `
			SELECT count(*) FROM queue_rate_tokens qrt
			WHERE qrt.tenant_id = ? AND qrt.queue_name = ? AND qrt.expires_at > NOW(6)
		`, c.tenantID, *c.key).Scan(&rateHeld)
		if err != nil {
			return false, fmt.Errorf("claim workflows: count queue rate tokens: %w", err)
		}
		if rateHeld >= *ql.rateLimit {
			return false, nil // rate-limited
		}
	}
	if holderExists {
		res, err := tx.ExecContext(ctx, `
			UPDATE queue_holders
			SET worker_id = ?, expires_at = DATE_ADD(NOW(6), INTERVAL ? SECOND)
			WHERE tenant_id = ? AND queue_name = ? AND workflow_id = ?
		`, workerID, int64(claimedKeyTTL.Seconds()), c.tenantID, *c.key, c.id)
		if err != nil {
			return false, fmt.Errorf("claim workflows: move queue holder: %w", err)
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	}
	res, err := tx.ExecContext(ctx, `
		INSERT IGNORE INTO queue_holders (tenant_id, queue_name, workflow_id, expires_at, worker_id)
		VALUES (?, ?, ?, DATE_ADD(NOW(6), INTERVAL ? SECOND), ?)
	`, c.tenantID, *c.key, c.id, int64(claimedKeyTTL.Seconds()), workerID)
	if err != nil {
		return false, fmt.Errorf("claim workflows: acquire queue holder: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	if ql.rateLimit != nil {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO queue_rate_tokens (tenant_id, queue_name, workflow_id, expires_at)
			VALUES (?, ?, ?, DATE_ADD(NOW(6), INTERVAL ? SECOND))
		`, c.tenantID, *c.key, c.id, int64(*ql.ratePeriodSeconds)); err != nil {
			return false, fmt.Errorf("claim workflows: record rate token: %w", err)
		}
	}
	return true, nil
}

// ClaimStickyWorkflows atomically claims up to limit runnable workflow instances
// that are sticky to this worker. Uses SELECT ... FOR UPDATE SKIP LOCKED with
// sticky_worker_id filtering for low-contention claiming.
func (s *MySQLStore) ClaimStickyWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: begin: %w", err)
	}
	defer tx.Rollback()

	tqClause, tqArgs := s.taskQueueClause()

	// Step 1: Select IDs with SKIP LOCKED (sticky filter).
	// Arg order: sticky_worker_id, task_queue values..., tenant_id, limit
	selArgs := make([]any, 0)
	selArgs = append(selArgs, workerID)
	selArgs = append(selArgs, tqArgs...)
	selArgs = append(selArgs, s.tenantID, limit)
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT id FROM workflow_instances
		WHERE status = 'ready'
		  AND next_wake_at <= NOW(6)
		  AND sticky_worker_id = ?
		  AND task_queue IN (%s)
  AND (workflow_instances.concurrency_key_hash IS NULL
       OR NOT EXISTS (SELECT 1 FROM concurrency_keys ck
                       WHERE (ck.key_hash = workflow_instances.concurrency_key_hash
                         AND ck.tenant_id = workflow_instances.tenant_id
                         AND ck.workflow_id <> workflow_instances.id)
                         AND EXISTS (SELECT 1 FROM workflow_instances wi
                                      WHERE wi.id = ck.workflow_id AND wi.tenant_id = ck.tenant_id
                                        AND wi.status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled'))))
		  AND tenant_id = ?
		ORDER BY priority ASC, created_at
		LIMIT ?
		FOR UPDATE SKIP LOCKED
	`, tqClause), selArgs...)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: select: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("claim sticky workflows: scan id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim sticky workflows: rows: %w", err)
	}
	rows.Close()

	if len(ids) == 0 {
		tx.Rollback()
		return nil, nil
	}

	// Step 2: Update the claimed rows.
	idClause := inClausePlaceholders(len(ids))
	idArgs := make([]any, len(ids))
	for i, id := range ids {
		idArgs[i] = id
	}

	updateArgs := append([]any{workerID}, idArgs...)
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE workflow_instances
		SET status = 'running',
		    signal_seq_at_claim = signal_seq,
		    signal_consumed_at_claim = signal_consumed_seq,
		    assigned_to = ?,
		    heartbeat_at = NOW(6),
		    started_at = COALESCE(started_at, NOW(6)),
		    generation = generation + 1
		WHERE id IN (%s)
	`, idClause), updateArgs...)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: update: %w", err)
	}

	// Step 3: Fetch the full rows.
	rows2, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, def_name, def_version, status, input, COALESCE(assigned_to, ''), next_wake_at, tenant_id, created_at, error_code, error_op, generation, COALESCE(priority, 0) AS priority, COALESCE(trace_id, '') AS trace_id, COALESCE(pending_terminal_status, '') AS pending_terminal_status
		FROM workflow_instances
		WHERE id IN (%s)
	`, idClause), idArgs...)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: fetch: %w", err)
	}
	defer rows2.Close()

	var wfs []*WorkflowInstance
	for rows2.Next() {
		var wf WorkflowInstance
		if err := s.dialect.scanWorkflowInstanceExtra(rows2, &wf); err != nil {
			return nil, fmt.Errorf("claim sticky workflows scan: %w", err)
		}
		wfs = append(wfs, &wf)
	}
	if err := rows2.Err(); err != nil {
		return nil, fmt.Errorf("claim sticky workflows rows: %w", err)
	}

	return s.finishClaim(ctx, tx, workerID, limit, wfs)
}

// ---------------------------------------------------------------------------
// CompleteWorkflow, FailWorkflow, ReleaseWorkflow
// ---------------------------------------------------------------------------

// CompleteWorkflow marks a workflow as completed with a result.
func (s *MySQLStore) CompleteWorkflow(ctx context.Context, workflowID, workerID string, generation int64, result string, queryState map[string]string) error {
	// Coerce, as FinalizeWorkflowSegment does. The result column is jsonb on
	// PostgreSQL and JSON on MySQL, and the raw string is not guaranteed to be
	// either -- a workflow that continues as new never returned a value, so the
	// result here is "", which is not valid JSON. Writing it raw failed the
	// whole run with
	//
	//	pq: invalid input syntax for type json (22P02)
	//
	// so continue-as-new did not work at all on PostgreSQL. coerceResultJSON
	// existed for exactly this and was called from one path out of three.
	resultJSON := coerceResultJSON(ctx, s.log(), workflowID, result)

	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("complete workflow: begin: %w", err)
	}
	defer tx.Rollback()

	qsJSON := marshalQueryState(queryState)
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'done', result = ?, completed_at = NOW(6), completed_by = assigned_to, assigned_to = NULL, query_state = ?
		WHERE id = ? AND assigned_to = ? AND tenant_id = ? AND generation = ?
	`, resultJSON, qsJSON, workflowID, workerID, s.tenantID, generation)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete workflow: rows affected: %w", err)
	}
	if n == 0 {
		// Another worker now owns this workflow. Roll back rather than
		// commit: the post-commit cleanup below is not safe to run on the
		// new owner's behalf.
		return ErrFenceLost
	}

	// No idempotency write on the success path. idempotency_keys.result was
	// written here and read nowhere, so cleat#1049 dropped the column; a
	// completed run now records nothing on that table. The failure path is
	// unchanged -- FailWorkflow and MoveToDeadLetterQueue still write
	// error_msg, still filtered `AND tenant_id`, which is where the Finding
	// S1 tenant-scope guard now lives.

	if err := tx.Commit(); err != nil {
		return err
	}

	releaseWorkflowResources(s.log(), s, workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// FailWorkflow marks a workflow as failed.
func (s *MySQLStore) FailWorkflow(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("fail workflow: begin: %w", err)
	}
	defer tx.Rollback()

	qsJSON := marshalQueryState(queryState)
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'failed',
		    error_msg = ?,
		    error_code = ?,
		    error_op = ?,
		    completed_at = NOW(6),
		    completed_by = assigned_to, assigned_to = NULL,
		    query_state = ?
		WHERE id = ? AND assigned_to = ? AND tenant_id = ? AND generation = ?
	`, errorMsg, errorCode, errorOp, qsJSON, workflowID, workerID, s.tenantID, generation)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("fail workflow: rows affected: %w", err)
	}
	if n == 0 {
		// Another worker now owns this workflow. Roll back rather than
		// commit: the idempotency-key write and post-commit cleanup below
		// are not safe to run on the new owner's behalf.
		return ErrFenceLost
	}

	// Record idempotency error within the transaction (best-effort).
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET error_msg = ? WHERE workflow_id = ? AND tenant_id = ?`,
		errorMsg, workflowID, s.tenantID); err != nil {
		s.log().WarnContext(ctx, "idempotency update failed", "error", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	releaseWorkflowResources(s.log(), s, workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// ReleaseWorkflow returns a workflow to the ready queue with a next wake time.
// Used when a workflow suspends (sleep/await signals).
func (s *MySQLStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("release workflow: begin: %w", err)
	}
	defer tx.Rollback()

	// Same CASE as ReapStaleInstances, for the same reason: a workflow whose
	// terminal outcome is already recorded is not runnable work, and a release
	// that called it 'ready' would undo the distinction D6 created the
	// 'terminating' status to make. Either status is claimable, so the phase
	// runs again either way -- this is about the status telling the truth
	// while it waits.
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = CASE WHEN pending_terminal_status IS NOT NULL
		                  THEN 'terminating' ELSE 'ready' END,
		    assigned_to = NULL, next_wake_at = ?
		WHERE id = ? AND assigned_to = ? AND tenant_id = ? AND generation = ?
	`, nextWakeAt, workflowID, workerID, s.tenantID, generation)
	if err != nil {
		return err
	}

	// A zero-row update is a lost fence, not a failure and not a success.
	// Reported rather than discarded because the caller branches on it:
	// cmd/cleat-worker's releaseWorkflow treats ErrFenceLost as "the no-op it
	// is" and logs at Debug, while any OTHER error is logged as "release
	// failed, workflow stays claimed until its lease expires" -- untrue of a
	// stale release on both counts. Until cleat#1223 that branch was dead on
	// PostgreSQL and MySQL, and the three sibling fenced writes (Complete,
	// Fail, Finalize) already reported a lost fence this way on all three
	// dialects.
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("release workflow: rows affected: %w", err)
	}
	if rows == 0 {
		return ErrFenceLost
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// StartNewRun
// ---------------------------------------------------------------------------

// StartNewRun creates a new workflow instance.
// If idempotencyKey is non-empty, provides exactly-once semantics: a subsequent
// call with the same key returns the existing workflow ID without creating a
// duplicate. Returns the workflow ID, whether it already existed, and any error.
// StartNewRun is the entry point without a concurrency key.
func (s *MySQLStore) StartNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int) (string, bool, error) {
	return s.startNewRun(ctx, runID, defName, defVersion, input, idempotencyKey, tenantID, priority, StartOptions{})
}

// StartNewRunWithConcurrencyKey records the key on the row in the INSERT that
// creates it. See the PostgreSQL implementation for why it is written by the
// insert and why this is not on the interface.
//
// The hash is computed in Go here, matching AcquireConcurrencyKey on this
// store; PostgreSQL hashes in SQL. That split is not tidiness -- it is how the
// existing rows were written, and migration 058 records what happens when the
// two conventions meet.
func (s *MySQLStore) StartNewRunWithConcurrencyKey(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int, concurrencyKey string) (string, bool, error) {
	return s.startNewRun(ctx, runID, defName, defVersion, input, idempotencyKey, tenantID, priority, StartOptions{ConcurrencyKey: concurrencyKey})
}

// StartNewRunWithOptions records every per-run value a start can set, in the
// INSERT that creates the run. See StartOptions.
func (s *MySQLStore) StartNewRunWithOptions(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int, opts StartOptions) (string, bool, error) {
	return s.startNewRun(ctx, runID, defName, defVersion, input, idempotencyKey, tenantID, priority, opts)
}

func (s *MySQLStore) startNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int, opts StartOptions) (string, bool, error) {
	concurrencyKey := opts.ConcurrencyKey
	runInstanceMs := msOrNil(opts.RunLimits.WasmInstanceTimeout)
	runWallClockMs := msOrNil(opts.RunLimits.WasmWallClockCeiling)
	runRetryMs := msOrNil(opts.RunLimits.HostRetryBudget)
	runMaxWorkflowMs := msOrNil(opts.RunLimits.MaxWorkflowDuration)
	// TYPED nils, not `any(nil)`. go-mssqldb infers the parameter type from the
	// Go value, and an untyped nil arrives as NVARCHAR NULL -- which SQL Server
	// refuses to put in a VARBINARY(32) column: "Implicit conversion from data
	// type nvarchar to varbinary is not allowed". A *string and a []byte carry
	// their own types, so NULL arrives as the right kind of NULL.
	//
	// It failed on the NO-KEY path, which is every ordinary run, so this is not
	// an edge case -- it is the common one.
	var ckText *string
	var ckHash []byte
	if concurrencyKey != "" {
		h := sha256.Sum256([]byte(concurrencyKey))
		ckText, ckHash = &concurrencyKey, h[:]
	}
	if runID == "" {
		runID = uuid.New().String()
	}
	if idempotencyKey != "" {
		// RETRIED ON A LOCK CONFLICT. cleat#1753.
		//
		// Concurrent starts sharing one key each run DELETE-then-INSERT against
		// the same key_hash. InnoDB takes a next-key lock on the index record the
		// DELETE scans -- under REPEATABLE READ, which is the default and what
		// cleat runs on -- and the racers then need insert-intention locks in each
		// other's locked range. InnoDB picks a victim and returns 1213.
		//
		// The victim's transaction is rolled back WHOLE, so nothing it wrote
		// survives and replaying it is sound -- the same argument the SQL Server
		// paths already make for their deadlock retries. On the retry the winner's
		// row is committed, so the INSERT IGNORE reports 0 and the caller takes the
		// replay path, which is the answer it should have had.
		//
		// POSTGRES DOES NOT NEED THIS and SQL Server already had it, which is why
		// this surfaced on one dialect: Postgres does not gap-lock the range a
		// non-matching DELETE scans, so its racers serialise on the unique index
		// alone and never form a cycle. Measured -- 8 racers, one key:
		//
		//	postgres  answered=8 replays=7 errors=0
		//	mysql     answered=1 replays=0 errors=7   all 1213
		//	mssql     answered=8 replays=7 errors=0
		return s.startNewRunUnderIdempotencyKey(ctx, runID, defName, defVersion,
			input, idempotencyKey, tenantID, priority, ckText, ckHash,
			runInstanceMs, runWallClockMs, runRetryMs, runMaxWorkflowMs)
	}

	// No idempotency key -- normal flow.
	tx, err := s.beginTx(ctx)
	if err != nil {
		return "", false, fmt.Errorf("start new run: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority, concurrency_key, concurrency_key_hash, run_wasm_instance_timeout_ms, run_wasm_wall_clock_ceiling_ms, run_host_retry_budget_ms, run_max_workflow_duration_ms)
		VALUES (?, ?, ?, 'ready', ?,
		        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = ? AND version = ? AND tenant_id = ?), 'default'),
		        ?, ?, ?, ?, ?, ?, ?, ?)
	`, runID, defName, defVersion, input, defName, defVersion, tenantID, tenantID, priority, ckText, ckHash, runInstanceMs, runWallClockMs, runRetryMs, runMaxWorkflowMs)
	if err != nil {
		return "", false, fmt.Errorf("start new run: %w", err)
	}
	return runID, false, tx.Commit()
}

// ---------------------------------------------------------------------------
// ContinueAsNew
// ---------------------------------------------------------------------------

// ContinueAsNew atomically creates a new workflow run AND completes the
// current one in a single database transaction. If the transaction fails
// neither operation takes effect. Returns the new run ID on success.
func (s *MySQLStore) ContinueAsNew(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput json.RawMessage, newEvents []EventRecord, result string, queryState map[string]string, priority int) (string, error) {
	// Coerce, as FinalizeWorkflowSegment does. The result column is jsonb on
	// PostgreSQL and JSON on MySQL, and the raw string is not guaranteed to be
	// either -- a workflow that continues as new never returned a value, so the
	// result here is "", which is not valid JSON. Writing it raw failed the
	// whole run with
	//
	//	pq: invalid input syntax for type json (22P02)
	//
	// so continue-as-new did not work at all on PostgreSQL. coerceResultJSON
	// existed for exactly this and was called from one path out of three.
	resultJSON := coerceResultJSON(ctx, s.log(), currentRunID, result)

	tx, err := s.beginTx(ctx)
	if err != nil {
		return "", fmt.Errorf("continue as new: begin: %w", err)
	}
	defer tx.Rollback()

	// Append events within the same transaction.
	if err := s.appendEventsInTx(ctx, tx, currentRunID, newEvents); err != nil {
		return "", fmt.Errorf("continue as new: append events: %w", err)
	}

	// Use the store's tenant scope to preserve tenant isolation.
	// Create the new workflow run.
	// Use the store's tenant scope to preserve tenant isolation.
	newRunID := uuid.New().String()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority, continued_from, parent_workflow_id, parent_close_policy)
		-- parent_workflow_id and parent_close_policy are INHERITED from the run
		-- being continued, not left NULL. A child that continues as new is
		-- still its parent's child: enforceParentClosePolicy selects on
		-- parent_workflow_id and NULL matches nothing, so a TERMINATE parent
		-- left every continued iteration running while reporting that it had
		-- stopped its child (cleat#955). Measured against a plain sibling
		-- child as a control -- that one WAS stopped by the same call, so the
		-- policy was working and only the link was missing.
		SELECT ?, ?, ?, 'ready', ?,
		       COALESCE((SELECT task_queue FROM workflow_defs WHERE name = ? AND version = ? AND tenant_id = ?), 'default'),
		       ?, ?, ?, p.parent_workflow_id, p.parent_close_policy
		FROM workflow_instances p WHERE p.id = ?
	`, newRunID, defName, defVersion, newInput, defName, defVersion, s.tenantID, s.tenantID, priority, currentRunID, currentRunID)
	if err != nil {
		return "", fmt.Errorf("continue as new: start new run: %w", err)
	}

	// Complete the current run.
	qsJSON := marshalQueryState(queryState)
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'done', result = ?, completed_at = NOW(6), completed_by = assigned_to, assigned_to = NULL, query_state = ?
		WHERE id = ? AND assigned_to = ? AND tenant_id = ? AND generation = ?
	`, resultJSON, qsJSON, currentRunID, workerID, s.tenantID, generation)
	if err != nil {
		return "", fmt.Errorf("continue as new: complete old run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("continue as new: rows affected: %w", err)
	}
	if n == 0 {
		// Another worker now owns this workflow. Roll back rather than
		// commit: this also discards the new run row we just inserted, so
		// a lost fence leaves no orphaned, unreachable continuation run
		// behind.
		return "", ErrFenceLost
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}

	releaseWorkflowResources(s.log(), s, currentRunID)
	s.enforceParentClosePolicy(context.Background(), currentRunID)

	return newRunID, nil
}

// ---------------------------------------------------------------------------
// FinalizeWorkflowSegment
// ---------------------------------------------------------------------------

// FinalizeWorkflowSegment atomically appends new events and updates the
// workflow status in a single database transaction. This eliminates the
// race between AppendEventHistoryBatch and the subsequent CompleteWorkflow /
// FailWorkflow / ReleaseWorkflow call.
//
// finalStatus must be one of:
//   - "done"   — marks the workflow as completed with the given result
//   - "failed" — marks the workflow as failed with the given error info
//   - "ready"  — returns the workflow to the ready queue (suspend)
//
// Fields not relevant to the chosen status are ignored.
func (s *MySQLStore) finalizeWorkflowSegmentInner(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
	if !validFinalStatus(finalStatus) {
		return fmt.Errorf("finalize workflow: unknown final status: %s", finalStatus)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finalize workflow: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Append new events within the same transaction.
	if err := s.appendEventsInTx(ctx, tx, runID, newEvents); err != nil {
		return fmt.Errorf("finalize workflow: append events: %w", err)
	}

	// Delegate the terminal UPDATEs (status, idempotency, parent wake,
	// await_child population) to a server-side stored procedure.
	// This replaces 5 individual round-trips with 1 procedure call.
	qsJSON := marshalQueryState(queryState)
	resultJSON := coerceResultJSON(ctx, s.log(), runID, result)

	// p_next_wake_at is only meaningful for the "ready" status; callers
	// finalizing as "done"/"failed" routinely pass the zero time.Time{}.
	// The go-sql-driver/mysql driver encodes a Go zero time as MySQL's
	// legacy zero-date sentinel "0000-00-00 00:00:00", which MySQL's
	// default strict sql_mode (NO_ZERO_DATE, on by default since 5.7)
	// rejects with Error 1292 "Incorrect datetime value". Postgres and
	// MSSQL both accept a year-1 timestamp fine, so this is MySQL-only.
	// Pass NULL instead when the caller didn't supply a real time.
	var nextWakeParam interface{}
	if !nextWakeAt.IsZero() {
		nextWakeParam = nextWakeAt
	}

	var fenceHeld bool
	if err := tx.QueryRowContext(ctx, `
		CALL finalize_workflow_status(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, runID, workerID, generation, finalStatus, resultJSON, errorCode, errorOp, string(qsJSON), nextWakeParam, s.notifyChannel).Scan(&fenceHeld); err != nil {
		return fmt.Errorf("finalize workflow: %w", err)
	}

	if !fenceHeld {
		// Another worker now owns this workflow (e.g. this worker stalled,
		// was reaped, and the workflow was reclaimed). Roll back rather
		// than commit: the events we just appended belong to a segment
		// that is no longer valid, and none of the post-commit cleanup
		// below is safe to run on the new owner's behalf.
		return ErrFenceLost
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort cleanup for terminal statuses (post-commit).
	if finalStatus == "done" || finalStatus == "failed" {
		releaseWorkflowResources(s.log(), s, runID)
		s.enforceParentClosePolicy(context.Background(), runID)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Heartbeat / HeartbeatBatchFenced
// ---------------------------------------------------------------------------

// Heartbeat updates the heartbeat timestamp to prevent timeout.
// Returns false if the workflow is no longer assigned to this worker.
func (s *MySQLStore) Heartbeat(ctx context.Context, workflowID, workerID string, generation int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET heartbeat_at = NOW(6)
		WHERE id = ? AND assigned_to = ? AND tenant_id = ? AND generation = ?
	`, workflowID, workerID, s.tenantID, generation)
	if err != nil {
		return false, fmt.Errorf("heartbeat: %w", err)
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// HeartbeatBatchFenced is PostgresStore.HeartbeatBatchFenced's MySQL twin --
// see that doc comment and the interface's. cleat#2008 replaced
// BatchHeartbeat with this at the worker's one call site rather than running
// both.
func (s *MySQLStore) HeartbeatBatchFenced(ctx context.Context, workerID string, runs []GenerationKey) ([]string, error) {
	if len(runs) == 0 {
		return nil, nil
	}
	byID := make(map[string]int64, len(runs))
	ids := make([]string, 0, len(runs))
	args := make([]any, 0, len(runs)+2)
	for _, r := range runs {
		byID[r.WorkflowID] = r.Generation
		ids = append(ids, r.WorkflowID)
		args = append(args, r.WorkflowID)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("heartbeat batch fenced: begin: %w", err)
	}
	defer tx.Rollback()

	// FOR UPDATE holds these rows through the UPDATE below, in the same
	// transaction -- a reclaim landing between a check and a separate update
	// statement is exactly the race this exists to close, not reopen.
	idClause := inClausePlaceholders(len(ids))
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, generation FROM workflow_instances
		WHERE assigned_to = ? AND status = 'running' AND tenant_id = ? AND id IN (%s)
		FOR UPDATE
	`, idClause), append([]any{workerID, s.tenantID}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("heartbeat batch fenced: select: %w", err)
	}
	eligible := make([]string, 0, len(runs))
	eligibleSet := make(map[string]bool, len(runs))
	for rows.Next() {
		var id string
		var gen int64
		if err := rows.Scan(&id, &gen); err != nil {
			rows.Close()
			return nil, fmt.Errorf("heartbeat batch fenced: scan: %w", err)
		}
		if wantGen, ok := byID[id]; ok && wantGen == gen {
			eligible = append(eligible, id)
			eligibleSet[id] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("heartbeat batch fenced: rows: %w", err)
	}
	rows.Close()

	lost := make([]string, 0, len(runs))
	for _, id := range ids {
		if !eligibleSet[id] {
			lost = append(lost, id)
		}
	}

	if len(eligible) > 0 {
		eligibleArgs := make([]any, 0, len(eligible)+1)
		for _, id := range eligible {
			eligibleArgs = append(eligibleArgs, id)
		}
		eligibleArgs = append(eligibleArgs, s.tenantID)
		eligibleClause := inClausePlaceholders(len(eligible))
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE workflow_instances SET heartbeat_at = NOW(6) WHERE id IN (%s) AND tenant_id = ?
		`, eligibleClause), eligibleArgs...); err != nil {
			return nil, fmt.Errorf("heartbeat batch fenced: update: %w", err)
		}
	}
	return lost, tx.Commit()
}

// ---------------------------------------------------------------------------
// MoveToDeadLetterQueue
// ---------------------------------------------------------------------------

// MoveToDeadLetterQueue marks a workflow as dead_lettered because it failed
// after exhausting all retry attempts. This is a terminal status similar to
// 'failed' but indicates the workflow was retried without success.
func (s *MySQLStore) MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("move to dead letter queue: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'dead_lettered', error_msg = ?, error_code = ?, error_op = ?,
		    completed_at = NOW(6), completed_by = assigned_to, assigned_to = NULL
		WHERE id = ? AND assigned_to = ? AND tenant_id = ? AND generation = ?
	`, errMsg, errorCode, errorOp, workflowID, workerID, s.tenantID, generation)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("move to dead letter queue: rows affected: %w", err)
	}
	if n == 0 {
		// Another worker now owns this workflow. Roll back rather than
		// commit: the idempotency-key write and post-commit cleanup below
		// are not safe to run on the new owner's behalf.
		return ErrFenceLost
	}

	// Record idempotency error within the transaction (best-effort).
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET error_msg = ? WHERE workflow_id = ? AND tenant_id = ?`,
		errMsg, workflowID, s.tenantID); err != nil {
		s.log().WarnContext(ctx, "idempotency update failed", "error", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	releaseWorkflowResources(s.log(), s, workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// RetryWorkflow moves a dead_lettered workflow back to a runnable state.
func (s *MySQLStore) RetryWorkflow(ctx context.Context, workflowID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', completed_by = assigned_to, assigned_to = NULL, heartbeat_at = NULL,
		    error_msg = NULL, error_code = NULL, error_op = NULL,
		    next_wake_at = NOW(6)
		WHERE id = ? AND status = 'dead_lettered' AND tenant_id = ?
	`, workflowID, s.tenantID)
	return err
}

// ---------------------------------------------------------------------------
// ReapStaleInstances
// ---------------------------------------------------------------------------

// ReapStaleInstances reclaims workflow instances that have been running
// but whose heartbeat has not been updated within the given timeout.
// Returns the number of instances reclaimed.
func (s *MySQLStore) ReapStaleInstances(ctx context.Context, timeout time.Duration, limit int) (int, error) {
	// See PostgresStore.ReapStaleInstances: a workflow reaped mid-defer-phase
	// goes back to 'terminating', because its terminal outcome is already
	// decided and calling it 'ready' would undo the distinction D6 created the
	// status to make.
	// The derived table is not decoration: MySQL rejects a LIMIT inside an
	// IN subquery ("This version of MySQL doesn't yet support 'LIMIT & IN/ALL/
	// ANY/SOME subquery'"), and wrapping it in SELECT ... FROM (...) t is the
	// documented way round. See the interface doc for why the sweep is bounded.
	result, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = CASE WHEN pending_terminal_status IS NOT NULL
		                  THEN 'terminating' ELSE 'ready' END,
		    assigned_to = NULL, heartbeat_at = NULL, generation = generation + 1,
		    reclaim_count = reclaim_count + 1
		WHERE id IN (
		    SELECT id FROM (
		        SELECT id FROM workflow_instances
		        WHERE status = 'running'
		          AND heartbeat_at < NOW(6) - INTERVAL ? SECOND
		          AND tenant_id = ?
		        ORDER BY heartbeat_at
		        LIMIT ?
		    ) t
		)
	`, int(timeout.Seconds()), s.tenantID, reapLimitArg(limit))
	if err != nil {
		return 0, fmt.Errorf("reap stale instances: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// ---- ParentClosePolicy ----

// enforceParentClosePolicy applies ParentClosePolicy to all child workflows
// of the given parent workflow. Runs as a best-effort operation.
// enforceParentClosePolicy applies a closing parent's policy to its children.
//
// Two defects, both from IMPROVEMENT-PLAN.md 2.50. It discarded the result of
// both ExecContext calls, so a failure was structurally invisible: the
// children of a closed parent were unaffected by its policy and nothing
// recorded it. And it used no transaction at all, so the two statements could
// apply partially -- TERMINATE children failed while REQUEST_CANCEL children
// were left unflagged, or the reverse, with no way to tell afterwards.
//
// Both are fixed here. The function stays void: the contract with callers has
// not changed, only whether a failure is observable.
func (s *MySQLStore) enforceParentClosePolicy(ctx context.Context, parentWorkflowID string) {
	s.enforceParentClosePolicyAt(ctx, parentWorkflowID, 0)
}

// enforceParentClosePolicyAt is enforceParentClosePolicy with the recursion
// depth carried explicitly. See cascadeIntoClosedChildren.
func (s *MySQLStore) enforceParentClosePolicyAt(ctx context.Context, parentWorkflowID string, depth int) {
	// Collected before the transaction: see releaseTerminatedChildren.
	terminated, err := s.childrenClosedByTerminate(ctx, parentWorkflowID)
	if err != nil {
		s.log().WarnContext(ctx, "enforceParentClosePolicy: could not list TERMINATE children; their concurrency keys and sticky-worker assignments stay held until TTL",
			"parent_workflow_id", parentWorkflowID, "error", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.log().WarnContext(ctx, "enforceParentClosePolicy: begin failed; children of a closed parent are unaffected by its close policy",
			"parent_workflow_id", parentWorkflowID, "error", err)
		return
	}
	defer tx.Rollback()

	// Terminate children with TERMINATE policy -- in two arms, split by
	// whether the child owes cleanup. See PostgresStore's for the reasoning;
	// both arms are in this one transaction, so the partition cannot apply
	// by halves. IMPROVEMENT-PLAN 3.114.
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'failed', error_msg = 'parent workflow terminated',
		    pending_terminal_status = NULL, defer_phase_deadline = NULL,
		    completed_at = NOW(6),
		    completed_by = assigned_to, assigned_to = NULL, generation = generation + 1
		WHERE parent_workflow_id = ?
		  AND parent_close_policy = 'TERMINATE'
		  AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
		  AND tenant_id = ?
		  AND NOT `+deferPhaseOwedSQL+`
	`, parentWorkflowID, s.tenantID); err != nil {
		s.log().WarnContext(ctx, "enforceParentClosePolicy: TERMINATE children not failed",
			"parent_workflow_id", parentWorkflowID, "error", err)
		return
	}

	//nolint:gosec // G202: the concatenated fragments are compile-time constants -- statusTerminating and deferPhaseOwedSQL are consts in defer_phase.go, deferPhaseDeadlineMySQL is a Sprintf over a duration const. No caller-controlled string reaches this.
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = '`+statusTerminating+`',
		    pending_terminal_status = 'failed',
		    defer_phase_deadline = `+deferPhaseDeadlineMySQL+`,
		    error_msg = 'parent workflow terminated',
		    next_wake_at = NOW(6),
		    assigned_to = NULL,
		    generation = generation + 1
		WHERE parent_workflow_id = ?
		  AND parent_close_policy = 'TERMINATE'
		  AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
		  AND tenant_id = ?
		  AND `+deferPhaseOwedSQL+`
	`, parentWorkflowID, s.tenantID); err != nil {
		s.log().WarnContext(ctx, "enforceParentClosePolicy: TERMINATE children with defers not moved to their defer phase",
			"parent_workflow_id", parentWorkflowID, "error", err)
		return
	}

	// Request cancellation for children with REQUEST_CANCEL policy.
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET cancellation_requested = true
		WHERE parent_workflow_id = ?
		  AND parent_close_policy = 'REQUEST_CANCEL'
		  AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
		  AND tenant_id = ?
	`, parentWorkflowID, s.tenantID); err != nil {
		s.log().WarnContext(ctx, "enforceParentClosePolicy: REQUEST_CANCEL children not flagged",
			"parent_workflow_id", parentWorkflowID, "error", err)
		return
	}

	if err := tx.Commit(); err != nil {
		s.log().WarnContext(ctx, "enforceParentClosePolicy: commit failed; children of a closed parent are unaffected by its close policy",
			"parent_workflow_id", parentWorkflowID, "error", err)
		return
	}

	releaseTerminatedChildren(s.log(), s, terminated)
	cascadeIntoClosedChildren(s.log(), depth, terminated, func(id string, d int) {
		s.enforceParentClosePolicyAt(ctx, id, d)
	})
}

// terminateChildrenQuery selects the children the TERMINATE arm is about to
// fail, so their resources can be released after it commits. Its WHERE must
// stay identical to that UPDATE's, or the two disagree about which children
// were closed.
func (s *MySQLStore) childrenClosedByTerminate(ctx context.Context, parentWorkflowID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM workflow_instances
		WHERE parent_workflow_id = ?
		  AND parent_close_policy = 'TERMINATE'
		  AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
		  AND tenant_id = ?
		  AND NOT `+deferPhaseOwedSQL+`
	`, parentWorkflowID, s.tenantID)
	if err != nil {
		return nil, err
	}
	return scanWorkflowIDs(rows)
}

// finishClaim commits a claim transaction and enforces the claim-limit
// invariant, releasing any excess rather than truncating it away. See
// enforceClaimLimit in claim_limit.go for why.
func (s *MySQLStore) finishClaim(ctx context.Context, tx *sql.Tx, workerID string, limit int, wfs []*WorkflowInstance) ([]*WorkflowInstance, error) {
	keep, excess := enforceClaimLimit(ctx, s.log(), "mysql", workerID, limit, wfs)
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for _, wf := range excess {
		if err := s.ReleaseWorkflow(context.Background(), wf.ID, workerID, wf.Generation, wf.NextWakeAt); err != nil {
			s.log().ErrorContext(ctx, "releasing an over-claimed workflow failed; it stays claimed until its lease expires",
				"worker_id", workerID, "workflow_id", wf.ID, "error", err)
		}
	}
	return keep, nil
}

// FinalizeWorkflowSegment wraps finalizeWorkflowSegmentInner so that a backend
// refusing a JSON value it was handed becomes a classified error rather than
// driver text. cleat#1460.
//
// WRAPPED AT THE BOUNDARY, not at each return, and that is the point: this
// function has a dozen error paths and will grow more, and a classification
// applied at one of them is a classification the next one silently lacks.
// wrapRejectedResult returns anything it does not recognise unchanged, so the
// blanket wrap costs nothing and cannot mislabel an unrelated failure.
func (s *MySQLStore) FinalizeWorkflowSegment(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
	return wrapRejectedResult(
		s.finalizeWorkflowSegmentRetrying(ctx, runID, workerID, generation, newEvents,
			finalStatus, result, errorCode, errorOp, queryState, nextWakeAt),
		runID, result)
}

// finalizeWorkflowSegmentRetrying retries finalizeWorkflowSegmentInner's whole
// transaction when InnoDB refuses it with a lock conflict. cleat#1883.
//
// SAME SHAPE AS startNewRunUnderIdempotencyKey (cleat#1753/#1755), on a
// DIFFERENT call site -- isDeadlockError and isLockWaitTimeout already existed
// and this is the second place they were needed and were not called. The
// argument for safety is the same one that function's comment gives:
// finalizeWorkflowSegmentInner opens exactly one transaction
// (`tx, err := s.db.BeginTx`), does the event append and the terminal update
// inside it, and `defer tx.Rollback()` unwinds ALL of it on any error path.
// InnoDB's deadlock victim is rolled back server-side before it returns 1213,
// so a losing attempt leaves nothing committed and replaying it from the top
// is sound.
//
// FOUND BY A PORTS TEST, NOT BY READING THE START-PATH FIX. cleat#1883's
// samples-go/mysql leg -- TestRepeatsOfOneSignalDoNotReachAQuorum, which
// finalizes a workflow after several concurrent signal deliveries race to
// quorum -- terminated with status=failed and
// `error: "... append events in tx: increment event_count: Error 1213 ..."`,
// the same unretried code straight out of the same two classifiers, at a call
// site #1755 did not touch. Postgres and MSSQL passed the identical test:
// neither serialises concurrent UPDATEs to one row the way InnoDB's gap locks
// do, the same asymmetry #1753's comment measures for the start path.
//
// NO SLEEP BETWEEN ATTEMPTS, for the same reason as the start path: the
// victim's lock is already released by the time this returns, so there is
// nothing to wait for and a backoff would only add latency to the path a
// worker is blocking on to report a workflow's outcome.
func (s *MySQLStore) finalizeWorkflowSegmentRetrying(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
	const maxAttempts = 8

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err := s.finalizeWorkflowSegmentInner(ctx, runID, workerID, generation, newEvents,
			finalStatus, result, errorCode, errorOp, queryState, nextWakeAt)
		if err == nil {
			return nil
		}
		if !isDeadlockError(err) && !isLockWaitTimeout(err) {
			return err
		}
		lastErr = err
	}
	// Reported as itself rather than as a bare 1213, for the same reason the
	// start path does: a caller seeing this has hit genuine sustained
	// contention on one workflow's finalize, and the attempt count is the
	// thing they can act on.
	return fmt.Errorf(
		"finalize workflow segment: %d attempts all lost a lock conflict: %w",
		maxAttempts, lastErr)
}

// startNewRunUnderIdempotencyKey runs the idempotent start, retrying the whole
// transaction when InnoDB refuses it with a lock conflict.
//
// THE CLASSIFIERS ALREADY EXISTED AND NOTHING CALLED THEM. isDeadlockError and
// isLockWaitTimeout are in mysql_store.go, have unit tests of their own, and had
// ZERO production callers before this -- so the engine could recognise 1213 and
// 1205 and never acted on either. cleat#1753 is what that cost.
//
// BOUNDED, AND THE BOUND IS A MARGIN RATHER THAN A MEASURED REQUIREMENT -- said
// that way round because the measurement does not support a tighter claim.
// Across 15 runs of 8 racers on one key, the DEEPEST retry index reached was 0:
// every racer that lost a lock conflict succeeded on its next attempt. So one
// retry sufficed every time it was observed, and 8 is headroom for a machine
// more contended than the one that measured it.
//
// The reason headroom is cheap here is structural: each attempt either commits
// or finds the winner already committed, so the population of contenders only
// shrinks. The retries exist to outlast a lock cycle, not to wait out a queue,
// and a run that genuinely needed all 8 would mean something other than this.
//
// NO SLEEP BETWEEN ATTEMPTS. The victim is chosen and rolled back immediately,
// so there is nothing to wait for -- the lock it needed is already released.
// A backoff here would add latency to the exact path a caller is blocking on.
func (s *MySQLStore) startNewRunUnderIdempotencyKey(
	ctx context.Context, runID, defName string, defVersion int, input json.RawMessage,
	idempotencyKey string, tenantID string, priority int,
	ckText *string, ckHash []byte,
	runInstanceMs, runWallClockMs, runRetryMs, runMaxWorkflowMs any,
) (string, bool, error) {
	const maxAttempts = 8

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		id, replayed, err := s.tryStartNewRunUnderIdempotencyKey(ctx, runID, defName,
			defVersion, input, idempotencyKey, tenantID, priority, ckText, ckHash,
			runInstanceMs, runWallClockMs, runRetryMs, runMaxWorkflowMs)
		if err == nil {
			return id, replayed, nil
		}
		if !isDeadlockError(err) && !isLockWaitTimeout(err) {
			return "", false, err
		}
		lastErr = err
	}
	// Reported as itself rather than as a bare 1213, because a caller that sees
	// this has hit genuine sustained contention on one key and the attempt count
	// is the thing they can act on.
	return "", false, fmt.Errorf(
		"start new run under idempotency key: %d attempts all lost a lock conflict: %w",
		maxAttempts, lastErr)
}

// tryStartNewRunUnderIdempotencyKey is one attempt. Every path through it either
// commits or leaves the transaction rolled back, which is what makes the caller
// above free to run it again.
func (s *MySQLStore) tryStartNewRunUnderIdempotencyKey(
	ctx context.Context, runID, defName string, defVersion int, input json.RawMessage,
	idempotencyKey string, tenantID string, priority int,
	ckText *string, ckHash []byte,
	runInstanceMs, runWallClockMs, runRetryMs, runMaxWorkflowMs any,
) (string, bool, error) {
	keyHash := sha256.Sum256([]byte(idempotencyKey))
	inputDigest := IdempotencyInputDigest(input)

	// Check for existing idempotency key, within this tenant.
	//
	// The tenant filter is not defence in depth: idempotency_keys was
	// keyed by key_hash alone, so an Idempotency-Key was global across
	// every tenant in the deployment. Two customers both choosing
	// "order-123" collided, and the second was handed the first's
	// workflow ID with alreadyExisted = true while its own workflow was
	// never started. The key is a client-supplied request header, so that
	// is the expected outcome of ordinary naming rather than an attack.
	// migrations/mysql/010_idempotency_keys_tenant_id.sql,
	// IMPROVEMENT-PLAN 3.10.
	var existingWfID string
	var existingDef sql.NullString
	var existingDigest sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT workflow_id, def_name, input_digest FROM idempotency_keys
		 WHERE key_hash = ? AND tenant_id = ? AND expires_at > NOW(6)`,
		keyHash[:], tenantID).Scan(&existingWfID, &existingDef, &existingDigest)
	if err == nil {
		// A hit must be for the SAME definition. NULL means the row predates
		// cleat#1047's backfill or its workflow has been purged -- unknown
		// rather than mismatched, so it is allowed through, which is exactly
		// today's behaviour for those rows.
		if existingDef.Valid && existingDef.String != defName {
			return "", false, fmt.Errorf("%w: key already started %q, this request names %q",
				ErrIdempotencyKeyDefMismatch, existingDef.String, defName)
		}
		if err := checkIdempotencyInput(existingDigest, inputDigest); err != nil {
			return "", false, err
		}
		return existingWfID, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	// THE EXPIRED-ROW SWEEP RUNS ONLY WHEN THERE IS ONE, AND THAT ORDERING IS
	// THE FIX. cleat#1753, second round.
	//
	// It used to run unconditionally, before the insert. Under REPEATABLE READ
	// -- the default, and what cleat runs on -- an equality DELETE that matches
	// NO row takes a gap lock on the gap where the row would go, and gap locks
	// are mutually COMPATIBLE. So every concurrent starter acquired one, and
	// then every one of them needed an insert-intention lock inside a gap the
	// others held. That is a guaranteed cycle rather than an unlucky one, and
	// InnoDB resolved it by killing starters.
	//
	// RETRYING IT WAS THE FIRST FIX AND IT WAS THE WRONG SHAPE. The deadlock is
	// structural, so more attempts only means more contenders re-entering the
	// same cycle. It held at 8 racers locally and exhausted 8 attempts on a
	// loaded CI runner -- measured on develop, after it merged.
	//
	// Inserting FIRST removes the lock on the common path: with no prior DELETE
	// nobody holds a gap lock, so racers serialise on the key itself and the
	// losers get a duplicate rather than a deadlock. The sweep still happens,
	// below, on the one path that needs it -- and there it MATCHES a row, so it
	// takes a record lock and not a gap lock.
	//
	// cleat#1671 is why the sweep cannot simply be deleted: an expired row
	// blocks the insert and reports 0 rows affected, identical to the
	// concurrent-insert case, and the re-read filters on expiry -- so without a
	// sweep that caller looks for a row it cannot see and gets sql.ErrNoRows
	// instead of a new run.

	// Insert idempotency key record. INSERT IGNORE handles the race where
	// two requests arrive with the same key simultaneously -- and, since
	// the delete above, ONLY that: a row that blocks here is necessarily
	// live.
	ttlSeconds := int(s.idempotencyKeyTTL.Seconds())
	res, err := tx.ExecContext(ctx,
		`INSERT IGNORE INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id, def_name, input_digest)
		 VALUES (?, ?, DATE_ADD(NOW(6), INTERVAL ? SECOND), ?, ?, ?)`,
		keyHash[:], runID, ttlSeconds, tenantID, defName, inputDigest)
	if err != nil {
		return "", false, err
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		// A row exists. It is EITHER a live winner to replay OR an expired row
		// to sweep, and the affected count alone cannot tell them apart --
		// which is precisely what cleat#1671 was. Ask which.
		var expired bool
		if err := tx.QueryRowContext(ctx,
			`SELECT expires_at <= NOW(6) FROM idempotency_keys
			 WHERE key_hash = ? AND tenant_id = ?`,
			keyHash[:], tenantID).Scan(&expired); err != nil {
			return "", false, fmt.Errorf("start new run: classify the blocking idempotency key: %w", err)
		}
		if expired {
			// Sweep it and take the key over. This DELETE matches a row, so it
			// takes a record lock rather than the gap lock that made the
			// unconditional sweep deadlock.
			//
			// The expiry predicate is still load bearing: a row refreshed by a
			// concurrent starter is LIVE, and deleting it would let two runs
			// hold one key. If it was refreshed between the SELECT and here this
			// matches nothing, the insert below reports the conflict again, and
			// the live branch answers it.
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM idempotency_keys
				 WHERE key_hash = ? AND tenant_id = ? AND expires_at <= NOW(6)`,
				keyHash[:], tenantID); err != nil {
				return "", false, fmt.Errorf("start new run: clear expired idempotency key: %w", err)
			}
			res, err = tx.ExecContext(ctx,
				`INSERT IGNORE INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id, def_name, input_digest)
				 VALUES (?, ?, DATE_ADD(NOW(6), INTERVAL ? SECOND), ?, ?, ?)`,
				keyHash[:], runID, ttlSeconds, tenantID, defName, inputDigest)
			if err != nil {
				return "", false, err
			}
			n, _ = res.RowsAffected()
		}
	}
	if n == 0 {
		// A LIVE row exists, so someone else won the race, and the re-read's own
		// expiry filter will find it (cleat#1671).
		//
		// Rollback and return the existing one.
		tx.Rollback()
		err := s.db.QueryRowContext(ctx,
			`SELECT workflow_id, def_name, input_digest FROM idempotency_keys
			 WHERE key_hash = ? AND tenant_id = ? AND expires_at > NOW(6)`,
			keyHash[:], tenantID).Scan(&existingWfID, &existingDef, &existingDigest)
		if err != nil {
			return "", false, err
		}
		// The concurrent winner must also be for THIS definition. Without
		// this the race path returns the other workflow's id even though
		// the lookup above refuses it -- the same defect, reachable only
		// under contention, which is where it would be hardest to see.
		if existingDef.Valid && existingDef.String != defName {
			return "", false, fmt.Errorf("%w: key already started %q, this request names %q",
				ErrIdempotencyKeyDefMismatch, existingDef.String, defName)
		}
		if err := checkIdempotencyInput(existingDigest, inputDigest); err != nil {
			return "", false, err
		}
		return existingWfID, true, nil
	}

	// Insert the workflow instance.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority, concurrency_key, concurrency_key_hash, run_wasm_instance_timeout_ms, run_wasm_wall_clock_ceiling_ms, run_host_retry_budget_ms, run_max_workflow_duration_ms)
		VALUES (?, ?, ?, 'ready', ?,
		        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = ? AND version = ? AND tenant_id = ?), 'default'),
		        ?, ?, ?, ?, ?, ?, ?, ?)
	`, runID, defName, defVersion, input, defName, defVersion, tenantID, tenantID, priority, ckText, ckHash, runInstanceMs, runWallClockMs, runRetryMs, runMaxWorkflowMs)
	if err != nil {
		return "", false, fmt.Errorf("start new run: %w", err)
	}

	return runID, false, tx.Commit()
}
