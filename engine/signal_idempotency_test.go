package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// A re-sent signal carrying the same token is absorbed. cleat#1121.
//
// Without a token a sender retrying after a timeout cannot tell a lost signal
// from a slow one, so it must choose between possibly losing the signal and
// possibly delivering it twice. This is what breaks the tie.
//
// Against real databases on every dialect, because the absorption is a unique
// constraint and each dialect spells the guarded insert differently --
// ON CONFLICT, INSERT IGNORE, and INSERT ... WHERE NOT EXISTS. No dialect
// stands in for another here.
func TestARepeatedSignalTokenIsAbsorbed(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			si, ok := store.(SignalIdempotencyStore)
			if !ok {
				t.Fatalf("%T does not implement SignalIdempotencyStore", store)
			}
			id := seedRunForSignals(t, ctx, store, "sig-idem")

			// First delivery with a token.
			already, err := si.DeliverSignalIdempotent(ctx, id, "approve", `{"by":"ops"}`, "tok-1")
			if err != nil {
				t.Fatalf("first delivery: %v", err)
			}
			if already {
				t.Fatalf("the FIRST delivery reported alreadyDelivered; nothing had been sent")
			}
			if n := drainSignals(t, ctx, store, id); n != 1 {
				t.Fatalf("one delivery produced %d signal(s), want 1", n)
			}

			// The retry: same token, same everything.
			already, err = si.DeliverSignalIdempotent(ctx, id, "approve", `{"by":"ops"}`, "tok-1")
			if err != nil {
				t.Fatalf("retry: %v", err)
			}
			if !already {
				t.Errorf("a retry with the same token reported alreadyDelivered=false")
			}
			// The assertion that matters. A store reporting the duplicate
			// while still writing the row would satisfy the check above and
			// deliver the signal twice, which is the defect itself.
			if n := drainSignals(t, ctx, store, id); n != 0 {
				t.Errorf("a retry with the same token produced %d further signal(s), want 0 "+
					"-- the duplicate was reported but not absorbed", n)
			}

			// A DIFFERENT token delivers again. Idempotency must not become a
			// once-ever rule: two genuine approvals are two signals.
			already, err = si.DeliverSignalIdempotent(ctx, id, "approve", `{"by":"other"}`, "tok-2")
			if err != nil {
				t.Fatalf("second token: %v", err)
			}
			if already {
				t.Errorf("a DIFFERENT token reported alreadyDelivered")
			}
			if n := drainSignals(t, ctx, store, id); n != 1 {
				t.Errorf("a second, different token produced %d signal(s), want 1 -- "+
					"idempotency must not become a once-ever rule", n)
			}

			// No token at all delivers unconditionally, and twice.
			// An empty key is the ABSENCE of a token, never a token equal to
			// "" -- otherwise two callers who both send no header collide.
			for i := 0; i < 2; i++ {
				already, err = si.DeliverSignalIdempotent(ctx, id, "approve", `{"by":"anon"}`, "")
				if err != nil {
					t.Fatalf("keyless delivery %d: %v", i, err)
				}
				if already {
					t.Errorf("a keyless delivery reported alreadyDelivered; \"\" is an " +
						"absence, not a token two callers can share")
				}
			}
			if n := drainSignals(t, ctx, store, id); n != 2 {
				t.Errorf("two keyless deliveries produced %d signal(s), want 2", n)
			}
		})
	}
}

// TestASignalTokenDoesNotCollideWithAStartToken is the Cadence case, and the
// reason the operation is folded into key_hash.
//
// One client request id legitimately drives a start AND a signal. Upstream
// registers exactly that pair under one request id, deliberately. If the two
// shared an identity the signal's lookup would hit the start's row -- so the
// signal would be absorbed as a duplicate having never been delivered, which is
// silent loss rather than a visible error.
func TestASignalTokenDoesNotCollideWithAStartToken(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			si, ok := store.(SignalIdempotencyStore)
			if !ok {
				t.Fatalf("%T does not implement SignalIdempotencyStore", store)
			}

			const shared = "one-client-request"

			// The start spends the token on the start path.
			runID, existed, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), shared, DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if existed {
				t.Fatalf("premise failed: the first start reported alreadyExisted")
			}

			// The same id, now naming a signal. It must deliver.
			already, err := si.DeliverSignalIdempotent(ctx, runID, "approve", `{}`, shared)
			if err != nil {
				t.Fatalf("signal with the start's token: %v", err)
			}
			if already {
				t.Errorf("a signal carrying the same token as a start was absorbed as a " +
					"duplicate.\n\n" +
					"The two operations share idempotency_keys, whose identity is " +
					"(key_hash, tenant_id). Folding the operation into key_hash is what " +
					"keeps them disjoint; without it this signal is silently never " +
					"delivered, and the client cannot tell.")
			}
			if n := drainSignals(t, ctx, store, runID); n != 1 {
				t.Errorf("the signal produced %d delivery/deliveries, want 1", n)
			}

			// And the start's own token is still spent: the signal did not
			// consume it, which is the other half of "disjoint".
			_, existed, err = store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), shared, DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("repeat start: %v", err)
			}
			if !existed {
				t.Errorf("after the signal, re-presenting the token to the START path " +
					"started a second run -- the signal consumed the start's row")
			}
		})
	}
}

func seedRunForSignals(t *testing.T, ctx context.Context, store WorkflowStore, tag string) string {
	t.Helper()
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: "test-workflow", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	id, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
		json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun(%s): %v", tag, err)
	}
	return id
}

// drainSignals consumes every pending delivery and returns how many there
// were, so each call reports what arrived SINCE THE LAST CALL.
//
// It is destructive, and the first version of this file called it countSignals
// and documented it as "non-destructive" -- which it never was, since polling
// without consuming returns the same oldest row forever. The counts then read
// 0, 1 and 2 where 1, 2 and 4 were expected, and the shortfall looked exactly
// like deliveries going missing. Draining is the only dialect-independent
// reader the store interface offers; asserting deltas is the honest way to use
// it.
func drainSignals(t *testing.T, ctx context.Context, store WorkflowStore, workflowID string) int {
	t.Helper()
	n := 0
	for {
		d, ok, err := store.PollSignal(ctx, workflowID, "approve")
		if err != nil {
			t.Fatalf("PollSignal: %v", err)
		}
		if !ok {
			return n
		}
		n++
		if err := store.ConsumeSignal(ctx, workflowID, d.ID); err != nil {
			t.Fatalf("ConsumeSignal: %v", err)
		}
		if n > 100 {
			t.Fatalf("PollSignal did not drain; %d rows and counting", n)
		}
	}
}

// TestEveryRealStoreCanAbsorbADuplicateSignal is the price of an optional
// interface.
//
// SignalIdempotencyStore is asserted at run time, so a store that never
// implements it does not fail to compile -- it silently loses idempotency, and
// the HTTP handler falls back to plain delivery. That is a regression nothing
// else would report: every existing signal test still passes, because plain
// delivery is what they assert.
//
// registeredBackends is the right population: it is exactly the stores this
// repo claims to support, and it grows when a dialect is added.
func TestEveryRealStoreCanAbsorbADuplicateSignal(t *testing.T) {
	if len(registeredBackends) == 0 {
		t.Fatal("no registered backends; this guard would pass vacuously")
	}
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			if _, ok := store.(SignalIdempotencyStore); !ok {
				t.Errorf("%T does not implement SignalIdempotencyStore.\n\n"+
					"The interface is asserted at run time, so this does not break the "+
					"build -- the handler quietly falls back to plain delivery and every "+
					"re-sent signal is a second signal again (cleat#1121).", store)
			}
		})
	}
}
