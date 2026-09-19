package main

import (
	"errors"
	"fmt"
	"sync"

	"github.com/cleat-team/cleat/engine"
)

// errRotatingClaimUnavailable reports that this worker cannot claim by rotation
// -- no store factory to open per-tenant stores with, or a store that cannot
// enumerate tenants. It is a static property of the configuration, so the
// caller reports it once rather than every tick.
var errRotatingClaimUnavailable = errors.New("rotating claim unavailable")

// defaultClaimTenantsPerTick bounds how many tenants one dispatch tick will
// poll.
//
// The ceiling exists because the rotating claim trades one query per tick for
// one enumeration plus up to k claims, and k has to be bounded by something
// other than the tenant count: a deployment with 10,000 tenants must not issue
// 10,000 queries per second. Unserved tenants are not skipped, they are served
// on a later tick -- the cursor is what makes that true.
//
// 16 is a starting point rather than a measurement. It is above the batch sizes
// a dispatch tick usually asks for, so in the common case the limit fills
// before the ceiling is reached and the ceiling costs nothing.
const defaultClaimTenantsPerTick = 16

// tenantRotation is the worker-local cursor over tenants.
//
// WORKER-LOCAL ON PURPOSE. The obvious alternative is a shared table of
// per-tenant service counters, which is the only way to implement weighted fair
// queueing -- and it puts contended mutable state on the hot path of every
// dispatch tick in the fleet. Round robin does not need it: each worker
// resumes where it stopped, and with several workers the rotations interleave.
// Nothing is persisted, so a restarted worker begins at the front of the list
// and converges again within a few ticks.
//
// The cursor is a tenant ID rather than an index. The list is re-read each
// tick and a tenant can be created or dropped between two of them, so an index
// would silently point at a different tenant; an ID that is no longer present
// is recognisable, and restarts the rotation rather than skipping an arbitrary
// distance into it.
type tenantRotation struct {
	mu         sync.Mutex
	lastServed string
}

// next returns tenants in rotation order, starting after the last one served.
//
// Returns at most `max` of them. The caller may stop early -- it usually will,
// because it stops once the claim limit is filled -- and reports back through
// advance() how far it actually got.
func (r *tenantRotation) next(tenants []string, max int) []string {
	if len(tenants) == 0 || max <= 0 {
		return nil
	}
	r.mu.Lock()
	last := r.lastServed
	r.mu.Unlock()

	// Start after the last tenant served. A cursor naming a tenant that is no
	// longer in the list -- dropped, or renamed out from under us -- starts
	// over rather than guessing: the alternative is to keep a position in a
	// list that has changed shape, which is how a rotation quietly stops
	// covering part of its range.
	start := 0
	if last != "" {
		for i, t := range tenants {
			if t == last {
				start = (i + 1) % len(tenants)
				break
			}
		}
	}

	if max > len(tenants) {
		max = len(tenants)
	}
	out := make([]string, 0, max)
	for i := 0; i < max; i++ {
		out = append(out, tenants[(start+i)%len(tenants)])
	}
	return out
}

// advance records the last tenant this tick actually visited.
//
// VISITED, not "returned work". A tenant that was polled and had nothing still
// had its turn, and leaving the cursor behind it would make the next tick poll
// the same idle tenants first -- which is the starvation this replaces, with
// the roles reversed.
func (r *tenantRotation) advance(tenantID string) {
	if tenantID == "" {
		return
	}
	r.mu.Lock()
	r.lastServed = tenantID
	r.mu.Unlock()
}

