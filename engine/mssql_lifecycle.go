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
// Claim Methods (C.3)
// ---------------------------------------------------------------------------

// ClaimWorkflow atomically claims a single runnable workflow instance.
func (s *MSSQLStore) ClaimWorkflow(ctx context.Context, workerID string) (*WorkflowInstance, error) {
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
// Uses UPDATE...OUTPUT with READPAST/UPDLOCK hints (SQL Server's equivalent
// of FOR UPDATE SKIP LOCKED) wrapped in a transaction with RLS context.
// ClaimWorkflows retries on errors SQL Server guarantees it rolled back --
// a deadlock victim claimed nothing, so replaying the claim is sound. Errors
// that leave the outcome unknown are not retried; see
// withRollbackGuaranteedRetry (IMPROVEMENT-PLAN.md 2.26).
// CountRunnableWorkflows mirrors claimWorkflowsOnce' candidate predicate
// exactly, minus the READPAST/UPDLOCK hints and the TOP.
//
// `AND tenant_id` is explicit here for the reason recorded on the claim itself
// (IMPROVEMENT-PLAN 3.91): dbo.fn_tenant_filter is off for the admin role, so
// on SQL Server that predicate IS the whole of the tenant scoping. A count
// without it would report other tenants' runnable work to this worker.
//
// The task queues travel as a comma-joined string through STRING_SPLIT, as the
// claim's do -- not because it is nicer, but because differing from the claim
// here is the one way this count can lie.
func (s *MSSQLStore) CountRunnableWorkflows(ctx context.Context) (int, error) {
	if len(s.taskQueues) == 0 {
		return 0, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM workflow_instances w
		LEFT JOIN queues q ON q.tenant_id = w.tenant_id AND q.name = w.concurrency_key AND q.disabled_at IS NULL
		WHERE w.status IN ('ready', 'terminating')
		  AND w.next_wake_at <= SYSUTCDATETIME()
		  AND w.task_queue IN (SELECT value FROM STRING_SPLIT(@p1, ','))
		  AND (
		    (q.name IS NULL AND (
		      w.concurrency_key_hash IS NULL
		      OR NOT EXISTS (SELECT 1 FROM concurrency_keys ck
		                      WHERE ck.key_hash = w.concurrency_key_hash
		                        AND ck.tenant_id = w.tenant_id
		                        AND ck.expires_at > SYSUTCDATETIME()
		                        AND ck.workflow_id <> w.id)
		    ))
		    OR
		    (q.name IS NOT NULL AND (
		      (
		        (SELECT count(*) FROM queue_holders qh
		          WHERE qh.tenant_id = w.tenant_id
		            AND qh.queue_name = q.name
		            AND qh.expires_at > SYSUTCDATETIME()
		            AND qh.workflow_id <> w.id) < q.concurrency_limit
		      )
		      AND (
		        q.rate_limit IS NULL OR
		        (SELECT count(*) FROM queue_rate_tokens qrt
		          WHERE qrt.tenant_id = w.tenant_id
		            AND qrt.queue_name = q.name
		            AND qrt.expires_at > SYSUTCDATETIME()) < q.rate_limit
		      )
		    ))
		  )
		  AND w.tenant_id = @p2
	`, strings.Join(s.taskQueues, ","), s.tenantID).Scan(&n)
	return n, err
}

func (s *MSSQLStore) ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	var claimed []*WorkflowInstance
	err := withRollbackGuaranteedRetry(ctx, "claim workflows", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		var err error
		claimed, err = s.claimWorkflowsOnce(ctx, workerID, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// claimWorkflowsOnce claims for THIS STORE'S TENANT ONLY.
//
// `AND tenant_id` in the candidate SELECT is the whole of that on SQL Server,
// and it was missing (3.91). dbo.fn_tenant_filter is off for any dbo.cleat_admin
// login (012_admin_role.sql), and requireCleatAdminMembership checks s.db -- the
// SAME POOL this runs on -- so on any deployment where ClaimWorkflowsAcrossTenants
// works at all, this ordinary claim was already returning every tenant's ready
// work and the -claim-across-tenants flag was guarding a widening that had
// already happened.
//
// The other two dialects disagreed with this one, which is what settled it:
// MySQL carries `AND tenant_id = ?` here explicitly, and PostgreSQL carries no
// predicate but claims inside beginTxWithRLS where the application role is
// genuinely subject to RLS (cross_tenant_claim_test.go tests exactly that, with
// a non-owning role). SQL Server had neither, so it was the only dialect with
// nothing enforcing it.
//
// Do not "simplify" this by sharing SQL with claimWorkflowsAcrossTenantsOnce.
// The difference between them is this one predicate, and that is the point.
func (s *MSSQLStore) claimWorkflowsOnce(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: begin: %w", err)
	}
	defer tx.Rollback()

	tqParam := s.buildTaskQueueParam()

	// Step 1: select candidate rows, locking them UPDLOCK/READPAST (SQL Server's
	// FOR UPDATE SKIP LOCKED) and carrying the registered flag via a LEFT JOIN.
	// The LEFT JOIN reads queues without a lock hint, so it does not lock the
	// queue rows -- those are locked in sorted order by the next step.
	rows, err := tx.QueryContext(ctx, `
		SELECT w.id, CONVERT(NVARCHAR(36), w.tenant_id) AS tenant_id, w.concurrency_key, w.concurrency_key_hash,
		       CASE WHEN q.name IS NOT NULL THEN 1 ELSE 0 END AS registered
		FROM workflow_instances w WITH (READPAST, UPDLOCK, ROWLOCK)
		LEFT JOIN queues q ON q.tenant_id = w.tenant_id AND q.name = w.concurrency_key AND q.disabled_at IS NULL
		WHERE w.status IN ('ready', 'terminating')
		  AND w.next_wake_at <= SYSUTCDATETIME()
		  AND w.task_queue IN (SELECT value FROM STRING_SPLIT(@p1, ','))
		  AND (
		    (q.name IS NULL AND (
		      w.concurrency_key_hash IS NULL
		      OR NOT EXISTS (SELECT 1 FROM concurrency_keys ck
		                      WHERE ck.key_hash = w.concurrency_key_hash
		                        AND ck.tenant_id = w.tenant_id
		                        AND ck.expires_at > SYSUTCDATETIME()
		                        AND ck.workflow_id <> w.id)
		    ))
		    OR
		    (q.name IS NOT NULL AND (
		      (
		        (SELECT count(*) FROM queue_holders qh
		          WHERE qh.tenant_id = w.tenant_id
		            AND qh.queue_name = q.name
		            AND qh.expires_at > SYSUTCDATETIME()
		            AND qh.workflow_id <> w.id) < q.concurrency_limit
		      )
		      AND (
		        q.rate_limit IS NULL OR
		        (SELECT count(*) FROM queue_rate_tokens qrt
		          WHERE qrt.tenant_id = w.tenant_id
		            AND qrt.queue_name = q.name
		            AND qrt.expires_at > SYSUTCDATETIME()) < q.rate_limit
		      )
		    ))
		  )
		  AND w.tenant_id = @p2
		ORDER BY w.priority ASC, w.created_at
		OFFSET 0 ROWS FETCH NEXT @p3 ROWS ONLY
	`, tqParam, s.tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: select candidates: %w", err)
	}
	var cands []claimCandidate
	for rows.Next() {
		var c claimCandidate
		var registered int
		if err := rows.Scan(&c.id, &c.tenantID, &c.key, &c.hash, &registered); err != nil {
			rows.Close()
			return nil, fmt.Errorf("claim workflows: scan candidate: %w", err)
		}
		c.registered = registered != 0
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("claim workflows: candidates rows: %w", err)
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

	// Step 4: update the claimed rows and read them back through OUTPUT.
	rows2, err := tx.QueryContext(ctx, `
		UPDATE workflow_instances
		SET status = 'running',
		    signal_seq_at_claim = signal_seq,
		    signal_consumed_at_claim = signal_consumed_seq,
		    assigned_to = @p1,
		    heartbeat_at = SYSUTCDATETIME(),
		    started_at = COALESCE(started_at, SYSUTCDATETIME()),
		    generation = generation + 1
		OUTPUT INSERTED.id, INSERTED.def_name, INSERTED.def_version,
		       INSERTED.status, INSERTED.input, INSERTED.assigned_to,
		       INSERTED.next_wake_at,
		       CONVERT(NVARCHAR(36), INSERTED.tenant_id) AS tenant_id,
		       INSERTED.created_at,
		       INSERTED.error_code, INSERTED.error_op, INSERTED.generation,
		       COALESCE(INSERTED.priority, 0) AS priority,
		       INSERTED.trace_id,
		       COALESCE(INSERTED.pending_terminal_status, '') AS pending_terminal_status
		WHERE id IN (SELECT value FROM STRING_SPLIT(@p2, ','))
		  AND tenant_id = @p3
	`, workerID, strings.Join(ids, ","), s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: %w", err)
	}
	defer rows2.Close()

	var wfs []*WorkflowInstance
	for rows2.Next() {
		var wf WorkflowInstance
		var nextWakeAt sql.NullTime
		var tenantID sql.NullString
		var createdAt sql.NullTime
		var inputStr string
		var errorCode, errorOp sql.NullString
		var traceID sql.NullString
		var pendingTerminal sql.NullString

		if err := rows2.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status,
			&inputStr, &wf.AssignedTo, &nextWakeAt, &tenantID, &createdAt, &errorCode, &errorOp, &wf.Generation, &wf.Priority, &traceID,
			&pendingTerminal); err != nil {
			return nil, fmt.Errorf("claim workflows scan: %w", err)
		}
		wf.TraceID = traceID.String

		wf.Input = json.RawMessage(inputStr)
		if nextWakeAt.Valid {
			wf.NextWakeAt = nextWakeAt.Time
		}
		if tenantID.Valid {
			wf.TenantID = tenantID.String
		}
		if createdAt.Valid {
			wf.CreatedAt = createdAt.Time
		}
		wf.ErrorCode = errorCode.String
		wf.ErrorOp = errorOp.String
		wf.PendingTerminalStatus = pendingTerminal.String
		wfs = append(wfs, &wf)
	}
	if err := rows2.Err(); err != nil {
		return nil, fmt.Errorf("claim workflows rows: %w", err)
	}

	if len(wfs) == 0 {
		tx.Rollback()
		return nil, nil
	}
	return s.finishClaim(ctx, tx, workerID, limit, wfs)
}

func (s *MSSQLStore) lockRegisteredQueueLimits(ctx context.Context, tx *sql.Tx, cands []claimCandidate) (map[string]registeredQueueLimits, error) {
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
	rows, err := tx.QueryContext(ctx, `
		SELECT name, concurrency_limit, rate_limit, rate_period_seconds, worker_concurrency FROM queues WITH (UPDLOCK, ROWLOCK)
		WHERE tenant_id = @p1
		  AND name IN (SELECT value FROM STRING_SPLIT(@p2, ','))
		  AND disabled_at IS NULL
		ORDER BY name
	`, s.tenantID, strings.Join(keys, ","))
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

func (s *MSSQLStore) acquireCandidateConcurrencyKey(ctx context.Context, tx *sql.Tx, c claimCandidate, limits map[string]registeredQueueLimits, workerID string) (bool, error) {
	if !c.registered {
		if c.hash == nil {
			return true, nil // no key at all
		}
		// Bare key: mutex via INSERT ... WHERE NOT EXISTS, the shape AcquireConcurrencyKey
		// already uses -- SQL Server has no INSERT IGNORE.
		res, err := tx.ExecContext(ctx, `
			INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at, tenant_id)
			SELECT @p1, @p2, @p3, DATEADD(second, @p4, SYSUTCDATETIME()), @p5
			WHERE NOT EXISTS (
				SELECT 1 FROM concurrency_keys
				WHERE key_hash = @p1 AND tenant_id = @p5 AND expires_at > SYSUTCDATETIME()
			)
		`, c.hash, c.key, c.id, int64(claimedKeyTTL.Seconds()), c.tenantID)
		if err != nil {
			return false, fmt.Errorf("claim workflows: acquire concurrency key: %w", err)
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			return true, nil
		}
		// Not inserted: either this run already holds the key (a re-claim after a
		// lost fence) or another run took it.
		var holder string
		err = tx.QueryRowContext(ctx,
			`SELECT workflow_id FROM concurrency_keys WHERE key_hash = @p1 AND tenant_id = @p2`,
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
		// The candidate predicate saw it registered, but the lock step did not.
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
		WHERE tenant_id = @p1 AND queue_name = @p2 AND workflow_id = @p3 AND expires_at > SYSUTCDATETIME()
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
		WHERE qh.tenant_id = @p1 AND qh.queue_name = @p2 AND qh.expires_at > SYSUTCDATETIME()
		  AND qh.workflow_id <> @p3
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
			WHERE qh.tenant_id = @p1 AND qh.queue_name = @p2 AND qh.worker_id = @p3
			  AND qh.expires_at > SYSUTCDATETIME()
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
			WHERE qrt.tenant_id = @p1 AND qrt.queue_name = @p2 AND qrt.expires_at > SYSUTCDATETIME()
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
			SET worker_id = @p4, expires_at = DATEADD(second, @p5, SYSUTCDATETIME())
			WHERE tenant_id = @p1 AND queue_name = @p2 AND workflow_id = @p3
		`, c.tenantID, *c.key, c.id, workerID, int64(claimedKeyTTL.Seconds()))
		if err != nil {
			return false, fmt.Errorf("claim workflows: move queue holder: %w", err)
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO queue_holders (tenant_id, queue_name, workflow_id, expires_at, worker_id)
		SELECT @p1, @p2, @p3, DATEADD(second, @p4, SYSUTCDATETIME()), @p5
		WHERE NOT EXISTS (
			SELECT 1 FROM queue_holders
			WHERE tenant_id = @p1 AND queue_name = @p2 AND workflow_id = @p3
		)
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
			VALUES (@p1, @p2, @p3, DATEADD(second, @p4, SYSUTCDATETIME()))
		`, c.tenantID, *c.key, c.id, int64(*ql.ratePeriodSeconds)); err != nil {
			return false, fmt.Errorf("claim workflows: record rate token: %w", err)
		}
	}
	return true, nil
}

// ClaimStickyWorkflows atomically claims up to limit runnable workflow instances
// that are sticky to this worker. Uses the sticky_worker_id filter for
// low-contention claiming. Returns fewer than limit if not enough sticky
// workflows are ready. Callers should fall back to ClaimWorkflows for remaining capacity.
// ClaimStickyWorkflows retries on errors SQL Server guarantees it rolled back --
// a deadlock victim claimed nothing, so replaying the claim is sound. Errors
// that leave the outcome unknown are not retried; see
// withRollbackGuaranteedRetry (IMPROVEMENT-PLAN.md 2.26).
func (s *MSSQLStore) ClaimStickyWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	var claimed []*WorkflowInstance
	err := withRollbackGuaranteedRetry(ctx, "claim sticky workflows", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		var err error
		claimed, err = s.claimStickyWorkflowsOnce(ctx, workerID, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// claimStickyWorkflowsOnce claims for this store's tenant only -- see
// claimWorkflowsOnce for why the predicate is load-bearing (3.91).
//
// `sticky_worker_id = @p1` is not a substitute for it. A worker id is not a
// tenant, and on a fleet where two tenants' workers were configured with the
// same id -- which nothing prevents, since the id is operator-chosen -- the
// sticky claim crossed tenants on a match rather than on a guess.
func (s *MSSQLStore) claimStickyWorkflowsOnce(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: begin: %w", err)
	}
	defer tx.Rollback()

	tqParam := s.buildTaskQueueParam()

	rows, err := tx.QueryContext(ctx, `
		UPDATE workflow_instances
		SET status = 'running',
		    signal_seq_at_claim = signal_seq,
		    signal_consumed_at_claim = signal_consumed_seq,
		    assigned_to = @p1,
		    heartbeat_at = SYSUTCDATETIME(),
		    started_at = COALESCE(started_at, SYSUTCDATETIME()),
		    generation = generation + 1
		OUTPUT INSERTED.id, INSERTED.def_name, INSERTED.def_version,
		       INSERTED.status, INSERTED.input, INSERTED.assigned_to,
		       INSERTED.next_wake_at,
		       -- CONVERT, not the raw column. SQL Server stores UNIQUEIDENTIFIER
		       -- in a mixed-endian layout, and go-mssqldb scans it into a Go
		       -- string as the 16 raw bytes rather than the canonical text --
		       -- "\x11\x11..." where the caller expects
		       -- "11111111-1111-1111-1111-111111111111". The same workaround is
		       -- applied in ResolveTenantFromAPIKey for the same reason.
		       --
		       -- This was cosmetic until the worker began routing execution on
		       -- WorkflowInstance.TenantID: 16 raw bytes are neither empty nor
		       -- equal to the worker's own tenant, so storeForTenant tried to
		       -- open a store for them and the factory rejected them as an
		       -- invalid UUID -- failing every workflow on SQL Server.
		       CONVERT(NVARCHAR(36), INSERTED.tenant_id) AS tenant_id,
		       INSERTED.created_at,
		       INSERTED.error_code, INSERTED.error_op, INSERTED.generation,
		       COALESCE(INSERTED.priority, 0) AS priority,
		       INSERTED.trace_id,
		       COALESCE(INSERTED.pending_terminal_status, '') AS pending_terminal_status
		WHERE id IN (
			SELECT id
			FROM workflow_instances WITH (READPAST, UPDLOCK, ROWLOCK)
			WHERE status = 'ready'
			  AND next_wake_at <= SYSUTCDATETIME()
			  AND sticky_worker_id = @p1
			  AND task_queue IN (SELECT value FROM STRING_SPLIT(@p2, ','))
  AND (workflow_instances.concurrency_key_hash IS NULL
       OR NOT EXISTS (SELECT 1 FROM concurrency_keys ck
                       WHERE ck.key_hash = workflow_instances.concurrency_key_hash
                         AND ck.tenant_id = workflow_instances.tenant_id
                         AND ck.expires_at > SYSUTCDATETIME()
                         AND ck.workflow_id <> workflow_instances.id))
			  AND tenant_id = @p4
			ORDER BY priority ASC, created_at
			OFFSET 0 ROWS FETCH NEXT @p3 ROWS ONLY
		)
	`, workerID, tqParam, limit, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: %w", err)
	}
	defer rows.Close()

	var wfs []*WorkflowInstance
	for rows.Next() {
		var wf WorkflowInstance
		var nextWakeAt sql.NullTime
		var tenantID sql.NullString
		var createdAt sql.NullTime
		var inputStr string
		var errorCode, errorOp sql.NullString
		var traceID sql.NullString
		var pendingTerminal sql.NullString

		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status,
			&inputStr, &wf.AssignedTo, &nextWakeAt, &tenantID, &createdAt, &errorCode, &errorOp, &wf.Generation, &wf.Priority, &traceID,
			&pendingTerminal); err != nil {
			return nil, fmt.Errorf("claim sticky workflows scan: %w", err)
		}
		wf.TraceID = traceID.String

		wf.Input = json.RawMessage(inputStr)
		if nextWakeAt.Valid {
			wf.NextWakeAt = nextWakeAt.Time
		}
		if tenantID.Valid {
			wf.TenantID = tenantID.String
		}
		if createdAt.Valid {
			wf.CreatedAt = createdAt.Time
		}
		wf.ErrorCode = errorCode.String
		wf.ErrorOp = errorOp.String
		wf.PendingTerminalStatus = pendingTerminal.String
		wfs = append(wfs, &wf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim sticky workflows rows: %w", err)
	}

	if len(wfs) == 0 {
		tx.Rollback()
		return nil, nil
	}
	return s.finishClaim(ctx, tx, workerID, limit, wfs)
}

// ---------------------------------------------------------------------------
// Workflow Lifecycle Methods (C.5)
// ---------------------------------------------------------------------------

// Heartbeat updates the heartbeat timestamp. Returns false if the workflow
// is no longer assigned to this worker.
func (s *MSSQLStore) Heartbeat(ctx context.Context, workflowID, workerID string, generation int64) (bool, error) {
	var out bool
	err := withRollbackGuaranteedRetry(ctx, "heartbeat", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		var err error
		out, err = s.heartbeatOnce(ctx, workflowID, workerID, generation)
		return err
	})
	if err != nil {
		return false, err
	}
	return out, nil
}

func (s *MSSQLStore) heartbeatOnce(ctx context.Context, workflowID, workerID string, generation int64) (bool, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return false, fmt.Errorf("heartbeat: begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET heartbeat_at = SYSUTCDATETIME()
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p3
	`, workflowID, workerID, generation)
	if err != nil {
		return false, fmt.Errorf("heartbeat: %w", err)
	}
	n, _ := result.RowsAffected()
	return n > 0, tx.Commit()
}

