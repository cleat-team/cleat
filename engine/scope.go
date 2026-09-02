package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero/api"
)

func (s *execSession) SetScope(ctx context.Context, m api.Module, objectType, instanceKey string, prevScopePtr, prevScopeMaxLen uint32) int64 {
	if s.isReplay {
		return s.replaySetScope(ctx, m, objectType, instanceKey, prevScopePtr, prevScopeMaxLen)
	}
	return s.freshSetScope(ctx, m, objectType, instanceKey, prevScopePtr, prevScopeMaxLen)
}

// forgetHeldScope drops scopeKey from the held set WITHOUT releasing it.
//
// The two halves of "the workflow no longer holds this scope" are separable on
// purpose. Releasing is a side effect on the concurrency-key store and must
// happen exactly once, in the segment that originally ran the step; forgetting
// is Go-side bookkeeping that every replay of that step has to reproduce, or
// releaseHeldScopes frees a key the workflow already gave up.
func (s *execSession) forgetHeldScope(scopeKey string) {
	for i, held := range s.heldScopes {
		if held == scopeKey {
			s.heldScopes = append(s.heldScopes[:i], s.heldScopes[i+1:]...)
			return
		}
	}
}

func (s *execSession) ClearScope(ctx context.Context) {
	if s.scopeSet && s.scopePrefix != "" {
		scopeKey := "vo:" + s.scopeObjType + ":" + s.scopeInstKey
		if s.engine.concurrencyKeyStore != nil {
			if err := s.engine.concurrencyKeyStore.ReleaseConcurrencyKey(ctx, scopeKey); err != nil {
				s.engine.log().ErrorContext(ctx, "release_concurrency_key failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
			}
		}
		s.forgetHeldScope(scopeKey)
	}
	s.scopeSet = false
	s.scopePrefix = ""
	s.scopeObjType = ""
	s.scopeInstKey = ""
}

func (s *execSession) freshSetScope(ctx context.Context, m api.Module, objectType, instanceKey string, prevScopePtr, prevScopeMaxLen uint32) int64 {

	// Save previous scope prefix to output buffer.
	prevScope := ""
	if s.scopeSet && s.scopePrefix != "" {
		prevScope = s.scopePrefix
		_, _ = s.writeResult(ctx, m, prevScopePtr, prevScope, prevScopeMaxLen)
	}

	if objectType == "" && instanceKey == "" {
		s.ClearScope(ctx)
		return 0
	}

	// If switching from an existing scope, release the old key first.
	if s.scopeSet && s.scopePrefix != "" {
		oldKey := "vo:" + s.scopeObjType + ":" + s.scopeInstKey
		if s.engine.concurrencyKeyStore != nil {
			if err := s.engine.concurrencyKeyStore.ReleaseConcurrencyKey(ctx, oldKey); err != nil {
				s.engine.log().ErrorContext(ctx, "release_concurrency_key failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
			}
		}
		s.forgetHeldScope(oldKey)
	}

	// Acquire new scope key.
	scopeKey := "vo:" + objectType + ":" + instanceKey
	if s.engine.concurrencyKeyStore != nil {
		acquired, err := s.engine.concurrencyKeyStore.AcquireConcurrencyKey(ctx, scopeKey, s.workflowID, 24*time.Hour)
		if err != nil {
			rec := EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeScopeAcquired,
				ScopeKey:  scopeKey,
				Err:       err.Error(),
			}
			s.recordEvent(rec)
			return packSimpleResult(1, 0)
		}
		if !acquired {
			rec := EventRecord{
				Step:      s.stepCount,
				EventType: EventTypeScopeAcquired,
				ScopeKey:  scopeKey,
				Err:       "scope held by another workflow",
			}
			s.recordEvent(rec)
			s.suspendErr = &SuspendError{
				Reason: fmt.Sprintf("virtual object scope %s held by another workflow", scopeKey),
				Until:  time.UnixMilli(s.nowMs).Add(5 * time.Second),
			}
			return 0
		}
		s.heldScopes = append(s.heldScopes, scopeKey)
	}

	// Record successful acquisition.
	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeScopeAcquired,
		ScopeKey:  scopeKey,
	}
	s.recordEvent(rec)

	s.scopeSet = true
	s.scopeObjType = objectType
	s.scopeInstKey = instanceKey
	s.scopePrefix = "vo:" + objectType + ":" + instanceKey + ":"
	return 0
}

