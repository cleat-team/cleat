package plugin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// TenantPools manages per-tenant database connection pools.
// Each pool connects directly as the tenant's login role, so there is
// no SET ROLE / RESET ROLE to escape — the connection IS the tenant.
type TenantPools struct {
	// Owner pool for administrative operations (claiming, migrations).
	OwnerDB *sql.DB

	mu       sync.Mutex
	pools    map[uuid.UUID]*sql.DB
	connStr  string // base connection string (without user/password — we add per-tenant)
}

// NewTenantPools creates a TenantPools manager.
// baseDSN is a connection string template like:
// "host=localhost port=5432 dbname=cleat sslmode=disable"
// The user and password are added per tenant.
func NewTenantPools(ownerDB *sql.DB, baseDSN string) *TenantPools {
	return &TenantPools{
		OwnerDB: ownerDB,
		pools:   make(map[uuid.UUID]*sql.DB),
		connStr: baseDSN,
	}
}

// For returns a tenant-scoped *sql.DB. Pools are created lazily and cached.
// Caller does NOT close the returned DB — TenantPools manages the lifecycle.
func (tp *TenantPools) For(ctx context.Context, tenantID uuid.UUID) (*sql.DB, error) {
	// In single-tenant mode, all workflows run under the default tenant
	// (zero UUID) and share the owner connection pool. No per-tenant
	// roles exist on managed PostgreSQL services.
	if tenantID == uuid.Nil {
		return tp.OwnerDB, nil
	}

	tp.mu.Lock()
	pool, ok := tp.pools[tenantID]
	tp.mu.Unlock()
	if ok {
		return pool, nil
	}

	// Look up tenant credentials from admin schema.
	var roleName, password string
	err := tp.OwnerDB.QueryRowContext(ctx,
		`SELECT role_name, password FROM admin.tenant_roles WHERE tenant_id = $1`,
		tenantID).Scan(&roleName, &password)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			log.Printf("tenant pool: no role for tenant %s — falling back to owner pool (single-tenant mode)", tenantID)
			return tp.OwnerDB, nil
		}
		return nil, fmt.Errorf("tenant pool: no role for tenant %s: %w", tenantID, err)
	}

	// Build DSN for the tenant.
	tenantDSN := fmt.Sprintf("%s user=%s password=%s", tp.connStr, roleName, password)
	pool, err = sql.Open("postgres", tenantDSN)
	if err != nil {
		return nil, fmt.Errorf("tenant pool for %s: open: %w", tenantID, err)
	}
	pool.SetMaxOpenConns(5)
	pool.SetMaxIdleConns(2)
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
