package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

func (s *PostgresStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", fmt.Errorf("start child workflow: begin: %w", err)
	}
	defer tx.Rollback()

	// cleat#2312: the child's own workflow_instances.input is sensitive-at-
	// rest, distinct from event.ChildInput in the parent's event_history
	// (already covered by encodeEventForStorage/cleat#2328).
	tc, err := s.tenantCipherForWrite()
	if err != nil {
		return "", fmt.Errorf("start child workflow: derive tenant key: %w", err)
	}
	sealedInput, err := encryptJSONColumnForStorage(inputJSON, tc)
	if err != nil {
		return "", fmt.Errorf("start child workflow: encrypt input: %w", err)
	}

	var runID, resolvedTaskQueue string
	var resolvedDefVersion int
	err = tx.QueryRowContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, parent_workflow_id, parent_close_policy, task_queue, tenant_id, priority)
		VALUES (gen_random_uuid(), $1,
		        CASE WHEN $3 > 0 THEN $3 ELSE (SELECT MAX(version) FROM workflow_defs WHERE name = $1 AND disabled_at IS NULL AND tenant_id = $5) END,
		        'ready', $2,
		        COALESCE(NULLIF($4, ''), 'ABANDON'),
		        COALESCE((SELECT task_queue FROM workflow_instances WHERE id = $2), 'default'),
			$5, $6)
		RETURNING id, task_queue, def_version
	`, defName, parentID, defVersion, parentClosePolicy, s.tenantID, priority).Scan(&runID, &resolvedTaskQueue, &resolvedDefVersion)
	if err != nil {
		return "", fmt.Errorf("start child workflow: %w", err)
	}

	// cleat#3245 Phase 3 step 2 gap fix (found scoping step 3's piece A):
	// StartChildWorkflow never got piece 1's workflow_leases/
	// workflow_payloads INSERT, unlike every top-level StartNewRun* variant
	// -- see insertLeaseAndPayloadRows' own doc. Every child workflow
	// started through this method has had no lease/payload row at all until
	// now, invisible because claimLeaseRows (piece 2) is a plain UPDATE, not
	// an upsert, so claiming one silently touched zero rows on the lease
	// side.
	//
	// resolvedDefVersion (migration 018 (folded into
	// migrations/postgres/001_schema.sql by #3358)) is captured via RETURNING rather
	// than re-evaluated, for the same reason resolvedTaskQueue already was
	// -- the INSERT above resolves an unspecified version through its own
	// MAX(version) subquery, and a second subquery here could read a
	// workflow_defs row that changed between the two statements. A child
	// workflow has no concurrency_key, run-limit overrides, or
	// continued_from of its own: this method's workflow_instances INSERT
	// has never carried those columns, so they are left unset here too.
	if err := s.insertLeaseAndPayloadRows(ctx, tx, runID, s.tenantID, resolvedTaskQueue, priority, sealedInput, leaseAndPayloadCols{
		DefName: defName, DefVersion: resolvedDefVersion, ParentWorkflowID: parentID,
	}); err != nil {
		return "", fmt.Errorf("start child workflow: %w", err)
	}

	pgNotify(ctx, tx, s.notifyChannel)
	return runID, tx.Commit()
}

// StartChildWorkflowAtomic creates a child workflow and records the parent's
// child_workflow event in a single transaction, guaranteeing exactly-once creation.

func (s *PostgresStore) StartChildWorkflowAtomic(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord, priority int) (string, error) {
	if childID == "" {
		childID = uuid.New().String()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.setRLSOnTx(tx); err != nil {
		return "", fmt.Errorf("start child workflow atomic: set rls: %w", err)
	}

	// Debug: check what MAX(version) resolves to.
	var resolvedVersion int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT MAX(version) FROM workflow_defs WHERE name = $1 AND disabled_at IS NULL AND tenant_id = $2), -1)`,
		defName, s.tenantID).Scan(&resolvedVersion); err != nil {
		resolvedVersion = -2
	}
	s.log().DebugContext(ctx, "StartChildWorkflowAtomic",
		"def_name", defName, "def_version", defVersion, "resolved_version", resolvedVersion, "tenant_id", s.tenantID, "parent_id", parentID)

	// cleat#2312: see the identical comment in StartChildWorkflow. tc is
	// re-derived below for the event's own encodeEventForStorage call too --
	// that is a second derivation (not shared with this one), which is fine
	// at the rate this function runs (once per child start, not once per
	// field): see tenantCipher's own doc comment for the measurement behind
	// "one derivation per event" rather than per field.
	tc, err := s.tenantCipherForWrite()
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: derive tenant key: %w", err)
	}
	sealedInput, err := encryptJSONColumnForStorage(inputJSON, tc)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: encrypt input: %w", err)
	}

	// 1. INSERT child workflow instance.
	var resolvedTaskQueue string
	var resolvedDefVersion int
	err = tx.QueryRowContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, parent_workflow_id, parent_close_policy, task_queue, tenant_id, priority)
		VALUES ($1, $2,
		        CASE WHEN $4 > 0 THEN $4 ELSE (SELECT MAX(version) FROM workflow_defs WHERE name = $2 AND disabled_at IS NULL AND tenant_id = $6) END,
		        'ready', $3,
		        COALESCE(NULLIF($5, ''), 'ABANDON'),
		        COALESCE((SELECT task_queue FROM workflow_instances WHERE id = $3), 'default'),
			$6, $7)
		RETURNING task_queue, def_version
	`, childID, defName, parentID, defVersion, parentClosePolicy, s.tenantID, priority).Scan(&resolvedTaskQueue, &resolvedDefVersion)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: insert child: %w", err)
	}

	// cleat#3245 Phase 3 step 2 gap fix (found scoping step 3's piece A):
	// same gap as StartChildWorkflow above, same fix -- see its comment,
	// including why resolvedDefVersion (migration 018) is captured via
	// RETURNING rather than re-evaluated.
	if err := s.insertLeaseAndPayloadRows(ctx, tx, childID, s.tenantID, resolvedTaskQueue, priority, sealedInput, leaseAndPayloadCols{
		DefName: defName, DefVersion: resolvedDefVersion, ParentWorkflowID: parentID,
	}); err != nil {
		return "", fmt.Errorf("start child workflow atomic: %w", err)
	}

	// 2. INSERT child_workflow event into the parent's event_history.
	event.RunID = childID
	// previousStoredChecksum, not a hand-rolled read: it runs on tx (so it sees
	// this transaction and carries its RLS/tenant context), qualifies by
	// tenant_id, and distinguishes "no predecessor" from a failed read. The
	// copy that used to be here ran on s.db -- the raw pool, no RLS context --
	// and discarded the error, so under a non-superuser role it silently
	// checksummed against an empty predecessor and broke the chain.
	prevCS, err := s.previousStoredChecksum(ctx, tx, parentID, event.Step)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: previous checksum: %w", err)
	}
	checksum := computeEventChecksum(event, prevCS)

	// The same encoding every other event_history writer on this dialect uses
	// -- see encodeEventForStorage's doc. Until cleat#2312/#2328 this INSERT
	// built its own plaintext payload by hand and had no payload_encoding
	// column at all, so a child_workflow event's child_input and payload were
	// never covered by --encrypt-sensitive-payloads even though both are
	// listed in EncryptedEventColumns: TestEncryptedEventColumnsIsComplete
	// only ever exercises encodeEventForStorage directly, so it had no
	// visibility into a write path that bypassed it entirely.
	//
	// The checksum above is computed BEFORE this call, over the plaintext
	// event -- encodeEventForStorage's doc explains why: VerifyWorkflowEvents
	// recomputes it from the decrypted record it loads, so a checksum over
	// ciphertext would make every workflow with a child fail verification.
	stored, err := encodeEventForStorage(event, s.encryption, s.encryptSensitivePayloads, tenantForAAD(s.tenantID))
	if err != nil {
		// Same accounting as appendOneEvent/execEventStmt/WriteCallIntent:
		// see the cleat#1317 note on those for why this is recorded here
		// rather than left silent.
		s.recordEncryptionFailure(ctx)
		return "", fmt.Errorf("start child workflow atomic: encode event: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, child_name, child_input, run_id, created_at, checksum, tenant_id, payload, payload_encoding)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (tenant_id, workflow_id, step) DO NOTHING
	`, parentID, event.Step, string(event.EventType),
		nullStr(event.ChildName), nullStr(stored.ChildInput), nullStr(childID),
		time.UnixMilli(event.TimestampMs), checksum, s.tenantID, stored.Payload, stored.Encoding)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: insert event: %w", err)
	}

	pgNotify(ctx, tx, s.notifyChannel)
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("start child workflow atomic: commit: %w", err)
	}
	return childID, nil
}

