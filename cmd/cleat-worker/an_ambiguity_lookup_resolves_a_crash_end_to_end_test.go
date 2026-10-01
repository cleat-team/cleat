package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// TestAmbiguityLookupEndToEnd_ResolvesACrashedCall is cleat#1984's acceptance
// table, driven end to end: a real PostgreSQL, a real wasmtime guest, a real
// --write-ahead-intent-ops/--ambiguity-lookup worker and a real crash -- not
// a mocked resolver (engine/callintent_test.go's TestResolveAmbiguity_*,
// which proves the engine's own contract in isolation) and not a mocked
// engine (an_ambiguous_call_is_settled_through_a_lookup_test.go's
// TestResolveCall_StatusCodeDecidesTheOutcome, which proves dbServiceCaller's
// HTTP status-code mapping in isolation, against a bare *dbServiceCaller
// nothing has wired to an engine). This proves the three pieces actually
// meet: that WithAmbiguityResolver(caller), wired by setup.go only when
// --ambiguity-lookup is configured, really does turn a genuine crashed call
// into either a resolved step or a retried one.
//
// "Crashed" is built the same way
// fenced_execution_acceptance_test.go builds a reclaim: synchronously, from
// inside the service's own HTTP handler, rather than by racing a timeout.
// freshCallWithIntent (engine/callintent.go) commits the intent row and only
// THEN dispatches -- so by the time the handler for "billing/charge" is
// reached, the pending row is already durable, and the handler can block
// there indefinitely while the test resets the row's ownership exactly as a
// second worker's reclaim would (same UPDATE as the fence test), then starts
// a second worker to replay it. Worker A's dispatch goroutine is abandoned,
// not cancelled -- a real crash does not get to finish its HTTP request
// either -- and released only in cleanup, after the assertions that matter
// have already run, so the leaked goroutine cannot complete behind
// workerB's row ownership and interfere with it (and httptest.Server.Close
// would hang forever waiting for it otherwise).
//
// Two rows of cleat#1984's table are covered. 500/unreachable ("cannot say")
// is NOT re-proven here: engine/callintent_test.go's
// TestResolveAmbiguity_FallsBackWhenItCannotSay and
// TestResolveCall_StatusCodeDecidesTheOutcome's "500" case already cover it
// at the layer where it is decided, and nothing about crossing the package
// boundary changes that decision -- a resolver error or a cannot-say
// outcome both short-circuit before anything worker-specific runs.
func TestAmbiguityLookupEndToEnd_ResolvesACrashedCall(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping the database-backed ambiguity-lookup acceptance test")
	}

	t.Run("200: the lookup settles the step, no retry", func(t *testing.T) {
		testAmbiguityLookupEndToEnd(t, "resolved", http.StatusOK, `{"charge_id":"ch_live"}`)
	})
	t.Run("404: the lookup reports not-sent, the guest retries once", func(t *testing.T) {
		testAmbiguityLookupEndToEnd(t, "not-sent", http.StatusNotFound, "")
	})
}

// recordedCallsWithKeys is recordedCalls plus the Idempotency-Key each
// request carried -- recordedCalls.add only keeps the op, and distinguishing
// the original dispatch from the guest's retry needs the key too (that is
// the entire content of the hazard this test exists to make observable).
type recordedCallsWithKeys struct {
	mu   sync.Mutex
	ops  []string
	keys []string
}

func (r *recordedCallsWithKeys) add(op, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, op)
	r.keys = append(r.keys, key)
}

func (r *recordedCallsWithKeys) countOp(op string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, got := range r.ops {
		if got == op {
			n++
		}
	}
	return n
}

// keysForOp returns the keys recorded for op, in request order.
func (r *recordedCallsWithKeys) keysForOp(op string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var keys []string
	for i, got := range r.ops {
		if got == op {
			keys = append(keys, r.keys[i])
		}
	}
	return keys
}

