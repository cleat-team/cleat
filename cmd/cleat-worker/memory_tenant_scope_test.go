package main

import (
	"context"
	"testing"
)

// cleat#1097: defEstimates was keyed by def_name alone, so two tenants running a
// same-named workflow fed one EWMA and the exported gauge reported a blend of
// both as if it were either.
//
// cleat#1040 fixed the PERSISTED half -- workflow_memory_stats carries tenant_id
// and the store is per-tenant -- which is why the database could be right and
// the gauge wrong at the same time. This is the in-memory half.
//
// Asserted as a PROPERTY of what the controller reports, not as a fact about the
// map's key type: a test that reached into defEstimates would pass for any
// keying that merely looks tenant-shaped, including one that never populates the
// tenant.
func TestTwoTenantsRunningTheSameWorkflowDoNotShareAnEstimate(t *testing.T) {
	// store, not nil: RecordWorkflowMemory persists asynchronously and falls
	// back to c.store when no per-tenant store is passed. A struct-literal
	// controller leaves both nil, and the goroutine dereferences it -- a panic
	// in a background goroutine takes the whole test binary down, and whether
	// it lands at all depends on the goroutine winning a race with the test
	// finishing. That is a flake generator, not a test failure.
	c := &MemoryController{
		store:        &mockStore{},
		defEstimates: make(map[memoryEstimateKey]float64),
	}

	const def = "checkout"
	const tenantA = "11111111-1111-1111-1111-111111111111"
	const tenantB = "22222222-2222-2222-2222-222222222222"

	// Deliberately far apart: a blend of the two is distinguishable from either.
	c.RecordWorkflowMemory(context.Background(), nil, tenantA, def, 10_000_000)
	c.RecordWorkflowMemory(context.Background(), nil, tenantB, def, 900_000_000)

	got := c.DefEstimates()

	a, okA := got[memoryEstimateKey{tenantID: tenantA, defName: def}]
	b, okB := got[memoryEstimateKey{tenantID: tenantB, defName: def}]
	if !okA || !okB {
		t.Fatalf("expected an estimate for each tenant, got %d entries: %v", len(got), got)
	}

	if a != 10_000_000 {
		t.Errorf("tenant A's estimate is %.0f, want 10000000.\n\n"+
			"A first sample seeds the EWMA directly, so anything else means tenant "+
			"B's 900MB sample reached tenant A's series -- which is cleat#1097.", a)
	}
	if b != 900_000_000 {
		t.Errorf("tenant B's estimate is %.0f, want 900000000 -- B's first sample "+
			"was blended with A's", b)
	}
	if len(got) != 2 {
		t.Errorf("got %d estimates for one def_name across two tenants, want 2: %v", len(got), got)
	}
}

// The other half, and the one a naive fix breaks: a single tenant's repeated
// samples must still smooth into one series. A key that accidentally varied per
// call -- or per workflow run -- would pass the test above and destroy the EWMA,
// since every sample would seed a fresh entry instead of updating one.
func TestOneTenantsRepeatedSamplesStillSmoothIntoOneEstimate(t *testing.T) {
	// store, not nil: RecordWorkflowMemory persists asynchronously and falls
	// back to c.store when no per-tenant store is passed. A struct-literal
	// controller leaves both nil, and the goroutine dereferences it -- a panic
	// in a background goroutine takes the whole test binary down, and whether
	// it lands at all depends on the goroutine winning a race with the test
	// finishing. That is a flake generator, not a test failure.
	c := &MemoryController{
		store:        &mockStore{},
		defEstimates: make(map[memoryEstimateKey]float64),
	}

	const def = "checkout"
	const tenant = "11111111-1111-1111-1111-111111111111"

	c.RecordWorkflowMemory(context.Background(), nil, tenant, def, 100_000_000)
	c.RecordWorkflowMemory(context.Background(), nil, tenant, def, 200_000_000)

	got := c.DefEstimates()
	if len(got) != 1 {
		t.Fatalf("two samples from one tenant produced %d estimates, want 1: %v", len(got), got)
	}

	// Read the sole entry rather than looking it up by key. Constructing the
	// key here would couple this test to the key SHAPE, and it did: with the
	// tenant dropped from the key -- the cleat#1097 defect -- this test failed
	// on a missing lookup rather than on smoothing, reporting a defect it does
	// not test. Smoothing is a property of the value, and the assertion above
	// has already established there is exactly one.
	var est float64
	for _, v := range got {
		est = v
	}
	if est == 200_000_000 {
		t.Errorf("the second sample replaced the first rather than smoothing into it; "+
			"got %.0f, which is the raw second sample and means the EWMA is not "+
			"updating an existing entry", est)
	}
	if est <= 100_000_000 || est >= 200_000_000 {
		t.Errorf("estimate %.0f is outside the two samples it smooths (100MB, 200MB)", est)
	}
}

// testTenant is the tenant the pre-cleat#1097 memory-controller tests implicitly
// assumed when the map was keyed by def_name alone. Naming it is the point: the
// old signatures could not say which tenant they were about, which is the defect
// those tests were written on the wrong side of.
const testTenant = "00000000-0000-0000-0000-000000000000"
