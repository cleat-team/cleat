package engine

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

func (s *execSession) AcquireLock(ctx context.Context, m api.Module, key string, ttlMs int64) int64 {
	if s.isReplay {
		return s.replayAcquireLock(ctx, m, key, ttlMs)
	}
	return s.freshAcquireLock(ctx, m, key, ttlMs)
}

// freshAcquireLock holds the refusal, so that BOTH ways in reach it: AcquireLock when the session is not replaying, and
// replayAcquireLock at the replay frontier. A defer segment replays its recorded history and so starts in replay; its body's first
// new call arrives through replayAcquireLock, and the refusal used to be only in AcquireLock's non-replay branch, which that call
// never reaches (cleat#4101, measured against a real store: the segment body took the lock). freshSetScope has the same shape for the
// same reason (cleat#4091).
func (s *execSession) freshAcquireLock(ctx context.Context, m api.Module, key string, ttlMs int64) int64 {
	// A fresh acquire is new work: it takes a distributed lock with a TTL, on
	// behalf of a workflow that has already terminated and will never reach the
	// release. A defer segment that got past here would leave the key held
	// until the TTL expired, which is the resource-leak shape 3.112 fixed from
	// the other direction (terminate released the locks its defers were for).
	if s.stopBeforeNewWork() {
		return callSuspendSentinel
	}
	var acquired bool
	if s.engine.concurrencyKeyStore != nil {
		// Check concurrency key quota before acquiring.
		if s.engine.maxQuotaConcurrencyKeys > 0 && s.engine.workflowStore != nil {
			count, err := s.engine.workflowStore.GetConcurrencyKeyCount(ctx, s.workflowID)
			if err != nil {
				s.engine.log().ErrorContext(ctx, "concurrency key quota check failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
				rec := EventRecord{
					Step:         s.stepCount,
					EventType:    EventTypeAcquireLock,
					LockKey:      key,
					LockTTLMs:    ttlMs,
					LockAcquired: 0,
					Err:          err.Error(),
				}
				s.recordEvent(rec)
				return packAcquireLockResult(false, 1)
			}
			if count >= s.engine.maxQuotaConcurrencyKeys {
				errMsg := fmt.Sprintf("workflow %s: concurrency key quota exceeded (current %d, max %d)", s.workflowID, count, s.engine.maxQuotaConcurrencyKeys)
				s.engine.log().ErrorContext(ctx, errMsg, "workflow_id", s.workflowID, "tenant_id", s.tenantID)
				rec := EventRecord{
					Step:         s.stepCount,
					EventType:    EventTypeAcquireLock,
					LockKey:      key,
					LockTTLMs:    ttlMs,
					LockAcquired: 0,
					Err:          errMsg,
				}
				s.recordEvent(rec)
				return packAcquireLockResult(false, 1)
			}
		}

		var err error
		acquired, err = s.engine.concurrencyKeyStore.AcquireConcurrencyKey(ctx, key, s.workflowID, guestMsDuration(ttlMs))
		if err != nil {
			rec := EventRecord{
				Step:         s.stepCount,
				EventType:    EventTypeAcquireLock,
				LockKey:      key,
				LockTTLMs:    ttlMs,
				LockAcquired: 0,
				Err:          err.Error(),
			}
			s.recordEvent(rec)
			return packAcquireLockResult(false, 1)
		}
	}

	a := 0
	if acquired {
		a = 1
	}
	rec := EventRecord{
		Step:         s.stepCount,
		EventType:    EventTypeAcquireLock,
		LockKey:      key,
		LockTTLMs:    ttlMs,
		LockAcquired: a,
	}
	s.recordEvent(rec)

	return packAcquireLockResult(acquired, 0)
}

func (s *execSession) replayAcquireLock(ctx context.Context, m api.Module, key string, ttlMs int64) int64 {
	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		// The type is checked BEFORE the step is consumed, as every other replay does: advancing first swallowed an event that belongs to another
		// call and carried on in replay one step out of line, with no divergence reported (cleat#4201, measured).
		if rec.EventType != EventTypeAcquireLock {
			s.replayDivergence(ctx, m, EventTypeAcquireLock, 0, 0)
			return packAcquireLockResult(false, 1)
		}
		if !s.advanceReplayStep(ctx, &rec) {
			return 0
		}
		if rec.Err != "" {
			return packAcquireLockResult(false, 1)
		}
		return packAcquireLockResult(rec.LockAcquired != 0, 0)
	}
	s.exitReplay()
	return s.freshAcquireLock(ctx, m, key, ttlMs)
}

func (s *execSession) ReleaseLock(ctx context.Context, m api.Module, key string) int64 {
	if s.isReplay {
		return s.replayReleaseLock(ctx, m, key)
	}
	return s.freshReleaseLock(ctx, m, key)
}

func (s *execSession) freshReleaseLock(ctx context.Context, m api.Module, key string) int64 {
	notHeld := false
	if s.engine.concurrencyKeyStore != nil {
		// s.workflowID is the holder the store compares against. The key itself
		// is guest-supplied and arbitrary, so without this the guest could name
		// any key in its tenant and the DELETE would take it (cleat#1188).
		released, err := s.engine.concurrencyKeyStore.ReleaseConcurrencyKey(ctx, key, s.workflowID)
		if err != nil {
			rec := EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeReleaseLock,
				LockKey:   key,
				Err:       err.Error(),
			}
			s.recordEvent(rec)
			return int64(1)
		}
		notHeld = !released
	}

	// Releasing something this workflow does not hold stays a SUCCESS, and that
	// is deliberate rather than inherited. A key whose TTL has passed is already
	// gone; the workflow releasing it has done nothing wrong and an error there
	// is one the guest cannot act on. TestPostgresStore_ReleaseConcurrencyKey_
	// NonExistent has asserted that contract since before this change.
	//
	// What was missing was any trace of it, which is why LockNotHeld is
	// recorded: a release that matched nothing and a release that freed a lock
	// were previously the same event.
	rec := EventRecord{
		Step:        s.stepCount,
		EventType:   EventTypeReleaseLock,
		LockKey:     key,
		LockNotHeld: notHeld,
	}
	s.recordEvent(rec)

	return 0
}

func (s *execSession) replayReleaseLock(ctx context.Context, m api.Module, key string) int64 {
	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		// Type first, then consume: see replayAcquireLock (cleat#4201).
		if rec.EventType != EventTypeReleaseLock {
			return s.replayDivergence(ctx, m, EventTypeReleaseLock, 0, 0)
		}
		if !s.advanceReplayStep(ctx, &rec) {
			return 0
		}
		if rec.Err != "" {
			return int64(1)
		}
		return 0
	}
	s.exitReplay()
	return s.freshReleaseLock(ctx, m, key)
}

// ---- SideEffect ----
