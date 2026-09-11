package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrTenantSettingsConflict is returned when the row changed between the read
// and the write. The caller re-reads and decides; nothing is applied.
//
// This exists because the obvious write path is last-write-wins and loses
// changes in silence: two operators, one raising a limit and one lowering a
// different one, and whichever commits second erases the other's field without
// either of them seeing an error. Cadence's
// configStorePersistenceTest.go::TestUpdateVersionCollisionFailure exists for
// exactly this, and was unportable to cleat only because the operation did not
// exist yet. cleat#1187.
var ErrTenantSettingsConflict = errors.New("tenant settings changed since they were read")

// TenantSettingsRevision is what a caller must present to write. It is the row's
// updated_at, which the table has already, so this needs no schema change.
//
// The zero value means "there was no row" -- a tenant that has never set an
// override -- and is the correct precondition for the first write.
type TenantSettingsRevision struct {
	UpdatedAt time.Time
	Existed   bool
}

// ReadTenantSettingsForUpdate returns the settings AND the revision to present
// back. Split from GetTenantSettings because the read path is on the hot
// execution path and should not carry a field only writers need.
func (s *PostgresStore) ReadTenantSettingsForUpdate(ctx context.Context) (TenantSettings, TenantSettingsRevision, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return TenantSettings{}, TenantSettingsRevision{}, fmt.Errorf("read tenant settings: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var instanceMs, wallClockMs, retryMs *int64
	var updatedAt time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT wasm_instance_timeout_ms, wasm_wall_clock_ceiling_ms, host_retry_budget_ms, updated_at
		FROM tenant_settings
		WHERE tenant_id = $1
	`, s.tenantID).Scan(&instanceMs, &wallClockMs, &retryMs, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TenantSettings{}, TenantSettingsRevision{}, nil
	}
	if err != nil {
		return TenantSettings{}, TenantSettingsRevision{}, fmt.Errorf("read tenant settings for %s: %w", s.tenantID, err)
	}
	return tenantSettingsFromMillis(instanceMs, wallClockMs, retryMs),
		TenantSettingsRevision{UpdatedAt: updatedAt, Existed: true}, nil
}

// WriteTenantSettings replaces this tenant's row, refusing if it changed since
// `rev` was read.
//
// A nil field means "leave it to the operator's flag" and is written as NULL --
// the same meaning the column already documents. Callers that want to change
// one value read, modify, and write the whole struct back; there is no partial
// update, because a partial update over a row with no revision check is exactly
// the silent-loss shape this refuses.
func (s *PostgresStore) WriteTenantSettings(ctx context.Context, settings TenantSettings, rev TenantSettingsRevision) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("write tenant settings: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	inst, wall, retry := tenantSettingsToMillis(settings)

	var n int64
	if !rev.Existed {
		// First write. ON CONFLICT DO NOTHING rather than an upsert: if a row
		// appeared since the read, this caller's precondition ("there was no
		// row") is false and it must be told, not silently merged with whatever
		// the other writer decided.
		res, execErr := tx.ExecContext(ctx, `
			INSERT INTO tenant_settings
				(tenant_id, wasm_instance_timeout_ms, wasm_wall_clock_ceiling_ms, host_retry_budget_ms, updated_at)
			VALUES ($1, $2, $3, $4, now())
			ON CONFLICT (tenant_id) DO NOTHING
		`, s.tenantID, inst, wall, retry)
		if execErr != nil {
			return fmt.Errorf("write tenant settings: %w", execErr)
		}
		n, err = res.RowsAffected()
	} else {
		res, execErr := tx.ExecContext(ctx, `
			UPDATE tenant_settings
			SET wasm_instance_timeout_ms = $2, wasm_wall_clock_ceiling_ms = $3,
			    host_retry_budget_ms = $4, updated_at = now()
			WHERE tenant_id = $1 AND updated_at = $5
		`, s.tenantID, inst, wall, retry, rev.UpdatedAt)
		if execErr != nil {
			return fmt.Errorf("write tenant settings: %w", execErr)
		}
		n, err = res.RowsAffected()
	}
	if err != nil {
		return fmt.Errorf("write tenant settings: rows affected: %w", err)
	}
	if n == 0 {
		// Zero rows means the precondition failed, and saying so is the whole
		// point: the alternative is reporting success over a write that did
		// not happen, which is the failure this repository keeps meeting in
		// other shapes.
		return ErrTenantSettingsConflict
	}
	return tx.Commit()
}

// tenantSettingsToMillis is the inverse of tenantSettingsFromMillis. A zero
// duration becomes NULL, not 0: the column documents NULL as "use the
// operator's flag", and 0 in these columns would be read back by
// tenantSettingsFromMillis as zero and mean the same thing -- but writing NULL
// keeps the row honest about which values a tenant has actually set.
func tenantSettingsToMillis(s TenantSettings) (inst, wall, retry *int64) {
	ms := func(d time.Duration) *int64 {
		if d <= 0 {
			return nil
		}
		v := int64(d / time.Millisecond)
		return &v
	}
	return ms(s.WasmInstanceTimeout), ms(s.WasmWallClockCeiling), ms(s.HostRetryBudget)
}
