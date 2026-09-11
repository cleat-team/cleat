package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RunLimitsReader reads the per-RUN tier of the three bounds.
//
// cleat#1187. tenant_settings gives a tenant the ability to tighten below the
// operator's flags; these columns give one run the ability to tighten below its
// tenant. Resolution is ClampToCeiling applied twice -- see Engine.runLimits.
//
// # Why this reuses TenantSettings rather than defining its own type
//
// The three fields are the same three, with the same meaning and the same
// "zero means unset" convention that ClampToCeiling is built around. A separate
// struct would be three identical fields plus a conversion, and the conversion
// is where a future fourth field gets added to one and not the other. The type
// name is about the SHAPE, and the tier is carried by the variable.
//
// # Why an optional interface
//
// Same reason GetConcurrencyKeyHolder and CountRunnableWorkflows are: adding a
// method to WorkflowStore edits every test double in the tree for a value none
// of them has an opinion about.
type RunLimitsReader interface {
	GetRunLimits(ctx context.Context, workflowID string) (TenantSettings, error)
}

// GetRunLimits reads one run's own limit overrides.
//
// A run with no row, or no overrides, returns the zero value -- which resolves
// to the tenant's settings, and through them to the operator's flags. That is
// the same "absent is not an error" shape GetTenantSettings has, and for the
// same reason: a missing override is the normal case, not a fault.
func (s *PostgresStore) GetRunLimits(ctx context.Context, workflowID string) (TenantSettings, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return TenantSettings{}, fmt.Errorf("run limits: begin: %w", err)
	}
	defer tx.Rollback()

	var instanceMs, wallClockMs, retryMs *int64
	err = tx.QueryRowContext(ctx, `
		SELECT run_wasm_instance_timeout_ms, run_wasm_wall_clock_ceiling_ms, run_host_retry_budget_ms
		FROM workflow_instances WHERE id = $1 AND tenant_id = $2
	`, workflowID, s.tenantID).Scan(&instanceMs, &wallClockMs, &retryMs)
	if errors.Is(err, sql.ErrNoRows) {
		return TenantSettings{}, tx.Commit()
	}
	if err != nil {
		return TenantSettings{}, fmt.Errorf("run limits: %w", err)
	}
	return tenantSettingsFromMillis(instanceMs, wallClockMs, retryMs), tx.Commit()
}

// GetRunLimits reads one run's own limit overrides. See the PostgreSQL
// implementation for the contract.
func (s *MySQLStore) GetRunLimits(ctx context.Context, workflowID string) (TenantSettings, error) {
	var instanceMs, wallClockMs, retryMs *int64
	err := s.db.QueryRowContext(ctx, `
		SELECT run_wasm_instance_timeout_ms, run_wasm_wall_clock_ceiling_ms, run_host_retry_budget_ms
		FROM workflow_instances WHERE id = ? AND tenant_id = ?
	`, workflowID, s.tenantID).Scan(&instanceMs, &wallClockMs, &retryMs)
	if errors.Is(err, sql.ErrNoRows) {
		return TenantSettings{}, nil
	}
	if err != nil {
		return TenantSettings{}, fmt.Errorf("run limits: %w", err)
	}
	return tenantSettingsFromMillis(instanceMs, wallClockMs, retryMs), nil
}

// GetRunLimits reads one run's own limit overrides. See the PostgreSQL
// implementation for the contract.
func (s *MSSQLStore) GetRunLimits(ctx context.Context, workflowID string) (TenantSettings, error) {
	var instanceMs, wallClockMs, retryMs *int64
	err := s.db.QueryRowContext(ctx, `
		SELECT run_wasm_instance_timeout_ms, run_wasm_wall_clock_ceiling_ms, run_host_retry_budget_ms
		FROM workflow_instances WHERE id = @p1 AND tenant_id = @p2
	`, workflowID, s.tenantID).Scan(&instanceMs, &wallClockMs, &retryMs)
	if errors.Is(err, sql.ErrNoRows) {
		return TenantSettings{}, nil
	}
	if err != nil {
		return TenantSettings{}, fmt.Errorf("run limits: %w", err)
	}
	return tenantSettingsFromMillis(instanceMs, wallClockMs, retryMs), nil
}
