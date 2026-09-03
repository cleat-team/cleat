package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetTenantSettings reads this store's tenant's row from dbo.tenant_settings.
//
// No explicit transaction, unlike the PostgreSQL path, and that is not a
// weaker guarantee: MSSQLStore's pool uses a wrapped connector that runs
// sp_set_session_context on every connection it hands out, so the session
// context dbo.fn_tenant_filter reads is already set on a plain query. The
// FILTER PREDICATE that 042_tenant_settings.sql installs therefore applies
// here exactly as it would inside beginTxWithContext.
//
// A filter predicate hides rows rather than raising, so a policy that stopped
// working would look like a tenant with no overrides -- the flag defaults,
// silently. engine/mssql_tenant_settings_rls_test.go is what stops that from
// being invisible: it reads another tenant's row with no tenant_id in the
// query text at all, so the policy is the only thing that can hide it, and it
// checks the policy is enabled before believing the result.
func (s *MSSQLStore) GetTenantSettings(ctx context.Context) (TenantSettings, error) {
	var instanceMs, wallClockMs, retryMs *int64
	err := s.db.QueryRowContext(ctx, `
		SELECT wasm_instance_timeout_ms, wasm_wall_clock_ceiling_ms, host_retry_budget_ms
		FROM dbo.tenant_settings
		WHERE tenant_id = @p1
	`, s.tenantID).Scan(&instanceMs, &wallClockMs, &retryMs)
	if errors.Is(err, sql.ErrNoRows) {
		return TenantSettings{}, nil
	}
	if err != nil {
		return TenantSettings{}, fmt.Errorf("get tenant settings for %s: %w", s.tenantID, err)
	}
	return tenantSettingsFromMillis(instanceMs, wallClockMs, retryMs), nil
}
