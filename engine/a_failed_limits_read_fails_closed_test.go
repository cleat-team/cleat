package engine

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// flakyLimitsStore answers both limit tiers, failing the first failFirst reads
// of each with err. Each tier counts its own reads.
//
// tenantHealthy and runHealthy make one tier answer every read, so a failure
// under test is the other tier's alone: with both failing, whichever is read
// second can supply the expected error when the first has been broken, and the
// test certifies a tier it never isolated. onTenantRead runs
// on each tenant read with its 1-based number, before the answer is chosen.
type flakyLimitsStore struct {
	WorkflowStore
	tenant, run           TenantSettings
	failFirst             int
	err                   error
	tenantHealthy         bool
	runHealthy            bool
	onTenantRead          func(n int)
	tenantReads, runReads int
}

func (f *flakyLimitsStore) GetTenantSettings(ctx context.Context) (TenantSettings, error) {
	f.tenantReads++
	if f.onTenantRead != nil {
		f.onTenantRead(f.tenantReads)
	}
	if !f.tenantHealthy && (f.failFirst < 0 || f.tenantReads <= f.failFirst) {
		return TenantSettings{}, f.err
	}
	return f.tenant, nil
}

func (f *flakyLimitsStore) GetRunLimits(ctx context.Context, _ string) (TenantSettings, error) {
	f.runReads++
	if !f.runHealthy && (f.failFirst < 0 || f.runReads <= f.failFirst) {
		return TenantSettings{}, f.err
	}
	return f.run, nil
}

// alwaysFail is failFirst for "every read fails".
const alwaysFail = -1

// limitsEngine builds an engine over a flakyLimitsStore whose backend counts
// the guest runs it was asked for.
func limitsEngine(t *testing.T, store *flakyLimitsStore) (*Engine, *configurableMockBackend, *atomic.Int32) {
	t.Helper()
	// The retry pause is real time otherwise, which would make every case here
	// wait on it.
	old := limitsReadBackoff
	limitsReadBackoff = time.Millisecond
	t.Cleanup(func() { limitsReadBackoff = old })

	var ran atomic.Int32
	backend := &configurableMockBackend{
		executeFn: func(context.Context, []byte, string, json.RawMessage, HostHandler) (*ExecResult, error) {
			ran.Add(1)
			return &ExecResult{Result: `{"ok":true}`}, nil
		},
	}
	e := NewEngine(nil, nil,
		WithBackend("go", backend),
		WithWorkflowStore(store),
		WithWasmWallClockCeiling(300*time.Second),
	)
	e.workflowID = "wf-limits-read"
	e.defName = "test-def"
	return e, backend, &ran
}

func run(e *Engine, ctx context.Context, b *configurableMockBackend) error {
	_, _, _, _, _, err := e.executeWithBackend(ctx, b, minimalWasm(), "test_entry", []byte(`{}`), nil)
	return err
}

// TestATransientFailureReadingTheTenantSettingsDoesNotRelaxATighterLimit is
// cleat#3565. A tenant whose instance timeout is tighter than the operator's
// (2s against a 30s default) lost it for the whole execution when ONE read
// failed: the failure resolved to "no override" and was memoised, so the guest
// ran under the wider bound. The read is retried, and the tenant's own value is
// what the backend is handed.
func TestATransientFailureReadingTheTenantSettingsDoesNotRelaxATighterLimit(t *testing.T) {
	store := &flakyLimitsStore{
		tenant:    TenantSettings{WasmInstanceTimeout: 2 * time.Second},
		failFirst: 1,
		err:       errors.New("get tenant settings: driver: bad connection"),
	}
	e, backend, _ := limitsEngine(t, store)

	if err := run(e, context.Background(), backend); err != nil {
		t.Fatalf("a single transient settings read failure failed the execution: %v", err)
	}
	if backend.gotInstanceTimeout != 2*time.Second {
		t.Fatalf("the backend was handed an instance timeout of %v, want the tenant's 2s.\n\n"+
			"A failed read was treated as 'no override' for the whole execution, which "+
			"gives a tenant with a TIGHTER limit the operator's wider one.",
			backend.gotInstanceTimeout)
	}
	if store.tenantReads != 2 {
		t.Errorf("the tenant settings were read %d times, want 2 (one failure, one retry)", store.tenantReads)
	}
}

