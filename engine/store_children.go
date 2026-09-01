package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

func (s *PostgresStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", fmt.Errorf("start child workflow: begin: %w", err)
	}
	defer tx.Rollback()

	var runID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, task_queue, tenant_id, priority)
		VALUES (gen_random_uuid(), $1,
		        CASE WHEN $4 > 0 THEN $4 ELSE (SELECT MAX(version) FROM workflow_defs WHERE name = $1 AND NOT deprecated) END,
		        'ready', $2, $3,
		        COALESCE(NULLIF($5, ''), 'ABANDON'),
		        COALESCE((SELECT task_queue FROM workflow_instances WHERE id = $3), 'default'),
			$6, $7)
		RETURNING id
	`, defName, inputJSON, parentID, defVersion, parentClosePolicy, s.tenantID, priority).Scan(&runID)
	if err != nil {
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
		`SELECT COALESCE((SELECT MAX(version) FROM workflow_defs WHERE name = $1 AND NOT deprecated), -1)`,
		defName).Scan(&resolvedVersion); err != nil {
		resolvedVersion = -2
	}
	s.log().DebugContext(ctx, "StartChildWorkflowAtomic",
		"def_name", defName, "def_version", defVersion, "resolved_version", resolvedVersion, "tenant_id", s.tenantID, "parent_id", parentID)

	// 1. INSERT child workflow instance.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, task_queue, tenant_id, priority)
		VALUES ($1, $2,
		        CASE WHEN $5 > 0 THEN $5 ELSE (SELECT MAX(version) FROM workflow_defs WHERE name = $2 AND NOT deprecated) END,
		        'ready', $3, $4,
		        COALESCE(NULLIF($6, ''), 'ABANDON'),
		        COALESCE((SELECT task_queue FROM workflow_instances WHERE id = $4), 'default'),
			$7, $8)
	`, childID, defName, inputJSON, parentID, defVersion, parentClosePolicy, s.tenantID, priority)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: insert child: %w", err)
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
	_, err = tx.ExecContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, child_name, child_input, run_id, created_at, checksum, tenant_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (workflow_id, step) DO NOTHING
	`, parentID, event.Step, string(event.EventType),
		nullStr(event.ChildName), nullStr(event.ChildInput), nullStr(childID),
		time.UnixMilli(event.TimestampMs), checksum, s.tenantID)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: insert event: %w", err)
	}

	pgNotify(ctx, tx, s.notifyChannel)
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("start child workflow atomic: commit: %w", err)
	}
	return childID, nil
}

// GetChildResult checks whether a child workflow has completed (status 'done' or 'failed').

func (s *PostgresStore) GetChildResult(ctx context.Context, runID string) (string, bool, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", false, fmt.Errorf("get child result: begin: %w", err)
	}
	defer tx.Rollback()

	var result string
	var status string
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(result, '{}'), status FROM workflow_instances WHERE id = $1
	`, runID).Scan(&result, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, tx.Commit()
	}
	if err != nil {
		return "", false, fmt.Errorf("get child result: %w", err)
	}
	if status == "done" || status == "failed" {
		// Compact, matching the convention GetWorkflowByID and
		// GetPromise/ListPromises already follow for JSONB result/payload
		// columns: PostgreSQL's jsonb text output always inserts a space
		// after every ':' and ',', so a result written as `{"child":"done"}`
		// otherwise comes back as `{"child": "done"}`.
		compacted := bytes.NewBuffer(nil)
		if err := json.Compact(compacted, []byte(result)); err == nil {
			result = compacted.String()
		}
		return result, true, tx.Commit()
	}
	return "", false, tx.Commit()
}

// GetChildCount returns the number of active (non-terminal) child workflows
// for the given parent workflow. Terminal statuses are excluded.

