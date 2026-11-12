package main

import (
	"context"
	"testing"
)

// cleat#3572: MemoryController.LoadEstimates ran every five minutes and did
// `c.defEstimates = keyed`, a whole-map replacement holding only the worker's own
// tenant. RecordWorkflowMemory writes every tenant's learned EWMAs into that same
// map, so each reload threw away what every other tenant had learned: the
// exported estimate gauge reads DefEstimates, so it stopped being refreshed for them
// (an UpDownCounter keeps its last value), and their next sample seeded the EWMA from one value instead of smoothing into what
// had been learned. (WorkflowMemoryEstimate, which would answer the 32 MB default
// for them, has no production caller, so nothing sized a run from the lost value.)
//
// Asserted through what the controller reports (DefEstimates), not through the
// map's shape.
func TestAReloadReplacesOnlyTheLoadedTenantsEstimates(t *testing.T) {
	const (
		own   = "11111111-1111-1111-1111-111111111111"
		other = "22222222-2222-2222-2222-222222222222"
	)
	stored := map[string]float64{"checkout": 40_000_000}
	c := &MemoryController{
		store: &mockStore{loadMemoryEstimatesFn: func(context.Context) (map[string]float64, error) {
			return stored, nil
		}},
		defEstimates: map[memoryEstimateKey]float64{
			// Another tenant's learned value: the thing the reload must not lose.
			{tenantID: other, defName: "checkout"}: 900_000_000,
			// A stale value of the loaded tenant's own, for a definition that is no
			// longer in its store: a reload that only ADDS would keep it forever.
			{tenantID: own, defName: "retired"}: 7_000_000,
			// And its own, to be replaced by the stored value.
			{tenantID: own, defName: "checkout"}: 1_000_000,
		},
	}

	if err := c.LoadEstimates(context.Background(), own); err != nil {
		t.Fatalf("LoadEstimates: %v", err)
	}
	got := c.DefEstimates()

	if v, ok := got[memoryEstimateKey{tenantID: other, defName: "checkout"}]; !ok || v != 900_000_000 {
		t.Errorf("another tenant's estimate is %v (present=%v), want 900000000: the reload discarded what that tenant had learned (cleat#3572)", v, ok)
	}
	if v := got[memoryEstimateKey{tenantID: own, defName: "checkout"}]; v != 40_000_000 {
		t.Errorf("the loaded tenant's estimate is %v, want the stored 40000000: the reload did not replace it", v)
	}
	if v, ok := got[memoryEstimateKey{tenantID: own, defName: "retired"}]; ok {
		t.Errorf("the loaded tenant still has an estimate (%v) for a definition its store no longer holds: the reload only adds, it does not replace", v)
	}
	if len(got) != 2 {
		t.Errorf("got %d estimates, want 2: %v", len(got), got)
	}
}

// A failed load must leave the map exactly as it was: the reload already returned
// before touching it, and the merge must not weaken that.
func TestAFailedReloadLeavesEveryEstimateAlone(t *testing.T) {
	const own = "11111111-1111-1111-1111-111111111111"
	c := &MemoryController{
		store: &mockStore{loadMemoryEstimatesFn: func(context.Context) (map[string]float64, error) {
			return nil, context.DeadlineExceeded
		}},
		defEstimates: map[memoryEstimateKey]float64{{tenantID: own, defName: "checkout"}: 1_000_000},
	}
	if err := c.LoadEstimates(context.Background(), own); err == nil {
		t.Fatal("LoadEstimates returned nil for a failed load")
	}
	if got := c.DefEstimates(); len(got) != 1 || got[memoryEstimateKey{tenantID: own, defName: "checkout"}] != 1_000_000 {
		t.Errorf("a failed reload changed the estimates: %v", got)
	}
}
