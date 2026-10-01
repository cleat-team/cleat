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
// either. For the "resolved" and "cannot-say" cases it is released only in
// cleanup, after the assertions that matter have already run, so the leaked
// goroutine cannot complete behind workerB's row ownership and interfere
// with it (and httptest.Server.Close would hang forever waiting for it
// otherwise). The "not-sent" case releases it deliberately earlier -- see
// below.
//
// All three rows of cleat#1984's table are covered, including 500 ("cannot
// say") -- added in round 2 after cleat-review and the coordinator
// independently measured that without it, RetryOnceOnFailure's blind retry
// (any error, not just a retryable one) made the 404 subtest pass even with
// NotSent degraded to CannotSay or the lookup answering 500: both produced
// the identical "two charges, two keys, done" shape as a correct not-sent
// resolution, so the 404 subtest was not actually discriminating what it
// claimed to. The fixture now retries only a Retryable() CallError, and the
// 500 subtest pins that an [AMBIGUOUS] failure is NOT retried and the run
// ends "failed" with exactly one charge -- the case that would have caught
// the original gap.
//
// Round 3 (coordinator): "sequential duplicates prove nothing, because the
// first finishes before the second one looks." The "not-sent" case's final
// assertion does not let that happen -- it unblocks the orphaned original
// only after the retry has already completed the workflow, so the two
// requests were genuinely in flight together, and confirms the service
// served the late original as an ordinary successful charge. That is the
// double-execution hazard docs/durable-calls.md documents, made observable
// rather than asserted from a comment.
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
	t.Run("500: cannot say, falls back to [AMBIGUOUS], no retry", func(t *testing.T) {
		testAmbiguityLookupEndToEnd(t, "cannot-say", http.StatusInternalServerError, "")
	})
}

// resultOf returns the workflow's terminal Result, the way a caller of the
// admin API would read it. Used only by the "resolved" case, to pin that a
// 200 lookup response reaches the guest's own output -- not just that the
// row completed without error, which a resolver that recorded the WRONG
// response (cleat-review, round 2: completed.Response left empty) would
// also satisfy.
func resultOf(t *testing.T, ctx context.Context, store engine.WorkflowStore, wfID string) string {
	t.Helper()
	wf, err := store.GetWorkflowByID(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if wf == nil {
		t.Fatalf("workflow %s not found", wfID)
	}
	return wf.Result
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
	chargeOrphanedServed := make(chan int, 1)
	var chargeHungOnce, unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(unblockCharge) }) }

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
				// By the time this is allowed to continue, the row may have
				// moved on to worker B (or completed) -- CompleteCallIntent's
				// own worker/generation fencing is what protects workerB's
				// row from this goroutine's engine-side write, if it races
				// it. But nothing fences the SERVICE side: this fixture has
				// no idempotency-key deduplication of its own, so from the
				// service's point of view this is just another request, and
				// it is served exactly like one. That is the point --
				// chargeOrphanedServed lets the "not-sent" case observe it
				// (coordinator, cleat#1984 round 3: "sequential duplicates
				// prove nothing, because the first finishes before the
				// second one looks" -- this is what makes the two
				// overlap instead).
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"charge_id":"ch_orphaned"}`)
				chargeOrphanedServed <- http.StatusOK
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
	// Runs before svc.Close() (LIFO): unblocks the hung handler, if the
	// "not-sent" case below has not already done so, so Close does not wait
	// forever for a request this test might otherwise never let finish.
	defer unblock()

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

	wantStatus := "done"
	if caseName == "cannot-say" {
		wantStatus = "failed"
	}
	if got := statusOf(t, ctx, store, wfID); got != wantStatus {
		t.Fatalf("status after worker B's run = %q, want %q (logs:\n%s)", got, wantStatus, logsB.String())
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
		// cleat-review, round 2: a resolver that recorded the step as
		// complete with an EMPTY response would also satisfy every
		// assertion above. The guest's actual output has to carry the
		// lookup's own answer through, not just an absence of error.
		if got, want := resultOf(t, ctx, store, wfID), `{"charge_id":"ch_live"}`; got != want {
			t.Errorf("workflow result = %q, want %q -- the lookup's response must reach the guest's "+
				"own output, not merely let the step complete", got, want)
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

		// HAZARD ASSERTION, not a correctness assertion -- coordinator,
		// cleat#1984 round 3. The double charge below IS the bug G2
		// documents (engine/callintent.go's resolveAmbiguity,
		// docs/durable-calls.md's "404 answer is a PROMISE"). This pins
		// CURRENT behaviour so a change to it is visible, not an assertion
		// that the behaviour is correct or intended.
		//
		// A sequential pair of duplicate requests would prove nothing here,
		// because the first has to finish before the second can even be
		// observed to exist. This does not: the retry above was dispatched
		// and completed, and the workflow finished, while the ORIGINAL
		// request was still sitting unanswered in the service's handler --
		// genuinely overlapping, not sequential. Only now is it unblocked,
		// and the service -- which has no idempotency-key fencing of its
		// own -- serves it as an ordinary, successful, SEPARATE charge.
		//
		// What a failure here means depends on which of the owner's three
		// options lands, and that is the point of a hazard assertion:
		//   - today, and under option (a) (the lookup service makes 404
		//     durable): this engine's behaviour does not change, so this
		//     test keeps passing -- it is a statement about a service
		//     without key fencing, which is exactly what this fixture is.
		//   - under option (c) (the engine re-sends under the ORIGINAL
		//     key instead of the guest retrying under a new one): the
		//     second charge stops happening and this test FAILS. That
		//     failure is the signal the behaviour changed, not a
		//     regression to fix by restoring this assertion.
		unblock()
		select {
		case code := <-chargeOrphanedServed:
			if code != http.StatusOK {
				t.Errorf("the orphaned original, once unblocked, got %d -- this is a HAZARD "+
					"assertion pinning current (unfenced) behaviour, not a correctness claim; if "+
					"it failed because the double-execution hazard was fixed (e.g. option (c), "+
					"the engine re-sending under the original key), update this assertion rather "+
					"than treat the failure as a regression", code)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the orphaned original never completed after being unblocked -- the overlap " +
				"this subtest exists to demonstrate was not actually observed")
		}
	case "cannot-say":
		// cleat-review + coordinator, round 2: without this subtest and
		// RetryOnceOnFailure's Retryable() check, a CannotSay outcome was
		// retried exactly like a NotSent one -- two charges, two keys,
		// "done" -- which is indistinguishable from the 404 case and is
		// also precisely the unsafe-guest behaviour G2 documents: retrying
		// an outcome the service never confirmed was safe to retry.
		if n := calls.countOp("billing/get_by_key"); n != 1 {
			t.Errorf("lookup called %d times, want 1", n)
		}
		if n := calls.countOp("billing/charge"); n != 1 {
			t.Errorf("billing/charge called %d times, want 1 -- a non-retryable [AMBIGUOUS] "+
				"failure must not be retried by this fixture's guest code", n)
		}
	}
}

// countOpAtLeast avoids importing testify for one boolean.
func (r *recordedCallsWithKeys) countOpAtLeast(op string, n int) bool {
	return r.countOp(op) >= n
}
