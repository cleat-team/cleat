package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// The WASM load is a read like any other in executeWorkflow, and until
// cleat#1931 it was the one read that did not go through the workflow's own
// tenant store.
//
// # Why the isolation tests we already have could not see this
//
// cleat-ports' tenant-isolation suite asserts that tenant A cannot READ
// tenant B's run. Those assertions pass whether or not B's run ever executes,
// so all three of them were green on the run that produced this bug: the
// failing signal came from the worker log, not from a test. A test that
// wants to see this has to assert the run got its module, which is what the
// first test below does.
//
// # And why a second test, with the same definition name in both tenants
//
// The two caches in front of the store were keyed on (name, version) with no
// tenant. Fixing the routing alone therefore does not fix the bug, it
// converts it: a colliding name would be served out of the cache before the
// store was consulted at all, and the wrong tenant's code would run without
// an error anywhere. Names are caller-chosen, so the collision is ordinary.

// wasmProbeStore records what a store was asked for, and answers only for the
// definitions it was given.
type wasmProbeStore struct {
	*mockStore
	tenant string
	defs   map[string][]byte
	asked  []string
}

func newWasmProbeStore(tenant string, defs map[string][]byte) *wasmProbeStore {
	ps := &wasmProbeStore{mockStore: &mockStore{}, tenant: tenant, defs: defs}
	ps.mockStore.loadWASMFn = func(_ context.Context, name string, version int) ([]byte, error) {
		ps.asked = append(ps.asked, name)
		b, ok := ps.defs[name]
		if !ok {
			// Byte-for-byte the shape PostgresStore.LoadWASM returns when RLS
			// filters the row out, which is what the worker actually saw.
			return nil, errors.New("wasm not found: " + name + " v1")
		}
		return b, nil
	}
	ps.mockStore.getWASMLengthFn = func(_ context.Context, name string, _ int) (int64, error) {
		b, ok := ps.defs[name]
		if !ok {
			return 0, errors.New("wasm not found: " + name + " v1")
		}
		return int64(len(b)), nil
	}
	return ps
}

// TestLoadWASM_ReadsThroughTheWorkflowsOwnTenantStore.
//
// The worker's own store does not hold the definition and the tenant's store
// does -- the arrangement the nightly hit, where tenant B had deployed
// isolation_leaf_b and the worker was tenant 0000...
func TestLoadWASM_ReadsThroughTheWorkflowsOwnTenantStore(t *testing.T) {
	workerStore := newWasmProbeStore(tenantSelf, map[string][]byte{})
	w := newTestWorker(workerStore.mockStore)
	defer w.cancel()
	w.storeTenantID = tenantSelf

	want := []byte("tenant B's module")
	tenantStore := newWasmProbeStore(tenantOther, map[string][]byte{"isolation_leaf_b": want})
	w.storeFactory = &recordingFactory{store: tenantStore.mockStore}

	execStore, release, err := w.storeForTenant(tenantOther)
	if err != nil {
		t.Fatalf("storeForTenant: %v", err)
	}
	defer release()

	got, err := w.loadWASM(execStore, w.cacheTenantFor(tenantOther), "isolation_leaf_b", 1)
	if err != nil {
		t.Fatalf("loading another tenant's definition failed: %v. This is the "+
			"nightly failure: the run is claimed, the store is routed, and then "+
			"the module is read as the wrong tenant.", err)
	}
	if string(got) != string(want) {
		t.Errorf("loaded %q, want %q", got, want)
	}
	if len(workerStore.asked) != 0 {
		t.Errorf("the worker's OWN store was asked for %v; a tenant's definition "+
			"must not be read through a store scoped to someone else", workerStore.asked)
	}
	// The control. Without it this test passes against a loadWASM that reads
	// nothing and returns the bytes from somewhere else entirely.
	if len(tenantStore.asked) == 0 {
		t.Fatal("the tenant's store was never asked, so nothing above was measured")
	}
}

