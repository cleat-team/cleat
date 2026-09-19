package engine

import (
	"database/sql"
	"sync"
	"sync/atomic"
	"time"
)

// leasedPool is one tenant's connection pool, plus the accounting that makes
// it safe to close.
//
// # Why this exists
//
// MySQLStoreFactory and MSSQLStoreFactory open a pool per tenant and released
// it only at Close(), so a worker that served a tenant once held its *sql.DB
// -- and the connection-opener goroutine database/sql runs behind it -- for
// the rest of the process's life. The connections themselves drain on their
// own (SetConnMaxLifetime is five minutes and database/sql does not reopen to
// fill an idle pool), so what accumulated was the struct, the map entry and
// the goroutine: small per tenant, unbounded in the number of tenants, and
// asymmetric with plugin.TenantPools next door, which has had both admission
// control and a reaper since cleat#1470.
//
// # Why a lease and not a timestamp
//
// Reclaiming a pool means calling (*sql.DB).Close, and every store already
// handed out from that pool keeps using it afterwards -- it holds the *sql.DB
// directly. Closing under one turns a memory tidy-up into "sql: database is
// closed" on a live query, which is the failure cmd/cleat-worker's sharded
// factory already warns about in prose ("the failure would be a closed pool
// under live traffic").
//
// An idle timestamp alone cannot answer this. A workflow can hold its store
// across an activity that does no database work for an hour, so "nothing has
// opened a store for this tenant recently" does not mean "nothing is using
// one". The lease counts the stores that have been handed out and not yet
// released, and a pool with an outstanding lease is never evicted however
// idle it looks.
type leasedPool struct {
	db *sql.DB

	// leases is how many stores opened from this pool have not yet been
	// closed. Evicting requires zero.
	//
	// ATOMIC RATHER THAN UNDER THE FACTORY'S MUTEX, matching the reasoning on
	// plugin.tenantPool.lastUsed: acquiring is on the hot path -- every
	// workflow a worker claims opens a store -- and making that contend on
	// the map's lock would put every tenant's dispatch behind every other
	// tenant's.
	leases atomic.Int32

	// lastUsed is Unix nanoseconds, stamped when a store is opened from this
	// pool and again when one is released.
	//
	// STAMPED AT CREATION TOO, by newLeasedPool. Without that a pool that has
	// been created but not yet used has lastUsed == 0, which is 1970 --
	// infinitely idle, and the first thing an eviction sweep takes. See
	// plugin's TestAPoolIsStampedWhenItIsCreated, which is the same bug one
	// package over.
	lastUsed atomic.Int64
}

// newLeasedPool wraps db, stamped as used now so it is not born idle.
func newLeasedPool(db *sql.DB, now time.Time) *leasedPool {
	p := &leasedPool{db: db}
	p.lastUsed.Store(now.UnixNano())
	return p
}

// acquire takes a lease and returns the closer that releases it.
func (p *leasedPool) acquire(now func() time.Time) *poolLease {
	p.leases.Add(1)
	p.lastUsed.Store(now().UnixNano())
	return &poolLease{pool: p, now: now}
}

// poolLease is the io.Closer OpenStore returns. Closing it says the caller is
// finished with the store, not that the pool should shut.
type poolLease struct {
	pool     *leasedPool
	now      func() time.Time
	released atomic.Bool
}

// Close releases the lease. It is idempotent.
//
// THE IDEMPOTENCE IS LOAD-BEARING, not politeness. A caller that both defers
// Close and closes explicitly -- or a wrapper that closes what it was handed
// and what it built -- would otherwise drive the count below zero, and a
// negative count reads as "nobody is using this" to an eviction sweep while a
// live store is still writing through the pool. Exactly the failure the lease
// exists to prevent, reached through the tidiest-looking call site.
func (l *poolLease) Close() error {
	if l.released.Swap(true) {
		return nil
	}
	l.pool.leases.Add(-1)
	// Stamped on release as well as acquisition, so a long execution that has
	// just finished leaves a fresh pool behind rather than one that looks
	// hours idle the instant its lease drops.
	l.pool.lastUsed.Store(l.now().UnixNano())
	return nil
}

// evictIdleLeasedPools removes from pools every entry that no store holds and
// that nothing has opened or released since maxIdle ago, closes them, and
// returns how many were evicted.
//
// NON-POSITIVE maxIdle EVICTS NOTHING, matching plugin.TenantPools.EvictIdle:
// "idle for zero seconds" describes a pool handed out microseconds ago, so
// taking it literally is a stall dressed as a policy, and a zero here is far
// likelier to be an unset config value than a request to close everything.
func evictIdleLeasedPools(mu *sync.RWMutex, pools map[string]*leasedPool, maxIdle time.Duration, now func() time.Time) int {
	if maxIdle <= 0 {
		return 0
	}
	cutoff := now().Add(-maxIdle).UnixNano()

	mu.Lock()
	var evicted []*leasedPool
	for id, p := range pools {
		// THE LEASE IS CHECKED FIRST AND IT IS NOT ADVISORY. An idle pool with
		// an outstanding lease belongs to a caller that is still holding the
		// store -- a workflow between two activities, most often -- and
		// closing it would fail that caller's next query.
		if p.leases.Load() > 0 {
			continue
		}
		if p.lastUsed.Load() >= cutoff {
			continue
		}
		evicted = append(evicted, p)
		delete(pools, id)
	}
	mu.Unlock()

	// CLOSED OUTSIDE THE LOCK AND ASYNCHRONOUSLY, for the reason
	// plugin.TenantPools.EvictIdle gives: (*sql.DB).Close waits for in-use
	// connections to be returned, so closing under the factory's mutex would
	// block every other tenant's OpenStore behind one tenant's in-flight
	// query. Removing the entry is what makes a pool unreachable; the close is
	// bookkeeping that can finish later.
	//
	// A lease cannot be taken on an entry that is no longer in the map, so
	// nothing can start using one of these between the unlock and the close.
	for _, p := range evicted {
		go func(db *sql.DB) { _ = db.Close() }(p.db)
	}
	return len(evicted)
}