func testAmbiguityLookupEndToEnd(t *testing.T, caseName string, lookupStatus int, lookupBody string) {
	ctx := context.Background()

	db := testutil.SuiteTestDB(t, "cleat_worker")
	store := engine.NewPostgresStore(db)

	wasmBytes := buildDeferFixture(t)
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}

	defName := "ambiguity-lookup-acceptance-" + caseName
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
		ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	wfID := fmt.Sprintf("ambiguity-lookup-acceptance-%s-%d", caseName, time.Now().UnixNano())
	input := json.RawMessage(`{"__entry_point":"RetryOnceOnFailure"}`)
	if _, _, err := store.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion,
		input, "", engine.DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	calls := &recordedCallsWithKeys{}
	chargeHung := make(chan struct{})
	unblockCharge := make(chan struct{})
	var chargeHungOnce sync.Once

	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op := strings.TrimPrefix(r.URL.Path, "/call/")
		key := r.Header.Get("Idempotency-Key")
		calls.add(op, key)
		switch op {
		case "billing/charge":
			if calls.countOp("billing/charge") == 1 {
				// The original attempt: block here, exactly where a real crash
				// would leave it -- WriteCallIntent has already committed by
				// the time freshCallWithIntent dispatches this request, so the
				// pending row this test resumes from is genuine, not
				// hand-written.
				chargeHungOnce.Do(func() { close(chargeHung) })
				<-unblockCharge
				// By the time this is allowed to continue, the row has moved
				// on to worker B (or completed). Respond so the handler
				// returns and the leaked goroutine stops rather than
				// completing a meaningful write; CompleteCallIntent's own
				// worker/generation fencing is what actually protects
				// workerB's row if this does race it.
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"charge_id":"ch_orphaned"}`)
				return
			}
			// The guest's own retry -- a fresh request, a fresh step, and (the
			// point of this fixture) a fresh key.
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"charge_id":"ch_retry"}`)
		case "billing/get_by_key":
			w.WriteHeader(lookupStatus)
			if lookupBody != "" {
				fmt.Fprint(w, lookupBody)
			}
		default:
			t.Errorf("unexpected call to %q", op)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer svc.Close()
	// Runs before svc.Close() (LIFO): unblocks the hung handler so Close does
	// not wait forever for a request this test deliberately never lets
	// finish on its own.
	defer func() { close(unblockCharge) }()

	oldOps := *writeAheadIntentOps
	*writeAheadIntentOps = "billing.charge"
	defer func() { *writeAheadIntentOps = oldOps }()

	lookups, err := parseAmbiguityLookup("billing.charge=billing.get_by_key")
	if err != nil {
		t.Fatalf("parseAmbiguityLookup: %v", err)
	}

	logsA := &syncBuffer{}
	workerA := newRealDeferPhaseWorker(t, db, store, logsA)
	workerA.id = "ambiguity-lookup-acceptance-worker-a-" + caseName
	workerA.serviceEndpoints = map[string]string{"billing": svc.URL}
	workerA.ambiguityLookup = lookups
	workerA.egress = &engine.EgressGuard{
		AllowLoopback:  true,
		TenantOptional: func(context.Context) bool { return true },
	}

	claimedA := claimOne(t, ctx, store, workerA.id, wfID)
	workerA.inflight.Store(wfID, claimedA)
	workerA.wg.Add(1)
	go workerA.executeWorkflow(claimedA)

	select {
	case <-chargeHung:
	case <-time.After(10 * time.Second):
		t.Fatalf("the original charge call never reached the service within 10s (logs:\n%s)", logsA.String())
	}

	// Simulate the crash: put the row back the way a second worker's reclaim
	// would find it. Same UPDATE as
	// TestAFencedExecutionMakesNoFurtherCallsAndASecondWorkerFinishesTheWorkflow;
	// worker A's own goroutine is left running against the hung handler and
	// is never waited on again.
	if _, err := db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL,
		    generation = generation + 1, reclaim_count = reclaim_count + 1
		WHERE id = $1`, wfID); err != nil {
		t.Fatalf("simulating the crash: %v", err)
	}

	logsB := &syncBuffer{}
	workerB := newRealDeferPhaseWorker(t, db, store, logsB)
	workerB.id = "ambiguity-lookup-acceptance-worker-b-" + caseName
	workerB.serviceEndpoints = map[string]string{"billing": svc.URL}
	workerB.ambiguityLookup = lookups
	workerB.egress = &engine.EgressGuard{
		AllowLoopback:  true,
		TenantOptional: func(context.Context) bool { return true },
	}

	claimedB := claimOne(t, ctx, store, workerB.id, wfID)
	workerB.inflight.Store(wfID, claimedB)
	runExecuteWorkflow(workerB, claimedB)

	if got := statusOf(t, ctx, store, wfID); got != "done" {
		t.Fatalf("status after worker B's run = %q, want %q (logs:\n%s)", got, "done", logsB.String())
	}
	if !calls.countOpAtLeast("billing/get_by_key", 1) {
		t.Fatalf("the lookup operation was never called, so this test exercised no resolver at all (logs:\n%s)", logsB.String())
	}
	lookupKeys := calls.keysForOp("billing/get_by_key")
	chargeKeys := calls.keysForOp("billing/charge")
	if len(chargeKeys) == 0 {
		t.Fatalf("billing/charge was never called at all (logs:\n%s)", logsA.String()+logsB.String())
	}
	if lookupKeys[0] != chargeKeys[0] {
		t.Errorf("lookup was asked about key %q, want the ORIGINAL attempt's key %q -- "+
			"resolving an ambiguity means asking about the attempt that actually happened",
			lookupKeys[0], chargeKeys[0])
	}

	switch caseName {
	case "resolved":
		if n := calls.countOp("billing/get_by_key"); n != 1 {
			t.Errorf("lookup called %d times, want 1", n)
		}
		if n := calls.countOp("billing/charge"); n != 1 {
			t.Errorf("billing/charge called %d times, want 1 -- a 200 settles the step from the "+
				"lookup's own response, the guest must not see a failure to retry", n)
		}
	case "not-sent":
		if n := calls.countOp("billing/charge"); n != 2 {
			t.Errorf("billing/charge called %d times, want 2 -- the original (orphaned) attempt "+
				"plus exactly one guest retry", n)
		}
		if len(chargeKeys) == 2 && chargeKeys[0] == chargeKeys[1] {
			t.Error("the guest's retry reused the original attempt's idempotency key -- it must " +
				"not, because it is a new DurableCall at a new step (engine/callintent.go's " +
				"resolveAmbiguity documents exactly this)")
		}
	}
}

// countOpAtLeast avoids importing testify for one boolean.
func (r *recordedCallsWithKeys) countOpAtLeast(op string, n int) bool {
	return r.countOp(op) >= n
}