// claimRotating claims up to limit workflows, spread across tenants in rotation
// order.
//
// This is the two-phase claim:
//
//  1. enumerate tenants through the ordinary connection (`admin.tenants` has no
//     RLS -- see engine.TenantLister), and
//  2. claim within each chosen tenant using the SAME statement the
//     single-tenant path runs, through a store scoped to that tenant, so the
//     claim executes under that tenant's own RLS context.
//
// Phase 2 is `storeForTenant(t).ClaimWorkflows(...)` and nothing more. That is
// the point of the design rather than a convenience: the per-tenant claim is
// already the tested, single-statement `FOR UPDATE SKIP LOCKED` query whose
// locking properties the comments in store_lifecycle.go establish at length,
// and this adds no SQL to it.
//
// # Why not one query with a window function
//
// `ROW_NUMBER() OVER (PARTITION BY tenant_id ...)` is the obvious way to cap
// each tenant's share in a single statement, and PostgreSQL does not permit
// `FOR UPDATE` in a query that uses window functions. Ranking would have to
// move to a CTE and locking to a second step keyed by id, which reopens the
// race the current statement closes in one. Syntax is not a good enough reason
// to give that up.
func (w *Worker) claimRotating(limit int) ([]*engine.WorkflowInstance, error) {
	if w.storeFactory == nil {
		// Without a factory, storeForTenant returns the worker's own store for
		// every tenant -- so the rotation would claim the same tenant k times
		// and call it fair. Refusing is the only honest answer.
		return nil, fmt.Errorf("%w: no store factory to open per-tenant stores with", errRotatingClaimUnavailable)
	}
	lister, ok := w.store.(engine.TenantLister)
	if !ok {
		return nil, fmt.Errorf("%w: this store cannot enumerate tenants", errRotatingClaimUnavailable)
	}

	tenants, err := lister.ListTenantIDs(w.ctx)
	if err != nil {
		return nil, fmt.Errorf("rotating claim: %w", err)
	}
	if len(tenants) == 0 {
		return nil, nil
	}

	perTick := w.claimTenantsPerTick
	if perTick <= 0 {
		perTick = defaultClaimTenantsPerTick
	}
	chosen := w.tenantRotation.next(tenants, perTick)
	if len(chosen) == 0 {
		return nil, nil
	}

	// The share is what makes this FAIR rather than merely per-tenant: without
	// it the first tenant in the rotation would fill the whole batch and the
	// rest would wait for the next tick, which is the original complaint moved
	// one level down. At least 1, so a batch smaller than the tenant count
	// still makes progress rather than asking every tenant for nothing.
	share := limit / len(chosen)
	if share < 1 {
		share = 1
	}

	var (
		claimed  []*engine.WorkflowInstance
		firstErr error
		lastSeen string
	)
	for _, tenantID := range chosen {
		if len(claimed) >= limit {
			break
		}
		want := share
		if remaining := limit - len(claimed); want > remaining {
			want = remaining
		}

		st, serr := w.storeForTenant(tenantID)
		if serr != nil {
			// One tenant whose store will not open must not stop the tick:
			// the others are still servable, and a worker that claims nothing
			// because one tenant is misconfigured is a worse outcome than a
			// worker that claims for the rest and says so.
			if firstErr == nil {
				firstErr = fmt.Errorf("rotating claim: tenant %s: %w", tenantID, serr)
			}
			lastSeen = tenantID
			continue
		}
		wfs, cerr := st.ClaimWorkflows(w.ctx, w.id, want)
		if cerr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("rotating claim: tenant %s: %w", tenantID, cerr)
			}
			lastSeen = tenantID
			continue
		}
		claimed = append(claimed, wfs...)
		lastSeen = tenantID
	}
	w.tenantRotation.advance(lastSeen)

	// An error is returned only when it is the whole story. With work in hand
	// the tick succeeded for the tenants it reached, and returning the error
	// instead would send the dispatch loop into its connection backoff while
	// holding workflows it had just claimed.
	if len(claimed) == 0 && firstErr != nil {
		return nil, firstErr
	}
	if firstErr != nil {
		w.logger.WarnContext(w.ctx, "rotating claim: some tenants could not be served this tick",
			"worker_id", w.id, "claimed", len(claimed), "error", firstErr)
	}
	return claimed, nil
}