// GetChildResult checks whether a child workflow has settled -- 'done', or
// any of 'failed'/'dead_lettered'/'terminated'/'cancelled' (see
// childOutcomeForSettledStatus).

func (s *PostgresStore) GetChildResult(ctx context.Context, runID string) (ChildOutcome, error) {
	// Resolve the chain first: the run the parent STARTED is not necessarily
	// the run that holds the answer. A child that continues as new leaves its
	// first run at status 'done' with an empty result -- 'done' because it was
	// superseded, not because it finished -- and reading that row returned {}
	// while the real result sat on the last run in the chain (cleat#955).
	//
	// Nothing distinguishes "done because it continued" from "done because it
	// finished" on the row itself. The successor lookup does: a run with no
	// successor is the terminal one. Only WHICH row is read changes here;
	// everything below -- the status test, the result compaction -- is
	// untouched.
	runID, err := terminalRunID(ctx, runID, s.successorOfRun)
	if err != nil {
		return ChildOutcome{}, err
	}
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return ChildOutcome{}, fmt.Errorf("get child result: begin: %w", err)
	}
	defer tx.Rollback()

	var result string
	var status string
	var errMsg sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(p.result, '{}'), w.status, p.error_msg
		FROM workflow_instances w
		JOIN workflow_payloads p ON p.id = w.id
		WHERE w.id = $1
	`, runID).Scan(&result, &status, &errMsg)
	if errors.Is(err, sql.ErrNoRows) {
		return ChildOutcome{}, tx.Commit()
	}
	if err != nil {
		return ChildOutcome{}, fmt.Errorf("get child result: %w", err)
	}

	// cleat#2312: decrypt before anything below inspects these values -- this
	// feeds AwaitChild's host call, not only display, so a plaintext parent
	// workflow must see the real result/error, not ciphertext or the COALESCE
	// default.
	if decrypted, derr := s.decryptPayloadJSON(result); derr == nil {
		result = decrypted
	} else {
		return ChildOutcome{}, fmt.Errorf("get child result: %w", derr)
	}
	if errMsg.Valid {
		decrypted, derr := s.decryptTextColumnFromStorage(errMsg.String, "error_msg")
		if derr != nil {
			return ChildOutcome{}, fmt.Errorf("get child result: %w", derr)
		}
		errMsg.String = decrypted
	}
	// Terminal in this function means settled -- see
	// childOutcomeForSettledStatus. Two definitions of terminal used to live
	// in this file alone: this function excluded dead_lettered until
	// cleat#1213, and then excluded terminated/cancelled until cleat#1974,
	// while GetChildCount forty lines down has excluded all five since
	// cleat#1153. Only this one decides whether a parent stops waiting -- so
	// a parent awaiting a child that exhausted its retries, or was
	// terminated, or was cancelled, suspended, was re-claimed on its
	// next_wake_at, replayed, got the same non-answer and suspended again,
	// for the life of the deployment. Nothing pushes a parent awake early;
	// the only wake is the timeout, so "the parent waits to be re-checked"
	// and "the parent replays forever" are the same behaviour here.
	//
	// A settled-but-not-done run's `result` column is never written --
	// migration 053 routes finalize's payload to `error_msg` on that branch
	// -- so returning the result for one would return the '{}' from the
	// COALESCE above, which is exactly the empty success cleat#1115 is
	// about. That is why compaction below only applies to the 'done' case:
	// childOutcomeForSettledStatus never reads `result` for any other
	// status.
	if status == statusDone {
		// Compact, matching the convention GetWorkflowByID and
		// GetPromise/ListPromises already follow for JSONB result/payload
		// columns: PostgreSQL's jsonb text output always inserts a space
		// after every ':' and ',', so a result written as `{"child":"done"}`
		// otherwise comes back as `{"child": "done"}`.
		compacted := bytes.NewBuffer(nil)
		if err := json.Compact(compacted, []byte(result)); err == nil {
			result = compacted.String()
		}
	}
	outcome, _ := childOutcomeForSettledStatus(status, result, errMsg)
	return outcome, tx.Commit()
}

// GetChildCount returns the number of ACTIVE child workflows for the given
// parent -- the number engine/children.go compares against maxQuotaChildren
// before allowing another child to be created.
//
// All four settled statuses are excluded, and 'terminated' was missing from
// that list until cleat#1153 groundwork: a terminated child went on holding its
// parent's quota permanently, so a parent that spawned and disposed of children
// in a loop eventually could not spawn at all. All three dialects had the same
// omission, independently hand-written.
//
// 'terminating' is deliberately NOT excluded. A child mid-shutdown is running
// its defer phase and can still do work, so it should still occupy a quota
// slot; it is released by the terminal write that follows. That is the "settled"
// question rather than the "can no longer run guest code" question -- see
// engine/status_vocabulary.go, which keeps the two apart, and
// engine/one_definition_of_settled_test.go, which fails if this list drifts
// from the canonical set again.
func (s *PostgresStore) GetChildCount(ctx context.Context, parentWorkflowID string) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("get child count for %s: begin: %w", parentWorkflowID, err)
	}
	defer tx.Rollback()

	// cleat#3245 Phase 3 step 3 piece E: reads workflow_leases rather than
	// workflow_instances. parent_workflow_id (migration 018) and status
	// (migration 016 (folded into migrations/postgres/001_schema.sql by
	// #3358)) are both mirrored there -- StartChildWorkflow/
	// StartChildWorkflowAtomic set ParentWorkflowID on their
	// insertLeaseAndPayloadRows call, and every terminal-status write
	// mirrors status onto workflow_leases (pieces 4a/4c/4d/6b/6d, B, C, D).
	var count int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_leases
		WHERE parent_workflow_id = $1 AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
	`, parentWorkflowID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get child count for %s: %w", parentWorkflowID, err)
	}
	return count, tx.Commit()
}

