package engine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// newCancellationTestStore builds a real PostgresStore against the configured
// test database. It deliberately does not go through registeredBackends: this
// suite needs the concrete *PostgresStore, and it needs the same store to act
// as both the workflow store and the engine's SignalStore, which is the pairing
// production uses and which every existing cancellation test mocks away.
func newCancellationTestStore(t *testing.T) (*PostgresStore, func()) {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	applyPostgresProcedures(t, db)
	testutil.CleanupPostgresTestData(t, db)
	store := NewPostgresStore(db)
	return store, func() {
		testutil.CleanupPostgresTestData(t, db)
		db.Close()
	}
}

// TestCancellationEndToEnd drives the whole cancellation path with nothing
// mocked: an operator requests cancellation through the same store method the
// worker's HTTP handler calls (cmd/cleat-worker/server.go:458), and a real
// workflow then executes on the real backend with that store wired in as the
// SignalStore.
//
// The assertion that matters is on the ServiceCaller, not on the result string.
// Cancellation exists to stop side effects; a test that only checks the engine
// *reported* cancellation would pass just as happily against an engine that
// reported it after making every call.
//
// This is not the first cancellation test: c26c332 added
// TestCancellationObservedEndToEnd (engine/host_dispatch_test.go:659), which is
// a good test and does key the store by workflow ID. But despite the name it is
// not end-to-end — it drives s.DurableCall directly against an in-memory
// keyedCancellationStore, with no database and no compiled module. Every
// cancellation test in the package shares that property: the workflow ID the
// engine passes is whatever the mock chooses to accept.
//
// That is what let the hardcoded "" survive. Here the ID has to match a row
// StartNewRun actually inserted, so an ID that does not resolve is a failure
// rather than a silent "not cancelled".
func TestCancellationEndToEnd(t *testing.T) {
	store, teardown := newCancellationTestStore(t)
	defer teardown()

	ctx := context.Background()
	setupTestData(t, store)

	runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
		json.RawMessage(`{}`), "cancel-e2e-1", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	const reason = "cancelled by operator"
	if err := store.RequestCancellation(ctx, runID, reason); err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}

	wasmPath := buildTestWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	backend, err := NewWasmtimeBackend(ctx)
	if err != nil {
		// Deliberately not a t.Skip, unlike the older tests in this package.
		// wasmtime is the backend of record and CGO is on by default, so it is
		// always available here — scripts/check-skips.sh case (c). Skipping
		// would mean a CGO_ENABLED=0 run reports this suite green without ever
		// exercising cancellation on the primary backend.
		t.Fatalf("wasmtime backend unavailable: %v (if this build disabled CGO, "+
			"that is the defect: it removes the primary backend entirely)", err)
	}
	defer backend.Close(ctx)

	caller := &mockCaller{}
	eng := NewEngine(rt, caller,
		WithBackend("go", backend),
		WithSignalStore(store),
		WithWorkflowID(runID),
	)

	input := []byte(`{"userID":"user-1","cart":[{"sku":"ABC-123","quantity":1}]}`)
	result, _, _, _, _, err := eng.Execute(ctx, wasmBytes, "place_order", input)
	if err != nil {
		t.Fatalf("Execute place_order: %v", err)
	}

	// The side-effect assertion. place_order makes seven durable calls when it
	// runs to completion; a cancelled workflow must make none.
	if len(caller.calls) != 0 {
		ops := make([]string, 0, len(caller.calls))
		for _, c := range caller.calls {
			ops = append(ops, c.Service+"."+c.Op)
		}
		t.Errorf("cancelled workflow performed %d side effects, want 0: %s",
			len(caller.calls), strings.Join(ops, ", "))
	}

	if !strings.Contains(result, "cancelled") {
		t.Errorf("result does not report cancellation: %q", result)
	}
}

// TestCancellationGuestAPIEndToEnd covers the other user-visible half: the
// host function behind h.PollCancellation() (engine/signaller.go:121), which is
// what examples/subscription and examples/travel branch on. It had no
// end-to-end coverage because no compiled fixture called it — see
// testdata/cancelpoll.
func TestCancellationGuestAPIEndToEnd(t *testing.T) {
	store, teardown := newCancellationTestStore(t)
	defer teardown()

	ctx := context.Background()
	setupTestData(t, store)

	runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
		json.RawMessage(`{}`), "cancel-e2e-3", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	const reason = "customer withdrew"
	if err := store.RequestCancellation(ctx, runID, reason); err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}

	caller := &mockCaller{}
	result := runCancelPollFixture(t, ctx, store, runID, caller)

	if len(caller.calls) != 0 {
		t.Errorf("cancelled workflow performed %d side effects, want 0", len(caller.calls))
	}
	// The reason assertion is not cosmetic, and it is the only one here that
	// can fail. Verified by breaking signaller.go's poll: the guest saw
	// cancelled=false, ran on to the durable call, and freshCall's own check
	// then refused it — so the side-effect count stayed 0 and the workflow
	// still stopped. The two checks are defence-in-depth and mask each other.
	// Only the reason distinguishes "the guest handled its cancellation" from
	// "the engine aborted the guest".
	if !strings.Contains(result, reason) {
		t.Errorf("result %q does not carry the cancellation reason %q", result, reason)
	}
}

