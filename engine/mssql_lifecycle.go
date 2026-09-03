package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

	rows, err := tx.QueryContext(ctx, `
		UPDATE workflow_instances
		SET status = 'running',
		    assigned_to = @p1,
		    heartbeat_at = SYSUTCDATETIME(),
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
		       INSERTED.trace_id
		WHERE id IN (
			SELECT id
			FROM workflow_instances WITH (READPAST, UPDLOCK, ROWLOCK)
			WHERE status = 'ready'
			  AND next_wake_at <= SYSUTCDATETIME()
			  AND task_queue IN (SELECT value FROM STRING_SPLIT(@p2, ','))
			  AND tenant_id = @p4
			ORDER BY priority ASC, created_at
			OFFSET 0 ROWS FETCH NEXT @p3 ROWS ONLY
		)
	`, workerID, tqParam, limit, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: %w", err)
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

		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status,
			&inputStr, &wf.AssignedTo, &nextWakeAt, &tenantID, &createdAt, &errorCode, &errorOp, &wf.Generation, &wf.Priority, &traceID); err != nil {
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
		wfs = append(wfs, &wf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim workflows rows: %w", err)
	}

	if len(wfs) == 0 {
		tx.Rollback()
		return nil, nil
	}
	return s.finishClaim(ctx, tx, workerID, limit, wfs)
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
		    assigned_to = @p1,
		    heartbeat_at = SYSUTCDATETIME(),
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
		       INSERTED.trace_id
		WHERE id IN (
			SELECT id
			FROM workflow_instances WITH (READPAST, UPDLOCK, ROWLOCK)
			WHERE status = 'ready'
			  AND next_wake_at <= SYSUTCDATETIME()
			  AND sticky_worker_id = @p1
			  AND task_queue IN (SELECT value FROM STRING_SPLIT(@p2, ','))
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

		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status,
			&inputStr, &wf.AssignedTo, &nextWakeAt, &tenantID, &createdAt, &errorCode, &errorOp, &wf.Generation, &wf.Priority, &traceID); err != nil {
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

// ClaimWorkflowsAcrossTenants claims runnable workflows for every tenant.
//
// Unlike PostgresStore and MySQLStore, this is not a different query: SQL
// Server's isolation was never an application-level "AND tenant_id = ?"
// predicate to begin with, it is entirely dbo.fn_tenant_filter (the RLS
// filter predicate bound to workflow_instances and six other tables). That
// predicate already special-cases exactly this call:
//
//	WHERE @tenant_id = CAST(SESSION_CONTEXT(N'tenant_id') AS UNIQUEIDENTIFIER)
//	   OR IS_ROLEMEMBER(N'cleat_admin') = 1
//
// so a connection whose login is a member of dbo.cleat_admin sees every row
// regardless of SESSION_CONTEXT -- unset, stale, or set to some other
// tenant's ID, it does not matter, because the OR admits the row on role
// membership alone. migrations/mssql/012_admin_role.sql documents the
// mechanism and, deliberately, ships the role with no members: granting it is
// a deployment decision.
//
// That is why claimWorkflowsAcrossTenantsOnce below runs the same SELECT FOR
// UPDATE / UPDATE / OUTPUT statement claimWorkflowsOnce does, with two
// differences. First, it never calls setSessionContext or beginTxWithContext
// -- setting SESSION_CONTEXT to any one tenant here would be misleading (this
// call is not scoped to a tenant) and is not even load-bearing for an admin
// connection, since the role check bypasses it either way. Second, its OUTPUT
// clause reads tenant_id through CONVERT(NVARCHAR(36), ...), which
// claimWorkflowsOnce and claimStickyWorkflowsOnce do not: they scan
// INSERTED.tenant_id (a UNIQUEIDENTIFIER) straight into a Go string, and
// go-mssqldb hands back that type's raw 16-byte storage rather than the
// hyphenated form -- the same hazard ResolveTenantFromAPIKey's doc comment
// already names ("MSSQL UNIQUEIDENTIFIER mixed-endian storage") and works
// around the same way. It went unnoticed there because nothing asserted on a
// claimed row's TenantID string content until this method's own integration
// test did. This call cannot inherit that: CrossTenantClaimer's entire
// contract is that the caller re-scopes on TenantID
// (cmd/cleat-worker's storeForTenant), so a mangled value here would silently
// misroute every claimed workflow rather than merely being an unused field.
// Whether the sibling methods should be fixed the same way is a separate
// question -- they don't currently promise TenantID to a caller that acts on
// it the way this one does -- and is left to whoever owns that call.
//
// What this method cannot do is turn a non-admin connection into one: it
// cannot grant itself the role, and running the claim without checking first
// would silently return at most one tenant's work (whatever SESSION_CONTEXT
// happens to hold, or nothing at all if it was never set) -- which reads
// exactly like an idle queue to whoever is watching the dispatch loop, the
// specific failure mode CrossTenantClaimer exists to avoid. So this checks
// IS_ROLEMEMBER itself, before touching workflow_instances, and returns
// ErrCrossTenantClaimUnsupported naming the missing grant and the file that
// documents it, rather than an empty and misleading claim. The worker warns
// once and falls back to the per-tenant claim, the same answer MySQL's
// per-tenant-database topology gets: the operator is told, and dispatch keeps
// running on what this connection can legitimately see.
func (s *MSSQLStore) ClaimWorkflowsAcrossTenants(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	if err := s.requireCleatAdminMembership(ctx); err != nil {
		return nil, err
	}

	var claimed []*WorkflowInstance
	err := withRollbackGuaranteedRetry(ctx, "claim workflows across tenants", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		var err error
		claimed, err = s.claimWorkflowsAcrossTenantsOnce(ctx, workerID, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// requireCleatAdminMembership fails loudly, naming the missing grant, when
// this store's connection cannot see across tenants. A claim run without this
// check would not fail -- it would succeed and quietly return the wrong
// answer, which is worse. See the doc comment on ClaimWorkflowsAcrossTenants.
func (s *MSSQLStore) requireCleatAdminMembership(ctx context.Context) error {
	var isMember sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT IS_ROLEMEMBER(N'cleat_admin')`,
	).Scan(&isMember); err != nil {
		return fmt.Errorf("claim workflows across tenants: check cleat_admin membership: %w", err)
	}
	if isMember.Int64 == 1 {
		return nil
	}
	// IS_ROLEMEMBER returns NULL (not 0) when the role itself does not exist,
	// which means migration 012 was never applied at all rather than merely
	// not granted. Both reach here as isMember.Int64 == 0 (NullInt64's zero
	// value), and the message below covers both: applying the migration is
	// also step one of granting membership.
	// ErrCrossTenantClaimUnsupported, not a bare error, and for the same reason
	// MySQL returns it: this is a provisioning gap, not a claim that went
	// wrong. The worker answers it by warning once and falling back to the
	// per-tenant claim, so a deployment that passed --claim-across-tenants
	// without granting the role keeps dispatching its own tenant's work instead
	// of failing every poll. The message carries the remediation, because the
	// warning is the only place an operator will see it.
	return fmt.Errorf("claim workflows across tenants: this connection is not a member of "+
		"dbo.cleat_admin, so the RLS filter predicate (dbo.fn_tenant_filter) admits only rows "+
		"whose tenant_id matches this connection's SESSION_CONTEXT -- claiming across tenants "+
		"through it would silently see at most one tenant's work and look exactly like an idle "+
		"queue for everyone else. Grant membership as documented in "+
		"migrations/mssql/012_admin_role.sql: CREATE LOGIN, CREATE USER, then "+
		"ALTER ROLE cleat_admin ADD MEMBER, and point this store's connection at that login: %w",
		ErrCrossTenantClaimUnsupported)
}

func (s *MSSQLStore) claimWorkflowsAcrossTenantsOnce(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	// No beginTxWithContext: that helper sets SESSION_CONTEXT to this store's
	// tenantID, which is irrelevant here on an admin connection (see the doc
	// comment above) and would misstate what this call is scoped to. A plain
	// transaction is enough -- the UPDATE...OUTPUT below is already atomic.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claim workflows across tenants: begin: %w", err)
	}
	defer tx.Rollback()

	tqParam := s.buildTaskQueueParam()

	rows, err := tx.QueryContext(ctx, `
		UPDATE workflow_instances
		SET status = 'running',
		    assigned_to = @p1,
		    heartbeat_at = SYSUTCDATETIME(),
		    generation = generation + 1
		OUTPUT INSERTED.id, INSERTED.def_name, INSERTED.def_version,
		       INSERTED.status, INSERTED.input, INSERTED.assigned_to,
		       INSERTED.next_wake_at, CONVERT(NVARCHAR(36), INSERTED.tenant_id) AS tenant_id,
		       INSERTED.created_at, INSERTED.error_code, INSERTED.error_op, INSERTED.generation,
		       COALESCE(INSERTED.priority, 0) AS priority,
		       INSERTED.trace_id
		WHERE id IN (
			SELECT id
			FROM workflow_instances WITH (READPAST, UPDLOCK, ROWLOCK)
			WHERE status = 'ready'
			  AND next_wake_at <= SYSUTCDATETIME()
			  AND task_queue IN (SELECT value FROM STRING_SPLIT(@p2, ','))
			ORDER BY priority ASC, created_at
			OFFSET 0 ROWS FETCH NEXT @p3 ROWS ONLY
		)
	`, workerID, tqParam, limit)
	if err != nil {
		return nil, fmt.Errorf("claim workflows across tenants: %w", err)
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

		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status,
			&inputStr, &wf.AssignedTo, &nextWakeAt, &tenantID, &createdAt, &errorCode, &errorOp, &wf.Generation, &wf.Priority, &traceID); err != nil {
			return nil, fmt.Errorf("claim workflows across tenants scan: %w", err)
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
		wfs = append(wfs, &wf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim workflows across tenants rows: %w", err)
	}

	if len(wfs) == 0 {
		tx.Rollback()
		return nil, nil
	}
	// finishClaim's excess-release path goes through ReleaseWorkflow, which on
	// MSSQL carries no explicit tenant_id predicate at all -- isolation here is
	// 100% dbo.fn_tenant_filter, so a release issued through this same admin
	// connection sees and releases an excess row regardless of which tenant it
	// belongs to. Unlike MySQLStore's cross-tenant claim, reusing finishClaim
	// is safe here.
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
	return withRollbackGuaranteedRetry(ctx, "complete workflow", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		return s.completeWorkflowOnce(ctx, workflowID, workerID, generation, result, queryState)
	})
}

func (s *MSSQLStore) completeWorkflowOnce(ctx context.Context, workflowID, workerID string, generation int64, result string, queryState map[string]string) error {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return fmt.Errorf("complete workflow: begin: %w", err)
	}
	defer tx.Rollback()

	qsJSON := marshalQueryState(queryState)
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'done', result = @p3, completed_at = SYSUTCDATETIME(), assigned_to = NULL, query_state = @p4
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p5
	`, workflowID, workerID, result, string(qsJSON), generation)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete workflow: rows affected: %w", err)
	}
	if n == 0 {
		// Another worker now owns this workflow. Roll back rather than
		// commit: the idempotency-key write and post-commit cleanup below
		// are not safe to run on the new owner's behalf.
		return ErrFenceLost
	}

	// Record idempotency result within the transaction (best-effort).
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET result = @p2 WHERE workflow_id = @p1`,
		workflowID, result); err != nil {
		s.log().WarnContext(ctx, "idempotency update failed", "error", err)
	}

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
		    assigned_to = NULL,
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
		`UPDATE idempotency_keys SET error_msg = @p2 WHERE workflow_id = @p1`,
		workflowID, errorMsg); err != nil {
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
		    completed_at = SYSUTCDATETIME(), assigned_to = NULL
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
		`UPDATE idempotency_keys SET error_msg = @p2 WHERE workflow_id = @p1`,
		workflowID, errMsg); err != nil {
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
		SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL,
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

	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, next_wake_at = @p3
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p4
	`, workflowID, workerID, nextWakeAt, generation)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("release workflow: rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("release workflow: no rows affected for %s", workflowID)
	}

	return tx.Commit()
}

// ContinueAsNew atomically creates a new workflow run and completes the current
// one in a single database transaction. Returns the new run ID on success.
// ContinueAsNew retries only on errors SQL Server guarantees it rolled back.
// See withRollbackGuaranteedRetry.
func (s *MSSQLStore) ContinueAsNew(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput json.RawMessage, newEvents []EventRecord, result string, queryState map[string]string, priority int) (string, error) {
	var newRunID string
	err := withRollbackGuaranteedRetry(ctx, "continue as new", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		var err error
		newRunID, err = s.continueAsNewOnce(ctx, currentRunID, workerID, generation, defName, defVersion, newInput, newEvents, result, queryState, priority)
		return err
	})
	if err != nil {
		return "", err
	}
	return newRunID, nil
}

func (s *MSSQLStore) continueAsNewOnce(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput json.RawMessage, newEvents []EventRecord, result string, queryState map[string]string, priority int) (string, error) {
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
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority)
		VALUES (@p1, @p2, @p3, 'ready', CAST(@p4 AS VARCHAR(MAX)),
		        ISNULL((SELECT task_queue FROM workflow_defs WHERE name = @p2 AND version = @p3 AND tenant_id = @p5), 'default'),
		        @p5, @p6)
	`, newRunID, defName, defVersion, newInput, s.tenantID, priority)
	if err != nil {
		return "", fmt.Errorf("continue as new: start new run: %w", err)
	}

	// Complete the current run.
	qsJSON := marshalQueryState(queryState)
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'done', result = @p3, completed_at = SYSUTCDATETIME(), assigned_to = NULL, query_state = @p4
		WHERE id = @p1 AND assigned_to = @p2 AND generation = @p5
	`, currentRunID, workerID, result, string(qsJSON), generation)
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
func (s *MSSQLStore) FinalizeWorkflowSegment(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
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
func (s *MSSQLStore) StartNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int) (string, bool, error) {
	var newID string
	var existed bool
	err := withRollbackGuaranteedRetry(ctx, "start new run", mssqlTxRetries, mssqlTxRetryDelay, func() error {
		var err error
		newID, existed, err = s.startNewRunOnce(ctx, runID, defName, defVersion, input, idempotencyKey, tenantID, priority)
		return err
	})
	if err != nil {
		return "", false, err
	}
	return newID, existed, nil
}

