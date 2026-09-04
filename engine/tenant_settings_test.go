package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// fakeSettingsStore is a WorkflowStore that answers GetTenantSettings and
// nothing else.
//
// The embedded interface is nil deliberately. It satisfies WorkflowStore's 99
// methods at compile time, and panics if the engine ever calls one of them on
// this path -- which is the behaviour wanted, not a hazard: these tests claim
// that reading tenant settings is all the executor does with the store here,
// and a nil-pointer panic naming the method is how that claim fails if it
// stops being true.
type fakeSettingsStore struct {
	WorkflowStore
	settings TenantSettings
	err      error
	calls    int
}

func (f *fakeSettingsStore) GetTenantSettings(ctx context.Context) (TenantSettings, error) {
	f.calls++
	return f.settings, f.err
}

// ---------------------------------------------------------------------------
// ClampToCeiling
// ---------------------------------------------------------------------------

func TestATenantCanOnlyTightenALimitNeverRaiseIt(t *testing.T) {
	const s = time.Second

	cases := []struct {
		name    string
		tenant  time.Duration
		ceiling time.Duration
		want    time.Duration
		why     string
	}{
		{
			name:   "no override falls back to the operator's value",
			tenant: 0, ceiling: 30 * s, want: 30 * s,
			why: "the common case: a tenant with no settings row at all",
		},
		{
			name:   "a tighter tenant value is honoured",
			tenant: 5 * s, ceiling: 30 * s, want: 5 * s,
			why: "the feature: a tenant lowering its own bound",
		},
		{
			name:   "a looser tenant value is clamped to the operator's",
			tenant: 300 * s, ceiling: 30 * s, want: 30 * s,
			why: "THE case this function exists for. Without the clamp any tenant " +
				"on a shared deployment raises its own bounds past what the " +
				"operator granted, and per-tenant settings become a hole rather " +
				"than a feature",
		},
		{
			name:   "equal values resolve to that value",
			tenant: 30 * s, ceiling: 30 * s, want: 30 * s,
			why: "the boundary between the two cases above",
		},
		{
			name:   "an unbounded operator still lets the tenant tighten",
			tenant: 5 * s, ceiling: 0, want: 5 * s,
			why: "zero means unbounded at the point of use, and tightening from " +
				"unbounded is still tightening",
		},
		{
			name:   "unbounded on both sides stays unbounded",
			tenant: 0, ceiling: 0, want: 0,
			why: "nothing to resolve",
		},
		{
			name:   "a negative tenant value is treated as absent, not as unbounded",
			tenant: -1 * s, ceiling: 30 * s, want: 30 * s,
			why: "a negative duration reaching here means the CHECK constraints " +
				"were bypassed. Falling back to the operator's value is the safe " +
				"direction; returning the negative would make `ceiling > 0` false " +
				"at the call site and remove the bound entirely",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClampToCeiling(tc.tenant, tc.ceiling)
			if got != tc.want {
				t.Errorf("ClampToCeiling(%v, %v) = %v, want %v\n\n%s",
					tc.tenant, tc.ceiling, got, tc.want, tc.why)
			}
		})
	}
}

func TestANonPositiveStoredValueIsDroppedRatherThanCarried(t *testing.T) {
	ms := func(v int64) *int64 { return &v }

	zero := int64(0)
	neg := int64(-1)
	got := tenantSettingsFromMillis(&zero, ms(5000), &neg)

	if got.WasmInstanceTimeout != 0 {
		t.Errorf("a stored 0 became %v, want 0 (absent)\n\n"+
			"Zero means UNBOUNDED at the point of use, so carrying it through "+
			"would let a tenant remove its own limit -- the one direction "+
			"ClampToCeiling forbids. The CHECK constraints refuse it in the "+
			"database; this is the second layer.", got.WasmInstanceTimeout)
	}
	if got.HostRetryBudget != 0 {
		t.Errorf("a stored -1 became %v, want 0 (absent)", got.HostRetryBudget)
	}
	if got.WasmWallClockCeiling != 5*time.Second {
		t.Errorf("a valid neighbour was disturbed: got %v, want 5s",
			got.WasmWallClockCeiling)
	}
}

