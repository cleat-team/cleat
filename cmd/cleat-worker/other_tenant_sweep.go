package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/cleat-team/cleat/engine"
)

// sweepOtherTenants runs fn against the store of every tenant the worker serves
// other than its own, and returns the sum of what fn reported. It is the shape
// cleat#3572's loops share, so each of them is the unchanged per-tenant statement
// plus one call to this, and none of them enumerates tenants again.
//
// It carries no cross-tenant privilege: each fn runs through THAT tenant's own
// store, so every statement is the one the worker already runs for its own
// tenant, under row-level security as that tenant. Tenants are opened and
// released one at a time (forEachOtherTenantStore).
//
// loop names the caller in the logs only. A tenant that fails is that tenant's
// failure and is logged and counted; the rest are still visited, and nothing here
// calls recordDBTrouble or recordReapSkip, which mean "this worker's own database
// is in trouble". It does nothing for a worker that cannot serve other tenants (no
// factory, a store that cannot list them, or --claim-across-tenants off).
//
// The merged reapers (cleat#3570, #3575, #3587) carry their own copies of this and
// are left alone: how often every worker enumerates every tenant is cleat#3573's.
func (w *Worker) sweepOtherTenants(loop string, fn func(ctx context.Context, st engine.WorkflowStore) (int64, error)) int64 {
	if !w.claimAcrossTenants {
		return 0
	}
	var total int64
	var failedTenants int
	var firstErr error
	sweep, err := w.forEachOtherTenantStore(func(tenantID string, st engine.WorkflowStore) {
		n, ferr := fn(w.ctx, st)
		total += n
		if ferr != nil {
			failedTenants++
			if firstErr == nil {
				firstErr = fmt.Errorf("tenant %s: %w", tenantID, ferr)
			}
		}
	})
	if err != nil {
		if !errors.Is(err, errRotatingClaimUnavailable) {
			w.logger.ErrorContext(w.ctx, loop+": could not list tenants; other tenants were not visited this pass",
				"worker_id", w.id, "error", err)
		}
		return 0
	}
	if sweep.OpenFailed > 0 || failedTenants > 0 {
		w.logger.WarnContext(w.ctx, loop+": some tenants could not be visited this pass; they wait for the next",
			"worker_id", w.id, "tenants", sweep.Visited, "open_failed", sweep.OpenFailed, "failed", failedTenants,
			"first_open_error", sweep.FirstErr, "first_error", firstErr)
	}
	return total
}
