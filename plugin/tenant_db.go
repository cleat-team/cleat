package plugin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// TenantPools manages per-tenant database connection pools.
// Each pool connects directly as the tenant's login role, so there is
// no SET ROLE / RESET ROLE to escape — the connection IS the tenant.
type TenantPools struct {
	// Owner pool for administrative operations (claiming, migrations).
	OwnerDB *sql.DB

	mu       sync.Mutex
	pools    map[string]*sql.DB
	connStr  string // base connection string (without user/password — we add per-tenant)
	maxConns int    // max open connections per tenant pool

	// secret derives each tenant's password. Nothing per-tenant is stored:
	// see TenantRolePassword and cleat#1307. Empty means no tenant pool can be
	// opened, which For() reports rather than working around.
	secret []byte
}

// NewTenantPools creates a TenantPools manager.
// maxConns is the max open connections per tenant pool (0 = default 25).
// baseDSN is a connection string template like:
// "host=localhost port=5432 dbname=cleat sslmode=disable"
// The user and password are added per tenant.
// secret is the worker's tenant-role key; each tenant's password is
// HMAC-SHA256(secret, tenant_id). It must be at least
// TenantRoleSecretMinBytes; a shorter or absent one makes For() fail rather
// than silently hand back the owner pool.
func NewTenantPools(ownerDB *sql.DB, baseDSN string, maxConns int, secret []byte) *TenantPools {
	if maxConns <= 0 {
		maxConns = 25
	}
	return &TenantPools{
		OwnerDB:  ownerDB,
		pools:    make(map[string]*sql.DB),
		maxConns: maxConns,
		connStr:  baseDSN,
		secret:   secret,
	}
}

// For returns a tenant-scoped *sql.DB. Pools are created lazily and cached.
// Caller does NOT close the returned DB — TenantPools manages the lifecycle.
func (tp *TenantPools) For(ctx context.Context, tenantID string) (*sql.DB, error) {
	// In single-tenant mode, all workflows run under the default tenant
	// (zero UUID) and share the owner connection pool. No per-tenant
	// roles exist on managed PostgreSQL services.
	if tenantID == "" {
		return tp.OwnerDB, nil
	}

	tp.mu.Lock()
	pool, ok := tp.pools[tenantID]
	tp.mu.Unlock()
	if ok {
		return pool, nil
	}

	// FAIL CLOSED, and this is the change that matters most in cleat#1307.
	//
	// This used to fall back to tp.OwnerDB on sql.ErrNoRows with a log line:
	// "no role for tenant %s — falling back to owner pool (single-tenant
	// mode)". That was harmless while TenantPools could not be constructed at
	// all. The moment it IS the isolation mechanism, it is a privilege
	// escalation: a tenant whose role was never provisioned silently gets the
	// OWNER connection, which sees every tenant's rows. The failure mode of a
	// missing row must be "no isolation available", not "isolation waived".
	//
	// Single-tenant deployments are unaffected because they never reach here:
	// tenantID == "" returns the owner pool above, which is the documented
	// single-tenant path.
	var roleName string
	err := tp.OwnerDB.QueryRowContext(ctx,
		`SELECT role_name FROM admin.tenant_roles WHERE tenant_id = $1`,
		tenantID).Scan(&roleName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf(
				"tenant pool: tenant %s has no provisioned role. Refusing to fall back to "+
					"the owner connection, which would see every tenant's rows. Provision it "+
					"with admin.create_tenant_role(tenant_id, password)", tenantID)
		}
		return nil, fmt.Errorf("tenant pool: look up role for tenant %s: %w", tenantID, err)
	}

	// The password is DERIVED, never read: admin.tenant_roles has no password
	// column since cleat#1307.
	password, err := TenantRolePassword(tp.secret, tenantID)
	if err != nil {
		return nil, fmt.Errorf("tenant pool: derive password for tenant %s: %w", tenantID, err)
	}

	// Build DSN for the tenant.
	tenantDSN := fmt.Sprintf("%s user=%s password=%s", tp.connStr, roleName, password)
	pool, err = sql.Open("postgres", tenantDSN)
	if err != nil {
		return nil, fmt.Errorf("tenant pool for %s: open: %w", tenantID, err)
	}
	pool.SetMaxOpenConns(tp.maxConns)
	pool.SetMaxIdleConns(max(2, tp.maxConns/5))
	pool.SetConnMaxLifetime(5 * time.Minute)

	tp.mu.Lock()
	tp.pools[tenantID] = pool
	tp.mu.Unlock()
	return pool, nil
}

// Close closes all tenant pools.
func (tp *TenantPools) Close() {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	for id, pool := range tp.pools {
		pool.Close()
		delete(tp.pools, id)
	}
}

// EvictIdle closes pools that haven't been used for the given duration.
// Returns the number of pools evicted.
func (tp *TenantPools) EvictIdle(maxIdle time.Duration) int {
	// For now, a simple implementation that just keeps all pools.
	// Can be enhanced with last-used tracking later.
	return 0
}