// OriginalChildRunIDs implements WorkflowStore. See the interface for why
// continued runs are excluded.
func (s *PostgresStore) OriginalChildRunIDs(ctx context.Context, parentWorkflowID string) ([]string, error) {
	// A TENANT-SCOPED TRANSACTION, not the bare pool, and not an
	// `AND tenant_id = $2` predicate either. workflow_leases carries the
	// same tenant_isolation_leases row-level security policy workflow_instances
	// did (migration 016): it raises on the first candidate row whatever the
	// WHERE clause says, so a tenant predicate does not substitute for
	// setting the tenant on the transaction. Written the bare way first and
	// caught by TestNoPostgresStatementReachesAnRLSTableWithoutTheTenantSet,
	// which says exactly that.
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("original child run ids for %s: begin: %w", parentWorkflowID, err)
	}
	defer tx.Rollback()

	// cleat#3245 Phase 3 step 3 piece E: reads workflow_leases rather than
	// workflow_instances. parent_workflow_id and continued_from (both
	// migration 018) are mirrored there -- see GetChildCount's comment
	// above for the write-side paths.
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM workflow_leases
		WHERE parent_workflow_id = $1 AND continued_from IS NULL
	`, parentWorkflowID)
	if err != nil {
		return nil, fmt.Errorf("original child run ids for %s: %w", parentWorkflowID, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("original child run ids for %s: scan: %w", parentWorkflowID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("original child run ids for %s: %w", parentWorkflowID, err)
	}
	return ids, tx.Commit()
}

// ReapStaleInstances reclaims workflow instances with stale heartbeats.

// GetChildCompletedAtMs returns the child's completion instant in Unix
// milliseconds. See the ChildWorkflowStore doc comment for why PollChild needs
// the instant rather than a boolean, and engine/children.go's
// pollChildIsDeterministic for the clock-domain caveat.
//
// completed_at is written by now() inside finalize_workflow_status, so this is
// the DATABASE clock.
func (s *PostgresStore) GetChildCompletedAtMs(ctx context.Context, runID string) (int64, bool, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("get child completed_at: begin: %w", err)
	}
	defer tx.Rollback()

	var completedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT completed_at FROM workflow_payloads WHERE id = $1
	`, runID).Scan(&completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, tx.Commit()
	}
	if err != nil {
		return 0, false, fmt.Errorf("get child completed_at: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("get child completed_at: commit: %w", err)
	}
	if !completedAt.Valid {
		return 0, false, nil
	}
	return completedAt.Time.UnixMilli(), true, nil
}