// TestCancellationUnknownWorkflowIDIsNotSilent pins the mechanism that let §1.3
// survive: CheckCancellation returns sql.ErrNoRows for an ID that matches no
// row, and every call site guards on `err == nil && cancelled`, so an ID that
// does not resolve reports "not cancelled" and the workflow proceeds.
//
// That is exactly what the hardcoded "" did for as long as it was there — the
// lookup was not failing loudly, it was missing quietly. This test records the
// behaviour at the store boundary so a future change to that guard is a
// deliberate one.
func TestCancellationUnknownWorkflowIDIsNotSilent(t *testing.T) {
	store, teardown := newCancellationTestStore(t)
	defer teardown()

	ctx := context.Background()
	setupTestData(t, store)

	cancelled, reason, err := store.PollCancellation(ctx, "no-such-workflow-id")

	if err == nil {
		t.Error("PollCancellation on an unknown workflow ID returned no error; " +
			"if this now succeeds, the call sites' `err == nil && cancelled` " +
			"guard silently treats an unresolvable ID as 'not cancelled'")
	}
	if cancelled {
		t.Errorf("cancelled = true for an unknown ID (reason %q)", reason)
	}

	// The empty string is the specific value §1.3 was about. It must behave the
	// same way — no row, an error, and no claim that the workflow is live.
	cancelled, _, err = store.PollCancellation(ctx, "")
	if err == nil {
		t.Error(`PollCancellation(ctx, "") returned no error; the empty workflow ID ` +
			"must not resolve to a row")
	}
	if cancelled {
		t.Error(`cancelled = true for the empty workflow ID`)
	}
}

// TestCancellationGuestAPIEndToEnd_NotCancelled is its control.
func TestCancellationGuestAPIEndToEnd_NotCancelled(t *testing.T) {
	store, teardown := newCancellationTestStore(t)
	defer teardown()

	ctx := context.Background()
	setupTestData(t, store)

	runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
		json.RawMessage(`{}`), "cancel-e2e-4", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	caller := &mockCaller{}
	result := runCancelPollFixture(t, ctx, store, runID, caller)

	if len(caller.calls) == 0 {
		t.Error("uncancelled workflow made no durable call; it stopped for some " +
			"reason other than cancellation")
	}
	if !strings.Contains(result, "completed") {
		t.Errorf("result = %q, want it to report completion", result)
	}
}

// runCancelPollFixture compiles and runs testdata/cancelpoll against the given
// store and workflow ID, returning the workflow result.
func runCancelPollFixture(t *testing.T, ctx context.Context, store *PostgresStore, runID string, caller *mockCaller) string {
	t.Helper()

	wasmPath := buildFixtureWasm(t, "cancelpoll")
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	backend, err := NewWasmtimeBackend(ctx)
	if err != nil {
		// Deliberately not a t.Skip, unlike the older tests in this package.
		// wasmtime is the backend of record and CGO is on by default, so it is
		// always available here — scripts/check-skips.sh case (c). Skipping
		// would mean a CGO_ENABLED=0 run reports this suite green without ever
		// exercising cancellation on the primary backend.
		t.Fatalf("wasmtime backend unavailable: %v (if this build disabled CGO, "+
			"that is the defect: it removes the primary backend entirely)", err)
	}
	defer backend.Close(ctx)

	eng := NewEngine(rt, caller,
		WithBackend("go", backend),
		WithSignalStore(store),
		WithWorkflowID(runID),
	)

	result, _, _, _, _, err := eng.Execute(ctx, wasmBytes, "poll_then_call",
		[]byte(`{"orderID":"ord-1"}`))
	if err != nil {
		t.Fatalf("Execute poll_then_call: %v", err)
	}
	return result
}

// TestCancellationEndToEnd_NotCancelled is the control. Without it the test
// above passes against an engine that refuses every call for any reason —
// including a lookup that errors and is swallowed by the `err == nil &&
// cancelled` guard at every call site.
func TestCancellationEndToEnd_NotCancelled(t *testing.T) {
	store, teardown := newCancellationTestStore(t)
	defer teardown()

	ctx := context.Background()
	setupTestData(t, store)

	runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
		json.RawMessage(`{}`), "cancel-e2e-2", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	wasmPath := buildTestWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	backend, err := NewWasmtimeBackend(ctx)
	if err != nil {
		// Deliberately not a t.Skip, unlike the older tests in this package.
		// wasmtime is the backend of record and CGO is on by default, so it is
		// always available here — scripts/check-skips.sh case (c). Skipping
		// would mean a CGO_ENABLED=0 run reports this suite green without ever
		// exercising cancellation on the primary backend.
		t.Fatalf("wasmtime backend unavailable: %v (if this build disabled CGO, "+
			"that is the defect: it removes the primary backend entirely)", err)
	}
	defer backend.Close(ctx)

	caller := &mockCaller{}
	eng := NewEngine(rt, caller,
		WithBackend("go", backend),
		WithSignalStore(store),
		WithWorkflowID(runID),
	)

	input := []byte(`{"userID":"user-1","cart":[{"sku":"ABC-123","quantity":1}]}`)
	if _, _, _, _, _, err := eng.Execute(ctx, wasmBytes, "place_order", input); err != nil {
		t.Fatalf("Execute place_order: %v", err)
	}

	if len(caller.calls) == 0 {
		t.Error("uncancelled workflow performed no side effects; the cancellation " +
			"check is refusing calls for some reason other than cancellation")
	}
}
