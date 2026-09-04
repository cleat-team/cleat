package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero/api"
)

func (s *execSession) AcquireLock(ctx context.Context, m api.Module, key string, ttlMs int64) int64 {
	if s.isReplay {
		return s.replayAcquireLock(ctx, m, key, ttlMs)
	}
	// A fresh acquire is new work: it takes a distributed lock with a TTL, on
	// behalf of a workflow that has already terminated and will never reach the
	// release. A defer segment that got past here would leave the key held
	// until the TTL expired, which is the resource-leak shape 3.112 fixed from
	// the other direction (terminate released the locks its defers were for).
	if s.stopBeforeNewWork() {
		return callSuspendSentinel
	}
	return s.freshAcquireLock(ctx, m, key, ttlMs)
}

func (s *execSession) freshAcquireLock(ctx context.Context, m api.Module, key string, ttlMs int64) int64 {
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
		acquired, err = s.engine.concurrencyKeyStore.AcquireConcurrencyKey(ctx, key, s.workflowID, time.Duration(ttlMs)*time.Millisecond)
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
		if !s.advanceReplayStep(ctx, &rec) {
			return 0
		}

		if rec.EventType != EventTypeAcquireLock {
			return packAcquireLockResult(false, 1)
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
	if s.engine.concurrencyKeyStore != nil {
		err := s.engine.concurrencyKeyStore.ReleaseConcurrencyKey(ctx, key)
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
	}

	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeReleaseLock,
		LockKey:   key,
	}
	s.recordEvent(rec)

	return 0
}

func (s *execSession) replayReleaseLock(ctx context.Context, m api.Module, key string) int64 {
	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if !s.advanceReplayStep(ctx, &rec) {
			return 0
		}

		if rec.EventType != EventTypeReleaseLock {
			return int64(1)
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