// BatchHeartbeat updates heartbeat_at for all workflows assigned to this worker
// with status 'running'. Uses a single UPDATE instead of N calls.
// NOTE: This intentionally does NOT check per-workflow generation because it
// operates on ALL running workflows for a worker, and generations differ per
// workflow. Individual generation-guarded operations (Heartbeat,
// CompleteWorkflow, FailWorkflow, etc.) prevent double-execution even if the
// batch heartbeat refreshes a stale workflow's heartbeat_at.
//
// AND THIS ONE MUST NOT GET A TENANT PREDICATE, which is worth saying out loud
// because every other unscoped statement in this file is a defect and an audit
// will find this one too (3.86). It is called on the WORKER'S OWN store
// (cmd/cleat-worker/setup.go's heartbeat loop), and under claim-across-tenants
// a worker legitimately holds instances belonging to many tenants -- the claim
// is deliberately cross-tenant and each instance then EXECUTES against a store
// scoped to its own tenant, but the heartbeat is one statement covering all of
// them. Scoping it to s.tenantID would silently stop refreshing every other
// tenant's instances until ReapStaleInstances took them, and nothing would say
// so.
//
// Note also that 3.77's "a generated id cannot be guessed" argument does not
// apply here in either direction: there is no id in this predicate at all. The
// key is the worker, and the set of rows a worker may touch is exactly the set
// it was handed.
func (s *MSSQLStore) BatchHeartbeat(ctx context.Context, workerID string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET heartbeat_at = SYSUTCDATETIME()
		WHERE assigned_to = @p1 AND status = 'running'
	`, workerID)
	if err != nil {
		return 0, fmt.Errorf("batch heartbeat: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, nil
}

// CompleteWorkflow marks a workflow as completed with a result.
// CompleteWorkflow retries only on errors SQL Server guarantees it rolled
// back, so a deadlock no longer loses the terminal write. ErrFenceLost is
// returned before the commit and is not an mssql.Error, so the fence
// semantics are untouched by the retry. See withRollbackGuaranteedRetry.
func (s *MSSQLStore) CompleteWorkflow(ctx context.Context, workflowID, workerID string, generation int64, result string, queryState map[string]string) error {
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

	return withRollbackGuaranteedRetry(ctx, "complete workflow", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.completeWorkflowOnce(ctx, workflowID, workerID, generation, resultJSON, queryState)
	})
}

func (s *MSSQLStore) completeWorkflowOnce(ctx context.Context, workflowID, workerID string, generation int64, resultJSON string, queryState map[string]string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("complete workflow: begin: %w", err)
	}
	defer tx.Rollback()

	qsJSON := marshalQueryState(queryState)
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'done', result = @p3, completed_at = SYSUTCDATETIME(), completed_by = assigned_to, assigned_to = NULL, query_state = @p4
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p5
	`, workflowID, workerID, resultJSON, string(qsJSON), generation)
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
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// FailWorkflow marks a workflow as failed.
// FailWorkflow retries only on errors SQL Server guarantees it rolled
// back, so a deadlock no longer loses the terminal write. ErrFenceLost is
// returned before the commit and is not an mssql.Error, so the fence
// semantics are untouched by the retry. See withRollbackGuaranteedRetry.
func (s *MSSQLStore) FailWorkflow(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
	return withRollbackGuaranteedRetry(ctx, "fail workflow", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.failWorkflowOnce(ctx, workflowID, workerID, generation, errorMsg, errorCode, errorOp, queryState)
	})
}

