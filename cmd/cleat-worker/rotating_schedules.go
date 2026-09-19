package main

import (
	"fmt"

	"github.com/cleat-team/cleat/engine"
)

// dueSchedulesByTenant reads due schedules for every tenant, one tenant at a
// time, through a store scoped to that tenant.
//
// # Why this is not the rotating claim
//
// The claim serves a BOUNDED SHARE of tenants per tick on purpose: work that
// waits a tick is work that waits a tick, and the cursor guarantees every
// tenant is reached. Cron cannot use that bargain. A schedule that is due must
// fire, and "it will fire on a later tick" is how a minutely cron becomes an
// every-few-minutes cron -- silently, and only on the deployments with enough
// tenants to notice.
//
// So this reads EVERY tenant each tick. The schedule loop runs on a 15-second
// ticker against cron whose finest granularity is a minute, which is what makes
// that affordable: the whole pass has to finish well inside the gap between
// ticks, not inside a claim's latency budget. At one millisecond per round trip
// a thousand tenants is about a second of a fifteen-second tick.
//
// THAT IS A REAL CEILING AND IT IS WORTH STATING. A deployment with tens of
// thousands of tenants on one worker will spend a noticeable fraction of each
// tick here, and the answer at that size is more workers with fewer tenants
// each rather than a longer pass -- the same conclusion --claim-tenants-per-tick
// reaches from the other direction.
//
// # Why it needs no grant
//
// The same asymmetry the rotating claim rests on: `admin.tenants` carries no
// row-level security, so the tenant list is readable through the ordinary
// connection, and each tenant's schedules are then read under that tenant's own
// RLS context. 024's `admin.get_due_schedules` needs a BYPASSRLS owner, which
// managed PostgreSQL cannot create -- so before this, a non-default tenant's
// cron could not fire on RDS, Cloud SQL or Azure at all.
func (w *Worker) dueSchedulesByTenant() ([]engine.Schedule, error) {
	if w.storeFactory == nil {
		return nil, fmt.Errorf("%w: no store factory to open per-tenant stores with", errRotatingClaimUnavailable)
	}
	lister, ok := w.store.(engine.TenantLister)
	if !ok {
		return nil, fmt.Errorf("%w: this store cannot enumerate tenants", errRotatingClaimUnavailable)
	}

	tenants, err := lister.ListTenantIDs(w.ctx)
	if err != nil {
		return nil, fmt.Errorf("rotating due-schedule read: %w", err)
	}

	var (
		due      []engine.Schedule
		firstErr error
		reached  int
	)
	for _, tenantID := range tenants {
		st, release, serr := w.storeForTenant(tenantID)
		if serr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("rotating due-schedule read: tenant %s: %w", tenantID, serr)
			}
			continue
		}
		schedules, gerr := st.GetDueSchedules(w.ctx)
		// Released per tenant rather than deferred: this loop reads EVERY
		// tenant every tick, so deferring would hold the whole estate's pools
		// open for the length of the sweep. The rows it returns are plain
		// data; firing a schedule opens its own store below.
		release()
		if gerr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("rotating due-schedule read: tenant %s: %w", tenantID, gerr)
			}
			continue
		}
		reached++
		due = append(due, schedules...)
	}

	// An error is returned only when NO tenant could be read. One tenant whose
	// store will not open must not stop the others' cron -- and the complement
	// matters just as much: a total failure that returned (nil, nil) would read
	// to the schedule loop as "nothing is due", which is indistinguishable from
	// a healthy quiet period and would hide a database outage.
	if reached == 0 && firstErr != nil {
		return nil, firstErr
	}
	if firstErr != nil {
		w.logger.WarnContext(w.ctx, "rotating due-schedule read: some tenants could not be read this tick",
			"worker_id", w.id, "tenants_read", reached, "tenants_total", len(tenants), "error", firstErr)
	}
	return due, nil
}
