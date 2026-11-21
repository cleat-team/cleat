package engine

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// AcquireLock and ReleaseLock are steps (cleat#4211): the dispatcher consumes the recorded event, checks it names THIS lock, and decides whether
// fresh work is allowed; the specs below hold only what is particular to a lock.

func (s *execSession) AcquireLock(ctx context.Context, m api.Module, key string, ttlMs int64) int64 {
	return s.step(ctx, m, &acquireLockStep, stepArgs{s1: key, i1: ttlMs})
}

func (s *execSession) ReleaseLock(ctx context.Context, m api.Module, key string) int64 {
	return s.step(ctx, m, &releaseLockStep, stepArgs{s1: key})
}

// lockIdentity compares the lock a call asks for with the one the recorded event names (cleat#4211). Without it a replay accepted ANY recorded
// acquire_lock or release_lock at this step, consumed it, and answered as though it were this call: a workflow whose code now takes lock "b"
// where its history took lock "a" was told it held "b" while the store held "a". The key is guest-supplied and recorded in every event these
// calls write, on both the success and the error paths, persisted as the payload's lock_key and carried through compaction (ChildName), so it is
// the one field that cannot legitimately differ between a run and its replay.
func lockIdentity(a stepArgs, rec *EventRecord) string {
	if rec.LockKey != a.s1 {
		return fmt.Sprintf("lock %q, recorded %q", a.s1, rec.LockKey)
	}
	return ""
}

// acquireLockStep is AcquireLock as a step. A fresh acquire is new work: it takes a distributed lock with a TTL, on behalf of a workflow that
// may already have terminated and will never reach the release. A defer segment's body that got past the refusal would leave the key held until
// the TTL expired, which is the resource-leak shape 3.112 fixed from the other direction (terminate released the locks its defers were for).
// The refusal is the dispatcher's, and it covers BOTH ways in: AcquireLock when the session is not replaying, and the replay frontier, which is
// where a defer segment's body reaches its first new call because a segment starts in replay (cleat#4101, measured against a real store: the
// segment body took the lock when the refusal lived only in the non-replay branch).
var acquireLockStep = stepSpec{
	Name:               "acquire_lock",
	NoDivergenceDetail: "its old message was the shared one: the step and both event types; " + divergenceSweepNote,
	Result:             layoutSimple,
	Effect:             effectEffect,
	Records:            recordsOne,
	Want:               EventTypeAcquireLock,
	Identity:           lockIdentity,
	Decode: func(c *stepCtx, rec *EventRecord) int64 {
		if rec.Err != "" {
			return packAcquireLockResult(false, 1)
		}
		return packAcquireLockResult(rec.LockAcquired != 0, 0)
	},
	Fresh: func(c *stepCtx) int64 {
		s, ctx, key, ttlMs := c.s, c.ctx, c.args.s1, c.args.i1
		var acquired bool
		if s.engine.concurrencyKeyStore != nil {
			// Check concurrency key quota before acquiring.
			if s.engine.maxQuotaConcurrencyKeys > 0 && s.engine.workflowStore != nil {
				count, err := s.engine.workflowStore.GetConcurrencyKeyCount(ctx, s.workflowID)
				if err != nil {
					s.engine.log().ErrorContext(ctx, "concurrency key quota check failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
					c.Record(EventRecord{
						Step:         s.stepCount,
						EventType:    EventTypeAcquireLock,
						LockKey:      key,
						LockTTLMs:    ttlMs,
						LockAcquired: 0,
						Err:          err.Error(),
					})
					return packAcquireLockResult(false, 1)
				}
				if count >= s.engine.maxQuotaConcurrencyKeys {
					errMsg := fmt.Sprintf("workflow %s: concurrency key quota exceeded (current %d, max %d)", s.workflowID, count, s.engine.maxQuotaConcurrencyKeys)
					s.engine.log().ErrorContext(ctx, errMsg, "workflow_id", s.workflowID, "tenant_id", s.tenantID)
					c.Record(EventRecord{
						Step:         s.stepCount,
						EventType:    EventTypeAcquireLock,
						LockKey:      key,
						LockTTLMs:    ttlMs,
						LockAcquired: 0,
						Err:          errMsg,
					})
					return packAcquireLockResult(false, 1)
				}
			}

			var err error
			acquired, err = s.engine.concurrencyKeyStore.AcquireConcurrencyKey(ctx, key, s.workflowID, guestMsDuration(ttlMs))
			if err != nil {
				c.Record(EventRecord{
					Step:         s.stepCount,
					EventType:    EventTypeAcquireLock,
					LockKey:      key,
					LockTTLMs:    ttlMs,
					LockAcquired: 0,
					Err:          err.Error(),
				})
				return packAcquireLockResult(false, 1)
			}
		}

		a := 0
		if acquired {
			a = 1
		}
		c.Record(EventRecord{
			Step:         s.stepCount,
			EventType:    EventTypeAcquireLock,
			LockKey:      key,
			LockTTLMs:    ttlMs,
			LockAcquired: a,
		})

		return packAcquireLockResult(acquired, 0)
	},
}

func init() { registerStepSpec(&acquireLockStep) }

// releaseLockStep is ReleaseLock as a step, and the one that is NEVER refused (Refuse: neverRefuse, which carries the measurement). Measured before the move, a_lock_call_in_each_stop_state_test.go:
// in a segment's body, after a worker shutdown, in a defer draining on a shutting-down worker and after an event that could not be written, AcquireLock is
// refused and ReleaseLock releases and records. A cleanup that cannot give a lock back leaves it held for its lease, so the move keeps that and
// declares it, where it used to be an entry in a list of exemptions. Whether a segment's body should be able to release is a separate decision.
var releaseLockStep = stepSpec{
	Name:               "release_lock",
	NoDivergenceDetail: "its old message was the shared one: the step and both event types; " + divergenceSweepNote,
	Result:             layoutSimple,
	Effect:             effectEffect,
	Refuse:             neverRefuse,
	Records:            recordsOne,
	Want:               EventTypeReleaseLock,
	Identity:           lockIdentity,
	// Not a stop site: after a suspension it answers the ordinary error code its decoders know (cleat#4285). Nothing is released, so the lock stays held until
	// its lease runs out, which is the cost of a guest that ignored the suspension.
	AfterSuspend: func(*stepCtx) int64 { return int64(1) },
	Decode: func(c *stepCtx, rec *EventRecord) int64 {
		if rec.Err != "" {
			return int64(1)
		}
		return 0
	},
	Fresh: func(c *stepCtx) int64 {
		s, ctx, key := c.s, c.ctx, c.args.s1
		notHeld := false
		if s.engine.concurrencyKeyStore != nil {
			// s.workflowID is the holder the store compares against. The key itself
			// is guest-supplied and arbitrary, so without this the guest could name
			// any key in its tenant and the DELETE would take it (cleat#1188).
			released, err := s.engine.concurrencyKeyStore.ReleaseConcurrencyKey(ctx, key, s.workflowID)
			if err != nil {
				c.Record(EventRecord{
					Step:      s.stepCount,
					EventType: EventTypeReleaseLock,
					LockKey:   key,
					Err:       err.Error(),
				})
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
		c.Record(EventRecord{
			Step:        s.stepCount,
			EventType:   EventTypeReleaseLock,
			LockKey:     key,
			LockNotHeld: notHeld,
		})

		return 0
	},
}

func init() { registerStepSpec(&releaseLockStep) }

// ---- SideEffect ----