// TestLoadWASM_DoesNotServeOneTenantsModuleToAnother covers both cache layers,
// which is why the disk cache is configured here rather than left nil.
func TestLoadWASM_DoesNotServeOneTenantsModuleToAnother(t *testing.T) {
	const shared = "orders"
	// THE TWO MODULES ARE THE SAME LENGTH, and that is the whole fixture.
	// loadWASM re-checks GetWASMLength on an in-memory hit and reloads when it
	// disagrees, so two modules of different sizes are told apart by the
	// staleness check no matter what the key says. Written that way first,
	// this test passed against a cache key with no tenant in it -- it was
	// measuring the length check, not the key. Equal lengths remove that
	// second mechanism and leave only the one under test. Two tenants whose
	// "orders" happen to be the same number of bytes is not a contrived case;
	// it is what a redeploy of a shared internal template looks like.
	aBytes := []byte("tenant A's orders module .....")
	bBytes := []byte("tenant B's orders module .....")
	if len(aBytes) != len(bBytes) {
		t.Fatalf("fixture is wrong: %d != %d bytes, so the staleness check "+
			"can distinguish them and the key is not what is being tested",
			len(aBytes), len(bBytes))
	}

	workerStore := newWasmProbeStore(tenantSelf, map[string][]byte{shared: aBytes})
	w := newTestWorker(workerStore.mockStore)
	defer w.cancel()
	w.storeTenantID = tenantSelf
	w.wasmDiskCache = engine.NewWasmDiskCache(t.TempDir(), 100)

	tenantStore := newWasmProbeStore(tenantOther, map[string][]byte{shared: bBytes})
	w.storeFactory = &recordingFactory{store: tenantStore.mockStore}

	// A first, so its bytes are the ones sitting in both caches under a key
	// that used to carry no tenant.
	gotA, err := w.loadWASM(w.store, w.cacheTenantFor(tenantSelf), shared, 1)
	if err != nil {
		t.Fatalf("tenant A cannot load its own module: %v", err)
	}
	if string(gotA) != string(aBytes) {
		t.Fatalf("tenant A loaded %q, want its own %q", gotA, aBytes)
	}

	execStore, release, err := w.storeForTenant(tenantOther)
	if err != nil {
		t.Fatalf("storeForTenant: %v", err)
	}
	defer release()

	gotB, err := w.loadWASM(execStore, w.cacheTenantFor(tenantOther), shared, 1)
	if err != nil {
		t.Fatalf("tenant B cannot load its own module: %v", err)
	}
	if string(gotB) != string(bBytes) {
		t.Errorf("tenant B ran %q, want %q. A cache keyed without the tenant "+
			"answers before the store is consulted, so this is not a failed "+
			"run -- it is one tenant executing another tenant's code.",
			gotB, bBytes)
	}

	// And the same question of the disk layer on its own, which a warm
	// in-memory cache would otherwise hide.
	if got := w.wasmDiskCache.LookupDef(tenantOther, shared, 1); string(got) != string(bBytes) {
		t.Errorf("the disk cache holds %q for tenant B, want %q", got, bBytes)
	}
	if got := w.wasmDiskCache.LookupDef(tenantSelf, shared, 1); string(got) != string(aBytes) {
		t.Errorf("the disk cache holds %q for tenant A, want %q", got, aBytes)
	}
}

// A workflow row with no tenant_id runs on the worker's own store, so its
// bytes must be cached under the worker's tenant. Keying on the row's "" would
// file every such worker's definitions under one shared name.
func TestLoadWASM_AnUnsetWorkflowTenantIsCachedUnderTheWorkersOwn(t *testing.T) {
	want := []byte("the worker's own module")
	workerStore := newWasmProbeStore(tenantSelf, map[string][]byte{"legacy": want})
	w := newTestWorker(workerStore.mockStore)
	defer w.cancel()
	w.storeTenantID = tenantSelf
	w.wasmDiskCache = engine.NewWasmDiskCache(t.TempDir(), 100)
	w.storeFactory = &recordingFactory{store: workerStore.mockStore}

	if got := w.cacheTenantFor(""); got != tenantSelf {
		t.Fatalf("cacheTenantFor(\"\") = %q, want the worker's own tenant %q -- "+
			"storeForTenant hands back the worker's store for an unset tenant, "+
			"so that is whose bytes these are", got, tenantSelf)
	}

	if _, err := w.loadWASM(w.store, w.cacheTenantFor(""), "legacy", 1); err != nil {
		t.Fatalf("loading an unset-tenant workflow's module failed: %v", err)
	}
	if got := w.wasmDiskCache.LookupDef(tenantSelf, "legacy", 1); string(got) != string(want) {
		t.Errorf("cached under %q as %q; want it filed under the worker's tenant", tenantSelf, got)
	}
	if got := w.wasmDiskCache.LookupDef("", "legacy", 1); got != nil {
		t.Errorf("cached under the empty tenant as well: %q", got)
	}
}

// The end-to-end shape: a full executeWorkflow for another tenant must not
// fail on the WASM read. This is the assertion the ports suite could not make.
func TestExecuteWorkflow_DoesNotFailAnotherTenantsRunOnTheWasmRead(t *testing.T) {
	probe := &tenantScopeProbe{}
	w := newTenantScopeWorker(t, tenantSelf, probe)
	defer w.cancel()

	tenantStore := newWasmProbeStore(tenantOther, map[string][]byte{"test-workflow": []byte("\x00asm")})
	tenantStore.mockStore.traceWorkflowFn = func(context.Context, string, string) error { return nil }
	tenantStore.mockStore.failWorkflowFn = func(_ context.Context, _, _ string, _ int64, errMsg, _, errOp string, _ map[string]string) error {
		probe.mu.Lock()
		defer probe.mu.Unlock()
		probe.failed, probe.failMsg, probe.failOp = true, errMsg, errOp
		return nil
	}
	w.storeFactory = &recordingFactory{store: tenantStore.mockStore}
	w.taskQueues = []string{"default"}

	runExecuteWorkflow(w, &engine.WorkflowInstance{
		ID: "wf-wasm-routed", DefName: "test-workflow", TenantID: tenantOther,
	})

	probe.mu.Lock()
	failed, failMsg := probe.failed, probe.failMsg
	probe.mu.Unlock()

	if failed && strings.Contains(failMsg, "wasm not found") {
		t.Errorf("another tenant's run was failed with %q. Its definition is "+
			"deployed; it was read as the wrong tenant.", failMsg)
	}
	// The control: the tenant's store must actually have been asked, or a
	// loadWASM that never ran would pass this too.
	if len(tenantStore.asked) == 0 {
		t.Fatal("the tenant's store was never asked for a module, so this test observed nothing")
	}
}