// The same, for the run tier: a run's own override is tighter than its
// tenant's, and an unreadable row resolved to the tier above.
func TestATransientFailureReadingTheRunLimitsDoesNotRelaxATighterLimit(t *testing.T) {
	store := &flakyLimitsStore{
		tenant:    TenantSettings{WasmInstanceTimeout: 10 * time.Second},
		run:       TenantSettings{WasmInstanceTimeout: time.Second},
		failFirst: 1,
		err:       errors.New("run limits: driver: bad connection"),
	}
	e, backend, _ := limitsEngine(t, store)

	if err := run(e, context.Background(), backend); err != nil {
		t.Fatalf("a single transient run-limits read failure failed the execution: %v", err)
	}
	if backend.gotInstanceTimeout != time.Second {
		t.Fatalf("the backend was handed an instance timeout of %v, want the run's 1s", backend.gotInstanceTimeout)
	}
}

// TestAPersistentFailureReadingTheLimitsFailsTheExecutionBeforeTheGuestRuns
// replaces TestAFailedSettingsReadFallsBackToTheFlagsRatherThanFailingTheWorkflow,
// which pinned the opposite on the argument that one bad table must not take
// down every execution. The fallback it protected is wider than the limit for
// exactly the tenants that set one, and "never wider" is false for them.
//
// What it cost: an execution that cannot read its limits does not run. The
// error is ErrLimitsUnavailable, which the worker turns into a release rather
// than a terminal failure, so the run waits and nothing is destroyed.
func TestAPersistentFailureReadingTheLimitsFailsTheExecutionBeforeTheGuestRuns(t *testing.T) {
	for _, tier := range []string{"tenant", "run"} {
		t.Run(tier, func(t *testing.T) {
			store := &flakyLimitsStore{
				tenant:        TenantSettings{WasmInstanceTimeout: 2 * time.Second},
				run:           TenantSettings{WasmInstanceTimeout: time.Second},
				failFirst:     alwaysFail,
				err:           errors.New("driver: bad connection"),
				tenantHealthy: tier == "run",    // the run tier alone fails in the "run" case
				runHealthy:    tier == "tenant", // and the tenant tier alone in the "tenant" case
			}
			e, backend, ran := limitsEngine(t, store)

			err := run(e, context.Background(), backend)
			if !errors.Is(err, ErrLimitsUnavailable) {
				t.Fatalf("err = %v, want ErrLimitsUnavailable.\n\nAn execution that cannot read "+
					"its limits must not run under the operator's, which may be wider.", err)
			}
			if n := ran.Load(); n != 0 {
				t.Fatalf("the guest ran %d time(s) with no readable limits", n)
			}
			reads := store.tenantReads
			if tier == "run" {
				reads = store.runReads
			}
			if reads != limitsReadAttempts {
				t.Errorf("the %s tier was read %d time(s), want %d: a failure must be retried before it is final",
					tier, reads, limitsReadAttempts)
			}
		})
	}
}

// TestAFailedLimitsReadIsNotRememberedAsTheAnswer is the other half of the
// memoisation: only a SUCCESSFUL read is kept. A failure that is remembered
// turns one dropped connection into a bound that lasts the whole execution.
func TestAFailedLimitsReadIsNotRememberedAsTheAnswer(t *testing.T) {
	store := &flakyLimitsStore{
		tenant:    TenantSettings{WasmInstanceTimeout: 2 * time.Second},
		failFirst: alwaysFail,
		err:       errors.New("driver: bad connection"),
	}
	e, backend, _ := limitsEngine(t, store)

	if err := run(e, context.Background(), backend); !errors.Is(err, ErrLimitsUnavailable) {
		t.Fatalf("first run: err = %v, want ErrLimitsUnavailable", err)
	}

	store.failFirst = 0 // the database is back
	if err := run(e, context.Background(), backend); err != nil {
		t.Fatalf("second run, database healthy again: %v", err)
	}
	if backend.gotInstanceTimeout != 2*time.Second {
		t.Fatalf("after recovery the backend was handed %v, want the tenant's 2s: the earlier "+
			"failure was remembered", backend.gotInstanceTimeout)
	}

	before := store.tenantReads
	if err := run(e, context.Background(), backend); err != nil {
		t.Fatalf("third run: %v", err)
	}
	if store.tenantReads != before {
		t.Errorf("a successful read was not kept: %d more read(s) on the third run", store.tenantReads-before)
	}
}