func (s *MSSQLStore) failWorkflowOnce(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("fail workflow: begin: %w", err)
	}
	defer tx.Rollback()

	qsJSON := marshalQueryState(queryState)
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'failed',
		    error_msg = @p3,
		    error_code = @p4,
		    error_op = @p5,
		    completed_at = SYSUTCDATETIME(),
		    completed_by = assigned_to, assigned_to = NULL,
		    query_state = @p6
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p7
	`, workflowID, workerID, errorMsg, errorCode, errorOp, string(qsJSON), generation)
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
		`UPDATE idempotency_keys SET error_msg = @p2 WHERE workflow_id = @p1 AND tenant_id = @p3`,
		workflowID, errorMsg, s.tenantID); err != nil {
		s.log().WarnContext(ctx, "idempotency update failed", "error", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	releaseWorkflowResources(s.log(), s, workflowID)
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// MoveToDeadLetterQueue marks a workflow as dead_lettered because it failed
// after exhausting all retry attempts.
// MoveToDeadLetterQueue retries only on errors SQL Server guarantees it rolled
// back, so a deadlock no longer loses the terminal write. ErrFenceLost is
// returned before the commit and is not an mssql.Error, so the fence
// semantics are untouched by the retry. See withRollbackGuaranteedRetry.
func (s *MSSQLStore) MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string) error {
	return withRollbackGuaranteedRetry(ctx, "move to dead letter queue", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.moveToDeadLetterQueueOnce(ctx, workflowID, workerID, generation, errMsg, errorCode, errorOp)
	})
}

func (s *MSSQLStore) moveToDeadLetterQueueOnce(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("move to dead letter queue: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'dead_lettered', error_msg = @p3, error_code = @p4, error_op = @p5,
		    completed_at = SYSUTCDATETIME(), completed_by = assigned_to, assigned_to = NULL
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p6
	`, workflowID, workerID, errMsg, errorCode, errorOp, generation)
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
		`UPDATE idempotency_keys SET error_msg = @p2 WHERE workflow_id = @p1 AND tenant_id = @p3`,
		workflowID, errMsg, s.tenantID); err != nil {
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
// Resets status to 'ready', clears the worker assignment and error fields,
// and sets next_wake_at to now so the workflow is re-queued immediately.
//
// `AND tenant_id` is load-bearing -- see TerminateWorkflow. `status =
// 'dead_lettered'` narrows the blast radius but does not close it: a workflow
// another tenant has given up on is exactly the kind whose id has already been
// pasted into a ticket.
func (s *MSSQLStore) RetryWorkflow(ctx context.Context, workflowID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', completed_by = assigned_to, assigned_to = NULL, heartbeat_at = NULL,
		    error_msg = NULL, error_code = NULL, error_op = NULL,
		    next_wake_at = SYSUTCDATETIME()
		WHERE id = @p1 AND status = 'dead_lettered' AND tenant_id = @p2
	`, workflowID, s.tenantID)
	return err
}

// ReleaseWorkflow returns a workflow to the ready queue with a next wake time.
func (s *MSSQLStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error {
	return withRollbackGuaranteedRetry(ctx, "release workflow", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.releaseWorkflowOnce(ctx, workflowID, workerID, generation, nextWakeAt)
	})
}

func (s *MSSQLStore) releaseWorkflowOnce(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error {
	tx, err := s.beginTxWithContext(ctx)
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
	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = CASE WHEN pending_terminal_status IS NOT NULL
		                  THEN 'terminating' ELSE 'ready' END,
		    assigned_to = NULL, next_wake_at = @p3
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p4
	`, workflowID, workerID, nextWakeAt, generation)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("release workflow: rows affected: %w", err)
	}
	// A zero-row update is a lost fence, not a failure and not a success.
	// cmd/cleat-worker's releaseWorkflow branches on ErrFenceLost and treats
	// it as "the no-op it is", logging at Debug; any OTHER error is logged as
	// "release failed, workflow stays claimed until its lease expires", which
	// is untrue of a stale release on both counts.
	//
	// This dialect already DETECTED the condition and reported it as a raw
	// error naming ROWS rather than the situation, so errors.Is was false and
	// SQL Server deployments logged that false warning on every stale release
	// while PostgreSQL and MySQL said nothing at all. Only the vocabulary
	// changes here; the behaviour was already right. cleat#1223.
	if rows == 0 {
		return ErrFenceLost
	}

	return tx.Commit()
}