func (s *execSession) replaySetScope(ctx context.Context, m api.Module, objectType, instanceKey string, prevScopePtr, prevScopeMaxLen uint32) int64 {

	// Save previous scope prefix to output buffer (reconstructed from replayed scope state).
	prevScope := ""
	if s.scopeSet && s.scopePrefix != "" {
		prevScope = s.scopePrefix
		_, _ = s.writeResult(ctx, m, prevScopePtr, prevScope, prevScopeMaxLen)
	}

	if objectType == "" && instanceKey == "" {
		// Reproduce ClearScope's bookkeeping, not its release. The fresh path
		// released this key in the segment that originally ran this step;
		// re-releasing here would be a side effect during replay. Dropping it
		// from heldScopes is what replay owes, and skipping that left
		// releaseHeldScopes to free a key the workflow had explicitly cleared
		// -- which, once another workflow has acquired that virtual object, is
		// releasing somebody else's lock.
		if s.scopeSet && s.scopePrefix != "" {
			s.forgetHeldScope("vo:" + s.scopeObjType + ":" + s.scopeInstKey)
		}
		s.scopeSet = false
		s.scopePrefix = ""
		s.scopeObjType = ""
		s.scopeInstKey = ""
		return 0
	}

	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if !s.advanceReplayStep(ctx, &rec) {
			return 0
		}

		if rec.EventType != EventTypeScopeAcquired {
			return packSimpleResult(1, 0)
		}

		if rec.Err != "" {
			// Previous attempt failed.
			// Do not set scope fields; switch to fresh to retry acquisition.
			s.exitReplay()
			return s.freshSetScope(ctx, m, objectType, instanceKey, prevScopePtr, prevScopeMaxLen)
		}

		// Acquisition was successful.
		//
		// Switching from another object: the fresh path released the previous
		// key and dropped it from the held set. Replay must reproduce the
		// second half only -- the release happened in the segment that
		// originally ran this step -- or releaseHeldScopes frees a key this
		// workflow gave up when it switched away, which is another workflow's
		// lock once that object has been re-acquired. Found by the
		// fresh-vs-replay parity property test on its first run;
		// IMPROVEMENT-PLAN 3.69.
		if s.scopeSet && s.scopePrefix != "" {
			s.forgetHeldScope("vo:" + s.scopeObjType + ":" + s.scopeInstKey)
		}
		s.scopeSet = true
		s.scopeObjType = objectType
		s.scopeInstKey = instanceKey
		s.scopePrefix = "vo:" + objectType + ":" + instanceKey + ":"
		s.heldScopes = append(s.heldScopes, "vo:"+objectType+":"+instanceKey)
		return 0
	}

	// Past recorded history -- switch to fresh execution.
	s.exitReplay()
	return s.freshSetScope(ctx, m, objectType, instanceKey, prevScopePtr, prevScopeMaxLen)
}

func (s *execSession) releaseHeldScopes(ctx context.Context) {
	if s.engine.concurrencyKeyStore == nil {
		return
	}
	for _, scopeKey := range s.heldScopes {
		if err := s.engine.concurrencyKeyStore.ReleaseConcurrencyKey(ctx, scopeKey); err != nil {
			s.engine.log().ErrorContext(ctx, "release_concurrency_key failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
		}
	}
	s.heldScopes = nil
}

func (s *execSession) GetScope(ctx context.Context, m api.Module, objTypePtr, objTypeMaxLen, instKeyPtr, instKeyMaxLen uint32) int64 {

	var objTypeLen, instKeyLen uint32
	if s.scopeSet {
		objTypeLen, _ = s.writeResult(ctx, m, objTypePtr, s.scopeObjType, objTypeMaxLen)
		instKeyLen, _ = s.writeResult(ctx, m, instKeyPtr, s.scopeInstKey, instKeyMaxLen)
	}

	return int64(uint64(objTypeLen)<<32 | uint64(instKeyLen))
}