// TestACancelledContextIsNotReportedAsUnreadableLimits: a fence or a shutdown
// cancels the context, the read fails with it, and that is the run being
// STOPPED. The worker tells a stopped run from a broken one by the context, so
// the engine must hand back the context's error rather than claim the limits
// were unreadable.
func TestACancelledContextIsNotReportedAsUnreadableLimits(t *testing.T) {
	store := &flakyLimitsStore{failFirst: alwaysFail, err: context.Canceled}
	e, backend, ran := limitsEngine(t, store)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := run(e, ctx, backend)
	if errors.Is(err, ErrLimitsUnavailable) {
		t.Fatalf("a cancelled context was reported as unreadable limits: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if ran.Load() != 0 {
		t.Fatal("the guest ran under a cancelled context")
	}
	if store.tenantReads > 1 {
		t.Errorf("a cancelled context was retried %d times", store.tenantReads)
	}
}

// TestACancellationOnTheLastAttemptIsStillAStop: the cancel arrives during the
// final try, after two plain failures. Reported as ErrLimitsUnavailable it would
// read as a broken database, when the run was fenced or the worker was stopping.
func TestACancellationOnTheLastAttemptIsStillAStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &flakyLimitsStore{
		failFirst: alwaysFail,
		err:       errors.New("driver: bad connection"),
	}
	store.onTenantRead = func(n int) {
		if n == limitsReadAttempts {
			cancel()
			store.err = context.Canceled
		}
	}
	e, backend, ran := limitsEngine(t, store)

	err := run(e, ctx, backend)
	if errors.Is(err, ErrLimitsUnavailable) {
		t.Fatalf("a cancellation on the last attempt was reported as unreadable limits: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if ran.Load() != 0 {
		t.Fatal("the guest ran")
	}
}

// TestAnAbsentLimitsRowIsAnEmptyReadNotAnError is the other side of failing
// closed, on every real dialect: a tenant with no settings row, and a run with
// no row of its own, are the ordinary case, and a store that reported either as
// an error would now refuse every execution on it. The fake stores above cannot
// say anything about that, because they decide for themselves what an absent
// row looks like.
func TestAnAbsentLimitsRowIsAnEmptyReadNotAnError(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			tr, ok := store.(TenantSettingsReader)
			if !ok {
				t.Fatalf("%T is not a TenantSettingsReader: every tenant on it would silently get the flags", store)
			}
			if s, err := tr.GetTenantSettings(ctx); err != nil || s != (TenantSettings{}) {
				t.Fatalf("a tenant with no settings row read as %+v, err=%v; want an empty read and no error", s, err)
			}
			rr, ok := store.(RunLimitsReader)
			if !ok {
				t.Fatalf("%T is not a RunLimitsReader", store)
			}
			if s, err := rr.GetRunLimits(ctx, "no-such-run-"+backend.Name()); err != nil || s != (TenantSettings{}) {
				t.Fatalf("a run with no row read as %+v, err=%v; want an empty read and no error", s, err)
			}

			// And through the engine's own preflight, which is what an execution calls.
			e := NewEngine(nil, nil, WithWorkflowStore(store))
			e.workflowID = "no-such-run-" + backend.Name()
			if err := e.ensureLimits(ctx); err != nil {
				t.Fatalf("ensureLimits on %s with no limits rows: %v", backend.Name(), err)
			}
		})
	}
}