// ContinueAsNew atomically creates a new workflow run and completes the current
// one in a single database transaction. Returns the new run ID on success.
// ContinueAsNew retries only on errors SQL Server guarantees it rolled back.
// See withRollbackGuaranteedRetry.
func (s *MSSQLStore) ContinueAsNew(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput json.RawMessage, newEvents []EventRecord, result string, queryState map[string]string, priority int) (string, error) {
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

	var newRunID string
	err := withRollbackGuaranteedRetry(ctx, "continue as new", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		var err error
		newRunID, err = s.continueAsNewOnce(ctx, currentRunID, workerID, generation, defName, defVersion, newInput, newEvents, resultJSON, queryState, priority)
		return err
	})
	if err != nil {
		return "", err
	}
	return newRunID, nil
}

func (s *MSSQLStore) continueAsNewOnce(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput json.RawMessage, newEvents []EventRecord, resultJSON string, queryState map[string]string, priority int) (string, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return "", fmt.Errorf("continue as new: begin: %w", err)
	}
	defer tx.Rollback()

	// Append events within the same transaction.
	if err := s.appendEventsInTx(ctx, tx, currentRunID, newEvents); err != nil {
		return "", fmt.Errorf("continue as new: append events: %w", err)
	}

	// Use the store's tenant scope to preserve tenant isolation.
	// Create the new workflow run with a Go-generated UUID.
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
		SELECT @p1, @p2, @p3, 'ready', CAST(@p4 AS VARCHAR(MAX)),
		       ISNULL((SELECT task_queue FROM workflow_defs WHERE name = @p2 AND version = @p3 AND tenant_id = @p5), 'default'),
		       @p5, @p6, @p7, p.parent_workflow_id, p.parent_close_policy
		FROM workflow_instances p WHERE p.id = @p7
	`, newRunID, defName, defVersion, newInput, s.tenantID, priority, currentRunID)
	if err != nil {
		return "", fmt.Errorf("continue as new: start new run: %w", err)
	}

	// Complete the current run.
	qsJSON := marshalQueryState(queryState)
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'done', result = @p3, completed_at = SYSUTCDATETIME(), completed_by = assigned_to, assigned_to = NULL, query_state = @p4
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p5
	`, currentRunID, workerID, resultJSON, string(qsJSON), generation)
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
// FinalizeWorkflowSegment retries only on errors SQL Server guarantees it rolled
// back, so a deadlock no longer loses the terminal write. ErrFenceLost is
// returned before the commit and is not an mssql.Error, so the fence
// semantics are untouched by the retry. See withRollbackGuaranteedRetry.
func (s *MSSQLStore) finalizeWorkflowSegmentInner(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
	return withRollbackGuaranteedRetry(ctx, "finalize workflow segment", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.finalizeWorkflowSegmentOnce(ctx, runID, workerID, generation, newEvents, finalStatus, result, errorCode, errorOp, queryState, nextWakeAt)
	})
}

