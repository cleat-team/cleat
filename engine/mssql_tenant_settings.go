package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetTenantSettings reads this store's tenant's row from dbo.tenant_settings.
//
// This used to run as a plain query on s.db, reasoning that MSSQLStore's pool
// connector sets SESSION_CONTEXT on every connection it hands out, so the
// session context dbo.fn_tenant_filter reads is already set. That reasoning
// holds for a store obtained the ordinary way, but not for one re-scoped via
// WithTenant after the fact: WithTenant only mutates s.tenantID and shares the
// original pool, whose connections carry the ORIGINAL tenant's
// SESSION_CONTEXT, not s.tenantID's. cleat#2210. beginTxWithContext sets it
// explicitly on every transaction it opens, so this now agrees with s.tenantID
// regardless of which tenant's pool the connection came from.
//
// A filter predicate hides rows rather than raising, so a policy that stopped
// working would look like a tenant with no overrides -- the flag defaults,
// silently. engine/mssql_tenant_settings_rls_test.go is what stops that from
// being invisible: it reads another tenant's row with no tenant_id in the
// query text at all, so the policy is the only thing that can hide it, and it
// checks the policy is enabled before believing the result.
func (s *MSSQLStore) GetTenantSettings(ctx context.Context) (TenantSettings, error) {
	tx, err := s.beginTxWithContext(ctx)
	if err != nil {
		return TenantSettings{}, fmt.Errorf("get tenant settings for %s: begin: %w", s.tenantID, err)
	}
	defer tx.Rollback()

	var instanceMs, wallClockMs, retryMs, maxWorkflowMs *int64
	err = tx.QueryRowContext(ctx, `
		SELECT wasm_instance_timeout_ms, wasm_wall_clock_ceiling_ms, host_retry_budget_ms, max_workflow_duration_ms
		FROM dbo.tenant_settings
		WHERE tenant_id = @p1
	`, s.tenantID).Scan(&instanceMs, &wallClockMs, &retryMs, &maxWorkflowMs)
	if errors.Is(err, sql.ErrNoRows) {
		return TenantSettings{}, nil
	}
	if err != nil {
		return TenantSettings{}, fmt.Errorf("get tenant settings for %s: %w", s.tenantID, err)
	}
	return tenantSettingsFromMillis(instanceMs, wallClockMs, retryMs, maxWorkflowMs), nil
}
