package main

import (
	"context"
	"database/sql"
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

// requireShutdownTestDB skips the database-backed shutdown acceptance tests when no PostgreSQL was asked for.
func requireShutdownTestDB(t *testing.T) {
	t.Helper()
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping the database-backed shutdown acceptance test")
	}
}

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
	requireShutdownTestDB(t)
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

	// And what the run IS afterwards (cleat#2285). It used to be written FAILED: the aborted wait reached the
	// guest as a failed call, the guest returned an error, and a terminal status is never reclaimed, so a
	// deploy lost the run. Released, it is `ready` again with no owner, for another worker to replay.
	var status string
	var errMsg sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT status, error_msg FROM workflow_instances WHERE id = $1`, wfID).Scan(&status, &errMsg); err != nil {
		t.Fatalf("reading the run's status: %v", err)
	}
	if status != "ready" {
		t.Errorf("a run cut off by shutdown is %q (%s), want %q: shutdown must release a run, never fail it (logs:\n%s)",
			status, errMsg.String, "ready", logs.String())
	}

	if got := calls.all(); len(got) != 1 {
		t.Errorf("service received %d call(s), want exactly 1 -- a second attempt means the "+
			"backoff simply finished before the 5s abort-wait timed out, not that it was "+
			"aborted: %v", len(got), got)
	}
}

// ctxBlindStore is a store whose terminal writes ignore the worker's cancellation. A real one cannot be
// relied on to refuse: the database driver notices a cancelled context, but only when it is next asked. This
// is what makes the worker's OWN check decide, instead of a `begin tx: context canceled` deciding for it.
type ctxBlindStore struct{ engine.WorkflowStore }

func (s ctxBlindStore) FinalizeWorkflowSegment(ctx context.Context, runID, workerID string, generation int64, newEvents []engine.EventRecord, finalStatus, result, errorCode, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
	return s.WorkflowStore.FinalizeWorkflowSegment(context.WithoutCancel(ctx), runID, workerID, generation, newEvents, finalStatus, result, errorCode, errorOp, queryState, nextWakeAt)
}

func (s ctxBlindStore) FailWorkflow(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
	return s.WorkflowStore.FailWorkflow(context.WithoutCancel(ctx), workflowID, workerID, generation, errorMsg, errorCode, errorOp, queryState)
}

// TestShutdownDuringABackoffNeverReachesAGuestThatCompensates is cleat#2285's version of the test above with a
// workflow that compensates on error. Before, the woken backoff came back to the guest as a failed call, and a
// worker shutting down ran the compensation and finished the run COMPLETED. Now the guest is told to stop:
// nothing is compensated, and the run is released. Two stores, so that each of the two release checks has to
// decide once: the real one (a cancelled context refuses the write) and one that ignores cancellation
// (nothing but the worker's own check stands between the outcome and the database).
func TestShutdownDuringABackoffNeverReachesAGuestThatCompensates(t *testing.T) {
	requireShutdownTestDB(t)
	for _, blind := range []bool{false, true} {
		name := "real store"
		if blind {
			name = "store that ignores cancellation"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := testutil.SuiteTestDB(t, "cleat_worker")
			store := engine.NewPostgresStore(db)

			wasmBytes := buildDeferFixture(t)
			meta, err := wasm.ReadMetadata(wasmBytes)
			if err != nil {
				t.Fatalf("ReadMetadata: %v", err)
			}
			defName := fmt.Sprintf("shutdown-compensate-%t-%d", blind, time.Now().UnixNano())
			if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
				Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
				ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}
			wfID := fmt.Sprintf("shutdown-compensate-%t-%d", blind, time.Now().UnixNano())
			if _, _, err := store.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion,
				json.RawMessage(`{"__entry_point":"RetryThenCompensate"}`), "", engine.DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			calls := &recordedCalls{}
			svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.add(strings.TrimPrefix(r.URL.Path, "/call/"))
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprint(w, `{"error":"always fails"}`)
			}))
			defer svc.Close()
			oldSvcURL := *benchSvcURL
			*benchSvcURL = svc.URL
			defer func() { *benchSvcURL = oldSvcURL }()

			logs := &syncBuffer{}
			var ws engine.WorkflowStore = store
			if blind {
				ws = ctxBlindStore{store}
			}
			worker := newRealDeferPhaseWorker(t, db, ws, logs)
			worker.id = "shutdown-compensate-worker"
			worker.egress = &engine.EgressGuard{
				AllowLoopback:  true,
				TenantOptional: func(context.Context) bool { return true },
			}
			claimed := claimOne(t, ctx, store, worker.id, wfID)
			worker.inflight.Store(wfID, claimed)

			done := make(chan struct{})
			worker.wg.Add(1)
			go func() { worker.executeWorkflow(claimed); close(done) }()

			deadline := time.Now().Add(5 * time.Second)
			for !calls.has("always-fails/op") {
				if time.Now().After(deadline) {
					t.Fatalf("the first attempt never reached the service (logs:\n%s)", logs.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond) // into the backoff select
			worker.cancel()

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("executeWorkflow did not return within 5s of worker.cancel() (logs:\n%s)", logs.String())
			}

			if calls.has("always-fails/compensate") {
				t.Errorf("the guest ran its compensation: a shutdown reached it as a failed call. service saw %v", calls.all())
			}
			var status string
			var errMsg sql.NullString
			if err := db.QueryRowContext(ctx, `SELECT status, error_msg FROM workflow_instances WHERE id = $1`, wfID).Scan(&status, &errMsg); err != nil {
				t.Fatalf("reading the run's status: %v", err)
			}
			if status != "ready" {
				t.Errorf("the run is %q (%s), want %q: it must be released for another worker (logs:\n%s)", status, errMsg.String, "ready", logs.String())
			}
		})
	}
}