func (s *MSSQLStore) finalizeWorkflowSegmentOnce(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
	if !validFinalStatus(finalStatus) {
		return fmt.Errorf("finalize workflow: unknown final status: %s", finalStatus)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finalize workflow: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.setSessionContext(tx); err != nil {
		return fmt.Errorf("finalize workflow: set session: %w", err)
	}

	// Append new events within the same transaction.
	if err := s.appendEventsInTx(ctx, tx, runID, newEvents); err != nil {
		return fmt.Errorf("finalize workflow: append events: %w", err)
	}

	// Delegate the terminal UPDATEs (status, idempotency, parent wake,
	// await_child population) to a server-side stored procedure.
	// This replaces 5 individual round-trips with 1 procedure call.
	qsJSON := marshalQueryState(queryState)
	resultJSON := coerceResultJSON(ctx, s.log(), runID, result)

	var fenceHeld bool
	if err := tx.QueryRowContext(ctx, `
		EXEC finalize_workflow_status @p1, @p2, @p3, @p4, @p5, @p6, @p7, @p8, @p9, @p10
	`, runID, workerID, generation, finalStatus, resultJSON, errorCode, errorOp, string(qsJSON), nextWakeAt, s.notifyChannel).Scan(&fenceHeld); err != nil {
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

// RequestCancellation sets the cancellation flag on a workflow.
//
// `AND tenant_id` is load-bearing -- see TerminateWorkflow. The id reaches here
// from cmd/cleat-worker/server.go's handleCancel, out of the URL path.
func (s *MSSQLStore) RequestCancellation(ctx context.Context, workflowID, reason string) error {
	return withRollbackGuaranteedRetry(ctx, "request cancellation", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.requestCancellationOnce(ctx, workflowID, reason)
	})
}

func (s *MSSQLStore) requestCancellationOnce(ctx context.Context, workflowID, reason string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("request cancellation: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET cancellation_requested = 1, cancellation_reason = @p2
		WHERE id = @p1 AND tenant_id = @p3
	`, workflowID, reason, s.tenantID)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// CheckCancellation checks if a workflow has been cancelled.
func (s *MSSQLStore) CheckCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	var cancelled bool
	var reason sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT cancellation_requested, cancellation_reason
		FROM workflow_instances WHERE id = @p1
	`, workflowID).Scan(&cancelled, &reason)
	if err != nil {
		return false, "", err
	}
	return cancelled, reason.String, nil
}

// StartNewRun creates a new workflow instance.
// If idempotencyKey is non-empty, provides exactly-once semantics.
// StartNewRun is the entry point without a concurrency key.
func (s *MSSQLStore) StartNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int) (string, bool, error) {
	return s.startNewRun(ctx, runID, defName, defVersion, input, idempotencyKey, tenantID, priority, StartOptions{})
}

// StartNewRunWithConcurrencyKey records the key on the row in the INSERT that
// creates it. See the PostgreSQL implementation for why it is written by the
// insert and why this is not on the interface.
//
// Hashed in Go, matching AcquireConcurrencyKey on this store. Doing it in SQL
// here would be silently wrong: HASHBYTES over NVARCHAR hashes UTF-16, and
// every existing key_hash was written from Go over UTF-8, so the two never
// match. Migration 058's comment carries the full account.
func (s *MSSQLStore) StartNewRunWithConcurrencyKey(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int, concurrencyKey string) (string, bool, error) {
	return s.startNewRun(ctx, runID, defName, defVersion, input, idempotencyKey, tenantID, priority, StartOptions{ConcurrencyKey: concurrencyKey})
}

// StartNewRunWithOptions records every per-run value a start can set, in the
// INSERT that creates the run. See StartOptions.
func (s *MSSQLStore) StartNewRunWithOptions(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int, opts StartOptions) (string, bool, error) {
	return s.startNewRun(ctx, runID, defName, defVersion, input, idempotencyKey, tenantID, priority, opts)
}

func (s *MSSQLStore) startNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int, opts StartOptions) (string, bool, error) {
	var newID string
	var existed bool
	err := withRollbackGuaranteedRetry(ctx, "start new run", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		var err error
		newID, existed, err = s.startNewRunOnce(ctx, runID, defName, defVersion, input, idempotencyKey, tenantID, priority, opts)
		return err
	})
	if err != nil {
		return "", false, err
	}
	return newID, existed, nil
}

