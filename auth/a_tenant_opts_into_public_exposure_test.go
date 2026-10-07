package auth

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// readAllowPublicExposure reads the column directly, bypassing TenantStore,
// so the test is not checking SetTenantAllowsPublicExposure against itself.
func readAllowPublicExposure(t *testing.T, db *sql.DB, dialect string, tenantID uuid.UUID) bool {
	t.Helper()
	var stmt string
	switch dialect {
	case DialectMySQL:
		stmt = `SELECT allow_public_exposure FROM tenants WHERE tenant_id = ?`
	case DialectMSSQL:
		stmt = `SELECT allow_public_exposure FROM admin.tenants WHERE tenant_id = @p1`
	default:
		stmt = `SELECT allow_public_exposure FROM admin.tenants WHERE tenant_id = $1`
	}
	var allowed bool
	if err := db.QueryRow(stmt, tenantID.String()).Scan(&allowed); err != nil {
		t.Fatalf("readAllowPublicExposure (%s): %v", dialect, err)
	}
	return allowed
}

// SetTenantAllowsPublicExposure grants and revokes on all three dialects, and
// a tenant with no row is neither -- it is ErrTenantNotFound, same contract
// as SetTenantSuspended (cleat#1986).
func TestSetTenantAllowsPublicExposure(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMySQL, DialectMSSQL} {
		t.Run(dialect, func(t *testing.T) {
			db := testutil.TestDB(t, testutil.Dialect(dialect))
			ts, err := NewTenantStoreForDialect(db, dialect)
			if err != nil {
				t.Fatalf("NewTenantStoreForDialect(%q): %v", dialect, err)
			}

			tenantID := seedTenant(t, db, dialect, "pub-exposure-"+dialect)

			// Every tenant seedTenant produces is freshly inserted (or, on
			// MySQL, the one pre-existing default) with no explicit value for
			// this column -- the migration's NOT NULL DEFAULT false applies,
			// so the starting state is the safe one without this test setting
			// it first.
			if got := readAllowPublicExposure(t, db, dialect, tenantID); got {
				t.Fatalf("tenant %s already opted into public exposure before this test touched it "+
					"(fine on MySQL's shared default tenant if a prior run left it set; "+
					"not fine as the default for a freshly seeded tenant on %s)", tenantID, dialect)
			}

			if err := ts.SetTenantAllowsPublicExposure(context.Background(), tenantID, true); err != nil {
				t.Fatalf("SetTenantAllowsPublicExposure(true): %v", err)
			}
			if got := readAllowPublicExposure(t, db, dialect, tenantID); !got {
				t.Errorf("after granting, column reads false")
			}

			if err := ts.SetTenantAllowsPublicExposure(context.Background(), tenantID, false); err != nil {
				t.Fatalf("SetTenantAllowsPublicExposure(false): %v", err)
			}
			if got := readAllowPublicExposure(t, db, dialect, tenantID); got {
				t.Errorf("after revoking, column still reads true")
			}
		})
	}
}

func TestSetTenantAllowsPublicExposure_NoSuchTenant(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMySQL, DialectMSSQL} {
		t.Run(dialect, func(t *testing.T) {
			db := testutil.TestDB(t, testutil.Dialect(dialect))
			ts, err := NewTenantStoreForDialect(db, dialect)
			if err != nil {
				t.Fatalf("NewTenantStoreForDialect(%q): %v", dialect, err)
			}

			err = ts.SetTenantAllowsPublicExposure(context.Background(), uuid.New(), true)
			if !errors.Is(err, ErrTenantNotFound) {
				t.Errorf("SetTenantAllowsPublicExposure on a nonexistent tenant: err = %v, want ErrTenantNotFound", err)
			}
		})
	}
}
