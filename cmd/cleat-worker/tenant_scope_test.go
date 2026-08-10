package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// A worker holds one store, opened as one tenant, and every write in
// executeWorkflow goes through it. The engine is separately told
// WithTenantID(wf.TenantID). These tests are the only thing comparing the two.
//
// The check cannot fire today, because the claim only returns rows for the
// store's own tenant. That is exactly why it is worth testing now: the
// restriction is load-bearing in a way nothing records, and the first change
// that widens the claim would otherwise write one tenant's history under
// another's ID with RLS satisfied at every step.

type tenantScopeProbe struct {
	mu      sync.Mutex
	traced  bool
	failed  bool
	failMsg string
	failCod string
	failOp  string
}

// probedMockStore is a mockStore whose trace and fail calls land in probe.
func probedMockStore(probe *tenantScopeProbe) *mockStore {
	ms := &mockStore{}
	ms.traceWorkflowFn = func(_ context.Context, _, _ string) error {
		probe.mu.Lock()
		defer probe.mu.Unlock()
		probe.traced = true
		return nil
	}
	ms.failWorkflowFn = func(_ context.Context, _, _ string, _ int64, errMsg, errCode, errOp string, _ map[string]string) error {
		probe.mu.Lock()
		defer probe.mu.Unlock()
		probe.failed = true
		probe.failMsg = errMsg
		probe.failCod = errCode
		probe.failOp = errOp
		return nil
	}
	return ms
}

func newTenantScopeWorker(t *testing.T, storeTenant string, probe *tenantScopeProbe) *Worker {
	t.Helper()
	w := newTestWorker(probedMockStore(probe))
	w.storeTenantID = storeTenant
	return w
}

func runExecuteWorkflow(w *Worker, wf *engine.WorkflowInstance) {
	w.wg.Add(1)
	w.executeWorkflow(wf)
	w.wg.Wait()
}

// TestExecuteWorkflow_RefusesAnotherTenantsWorkflow.
//
// The assertion that matters most is not that it failed -- it is that
// TraceWorkflow was never called. That is the first store write in
// executeWorkflow, so proving it did not happen proves the check sits ahead of
// every write rather than somewhere in the middle, which would leave a
// half-written workflow under the wrong tenant.
func TestExecuteWorkflow_RefusesAnotherTenantsWorkflow(t *testing.T) {
	probe := &tenantScopeProbe{}
	w := newTenantScopeWorker(t, "00000000-0000-0000-0000-000000000000", probe)
	defer w.cancel()

	runExecuteWorkflow(w, &engine.WorkflowInstance{
		ID:       "wf-other-tenant",
		DefName:  "test-workflow",
		TenantID: "11111111-1111-1111-1111-111111111111",
	})

	probe.mu.Lock()
	defer probe.mu.Unlock()

	if probe.traced {
		t.Error("TraceWorkflow ran for another tenant's workflow; the scope check is not ahead of the first store write")
	}
	if !probe.failed {
		t.Fatal("another tenant's workflow was neither refused nor failed")
	}
	if probe.failOp != "tenant_scope" {
		t.Errorf("errorOp = %q, want tenant_scope", probe.failOp)
	}
	if probe.failCod != engine.ErrPermanent.String() {
		t.Errorf("errorCode = %q, want %s -- no worker exists that could run it later",
			probe.failCod, engine.ErrPermanent.String())
	}
	// Both tenants have to appear, or the operator cannot tell which worker
	// refused which workflow.
	for _, want := range []string{"11111111-1111-1111-1111-111111111111", "00000000-0000-0000-0000-000000000000"} {
		if !strings.Contains(probe.failMsg, want) {
			t.Errorf("failure message does not name tenant %s: %s", want, probe.failMsg)
		}
	}
}

// TestExecuteWorkflow_AdmitsItsOwnTenant is the false-positive half. Without
// it, a check that refused everything would pass the test above.
func TestExecuteWorkflow_AdmitsItsOwnTenant(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tenantID string
	}{
		{"same tenant", "00000000-0000-0000-0000-000000000000"},
		// Rows written before tenant_id existed, and every test fixture that
		// does not set one. Refusing these would stop the worker dead.
		{"unset tenant", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &tenantScopeProbe{}
			w := newTenantScopeWorker(t, "00000000-0000-0000-0000-000000000000", probe)
			defer w.cancel()

			runExecuteWorkflow(w, &engine.WorkflowInstance{
				ID:       "wf-own-tenant",
				DefName:  "test-workflow",
				TenantID: tc.tenantID,
			})

			probe.mu.Lock()
			defer probe.mu.Unlock()

			// It gets past the check and reaches the first write. What happens
			// after that (no WASM is registered in this harness) is not what
			// this test is about.
			if !probe.traced {
				t.Error("the scope check refused a workflow this worker owns")
			}
			if probe.failed && strings.Contains(probe.failMsg, "tenant") {
				t.Errorf("failed on tenant grounds: %s", probe.failMsg)
			}
		})
	}
}