func TestNoSettingsRowIsNotAnError(t *testing.T) {
	// tenantSettingsFromMillis is what every dialect's read path returns, and
	// all three map "no row" to the zero value rather than to an error. A
	// tenant that has never set an override is the common case; making it an
	// error would fail every workflow on a deployment nobody had configured.
	got := tenantSettingsFromMillis(nil, nil, nil)
	if got != (TenantSettings{}) {
		t.Errorf("three NULL columns resolved to %+v, want the zero value", got)
	}
}

// ---------------------------------------------------------------------------
// Through the engine: two tenants whose values differ
// ---------------------------------------------------------------------------

// deadlineSeenBy runs one execution and reports the deadline the backend was
// handed -- i.e. what the guest would actually be bounded by.
//
// This goes through executeWithBackend rather than calling wallClockCeiling
// directly, because the failure 3.94 warns about is precisely a value that is
// read correctly and then not reaching the place that uses it. 3.90 is the
// precedent: the timeout was applied in two places and fixing one changed
// nothing observable.
func deadlineSeenBy(t *testing.T, opts []EngineOption, s TenantSettings) time.Duration {
	t.Helper()

	var remaining time.Duration
	var haveDeadline bool
	backend := &configurableMockBackend{
		executeFn: func(ctx context.Context, _ []byte, _ string, _ json.RawMessage, _ HostHandler) (*ExecResult, error) {
			if d, ok := ctx.Deadline(); ok {
				haveDeadline, remaining = true, time.Until(d)
			}
			return &ExecResult{Result: `{"ok":true}`}, nil
		},
	}

	e := NewEngine(nil, nil, append(opts,
		WithBackend("go", backend),
		WithWorkflowStore(&fakeSettingsStore{settings: s}),
	)...)
	e.workflowID = "wf-tenant-settings"
	e.defName = "test-def"

	if _, _, _, _, _, err := e.executeWithBackend(
		context.Background(), backend, minimalWasm(), "test_entry", []byte(`{}`), nil,
	); err != nil {
		t.Fatalf("execution failed: %v", err)
	}
	if !haveDeadline {
		t.Fatal("the backend was handed a context with no deadline at all, so " +
			"this test cannot measure anything. Something removed the wall-clock " +
			"ceiling from executor.go rather than changing its value.")
	}
	return remaining
}

func TestTwoTenantsGetDifferentWallClockCeilings(t *testing.T) {
	// One operator flag, well clear of any scheduling noise. The assertions
	// below are on buckets hundreds of seconds apart, so this test has no
	// wall-clock race in it -- deliberately, after one shipped in #609 and
	// failed CI on an unrelated PR.
	flag := []EngineOption{WithWasmWallClockCeiling(300 * time.Second)}

	noOverride := deadlineSeenBy(t, flag, TenantSettings{})
	tighter := deadlineSeenBy(t, flag, TenantSettings{WasmWallClockCeiling: 5 * time.Second})
	looser := deadlineSeenBy(t, flag, TenantSettings{WasmWallClockCeiling: 3000 * time.Second})

	if noOverride < 200*time.Second {
		t.Errorf("a tenant with no override got %v, expected the operator's 300s.\n\n"+
			"The settings read is returning something for a tenant that set nothing.",
			noOverride)
	}

	if tighter > 60*time.Second {
		t.Errorf("a tenant that set 5s got %v.\n\n"+
			"This is the whole feature: the per-tenant value was read but did not "+
			"reach the context deadline the backend runs under. Check that "+
			"executor.go still calls e.wallClockCeiling(execCtx) at BOTH sites -- "+
			"3.90's failure was exactly one of two sites being fixed.", tighter)
	}

	if tighter >= noOverride {
		t.Errorf("the tenant that tightened (%v) did not end up below the tenant "+
			"that set nothing (%v).\n\n"+
			"Asserting the DIFFERENCE, not that one tenant's value is honoured: a "+
			"bug that returned the same number for every tenant would satisfy the "+
			"absolute bounds above and fail here.", tighter, noOverride)
	}

	if looser < 200*time.Second {
		t.Errorf("a tenant that asked for 3000s got %v, expected it clamped to the "+
			"operator's 300s", looser)
	}
	if looser > 400*time.Second {
		t.Errorf("a tenant that asked for 3000s got %v -- it was granted MORE than "+
			"the operator's 300s flag.\n\n"+
			"This is the escalation case. On a shared deployment it means any "+
			"tenant can hold a worker for as long as it likes by writing its own "+
			"settings row, which is worse than having no per-tenant settings at "+
			"all. See ClampToCeiling.", looser)
	}
}