func (s *MSSQLStore) startNewRunOnce(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int) (string, bool, error) {
	if runID == "" {
		runID = uuid.New().String()
	}
	if idempotencyKey != "" {
		keyHash := sha256.Sum256([]byte(idempotencyKey))

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
		err := s.db.QueryRowContext(ctx,
			`SELECT workflow_id FROM idempotency_keys
			 WHERE key_hash = @p1 AND tenant_id = @p2 AND expires_at > SYSUTCDATETIME()`,
			keyHash[:], tenantID).Scan(&existingWfID)
		if err == nil {
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

		// Insert idempotency key record. INSERT...WHERE NOT EXISTS handles the
		// race where two requests arrive with the same key simultaneously.
		ttlSeconds := int(s.idempotencyKeyTTL.Seconds())
		result, err := tx.ExecContext(ctx,
			`INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id)
			 SELECT @p1, @p2, DATEADD(SECOND, @p3, SYSUTCDATETIME()), @p4
			 WHERE NOT EXISTS (
			     SELECT 1 FROM idempotency_keys
			     WHERE key_hash = @p1 AND tenant_id = @p4
			 )`,
			keyHash[:], runID, ttlSeconds, tenantID)
		if err != nil {
			return "", false, err
		}

		n, _ := result.RowsAffected()
		if n == 0 {
			// Key was inserted concurrently — rollback and return the existing one.
			tx.Rollback()
			err := s.db.QueryRowContext(ctx,
				`SELECT workflow_id FROM idempotency_keys
				 WHERE key_hash = @p1 AND tenant_id = @p2 AND expires_at > SYSUTCDATETIME()`,
				keyHash[:], tenantID).Scan(&existingWfID)
			if err != nil {
				return "", false, err
			}
			return existingWfID, true, nil
		}

		if err := s.setSessionContext(tx); err != nil {
			return "", false, fmt.Errorf("start new run: set session: %w", err)
		}

		// Insert the workflow instance.
		_, err = tx.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority)
			VALUES (@p1, @p2, @p3, 'ready', CAST(@p4 AS NVARCHAR(MAX)),
			        ISNULL((SELECT task_queue FROM workflow_defs WHERE name = @p2 AND version = @p3 AND tenant_id = @p5), 'default'),
			        @p5, @p6)
		`, runID, defName, defVersion, string(input), tenantID, priority)
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
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority)
		VALUES (@p1, @p2, @p3, 'ready', CAST(@p4 AS NVARCHAR(MAX)),
		        ISNULL((SELECT task_queue FROM workflow_defs WHERE name = @p2 AND version = @p3 AND tenant_id = @p5), 'default'),
		        @p5, @p6)
	`, runID, defName, defVersion, string(input), tenantID, priority)
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
func (s *MSSQLStore) enforceParentClosePolicy(ctx context.Context, parentWorkflowID string) {
	steps := []struct {
		policy string
		query  string
	}{
		{"TERMINATE", `
		UPDATE workflow_instances
		SET status = 'failed', error_msg = 'parent workflow terminated'
		WHERE parent_workflow_id = @p1
		  AND parent_close_policy = 'TERMINATE'
		  AND status NOT IN ('done', 'failed')
	`},
		{"REQUEST_CANCEL", `
		UPDATE workflow_instances
		SET cancellation_requested = 1
		WHERE parent_workflow_id = @p1
		  AND parent_close_policy = 'REQUEST_CANCEL'
		  AND status NOT IN ('done', 'failed')
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
				if _, err := tx.ExecContext(ctx, step.query, parentWorkflowID); err != nil {
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
		  AND status NOT IN ('done', 'failed')
	`, parentWorkflowID)
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
