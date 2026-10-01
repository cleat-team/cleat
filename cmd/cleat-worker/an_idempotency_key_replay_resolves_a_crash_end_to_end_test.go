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

// TestIdempotencyKeyReplayEndToEnd_ResolvesACrashedCall is cleat#2897's
// acceptance test, driven end to end: a real PostgreSQL, a real wasmtime
// guest, a real --write-ahead-intent-ops/--idempotency-key-ops worker and a
// real crash -- the same shape as
// an_ambiguity_lookup_resolves_a_crash_end_to_end_test.go's
// TestAmbiguityLookupEndToEnd_ResolvesACrashedCall, for the sibling
// mechanism. engine/an_ambiguity_is_resolved_by_idempotency_key_replay_test.go
// proves the engine's own contract (fake replayer) and
// an_idempotency_key_replay_is_settled_by_status_code_test.go proves
// dbServiceCaller's HTTP status-code mapping (bare caller, no engine). This
// proves the three pieces actually meet.
//
// THE FIXTURE IS RetryOnceOnFailure, REUSED FROM THE AMBIGUITY-LOOKUP TEST,
// NOT TwoSequentialCalls -- deliberately. An earlier version of this test used
// TwoSequentialCalls and asserted on the STORED event response, read back via
// LoadEventHistory after the workflow completed. That read back zero rows:
// this worker test harness (newRealDeferPhaseWorker, shared with the fence
// and ambiguity-lookup acceptance tests, none of which ever call
// LoadEventHistory) does not leave event_history queryable after a workflow
// finishes -- confirmed by a direct count immediately after WriteCallIntent
// (1 row) against the same count after completion (0 rows, same behaviour on
// the known-working fence acceptance test's workflow once checked the same
// way). RetryOnceOnFailure avoids the question entirely: `h.DurableCall`'s
// OWN return value is what the guest returns as the workflow Result, so the
// resolved response is observable through resultOf (GetWorkflowByID) the
// same way the ambiguity-lookup "resolved" subtest already proves it, with
// no assumption about what survives in event_history after completion.
//
// THE DEDUP FIXTURE HAS EXACTLY ONE RELEVANT KEY THROUGHOUT THIS TEST,
// DELIBERATELY: worker A's original dispatch and worker B's key-replay retry
// both derive DurableCallIdempotencyKey(wfID, "", 0) -- same workflow, same
// run, same step -- which is item 1's whole point and lets the fixture be a
// plain state machine (unseen -> blocked -> completed) rather than a map.
// A request arriving while state is "blocked" is a genuinely CONCURRENT
// same-key request, and gets Stripe's documented 409 for that case; the
// engine must read that as "retry later", not a failure (item 2).
//
// GUARDING AGAINST A TEST THAT CANNOT FAIL (coordinator's flag before this
// was written): the resolved response is a NONCE generated inside the
// handler at the moment the orphaned original is unblocked
// (fmt.Sprintf(...%d, time.Now().UnixNano())), not a fixture constant --
// reading a hardcoded string back would pass whether or not the retry
// actually received the ORIGINAL's answer.
func TestIdempotencyKeyReplayEndToEnd_ResolvesACrashedCall(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping the database-backed idempotency-key-replay acceptance test")
	}
	ctx := context.Background()

	db := testutil.SuiteTestDB(t, "cleat_worker")
	store := engine.NewPostgresStore(db)

	wasmBytes := buildDeferFixture(t)
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}

	const defName = "idempotency-key-replay-acceptance"
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
		ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	wfID := fmt.Sprintf("idempotency-key-replay-acceptance-%d", time.Now().UnixNano())
	input := json.RawMessage(`{"__entry_point":"RetryOnceOnFailure"}`)
	if _, _, err := store.StartNewRun(ctx, wfID, defName, meta.WorkflowVersion,
		input, "", engine.DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	calls := &recordedCallsWithKeys{}

	const (
		stateUnseen = iota
		stateBlocked
		stateCompleted
	)
	var mu sync.Mutex
	state := stateUnseen
	var storedResponse string
	chargeHung := make(chan struct{})
	gotConflict := make(chan struct{}, 8)
	unblockCharge := make(chan struct{})
	var chargeHungOnce, unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(unblockCharge) }) }

	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op := strings.TrimPrefix(r.URL.Path, "/call/")
		key := r.Header.Get("Idempotency-Key")
		calls.add(op, key)
		switch op {
		case "billing/charge":
			mu.Lock()
			switch state {
			case stateCompleted:
				resp := storedResponse
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, resp)
				return
			case stateBlocked:
				mu.Unlock()
				w.WriteHeader(http.StatusConflict)
				fmt.Fprint(w, `{"error":"a request under this idempotency key is still being processed"}`)
				select {
				case gotConflict <- struct{}{}:
				default:
				}
				return
			default: // stateUnseen
				state = stateBlocked
				mu.Unlock()
			}
			chargeHungOnce.Do(func() { close(chargeHung) })
			<-unblockCharge
			resp := fmt.Sprintf(`{"charge_id":"ch_%d"}`, time.Now().UnixNano())
			mu.Lock()
			state = stateCompleted
			storedResponse = resp
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, resp)
		default:
			t.Errorf("unexpected call to %q", op)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer svc.Close()
	defer unblock()

	oldWriteAhead := *writeAheadIntentOps
	*writeAheadIntentOps = "billing.charge"
	defer func() { *writeAheadIntentOps = oldWriteAhead }()

	ops, err := parseIdempotencyKeyOps("billing.charge")
	if err != nil {
		t.Fatalf("parseIdempotencyKeyOps: %v", err)
	}

	logsA := &syncBuffer{}
	workerA := newRealDeferPhaseWorker(t, db, store, logsA)
	workerA.id = "idempotency-key-replay-acceptance-worker-a"
	workerA.serviceEndpoints = map[string]string{"billing": svc.URL}
	workerA.idempotencyKeyOps = ops
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
		t.Fatalf("the original billing/charge call never reached the service within 10s (logs:\n%s)", logsA.String())
	}

	// Simulate the crash: same UPDATE as the fence and ambiguity-lookup
	// acceptance tests. Worker A's own goroutine is left running against the
	// hung handler and is never waited on again.
	if _, err := db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL,
		    generation = generation + 1, reclaim_count = reclaim_count + 1
		WHERE id = $1`, wfID); err != nil {
		t.Fatalf("simulating the crash: %v", err)
	}

	logsB := &syncBuffer{}
	workerB := newRealDeferPhaseWorker(t, db, store, logsB)
	workerB.id = "idempotency-key-replay-acceptance-worker-b"
	workerB.serviceEndpoints = map[string]string{"billing": svc.URL}
	workerB.idempotencyKeyOps = ops
	workerB.egress = &engine.EgressGuard{
		AllowLoopback:  true,
		TenantOptional: func(context.Context) bool { return true },
	}

	claimedB := claimOne(t, ctx, store, workerB.id, wfID)
	workerB.inflight.Store(wfID, claimedB)

	done := make(chan struct{})
	go func() {
		runExecuteWorkflow(workerB, claimedB)
		close(done)
	}()

	// GENUINE OVERLAP, NOT A SEQUENTIAL PAIR (coordinator, cleat#1984 round 3,
	// the same lesson this file's package comment cites): wait for the
	// retry's first dispatch to actually land and be refused with 409 BEFORE
	// unblocking the orphaned original. If the original were unblocked first,
	// the retry might simply find state already "completed" and this test
	// would never exercise the 409 path item 2 exists for.
	select {
	case <-gotConflict:
	case <-time.After(10 * time.Second):
		t.Fatalf("the key-replay retry never received a 409 within 10s -- either it never "+
			"dispatched, or it did not reuse the original's key (logs:\n%s)", logsB.String())
	}
	unblock()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("worker B's run did not finish within 10s after the original was unblocked (logs:\n%s)", logsB.String())
	}

	if got := statusOf(t, ctx, store, wfID); got != "done" {
		t.Fatalf("status after worker B's run = %q, want \"done\" (logs:\n%s)", got, logsB.String())
	}

	chargeKeys := calls.keysForOp("billing/charge")
	if len(chargeKeys) < 2 {
		t.Fatalf("billing/charge was called %d times, want at least 2 (the orphaned original plus "+
			"at least one key-replay attempt) (logs:\n%s)", len(chargeKeys), logsA.String()+logsB.String())
	}
	for i, k := range chargeKeys {
		if k != chargeKeys[0] {
			t.Errorf("billing/charge call %d carried key %q, want %q -- every dispatch of this "+
				"step must carry the SAME key, original and retries alike", i, k, chargeKeys[0])
		}
	}

	// THE DISCRIMINATING ASSERTION: the workflow's own Result must be the
	// NONCE the handler generated when the ORPHANED ORIGINAL was unblocked --
	// RetryOnceOnFailure returns h.DurableCall's response verbatim on
	// success, so this is only satisfied if the ambiguity actually resolved
	// to that specific response rather than, say, an empty one, a retry's
	// own (never-executed, in this scenario) second attempt, or a fixture
	// constant a less careful test might have hardcoded.
	mu.Lock()
	wantResponse := storedResponse
	mu.Unlock()
	if got := resultOf(t, ctx, store, wfID); got != wantResponse {
		t.Errorf("workflow result = %q, want the ORPHANED ORIGINAL's actual response %q -- the "+
			"guest's own output must carry the response the service's key table resolved to, not "+
			"merely let the step complete", got, wantResponse)
	}

	// RetryOnceOnFailure's OWN retry branch must never have fired: a
	// RESOLVED ambiguity reaches the guest as an ordinary successful
	// DurableCall, with no error to retry on. If it had fired, the guest's
	// second DurableCall would be a NEW step and therefore a NEW idempotency
	// key -- which the "every key equals chargeKeys[0]" loop above would
	// already have caught, since that retry would reach this same handler
	// under a third, distinct key.
}