func (s *PostgresStore) GetChildCount(ctx context.Context, parentWorkflowID string) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("get child count for %s: begin: %w", parentWorkflowID, err)
	}
	defer tx.Rollback()

	var count int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_instances
		WHERE parent_workflow_id = $1 AND status NOT IN ('done', 'failed', 'dead_lettered')
	`, parentWorkflowID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get child count for %s: %w", parentWorkflowID, err)
	}
	return count, tx.Commit()
}

// tenantSchemaPrefix is the prefix admin.create_tenant_role gives each tenant's
// schema: 'tenant_' || replace(tenant_id::text, '-', '_').
const tenantSchemaPrefix = "tenant_"

// tenantIDForSchema recovers the tenant a schema belongs to, for schemas named
// by admin.create_tenant_role (migrations/postgres/001_schema.sql), which
// creates `tenant_<uuid with - replaced by _>` for each tenant.
//
// A cross-schema child belongs to the *target* schema's tenant, not the
// parent's: the whole point of the feature is that schema B is a separate
// microservice, and the child runs as part of B. So the attribution has to
// come from the target schema, and the naming convention is the only mapping
// the engine has -- peer schemas are configured by name alone
// (--peer-schemas), with no tenant attached.
//
// Returns ok=false for any schema not following the convention, which is not
// an error: see StartChildWorkflowInSchema for what happens then.
func tenantIDForSchema(schema string) (string, bool) {
	if !strings.HasPrefix(schema, tenantSchemaPrefix) {
		return "", false
	}
	candidate := strings.ReplaceAll(strings.TrimPrefix(schema, tenantSchemaPrefix), "_", "-")
	parsed, err := uuid.Parse(candidate)
	if err != nil {
		return "", false
	}
	return parsed.String(), true
}

// StartChildWorkflowInSchema creates a child workflow in the given target schema.
// Implements CrossSchemaChildStore for cross-instance workflow cooperation.
//
// Tenant attribution: the child belongs to the target schema's tenant, because
// the target schema is a different microservice and the child runs as part of
// it. Where that tenant is recoverable from the schema name (the convention
// admin.create_tenant_role establishes), this sets both the RLS context and the
// tenant_id column to it, so the row is attributed to the destination.
//
// Where it is not recoverable -- an operator-chosen peer schema name like
// "svc_billing" -- the engine genuinely does not know which tenant owns the
// destination, so it writes neither, and the destination table's own DEFAULT
// applies. Writing the *parent's* tenant would be worse than writing nothing:
// it would silently file one service's workflow under another service's tenant.
// If the destination enforces RLS, that insert will be refused, which is the
// correct outcome for "we cannot say who this belongs to" -- see
// IMPROVEMENT-PLAN §2.23.
func (s *PostgresStore) StartChildWorkflowInSchema(ctx context.Context, targetSchema, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
	targetTenant, haveTenant := tenantIDForSchema(targetSchema)

	qs := pq.QuoteIdentifier(targetSchema)
	tenantCol, tenantVal := "", ""
	if haveTenant {
		tenantCol, tenantVal = ", tenant_id", ", $7"
	}
	q := fmt.Sprintf(`
		INSERT INTO %s.workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, task_queue, priority%s)
		VALUES (gen_random_uuid(), $1,
		        CASE WHEN $4 > 0 THEN $4 ELSE (SELECT MAX(version) FROM %s.workflow_defs WHERE name = $1 AND NOT deprecated) END,
		        'ready', $2, $3,
		        COALESCE(NULLIF($5, ''), 'ABANDON'),
		        COALESCE((SELECT task_queue FROM %s.workflow_instances WHERE id = $3), 'default'), $6%s)
		RETURNING id
	`, qs, tenantCol, qs, qs, tenantVal)

	args := []any{defName, inputJSON, parentID, defVersion, parentClosePolicy, priority}
	if haveTenant {
		args = append(args, targetTenant)
	}

	if !haveTenant {
		// No tenant to establish, so no transaction is needed either -- keep the
		// single-round-trip path this function has always had.
		var runID string
		if err := s.db.QueryRowContext(ctx, q, args...).Scan(&runID); err != nil {
			return "", fmt.Errorf("start child workflow in schema %q: %w", targetSchema, err)
		}
		return runID, nil
	}

	// set_config(..., true) is transaction-local, so the INSERT has to share a
	// transaction with it. Without one, each statement runs in its own implicit
	// transaction and the setting is discarded before the INSERT sees it.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("start child workflow in schema %q: begin: %w", targetSchema, err)
	}
	defer tx.Rollback()

	// The *target* tenant, deliberately, not s.tenantID. This is the one place
	// the engine writes a row on behalf of another tenant, and it is gated by
	// the --peer-schemas allowlist plus whatever grants the destination schema
	// has given this role.
	if _, err := tx.ExecContext(ctx, "SELECT set_config('cleat.tenant_id', $1, true)", targetTenant); err != nil {
		return "", fmt.Errorf("start child workflow in schema %q: set tenant context: %w", targetSchema, err)
	}

	var runID string
	if err := tx.QueryRowContext(ctx, q, args...).Scan(&runID); err != nil {
		return "", fmt.Errorf("start child workflow in schema %q: %w", targetSchema, err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("start child workflow in schema %q: commit: %w", targetSchema, err)
	}
	return runID, nil
}

// GetChildResultInSchema polls a child workflow in the given target schema.

func (s *PostgresStore) GetChildResultInSchema(ctx context.Context, targetSchema, runID string) (string, bool, error) {
	var result string
	var status string
	q := fmt.Sprintf(`SELECT COALESCE(result, '{}'), status FROM %s.workflow_instances WHERE id = $1`,
		pq.QuoteIdentifier(targetSchema))
	err := s.db.QueryRowContext(ctx, q, runID).Scan(&result, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get child result in schema %q: %w", targetSchema, err)
	}
	if status == "done" || status == "failed" {
		return result, true, nil
	}
	return "", false, nil
}

// ReapStaleInstances reclaims workflow instances with stale heartbeats.
