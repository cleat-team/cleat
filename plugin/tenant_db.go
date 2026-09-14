package plugin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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

	// clock is time.Now unless a test replaces it, and it exists so that
	// eviction can be tested without sleeping. An assertion that waits for real
	// time to pass is measuring the scheduler as much as the code, and the
	// usual repair -- widening the window -- makes it slower and no more
	// truthful. With an injectable clock the test states the elapsed time it
	// means and gets exactly that.
	clock func() time.Time
}

// now reports the current time through the injectable clock.
func (tp *TenantPools) now() time.Time {
	if tp.clock == nil {
		return time.Now()
	}
	return tp.clock()
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

	// lastUsed is Unix nanoseconds, stamped every time For() hands this pool
	// out. cleat#1470.
	//
	// ATOMIC RATHER THAN UNDER tp.mu, because the stamp happens on the hot
	// path: every workflow the worker claims for a known tenant takes it, and
	// making that contend on the map's mutex would put every tenant's dispatch
	// behind every other tenant's. The reader (EvictIdle) tolerates a stamp
	// racing with its own read -- it would evict a pool used microseconds ago,
	// which costs one reopen and is not a correctness question.
	//
	// A pool that has never been handed out still has a stamp: For() sets it on
	// the way out of the open, so "never used" and "used at time zero" are not
	// confusable. Without that an unused pool looks infinitely idle and is the
	// first thing evicted, which is wrong for a pool that was just created for
	// a caller who is about to use it.
	lastUsed atomic.Int64
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
		entry.lastUsed.Store(tp.now().UnixNano())
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
	entry.lastUsed.Store(tp.now().UnixNano())
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

	// SetConnMaxIdleTime is "evict the least-recently-used individual
	// connection", delegated to the stdlib. cleat#1470.
	//
	// *sql.DB exposes no way to address, enumerate or close a PARTICULAR
	// connection -- its whole pool-control surface is four setters, Stats and
	// Close -- so a global LRU ordering over individual connections cannot be
	// written against it. What can be written is this, and under LIFO reuse it
	// lands on the same connections.
	//
	// db.conn() takes freeConn[LAST], so the hottest connection is reused every
	// time and a cold tail accumulates at the front. The connections that
	// exceed an idle window are therefore exactly the coldest ones, which is
	// the ordering the instruction asked for. LIFO is load-bearing here rather
	// than incidental: under FIFO every connection would be touched in turn,
	// none would ever look idle, and this setting would never fire.
	//
	// Distinct from SetConnMaxLifetime above, which closes a connection for
	// being OLD however busy it is. This closes one for being UNUSED.
	pool.SetConnMaxIdleTime(tenantConnIdleTimeout)

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

// tenantConnIdleTimeout is how long an individual connection may sit unused in
// a tenant pool before database/sql closes it.
//
// Two minutes, against a five-minute SetConnMaxLifetime: shorter than the
// lifetime so that idleness is what usually reclaims a connection, and long
// enough that a tenant polled once a minute keeps its connections warm rather
// than reconnecting on every tick.
const tenantConnIdleTimeout = 2 * time.Minute

// EvictIdle closes pools whose last use is older than maxIdle, and returns how
// many were evicted.
//
// THIS IS OPPORTUNISTIC HYGIENE. IT IS NOT THE BOUND, AND MUST NOT BECOME IT.
// cleat#1470.
//
// A timer-driven sweep can only shrink an overshoot after the fact: between two
// sweeps the pool count is whatever demand made it. The thing a connection
// budget has to guarantee is an INVARIANT -- never exceed it -- and an invariant
// has to be enforced at the moment it would be violated, which is admission:
// For() opening a pool for an unseen tenant. That is why the repository owner
// rejected TTL eviction as the mechanism:
//
//	"TTL eviction just seems wrong. We need to respect the global limit on
//	 connections, so the eviction and replacement policy can't just be TTL.
//	 When we're at the limit and need to open a new connection, we must close
//	 some other connection."
//
// Keeping this function is still worth it -- releasing a pool no tenant has
// touched in an hour is good housekeeping when nowhere near any limit -- but
// wiring a budget to rest on it would rebuild the rejected design under a new
// name. Whoever adds the admission hook should put the bound in For(), not
// here.
//
// NON-POSITIVE maxIdle EVICTS NOTHING, deliberately. "Idle for zero seconds"
// describes every pool including one handed out microseconds ago, so treating
// it literally is a stall dressed as a policy; a zero here is far more likely
// to be an unset config value than a request to close everything.
func (tp *TenantPools) EvictIdle(maxIdle time.Duration) int {
	if maxIdle <= 0 {
		// Not "evict everything". A non-positive idle window would close every
		// pool including ones handed out microseconds ago, which is a stall
		// dressed as a policy. The caller almost certainly meant a duration and
		// got a zero value.
		return 0
	}

	cutoff := tp.now().Add(-maxIdle).UnixNano()

	tp.mu.Lock()
	var evicted []*tenantPool
	for id, entry := range tp.pools {
		if entry.lastUsed.Load() < cutoff {
			evicted = append(evicted, entry)
			delete(tp.pools, id)
		}
	}
	tp.mu.Unlock()

	// CLOSED OUTSIDE THE LOCK, AND ASYNCHRONOUSLY. (*sql.DB).Close() waits for
	// in-use connections to be returned, so closing under tp.mu would block
	// every other tenant's For() behind one tenant's in-flight query -- turning
	// a capacity control into a latency fault. Removing from the map is what
	// makes a pool unreachable; the close is bookkeeping that can finish later.
	//
	// The count is returned BEFORE the closes complete, and that is deliberate:
	// it reports how many pools were evicted, not how many sockets have shut.
	for _, entry := range evicted {
		go func(e *tenantPool) {
			<-e.ready
			if e.db != nil {
				_ = e.db.Close()
			}
		}(entry)
	}
	return len(evicted)
}

// TenantPoolStats reports what the tenant pools are currently costing.
//
// It exists because nothing summed them. cleat#1486: a worker opens six
// independent pools and no code anywhere adds them up, so "how many connections
// does a worker use" had no answer that was not a hand calculation from flag
// defaults -- and the tenant pools are the term that cannot be calculated at
// all, because it depends on how many tenants this worker has touched.
type TenantPoolStats struct {
	// Pools is how many tenant pools are live.
	Pools int

	// OpenConnections is the sum of Stats().OpenConnections across them: the
	// connections that EXIST right now, in use or idle.
	//
	// THIS IS THE NUMBER A BUDGET IS ABOUT, and it is not Pools*maxConns. A
	// pool that has served two concurrent queries holds two connections, not
	// twenty-five; one untouched for a ConnMaxLifetime holds none at all. The
	// per-tenant ceiling is what a pool MAY reach, which is why documenting a
	// worker's cost as tenants*maxConns overstates it by roughly an order of
	// magnitude.
	OpenConnections int

	// InUse is the subset currently executing a query. OpenConnections-InUse is
	// the idle tail that SetConnMaxIdleTime reclaims.
	InUse int
}

// Stats sums the live tenant pools.
//
// Pools still opening are counted in Pools and contribute nothing to the
// connection counts, which is accurate rather than convenient: an entry whose
// `ready` is not yet closed has no *sql.DB to ask, and it has opened no
// connections either -- sql.Open does not connect.
func (tp *TenantPools) Stats() TenantPoolStats {
	tp.mu.Lock()
	entries := make([]*tenantPool, 0, len(tp.pools))
	for _, e := range tp.pools {
		entries = append(entries, e)
	}
	tp.mu.Unlock()

	// Asked OUTSIDE the lock. DB.Stats takes the pool's own mutex, so holding
	// tp.mu across every pool's Stats call would serialise the map against
	// every pool's internal contention -- on the hot path For() shares.
	st := TenantPoolStats{Pools: len(entries)}
	for _, e := range entries {
		select {
		case <-e.ready:
			if e.db != nil {
				s := e.db.Stats()
				st.OpenConnections += s.OpenConnections
				st.InUse += s.InUse
			}
		default:
			// Still opening: counted as a pool, contributing no connections.
		}
	}
	return st
}
