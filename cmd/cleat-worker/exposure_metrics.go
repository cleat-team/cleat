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
// So the worker resolves the class here, at EXECUTION, and hands the internal
// names to the Metrics set. It resolves a whole TENANT at a time rather than a
// definition at a time: the class is per definition, but the answer the filter
// wants is the tenant's internal-name set, and one ListWorkflowDefs per tenant
// per refresh is cheaper than a lookup per execution on the hot path.

import (
	"context"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// exposureMarkRefresh bounds how stale the names handed to the /metrics filter
// may be. The class is MUTABLE per (name, version) -- a redeploy can tighten a
// definition to internal -- so a cache with no expiry could miss a tightening
// and leak the name. Marking is additive, so a name is only ever ADDED; a
// loosening leaves it marked, which over-hides a series and never leaks.
const exposureMarkRefresh = 30 * time.Second

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

	st, release, err := w.storeForTenant(wf.TenantID)
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