// TestTheThreeSettingsBoundDifferentThings replaces
// TestOnlyTheWallClockCeilingIsWiredYet, which described a state that no longer
// exists: all three settings are wired now, the instance timeout by step 5b and
// the retry budget by step 4.
//
// That test also did not do what it said. Its comment promised it would "fail
// the day either one starts having an effect", but it only ever measured the
// WALL-CLOCK deadline -- and neither of the other two settings bounds wall
// clock, so both shipped and it stayed green. A test whose stated trigger it
// cannot detect is worse than no test, because the next reader trusts it.
//
// What is worth keeping is the invariant underneath: the three settings bound
// three DIFFERENT things and must not leak into each other's bound. That is
// IMPROVEMENT-PLAN 3.90's whole finding -- wasmInstanceTimeout was once applied
// as a context deadline as well as an epoch fence, so a workflow waiting on a
// slow service died of "execution timed out" and the epoch fence's exclusion of
// host wait was unobservable. This asserts the separation directly.
func TestTheThreeSettingsBoundDifferentThings(t *testing.T) {
	flag := []EngineOption{WithWasmWallClockCeiling(300 * time.Second)}

	base := deadlineSeenBy(t, flag, TenantSettings{})
	withOthers := deadlineSeenBy(t, flag, TenantSettings{
		WasmInstanceTimeout: 1 * time.Second,
		HostRetryBudget:     1 * time.Second,
	})

	if withOthers < 200*time.Second {
		t.Fatalf("setting WasmInstanceTimeout and HostRetryBudget moved the "+
			"wall-clock deadline to %v (it is %v when neither is set).\n\n"+
			"They must not. The instance timeout is an EPOCH fence bounding "+
			"guest execution, and the retry budget bounds one host call's "+
			"backoff; neither bounds wall clock. A value leaking into the "+
			"context deadline is the conflation IMPROVEMENT-PLAN 3.90 removed, "+
			"and it makes the epoch fence's exclusion of host wait "+
			"unobservable again.",
			withOthers, base)
	}
}

func TestAFailedSettingsReadFallsBackToTheFlagsRatherThanFailingTheWorkflow(t *testing.T) {
	backend := &configurableMockBackend{}
	store := &fakeSettingsStore{err: errors.New("settings table is unreachable")}

	e := NewEngine(nil, nil,
		WithBackend("go", backend),
		WithWorkflowStore(store),
		WithWasmWallClockCeiling(300*time.Second),
	)
	e.workflowID = "wf-settings-read-fails"
	e.defName = "test-def"

	if _, _, _, _, _, err := e.executeWithBackend(
		context.Background(), backend, minimalWasm(), "test_entry", []byte(`{}`), nil,
	); err != nil {
		t.Fatalf("a settings read failure failed the workflow: %v\n\n"+
			"It must not. Treating an unreadable settings row as fatal lets one "+
			"bad table take down every execution on the deployment, and the "+
			"fallback direction is safe: the operator's own limits, never wider.",
			err)
	}
	if store.calls == 0 {
		t.Fatal("the store was never asked for settings, so this test proved " +
			"nothing about what happens when the read fails")
	}
}

func TestTheSettingsReadIsMemoisedAcrossBothCeilingCallSites(t *testing.T) {
	backend := &configurableMockBackend{}
	store := &fakeSettingsStore{settings: TenantSettings{WasmWallClockCeiling: 5 * time.Second}}

	e := NewEngine(nil, nil,
		WithBackend("go", backend),
		WithWorkflowStore(store),
		WithWasmWallClockCeiling(300*time.Second),
	)
	e.workflowID = "wf-settings-memoised"
	e.defName = "test-def"

	for i := 0; i < 3; i++ {
		if _, _, _, _, _, err := e.executeWithBackend(
			context.Background(), backend, minimalWasm(), "test_entry", []byte(`{}`), nil,
		); err != nil {
			t.Fatalf("execution %d failed: %v", i, err)
		}
	}

	if store.calls != 1 {
		t.Errorf("the settings row was read %d times, want 1.\n\n"+
			"This read sits on the dispatch path, so an unmemoised version is a "+
			"database round trip per workflow execution.", store.calls)
	}
}
