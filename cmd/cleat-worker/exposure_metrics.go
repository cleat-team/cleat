package main

// cleat#3001, the /metrics member of the class.
//
// An unauthenticated scrape renders every series labelled with a definition
// name, so an `internal` definition's NAME reached a route that never consults
// the class. The fix is a serve-time filter (monitoring/prometheus), and that
// filter needs a class the metric record path does not have: Exposure lives on
// the definition row, while the record sites -- executeWorkflow and the
// Record*/Add* calls below it -- hold only the *WorkflowInstance.
//
// So the worker resolves the class here and hands the internal names to the
// Metrics set, TWO ways:
//
//   - noteInternalDefs, on the execution path, throttled per tenant -- the
//     common case, and the only one that runs when a tenant is active.
//   - internalDefsSweepLoop, on a ticker -- the BOUND. Without it, a definition
//     that runs while public and is then tightened to internal, after which the
//     tenant executes nothing further, is never re-resolved and keeps naming
//     itself on the scrape until the process restarts. cleat-review reproduced
//     exactly that against the throttle alone.
//
// Both resolve a whole TENANT at a time rather than a definition at a time:
// the class is per definition, but the answer the filter wants is the tenant's
// internal-name set, and one ListWorkflowDefs per tenant per refresh is cheaper
// than a lookup per execution on the hot path.

import (
	"context"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// exposureMarkRefresh bounds how stale the names handed to the /metrics filter
// may be while a tenant keeps executing. The class is MUTABLE per (name,
// version) -- a redeploy can tighten a definition to internal -- so a cache
// with no expiry could miss a tightening and leak the name. Marking is
// additive, so a name is only ever ADDED; a loosening leaves it marked, which
// over-hides a series and never leaks.
const exposureMarkRefresh = 30 * time.Second

// exposureSweepInterval is how often the sweep re-resolves every tenant this
// worker has executed for. It is deliberately larger than exposureMarkRefresh:
// an ACTIVE tenant is already refreshed per execution, so the sweep exists to
// bound the INACTIVE case, and one ListWorkflowDefs per tenant per five minutes
// is a cheap ceiling on how long a tightened name can outlive its last run.
const exposureSweepInterval = 5 * time.Minute

// noteInternalDefs resolves wf's tenant's internal definition names and records
// them on the metrics filter, at most once per exposureMarkRefresh per tenant.
//
// It runs on the execution path, so its failure modes are deliberately quiet: a
// worker without a metrics handle, or a store that will not open, is not an
// execution error, and a resolution failure is retried after the refresh rather
// than logged per run.
func (w *Worker) noteInternalDefs(ctx context.Context, wf *engine.WorkflowInstance) {
	if w.Metrics == nil || wf == nil {
		return
	}
	tenant := w.cacheTenantFor(wf.TenantID)
	if last, ok := w.internalDefsRefreshed.Load(tenant); ok {
		if time.Since(last.(time.Time)) < exposureMarkRefresh {
			return
		}
	}
	// Record the ATTEMPT before resolving, not the success: a store that is
	// failing must not turn every execution into a query.
	w.internalDefsRefreshed.Store(tenant, time.Now())
	w.markTenantInternalDefs(ctx, tenant)
}

// markTenantInternalDefs resolves tenant's internal definition names and hands
// them to the metrics filter. It does not throttle; the callers decide when to
// run it.
//
// The store is opened as `tenant`, which both callers pass already normalised
// by cacheTenantFor. That is the SAME key storeForTenant routes on -- not a
// second scoping rule -- and it is why the sweep can hand the refresh map's keys
// straight back: the map is keyed by exactly this value.
func (w *Worker) markTenantInternalDefs(ctx context.Context, tenant string) {
	if w.Metrics == nil {
		return
	}
	st, release, err := w.storeForTenant(tenant)
	if err != nil {
		return
	}
	defer release()

	defs, err := st.ListWorkflowDefs(ctx, "")
	if err != nil {
		return
	}
	var internal []string
	for i := range defs {
		if defs[i].Exposure.OrDefault() == engine.ExposureInternal {
			internal = append(internal, defs[i].Name)
		}
	}
	w.Metrics.NoteInternalDefs(internal...)
}

// sweepInternalDefs re-resolves every tenant this worker has executed for --
// the keys of internalDefsRefreshed.
//
// It is the sweep half of the design, and it exists because the execution-path
// throttle is a bound ONLY while the tenant keeps executing: a definition that
// runs while public and is then TIGHTENED to internal, with no further
// execution, would never be re-resolved, and its series would keep naming it on
// an unauthenticated endpoint until the process restarted (cleat-review
// reproduced this: "scrape still names it: true"). The sweep bounds that at
// exposureSweepInterval, for every tenant, regardless of which writer tightened
// the definition -- which the exposure-write hook alone would not, since another
// worker's deploy is invisible here.
func (w *Worker) sweepInternalDefs(ctx context.Context) {
	if w.Metrics == nil {
		return
	}
	w.internalDefsRefreshed.Range(func(key, _ any) bool {
		if tenant, ok := key.(string); ok {
			w.markTenantInternalDefs(ctx, tenant)
		}
		return true
	})
}

// internalDefsSweepLoop drives sweepInternalDefs on a ticker.
//
// A dedicated loop rather than a call inside metricsSweepLoop: that loop returns
// early on a store that does not implement MetricsStore (MySQL and SQL Server
// today), and this sweep needs only ListWorkflowDefs, which every store has --
// so hanging it there would leave exactly those dialects unbounded.
func (w *Worker) internalDefsSweepLoop() {
	defer w.wg.Done()
	w.healthTracker.setInterval("internal_defs_sweep", exposureSweepInterval)
	ticker := time.NewTicker(exposureSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.getLoopCtx("internal_defs_sweep").Done():
			return
		case <-ticker.C:
			w.healthTracker.recordRun("internal_defs_sweep")
			w.sweepInternalDefs(w.ctx)
		}
	}
}
