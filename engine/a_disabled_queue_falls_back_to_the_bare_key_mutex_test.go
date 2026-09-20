package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// Disabling a queue drops admission to ONE. It does not stop the work.
//
// # Why this needed a test rather than a reading
//
// Every claim statement joins `queues` with `AND q.disabled_at IS NULL`, so a
// disabled queue is indistinguishable from an unregistered name and falls to
// the bare-key arm -- the concurrency_keys mutex. Nothing asserted that, and
// the only prose describing it said the opposite: DisableQueue's own doc
// comment read "new acquisition is refused, existing holders drain" until this
// test was written. That comment was accurate about the intent of the PR it
// shipped in, where nothing read the table at all, and wrong from the moment
// the claim path was generalised.
//
// An operator now reaches this through `cleatctl queue disable`, whose output
// states the fallback in as many words. This is the test standing behind that
// message, which is why it asserts ONE rather than merely "fewer than the
// limit" -- the failure that matters is an operator disabling a queue to stop
// it and quietly getting a serial queue instead, and "fewer" would pass for
// zero too.
//
// # Why the fallback is right, and not fixed to a refusal
//
// Refusing would let one operator command wedge every run carrying that key,
// with nothing in any error path to say why -- a start still succeeds, the
// work simply never runs. Degrading to N=1 is strictly the safer direction:
// it never admits MORE than the limit did, it never stalls, and
// `cleatctl suspend-tenant` already exists for the "stop the work" intent.
func TestADisabledQueueFallsBackToTheBareKeyMutex(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			const limit = 3
			name := queueTestName("disabled")
			db := queueClaimTestDB(t, store)
			qs := NewQueueStore(db, backend.Name())
			if err := qs.CreateQueue(ctx, DefaultTenantUUID, name, limit); err != nil {
				t.Fatalf("CreateQueue: %v", err)
			}
			if err := qs.DisableQueue(ctx, DefaultTenantUUID, name); err != nil {
				t.Fatalf("DisableQueue: %v", err)
			}

			starter, ok := store.(claimQueueConcurrencyKeyStarter)
			if !ok {
				t.Fatalf("%T cannot record a concurrency key", store)
			}
			for i := 0; i < 10; i++ {
				if _, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
					fmt.Sprintf("disabled-%d", i), "test-workflow", 1, json.RawMessage(`{}`),
					"", DefaultTenantUUID, 0, name); err != nil {
					t.Fatalf("start run %d: %v", i, err)
				}
			}

			claimed, err := store.ClaimWorkflows(ctx, "worker-1", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}
			if len(claimed) != 1 {
				t.Fatalf("a disabled queue admitted %d runs at once; want 1.\n\n"+
					"Disabling must return the key to the bare concurrency_keys mutex, which is "+
					"what an unregistered name gets. %d means the declared limit is still in "+
					"force (the claim's LEFT JOIN has stopped excluding disabled rows); 0 means "+
					"disabling now STOPS the work, which is the behaviour cleatctl's own output "+
					"promises it does not have.", len(claimed), len(claimed))
			}
			// It took the mutex, not a slot. A disabled queue must leave
			// queue_holders alone -- a holder row written here would be
			// counted against the limit if the queue were ever re-enabled.
			if got := mustCountQueueHolders(t, ctx, db, backend.Name(), name); got != 0 {
				t.Errorf("a disabled queue took %d queue_holders row(s); want 0", got)
			}

			// THE CONTROL, and the EnableQueue round trip in one step. Without
			// it the assertion above passes just as well against a claim that
			// can never admit more than one for reasons having nothing to do
			// with disabling -- a bad seed, a task-queue mismatch, anything.
			// The same ten runs must admit the declared limit once the queue
			// is live again.
			if err := store.TerminateWorkflow(ctx, claimed[0].ID, "done"); err != nil {
				t.Fatalf("TerminateWorkflow: %v", err)
			}
			if err := qs.EnableQueue(ctx, DefaultTenantUUID, name); err != nil {
				t.Fatalf("EnableQueue: %v", err)
			}
			live, err := store.ClaimWorkflows(ctx, "worker-2", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows after enable: %v", err)
			}
			if len(live) != limit {
				t.Fatalf("after re-enabling, claimed %d, want %d. The control for the assertion "+
					"above: the same seed admits the declared limit when the queue is live, so "+
					"the 1 above was the disable and not the fixture.", len(live), limit)
			}
		})
	}
}
