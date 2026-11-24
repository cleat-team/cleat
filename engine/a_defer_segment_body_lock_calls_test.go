package engine

// cleat#4101 audit rows: acquire_lock_ms, release_lock.
//
// The cleat#4079 audit could not probe these two (every mode raised TypeError until cleat#4081 was fixed). They are measured here the way
// a_defer_segment_body_does_not_change_durable_state_test.go measures create_promise and set_scope: against a real store, with an
// ordinary session as the control, so that "no effect" for the segment body cannot be the probe failing to see one.
//
// THE TWO ROWS WANT OPPOSITE ANSWERS, which is why they are separate tests:
//
//	acquire_lock_ms  takes a TTL'd row for a workflow that has already terminated and will never reach the release, so a segment
//	                 body must NOT be able to make it.
//	release_lock     is what a cleanup does. The cleat#4101 table says it has no refusing site "and it should not", so a segment body
//	                 MUST be able to make it, or a deferred release of a held lock would be silently skipped.

import (
	"context"
	"testing"
	"time"
)

func TestADeferSegmentBodyDoesNotAcquireALock(t *testing.T) {
	for _, backend := range registeredBackends {
		for _, inSegment := range []bool{false, true} {
			name := "ordinary session"
			if inSegment {
				name = "defer segment body"
			}
			t.Run(backend.Name()+"/"+name, func(t *testing.T) {
				store, teardown := backend.Setup(t)
				defer teardown()
				setupTestData(t, store)
				cks, ok := store.(ConcurrencyKeyStore)
				if !ok {
					t.Fatalf("UNMEASURED: %T is not a ConcurrencyKeyStore", store)
				}
				id := auditRun(t, store, "audit-acquire-lock")

				s := bodyOfADeferSegment([]EngineOption{WithConcurrencyKeyStore(cks)}, inSegment, id)
				got := s.AcquireLock(context.Background(), nil, "audit-lock-key", int64(time.Hour/time.Millisecond))

				// Release is the probe: it reports whether a row held by THIS workflow was removed.
				held, err := cks.ReleaseConcurrencyKey(context.Background(), "audit-lock-key", id)
				if err != nil {
					t.Fatalf("UNMEASURED: ReleaseConcurrencyKey: %v", err)
				}
				if !inSegment {
					if !held {
						t.Fatalf("control: an ordinary acquire_lock left no held key (returned %#x), so this probe cannot see one", got)
					}
					return
				}
				if held {
					t.Errorf("a defer segment's body took a lock (returned %#x).\n\n"+
						"The key is a TTL'd row in the concurrency-key store, taken on behalf of a workflow that has already "+
						"terminated and will never reach the release; it stays held until the TTL passes.", got)
				}
				if got != callSuspendSentinel {
					t.Errorf("returned %#x, want the stop sentinel %#x", got, callSuspendSentinel)
				}
			})
		}
	}
}

func TestADeferSegmentBodyCanReleaseALockItHolds(t *testing.T) {
	for _, backend := range registeredBackends {
		for _, inSegment := range []bool{false, true} {
			name := "ordinary session"
			if inSegment {
				name = "defer segment body"
			}
			t.Run(backend.Name()+"/"+name, func(t *testing.T) {
				store, teardown := backend.Setup(t)
				defer teardown()
				setupTestData(t, store)
				cks, ok := store.(ConcurrencyKeyStore)
				if !ok {
					t.Fatalf("UNMEASURED: %T is not a ConcurrencyKeyStore", store)
				}
				id := auditRun(t, store, "audit-release-lock")

				// Seed the row the release would remove, held by this workflow, BEFORE reading any result: a release of nothing
				// reports success whether or not the call did anything.
				acquired, err := cks.AcquireConcurrencyKey(context.Background(), "audit-release-key", id, time.Hour)
				if err != nil || !acquired {
					t.Fatalf("UNMEASURED: could not seed the held lock (acquired=%v err=%v)", acquired, err)
				}

				s := bodyOfADeferSegment([]EngineOption{WithConcurrencyKeyStore(cks)}, inSegment, id)
				got := s.ReleaseLock(context.Background(), nil, "audit-release-key")

				// Probe: releasing again reports whether the row was still there.
				stillHeld, err := cks.ReleaseConcurrencyKey(context.Background(), "audit-release-key", id)
				if err != nil {
					t.Fatalf("UNMEASURED: ReleaseConcurrencyKey: %v", err)
				}
				if stillHeld {
					t.Errorf("release_lock left the lock held (returned %#x, in segment body: %v): a deferred release of a held lock would be skipped", got, inSegment)
				}
				if got != 0 {
					t.Errorf("release_lock returned %#x, want 0", got)
				}
			})
		}
	}
}
