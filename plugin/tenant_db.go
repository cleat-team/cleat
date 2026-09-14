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
	pools    map[string]*tenantPool
	connStr  string // base connection string (without user/password — we add per-tenant)
	maxConns int    // max open connections per tenant pool

	// secret derives each tenant's password. Nothing per-tenant is stored:
	// see TenantRolePassword and cleat#1307. Empty means no tenant pool can be
	// opened, which For() reports rather than working around.
	secret []byte
}

// tenantPool is one tenant's pool, plus the means to wait for it while it is
// still being opened.
//
// The map holds these rather than *sql.DB so that a second caller arriving
// mid-open has something to wait ON. Before cleat#1508 it had nothing: For()
// released the lock to do the role lookup and the open, so every concurrent
// first-touch for one tenant opened its own pool and all but the last were
// orphaned -- not in the map, so Close() never reached them and EvictIdle
// could not have either. Measured at 20 distinct pools from 32 concurrent
// calls, which at the default maxConns is up to 475 leaked connections from
// one tenant's first burst.
type tenantPool struct {
	// ready is closed when db and err are final. Readers must not touch
	// either field before it is closed.
	ready chan struct{}
	db    *sql.DB
	err   error
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
		pools:    make(map[string]*tenantPool),
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

	// SINGLE-FLIGHT PER TENANT. The first caller for an unseen tenant installs
	// an entry and opens; everyone else waits on that entry. cleat#1508.
	//
	// Not "hold the mutex across the open", which would also be correct and is
	// the wrong shape: the role lookup below is a database round trip, so one
	// slow query would serialise the first open of EVERY tenant.
	tp.mu.Lock()
	if entry, ok := tp.pools[tenantID]; ok {
		tp.mu.Unlock()
		select {
		case <-entry.ready:
			// A waiter gets the opener's outcome, including its failure. It
			// does not retry here: the opener removes a failed entry from the
			// map, so the next CALL opens again. Retrying inside the wait
			// would turn one bad role lookup into a thundering herd of them.
			return entry.db, entry.err
		case <-ctx.Done():
			// Do not block a cancelled caller on somebody else's open. The
			// opener carries on; its result is still cached for whoever wants
			// it.
			return nil, fmt.Errorf("tenant pool for %s: waiting for another caller's open: %w",
				tenantID, ctx.Err())
		}
	}
	entry := &tenantPool{ready: make(chan struct{})}
	tp.pools[tenantID] = entry
	tp.mu.Unlock()

	entry.db, entry.err = tp.open(ctx, tenantID)
	close(entry.ready)

	if entry.err != nil {
		// A FAILED OPEN IS NOT CACHED. The pre-cleat#1508 code cached nothing
		// on failure, so every call retried; keeping a failed entry would make
		// one transient role-lookup error poison this tenant for the lifetime
		// of the process. Guarded on identity so a concurrent Close() that
		// already replaced or removed the entry is not undone.
		tp.mu.Lock()
		if tp.pools[tenantID] == entry {
			delete(tp.pools, tenantID)
		}
		tp.mu.Unlock()
	}
	return entry.db, entry.err
}

// open builds one tenant's pool. It is called at most once per tenant per
// successful open, under the single-flight in For.
func (tp *TenantPools) open(ctx context.Context, tenantID string) (*sql.DB, error) {
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
	pool, err := sql.Open("postgres", tenantDSN)
	if err != nil {
		return nil, fmt.Errorf("tenant pool for %s: open: %w", tenantID, err)
	}
	pool.SetMaxOpenConns(tp.maxConns)
	pool.SetMaxIdleConns(max(2, tp.maxConns/5))
	pool.SetConnMaxLifetime(5 * time.Minute)

	// No write to tp.pools here. For() owns the map; open() only builds. That
	// separation is the fix -- the store-and-return that used to live here ran
	// outside any single-flight, so concurrent callers each stored their own.
	return pool, nil
}

// Close closes all tenant pools.
//
// It waits for any open still in flight rather than skipping it. An entry
// whose `ready` is not yet closed has an opener that will complete and hand
// back a live *sql.DB; closing the map without it would recreate the very leak
// cleat#1508 fixes, at shutdown instead of at startup.
//
// The snapshot is taken under the lock and the waiting is done outside it, so
// an in-flight open -- which is doing a database round trip -- cannot block
// every other close behind it.
func (tp *TenantPools) Close() {
	tp.mu.Lock()
	entries := make([]*tenantPool, 0, len(tp.pools))
	for id, entry := range tp.pools {
		entries = append(entries, entry)
		delete(tp.pools, id)
	}
	tp.mu.Unlock()

	for _, entry := range entries {
		<-entry.ready
		if entry.db != nil {
			entry.db.Close()
		}
	}
}

// EvictIdle closes pools that haven't been used for the given duration.
// Returns the number of pools evicted.
func (tp *TenantPools) EvictIdle(maxIdle time.Duration) int {
	// For now, a simple implementation that just keeps all pools.
	// Can be enhanced with last-used tracking later.
	return 0
}
