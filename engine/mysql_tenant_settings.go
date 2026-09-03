package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetTenantSettings reads this store's tenant's row from tenant_settings.
//
// The API is identical to the other two dialects -- 3.94 step 2's option C --
// but the isolation story is not, and the difference is a consequence rather
// than a gap. tiers.yaml D1 makes MySQL single-tenant, enforced since
// migrations/mysql/038_single_tenant_guard.sql by a unique key that refuses a
// second tenant outright. A table that can only hold one tenant's row has no
// cross-tenant read to prevent, so there is no MySQL counterpart to the
// PostgreSQL policy or the SQL Server security policy, and the `tenant_id = ?`
// predicate here is the only scoping there is.
func (s *MySQLStore) GetTenantSettings(ctx context.Context) (TenantSettings, error) {
	var instanceMs, wallClockMs, retryMs *int64
	err := s.db.QueryRowContext(ctx, `
		SELECT wasm_instance_timeout_ms, wasm_wall_clock_ceiling_ms, host_retry_budget_ms
		FROM tenant_settings
		WHERE tenant_id = ?
	`, s.tenantID).Scan(&instanceMs, &wallClockMs, &retryMs)
	if errors.Is(err, sql.ErrNoRows) {
		return TenantSettings{}, nil
	}
	if err != nil {
		return TenantSettings{}, fmt.Errorf("get tenant settings for %s: %w", s.tenantID, err)
	}
	return tenantSettingsFromMillis(instanceMs, wallClockMs, retryMs), nil
}
