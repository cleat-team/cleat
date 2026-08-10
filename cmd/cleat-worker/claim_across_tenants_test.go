package main

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// crossTenantMockStore is a mockStore that also implements
// engine.CrossTenantClaimer, so the dispatch loop can tell the two claims apart.
type crossTenantMockStore struct {
	*mockStore
	mu      sync.Mutex
	crossN  int
	scopedN int
}

func (m *crossTenantMockStore) ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
	m.mu.Lock()
	m.scopedN++
	m.mu.Unlock()
	return nil, nil
}

func (m *crossTenantMockStore) ClaimWorkflowsAcrossTenants(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
	m.mu.Lock()
	m.crossN++
	m.mu.Unlock()
	return nil, nil
}

func (m *crossTenantMockStore) counts() (cross, scoped int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.crossN, m.scopedN
}

// TestClaimGeneral_UsesTheCrossTenantClaimOnlyWhenAsked.
//
// Both halves matter. Claiming across tenants when nobody asked would widen
// what a worker executes as a side effect of upgrading; not claiming across
// tenants when asked would leave every non-default tenant's work unrun with no
// indication why.
func TestClaimGeneral_UsesTheCrossTenantClaimOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		enabled               bool
		wantCross, wantScoped int
	}{
		{"flag off", false, 0, 1},
		{"flag on", true, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms := &crossTenantMockStore{mockStore: &mockStore{}}
			w := newTestWorker(ms.mockStore)
			defer w.cancel()
			w.store = ms
			w.claimAcrossTenants = tc.enabled

			if _, err := w.claimGeneral(5); err != nil {
				t.Fatalf("claimGeneral: %v", err)
			}
			cross, scoped := ms.counts()
			if cross != tc.wantCross || scoped != tc.wantScoped {
				t.Errorf("cross=%d scoped=%d, want cross=%d scoped=%d",
					cross, scoped, tc.wantCross, tc.wantScoped)
			}
		})
	}
}

// TestClaimGeneral_FallsBackWhenTheStoreCannotClaimAcrossTenants.
//
// The flag says what the operator wants; the store says what the dialect and
// the deployment's grants can actually do. On a mixed fleet those disagree, and
// the honest answer is to keep claiming this worker's own tenant rather than to
// stop claiming at all.
func TestClaimGeneral_FallsBackWhenTheStoreCannotClaimAcrossTenants(t *testing.T) {
	var scoped int
	ms := &mockStore{}
	ms.claimWorkflowsFn = func(_ context.Context, _ string, _ int) ([]*engine.WorkflowInstance, error) {
		scoped++
		return nil, nil
	}
	w := newTestWorker(ms)
	defer w.cancel()
	w.claimAcrossTenants = true // asked for, but mockStore does not implement it

	for i := 0; i < 3; i++ {
		if _, err := w.claimGeneral(5); err != nil {
			t.Fatalf("claimGeneral: %v", err)
		}
	}
	if scoped != 3 {
		t.Errorf("tenant-scoped claims = %d, want 3; the loop stopped claiming instead of falling back", scoped)
	}
}

// unsupportedCrossTenantStore implements CrossTenantClaimer but refuses, the
// way a MySQL store on the per-tenant-database topology does.
type unsupportedCrossTenantStore struct {
	*mockStore
	mu      sync.Mutex
	scopedN int
}

func (m *unsupportedCrossTenantStore) ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
	m.mu.Lock()
	m.scopedN++
	m.mu.Unlock()
	return nil, nil
}

func (m *unsupportedCrossTenantStore) ClaimWorkflowsAcrossTenants(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
	return nil, fmt.Errorf("per-tenant databases: %w", engine.ErrCrossTenantClaimUnsupported)
}

// TestClaimGeneral_FallsBackWhenTheStoreRefusesTheCrossTenantClaim.
//
// A store can implement the interface and still be unable to honour it -- MySQL
// on the topology cmd/cleat-worker actually builds gives each tenant its own
// database, so there is no predicate to drop and no rows to widen to.
//
// The failure must not surface as a claim error. Returning one every tick would
// read as a database fault and, worse, would stop the loop claiming anything at
// all -- so a worker that asked for something its store cannot do would stop
// running the work it CAN do.
func TestClaimGeneral_FallsBackWhenTheStoreRefusesTheCrossTenantClaim(t *testing.T) {
	ms := &unsupportedCrossTenantStore{mockStore: &mockStore{}}
	w := newTestWorker(ms.mockStore)
	defer w.cancel()
	w.store = ms
	w.claimAcrossTenants = true

	for i := 0; i < 3; i++ {
		if _, err := w.claimGeneral(5); err != nil {
			t.Fatalf("claimGeneral returned an error instead of falling back: %v", err)
		}
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.scopedN != 3 {
		t.Errorf("tenant-scoped claims = %d, want 3; the loop stopped claiming its own tenant's work too", ms.scopedN)
	}
}
