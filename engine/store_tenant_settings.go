package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetTenantSettings reads this store's tenant's row from tenant_settings.
//
// The read runs inside beginTxWithRLS, so the row-level security policy
// 039_tenant_settings.sql installs is in force. That matters more here than the
// `tenant_id = $1` predicate below does: the predicate is the layer a bug can
// remove, and the policy is the layer that still holds when it does. Both are
// present deliberately -- CLAUDE.md's standing warning is about assertions that
// pass because of a layer other than the one under test, and
// engine/tenant_settings_rls_test.go proves each separately by breaking it.
//
// No row is not an error. A tenant that has never set an override has no row,
// which is the common case, and it resolves to the operator's flags.
func (s *PostgresStore) GetTenantSettings(ctx context.Context) (TenantSettings, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return TenantSettings{}, fmt.Errorf("get tenant settings: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var instanceMs, wallClockMs, retryMs *int64
	err = tx.QueryRowContext(ctx, `
		SELECT wasm_instance_timeout_ms, wasm_wall_clock_ceiling_ms, host_retry_budget_ms
		FROM tenant_settings
		WHERE tenant_id = $1
	`, s.tenantID).Scan(&instanceMs, &wallClockMs, &retryMs)
	if errors.Is(err, sql.ErrNoRows) {
		return TenantSettings{}, nil
	}
	if err != nil {
		return TenantSettings{}, fmt.Errorf("get tenant settings for %s: %w", s.tenantID, err)
	}
	return tenantSettingsFromMillis(instanceMs, wallClockMs, retryMs), nil
}
