//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// §3.94 step 5b: the epoch fence, resolved per tenant.
//
// Three tests, because there are three separate things that can be wrong and a
// green run on any one of them is consistent with the other two being broken:
//
//  1. the engine reads the tenant's value and hands it to the backend;
//  2. the backend clamps it to the operator's ceiling rather than substituting;
//  3. the clamped value actually fences a running guest.
//
// 3.94's "what to be suspicious of" names the gap between 1 and 3 exactly -- "a
// settings row that is written, read, and then ignored because the value reaches
// the executor but not the backend". This is the item where that gap is widest,
// because the instance timeout is an epoch deadline set deep inside
// configureStore rather than a context deadline the executor applies itself.

// TestTheEngineHandsTheTenantsInstanceTimeoutToTheBackend is (1): delivery.
//
// It asserts through executeWithBackend rather than by calling the resolver,
// because calling the resolver would only prove the resolver returns what the
// fake store was given. What must hold is that the value survives the trip to
// PerExecution, which is the seam this step exists to open.
func TestTheEngineHandsTheTenantsInstanceTimeoutToTheBackend(t *testing.T) {
	const tenantTimeout = 7 * time.Second

	backend := &configurableMockBackend{}
	e := NewEngine(nil, nil,
		WithBackend("go", backend),
		WithWorkflowStore(&fakeSettingsStore{
			settings: TenantSettings{WasmInstanceTimeout: tenantTimeout},
		}),
	)
	e.workflowID = "wf-tenant-instance-timeout"
	e.defName = "test-def"

	if _, _, _, _, _, err := e.executeWithBackend(
		context.Background(), backend, minimalWasm(), "test_entry", []byte(`{}`), nil,
	); err != nil {
		t.Fatalf("executeWithBackend: %v", err)
	}

	if backend.gotInstanceTimeout != tenantTimeout {
		t.Fatalf("the backend was handed %v, want the tenant's %v.\n\n"+
			"The settings row is read but does not reach PerExecution, so the "+
			"epoch fence still runs every tenant at the operator's value. That "+
			"is 3.94's 'reaches the executor but not the backend' failure.",
			backend.gotInstanceTimeout, tenantTimeout)
	}
}

// TestATenantCanTightenTheInstanceTimeoutButNotWidenIt is (2): the clamp.
//
// Asserted on the per-execution backend's own limits rather than through a run,
// so every direction is covered in microseconds instead of one slow case each.
// The widening row is the one that matters: a tenant that could raise this
// bound could hold a worker's CPU for as long as it liked, and
// --wasm-instance-timeout would be advisory.
func TestATenantCanTightenTheInstanceTimeoutButNotWidenIt(t *testing.T) {
	ctx := context.Background()
	const operator = 30 * time.Second

	b, err := NewWasmtimeBackend(ctx, WithWasmtimeExecutionTimeout(operator))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	defer b.Close(ctx)

	cases := []struct {
		name   string
		tenant time.Duration
		want   time.Duration
	}{
		{"tenant tightens it", 5 * time.Second, 5 * time.Second},
		{"tenant tries to widen it", 10 * time.Minute, operator},
		{"tenant sets nothing", 0, operator},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			per, ok := b.PerExecution(tc.tenant).(*wasmtimeBackend)
			if !ok {
				t.Fatalf("PerExecution returned %T, want *wasmtimeBackend", b.PerExecution(tc.tenant))
			}
			if got := per.limits.executionTimeout; got != tc.want {
				t.Fatalf("executionTimeout = %v, want %v (operator %v, tenant %v)",
					got, tc.want, operator, tc.tenant)
			}
			// The root backend must be untouched: PerExecution copies, and a
			// tenant whose value leaked back onto the shared backend would
			// apply to every other tenant on this worker.
			if b.limits.executionTimeout != operator {
				t.Fatalf("the root backend's executionTimeout changed to %v; "+
					"a per-execution clone must not write back to the shared "+
					"backend, or one tenant's setting becomes everyone's",
					b.limits.executionTimeout)
			}
		})
	}
}

// TestATenantsTighterInstanceTimeoutActuallyFencesTheGuest is (3), and it is
// the one that cannot pass for the wrong reason.
//
// The operator grants 30s; the tenant asks for 200ms; the guest is the
// hand-written infinite loop that never calls back into the host, so nothing
// but the epoch fence can stop it. If the tenant's value did not reach
// configureStore this would run for 30 seconds instead of well under one, so
// the elapsed time is the assertion rather than a comment about it.
func TestATenantsTighterInstanceTimeoutActuallyFencesTheGuest(t *testing.T) {
	ctx := context.Background()
	const operator = 30 * time.Second
	const tenant = 200 * time.Millisecond

	b, err := NewWasmtimeBackend(ctx, WithWasmtimeExecutionTimeout(operator))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	defer b.Close(ctx)

	wasmBytes := mustWat2Wasm(t, infiniteLoopGoStartWat)

	type outcome struct {
		err error
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		_, err := b.PerExecution(tenant).Execute(
			ctx, wasmBytes, "_start", json.RawMessage(`{}`), &mockHostHandler{})
		done <- outcome{err}
	}()

	select {
	case o := <-done:
		elapsed := time.Since(start)
		if o.err == nil {
			t.Fatal("an infinite loop returned no error; the fence did not fire at all")
		}
		if !strings.Contains(o.err.Error(), "execution time limit exceeded") {
			t.Fatalf("error %q does not name the time limit", o.err)
		}
		// The discriminator. Generous enough not to be a race on a loaded
		// machine -- the two candidate bounds are 200ms and 30s, so anything
		// under a few seconds can only be the tenant's.
		if elapsed > 5*time.Second {
			t.Fatalf("the guest ran for %s against a 200ms tenant bound and a "+
				"30s operator bound.\n\n"+
				"It was fenced at the OPERATOR's value, so the tenant's "+
				"instance timeout never reached configureStore -- the settings "+
				"row is being read and discarded.", elapsed)
		}
		t.Logf("fenced after %s (tenant %s, operator %s)", elapsed, tenant, operator)
	case <-time.After(20 * time.Second):
		t.Fatal("the guest was never interrupted; the epoch fence is not running")
	}
}