func (s *MSSQLStore) startNewRunOnce(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int, opts StartOptions) (string, bool, error) {
	concurrencyKey := opts.ConcurrencyKey
	runInstanceMs := msOrNil(opts.RunLimits.WasmInstanceTimeout)
	runWallClockMs := msOrNil(opts.RunLimits.WasmWallClockCeiling)
	runRetryMs := msOrNil(opts.RunLimits.HostRetryBudget)
	runMaxWorkflowMs := msOrNil(opts.RunLimits.MaxWorkflowDuration)
	// Computed here rather than in the caller because this function is RETRIED
	// -- withRollbackGuaranteedRetry may run it several times -- and the hash
	// must be identical on every attempt. It is a pure function of the key, so
	// this is cheap and cannot drift between retries.
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
		// migrations/mssql/010_idempotency_keys_tenant_id.sql,
		// IMPROVEMENT-PLAN 3.10.
		var existingWfID string
		var existingDef sql.NullString
		var existingDigest sql.NullString
		err := s.db.QueryRowContext(ctx,
			`SELECT workflow_id, def_name, input_digest FROM idempotency_keys
			 WHERE key_hash = @p1 AND tenant_id = @p2 AND expires_at > SYSUTCDATETIME()`,
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

		// Use the provided runID (already generated above).

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return "", false, err
		}
		defer tx.Rollback()

		// Clear this key's row if its TTL has passed, so the insert below can
		// take the key over.
		//
		// WITHOUT THIS, `n == 0` BELOW HAS TWO CAUSES AND THE CODE ASSUMES
		// ONE: an expired row still blocks the insert and still reports no
		// rows affected, identical to the concurrent-insert case it is read
		// as. Only one of the two has a winner to re-read, and the re-read
		// filters on expiry, so for the other it looks for a row it cannot see
		// and the caller gets sql.ErrNoRows instead of a new run. cleat#1671.
		//
		// Not a visibility fix: letting the re-read see the expired row would
		// hand the caller a workflow id whose key the TTL already retired.
		//
		// The expiry predicate is load bearing -- a row refreshed by a
		// concurrent starter is LIVE, and deleting it would let two runs hold
		// one key. Then this matches nothing, the insert reports the conflict,
		// and that path is already correct.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM idempotency_keys
			 WHERE key_hash = @p1 AND tenant_id = @p2 AND expires_at <= SYSUTCDATETIME()`,
			keyHash[:], tenantID); err != nil {
			return "", false, fmt.Errorf("start new run: clear expired idempotency key: %w", err)
		}

		// Insert idempotency key record. INSERT...WHERE NOT EXISTS handles the
		// race where two requests arrive with the same key simultaneously --
		// and, since the delete above, ONLY that: a row the NOT EXISTS finds
		// is necessarily live.
		ttlSeconds := int(s.idempotencyKeyTTL.Seconds())
		result, err := tx.ExecContext(ctx,
			`INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id, def_name, input_digest)
			 SELECT @p1, @p2, DATEADD(SECOND, @p3, SYSUTCDATETIME()), @p4, @p5, @p6
			 WHERE NOT EXISTS (
			     SELECT 1 FROM idempotency_keys
			     WHERE key_hash = @p1 AND tenant_id = @p4
			 )`,
			keyHash[:], runID, ttlSeconds, tenantID, defName, inputDigest)

		// A DUPLICATE KEY HERE IS THE RACE, NOT A FAILURE. cleat#1753.
		//
		// `INSERT ... WHERE NOT EXISTS` is two operations, and SQL Server does
		// not hold a lock between them at READ COMMITTED: two starters can both
		// find NOT EXISTS true and both proceed, and the loser's insert violates
		// pk_idempotency_keys with 2627. That is precisely the condition the
		// `n == 0` branch below already handles -- a live row exists and this
		// caller should replay it -- so it is routed there rather than returned.
		//
		// MEASURED. Six rounds of eight racers on one key, before this:
		//
		//	Violation of PRIMARY KEY constraint 'pk_idempotency_keys'.
		//	Cannot insert duplicate key in object 'dbo.idempotency_keys'. (2627)
		//
		// A single round passed, repeatedly, which is why this survived: the
		// window needs more than one race to show up reliably.
		//
		// NOT AN UPDLOCK/HOLDLOCK HINT, which is the other standard repair.
		// That serialises the check and the insert, and it converts this into
		// the classic SQL Server upsert deadlock under the same contention --
		// trading a duplicate-key error every caller can recover from for a
		// 1205 that needs its own retry. Reading the outcome is cheaper than
		// preventing it, and the outcome is already meaningful here.
		//
		// The statement is rolled back, not the transaction: a constraint
		// violation does not abort under the default XACT_ABORT OFF, and this
		// path rolls the transaction back itself a few lines down regardless.
		var n int64
		if err != nil {
			if !isMSSQLDuplicateKey(err) {
				return "", false, err
			}
			n = 0
		} else {
			n, _ = result.RowsAffected()
		}
		if n == 0 {
			// A LIVE row exists, so someone else won the race. After the
			// delete above the NOT EXISTS can only match a row whose TTL has
			// not passed, so the re-read's own expiry filter will find it
			// (cleat#1671).
			//
			// Rollback and return the existing one.
			tx.Rollback()
			err := s.db.QueryRowContext(ctx,
				`SELECT workflow_id, def_name, input_digest FROM idempotency_keys
				 WHERE key_hash = @p1 AND tenant_id = @p2 AND expires_at > SYSUTCDATETIME()`,
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

		if err := s.setSessionContext(tx); err != nil {
			return "", false, fmt.Errorf("start new run: set session: %w", err)
		}

		// Insert the workflow instance.
		_, err = tx.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority, concurrency_key, concurrency_key_hash, run_wasm_instance_timeout_ms, run_wasm_wall_clock_ceiling_ms, run_host_retry_budget_ms, run_max_workflow_duration_ms)
			VALUES (@p1, @p2, @p3, 'ready', CAST(@p4 AS NVARCHAR(MAX)),
			        ISNULL((SELECT task_queue FROM workflow_defs WHERE name = @p2 AND version = @p3 AND tenant_id = @p5), 'default'),
			        @p5, @p6, @p7, @p8, @p9, @p10, @p11, @p12)
		`, runID, defName, defVersion, string(input), tenantID, priority, ckText, ckHash, runInstanceMs, runWallClockMs, runRetryMs, runMaxWorkflowMs)
		if err != nil {
			return "", false, fmt.Errorf("start new run: %w", err)
		}

		return runID, false, tx.Commit()
	}

	// No idempotency key — normal flow.
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return "", false, fmt.Errorf("start new run: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority, concurrency_key, concurrency_key_hash, run_wasm_instance_timeout_ms, run_wasm_wall_clock_ceiling_ms, run_host_retry_budget_ms, run_max_workflow_duration_ms)
		VALUES (@p1, @p2, @p3, 'ready', CAST(@p4 AS NVARCHAR(MAX)),
		        ISNULL((SELECT task_queue FROM workflow_defs WHERE name = @p2 AND version = @p3 AND tenant_id = @p5), 'default'),
		        @p5, @p6, @p7, @p8, @p9, @p10, @p11, @p12)
	`, runID, defName, defVersion, string(input), tenantID, priority, ckText, ckHash, runInstanceMs, runWallClockMs, runRetryMs, runMaxWorkflowMs)
	if err != nil {
		return "", false, fmt.Errorf("start new run: %w", err)
	}
	return runID, false, tx.Commit()
}

// ---------------------------------------------------------------------------
// Best-effort cleanup helpers
// ---------------------------------------------------------------------------

// enforceParentClosePolicy applies ParentClosePolicy to all child workflows
// of the given parent workflow. Best-effort post-commit cleanup.
// enforceParentClosePolicy applies a terminated parent's close policy to its
// children.
//
// It used to discard every error it produced: neither ExecContext's nor
// Commit's return value was assigned, in either of its two transactions. So
// when this failed -- a deadlock against a worker claiming one of those
// children is the obvious way -- the children of a terminated parent simply
// kept running, and nothing recorded it. That is the 1.2 shape: a write whose
// failure is structurally invisible.
//
// The errors are now checked, retried when SQL Server guarantees the
// transaction rolled back, and logged when they survive that. The function
// stays void because its callers treat it as best-effort post-commit cleanup;
// what changes is that a failure is now observable instead of silent.
// enforceParentClosePolicy closes the children of a parent that has reached a
// terminal state.
//
// `AND tenant_id` on both steps and on childrenClosedByTerminate is
// load-bearing (3.92), and the reason is specific to how this is CALLED rather
// than to the statements themselves: preemptivelySettleOnce invokes this
// unconditionally after its commit and never looks at how many rows the
// terminate touched. 3.86 put a tenant predicate on the terminate itself, so a
// cross-tenant terminate now matches no parent -- and then reached here anyway
// with an id belonging to somebody else.
//
// Measured before the fix, on a dbo.cleat_admin pool with tenant B terminating
// tenant A's parent:
//
//	AFTER: tenant A's child status="failed" error_msg="parent workflow terminated"
//
// while A's PARENT was untouched and still running. That is worse than an
// ordinary unauthorised write: the error message names a cause that did not
// happen, and nothing in A's history explains it.
//
// Children always carry their parent's tenant -- StartChildWorkflow and
// StartChildWorkflowAtomic both insert tenant_id = s.tenantID -- and this runs
// on the store that owns the parent, so the predicate changes nothing on the
// working path.
//
// THE DEEPER FIX IS NOT THIS ONE, and is deliberately not taken here.
// adminForceResolve does what preemptivelySettleOnce does not: it checks
// RowsAffected and returns adminNotFound before it can reach this function.
// "Do not cascade for a workflow you did not terminate" is the actual bug;
// a predicate on the cascade is its symptom-level twin. Closing it properly
// changes what the HTTP layer returns for an unknown id -- today a 200 -- so
// it wants its own change and its own argument. 3.92.
// mssqlParentCloseDeferPhase is the TERMINATE arm for children that owe
// cleanup, assembled once rather than concatenated at its use site.
//
// The shape is not stylistic. TestMSSQLTenantScopedTablesAreQueriedWithATenant-
// Predicate reads SQL out of SOURCE STRING LITERALS, and a statement assembled
// by concatenating a Go constant into the middle of one is invisible to it past
// the first fragment. Written inline, the guard saw only the text up to the
// opening quote of the status value -- no WHERE clause at all -- and reported
// this statement as having no tenant predicate. It has one. The guard could not
// see it.
//
// Do not quote SQL in backticks in a comment near here. The guard's extraction
// is a regex over backtick-delimited text and does not know a Go comment from
// code, so a comment quoting the offending fragment becomes an offending
// fragment. That is how the first version of THIS comment failed the guard it
// exists to explain.
//
// The available fix was an allowlist entry, which the guard invites and which
// would have been the wrong trade: it exempts the whole FUNCTION, so every
// future edit to enforceParentClosePolicy would go unchecked on the dialect
// where an explicit predicate is the entire isolation mechanism
// (dbo.fn_tenant_filter is off for any dbo.cleat_admin connection). One
// Sprintf keeps the statement, and the predicate, statically readable.
//
// The two interpolations are a compile-time constant expression and a
// package-level SQL fragment, neither reachable from a caller.
var mssqlParentCloseDeferPhase = fmt.Sprintf(`
		UPDATE workflow_instances
		SET status = 'terminating',
		    pending_terminal_status = 'failed',
		    defer_phase_deadline = %s,
		    error_msg = 'parent workflow terminated',
		    next_wake_at = SYSUTCDATETIME(),
		    assigned_to = NULL,
		    generation = generation + 1
		WHERE parent_workflow_id = @p1
		  AND parent_close_policy = 'TERMINATE'
		  AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
		  AND tenant_id = @p2
		  AND %s
	`, deferPhaseDeadlineMSSQL, deferPhaseOwedSQL)

func (s *MSSQLStore) enforceParentClosePolicy(ctx context.Context, parentWorkflowID string) {
	s.enforceParentClosePolicyAt(ctx, parentWorkflowID, 0)
}

// enforceParentClosePolicyAt is enforceParentClosePolicy with the recursion
// depth carried explicitly. See cascadeIntoClosedChildren.
func (s *MSSQLStore) enforceParentClosePolicyAt(ctx context.Context, parentWorkflowID string, depth int) {
	steps := []struct {
		policy string
		query  string
	}{
		// Two TERMINATE arms, split by whether the child owes cleanup. See
		// PostgresStore's enforceParentClosePolicy. IMPROVEMENT-PLAN 3.114.
		{"TERMINATE", `
		UPDATE workflow_instances
		SET status = 'failed', error_msg = 'parent workflow terminated',
		    pending_terminal_status = NULL, defer_phase_deadline = NULL,
		    completed_at = SYSUTCDATETIME(),
		    completed_by = assigned_to, assigned_to = NULL, generation = generation + 1
		WHERE parent_workflow_id = @p1
		  AND parent_close_policy = 'TERMINATE'
		  AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
		  AND tenant_id = @p2
		  AND NOT ` + deferPhaseOwedSQL + `
	`},
		{"TERMINATE (defer phase)", mssqlParentCloseDeferPhase},
		{"REQUEST_CANCEL", `
		UPDATE workflow_instances
		SET cancellation_requested = 1
		WHERE parent_workflow_id = @p1
		  AND parent_close_policy = 'REQUEST_CANCEL'
		  AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
		  AND tenant_id = @p2
	`},
	}

	// Collected before the UPDATE: see releaseTerminatedChildren.
	terminated, listErr := s.childrenClosedByTerminate(ctx, parentWorkflowID)
	if listErr != nil {
		s.log().WarnContext(ctx, "enforceParentClosePolicy: could not list TERMINATE children; their concurrency keys and sticky-worker assignments stay held until TTL",
			"parent_workflow_id", parentWorkflowID, "error", listErr)
	}

	for _, step := range steps {
		err := withRollbackGuaranteedRetry(ctx, "enforce parent close policy "+step.policy,
			mssqlTxRetries, mssqlTxRetryDelay, func() error {
				tx, err := s.beginTxWithContext(ctx)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				if _, err := tx.ExecContext(ctx, step.query, parentWorkflowID, s.tenantID); err != nil {
					return err
				}
				return tx.Commit()
			})
		if err != nil {
			s.log().WarnContext(ctx, "enforceParentClosePolicy failed; children of a terminated parent are unaffected by its close policy",
				"policy", step.policy, "parent_workflow_id", parentWorkflowID, "error", err)
			if step.policy == "TERMINATE" {
				terminated = nil
			}
		}
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
func (s *MSSQLStore) childrenClosedByTerminate(ctx context.Context, parentWorkflowID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM workflow_instances
		WHERE parent_workflow_id = @p1
		  AND parent_close_policy = 'TERMINATE'
		  AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
		  AND tenant_id = @p2
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
func (s *MSSQLStore) finishClaim(ctx context.Context, tx *sql.Tx, workerID string, limit int, wfs []*WorkflowInstance) ([]*WorkflowInstance, error) {
	keep, excess := enforceClaimLimit(ctx, s.log(), "mssql", workerID, limit, wfs)
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
func (s *MSSQLStore) FinalizeWorkflowSegment(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
	return wrapRejectedResult(
		s.finalizeWorkflowSegmentInner(ctx, runID, workerID, generation, newEvents,
			finalStatus, result, errorCode, errorOp, queryState, nextWakeAt),
		runID, result)
}
