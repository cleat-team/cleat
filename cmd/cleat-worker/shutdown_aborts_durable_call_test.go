package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// TestWorkerShutdownAbortsADurableCallsHostBackoffWait is cleat#2020's
// acceptance test: a real PostgreSQL, a real wasmtime guest, and a real
// Worker whose w.ctx is cancelled mid-backoff -- not the mocked engine call
// site a unit test on durablecalls.go alone would exercise. cleat#2008's own
// acceptance test (TestAFencedExecutionMakesNoFurtherCallsAndASecondWorkerFinishesTheWorkflow,
// this package) exists for exactly the same reason: a claim about what a real
// wasmtime host call's ctx does is only proven by making one.
//
// The fixture (testdata/deferfunc's RetryBacksOffOnHost) makes one
// DurableCallWithOptions, retry policy MaxAttempts=2 / InitialInterval=10s,
// short enough in worst case to run on the HOST side of the split
// DeferOnLongRetryPolicy documents (under --host-retry-budget's 60s
// default), against a service that always answers 503. So the first
// attempt's failure puts the execution inside durablecalls.go's backoff
// select for 10s, worker-held -- long enough for this test to observe the
// attempt, cancel the worker, and still leave most of the interval as
// margin between "aborted promptly" and "the interval simply elapsed".
//
// Before cleat#2020's fix, this test failed by TIMING OUT: every wasmtime
// host call builds its ctx from context.Background()
// (engine/wasmtime_hostfuncs.go), never descended from w.ctx, so the
// select's `case <-ctx.Done()` could never fire on a real worker shutdown.
// engine.WithShutdownSignal(w.ctx.Done()) is what gives the wait a channel
// that actually does.
func TestWorkerShutdownAbortsADurableCallsHostBackoffWait(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping the database-backed shutdown acceptance test")
	}
	ctx := context.Background()

	db := testutil.SuiteTestDB(t, "cleat_worker")
	store := engine.NewPostgresStore(db)

	wasmBytes := buildDeferFixture(t)
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}

	const defName = "shutdown-backoff-acceptance-worker"
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
		ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	wfID := fmt.Sprintf("shutdown-backoff-acceptance-%d", time.Now().UnixNano())
	input := json.RawMessage(`{"__entry_point":"RetryBacksOffOnHost"}`)
	if _, _, err := store.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion,
		input, "", engine.DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	calls := &recordedCalls{}
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op := strings.TrimPrefix(r.URL.Path, "/call/")
		calls.add(op)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":"always fails"}`)
	}))
	defer svc.Close()
	oldSvcURL := *benchSvcURL
	*benchSvcURL = svc.URL
	defer func() { *benchSvcURL = oldSvcURL }()

	logs := &syncBuffer{}
	worker := newRealDeferPhaseWorker(t, db, store, logs)
	worker.id = "shutdown-backoff-acceptance-worker"
	worker.egress = &engine.EgressGuard{
		AllowLoopback:  true,
		TenantOptional: func(context.Context) bool { return true },
	}

	claimed := claimOne(t, ctx, store, worker.id, wfID)
	worker.inflight.Store(wfID, claimed)

	start := time.Now()
	done := make(chan struct{})
	worker.wg.Add(1)
	go func() {
		worker.executeWorkflow(claimed)
		close(done)
	}()

	// Wait for the first attempt to reach the service -- that is what puts
	// the execution inside the backoff select, not merely queued to reach
	// it.
	attemptDeadline := time.Now().Add(5 * time.Second)
	for !calls.has("always-fails/op") {
		if time.Now().After(attemptDeadline) {
			t.Fatalf("the first attempt never reached the service within 5s (logs:\n%s)", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// calls.add happens inside the HTTP handler, which returns to
	// forwardToService before durablecalls.go reaches its select -- give
	// that a moment rather than racing it.
	time.Sleep(50 * time.Millisecond)

	worker.cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("executeWorkflow did not return within 5s of worker.cancel(); the backoff "+
			"wait was not aborted by shutdown and is riding out its own 10s interval "+
			"(logs:\n%s)", logs.String())
	}
	if elapsed := time.Since(start); elapsed >= 9*time.Second {
		t.Fatalf("executeWorkflow took %s, close to the fixture's 10s backoff interval; "+
			"the wait ran to completion instead of being aborted by shutdown (logs:\n%s)",
			elapsed, logs.String())
	}

	if got := calls.all(); len(got) != 1 {
		t.Errorf("service received %d call(s), want exactly 1 -- a second attempt means the "+
			"backoff simply finished before the 5s abort-wait timed out, not that it was "+
			"aborted: %v", len(got), got)
	}
}
