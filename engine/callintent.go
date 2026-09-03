package engine

// Write-ahead call intent: the engine half.
//
// IMPROVEMENT-PLAN 1.4 phase D; design in docs/durable-call-intent-design.md.
// The store half is in store_intent.go.

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// CallSemantics is the durability guarantee an operation asks for.
//
// The right guarantee depends on the operation and only the workflow author
// knows which one applies: a GET is safe to repeat, a card charge is not, and a
// card charge with an idempotency key is. One global policy cannot be correct
// for all three, and the costs differ by an order of magnitude -- so this is
// declared per operation, with a default that is exactly today's behaviour.
type CallSemantics int

const (
	// AtLeastOnce is the default and costs nothing extra: dispatch, then
	// record. A crash in between re-executes the call on replay. This is what
	// docs/durable-calls.md has always documented.
	AtLeastOnce CallSemantics = iota

	// WriteAheadIntent commits a pending row before dispatching, so replay
	// after a crash can report the outcome as ambiguous rather than silently
	// repeating the call. Costs one extra synchronous round trip per call.
	WriteAheadIntent
)

// intentOpKey is the "service.operation" key an operation is declared under.
func intentOpKey(service, operation string) string {
	return service + "." + operation
}

// WithWriteAheadIntentOps declares operations that must use WriteAheadIntent,
// as "service.operation" strings.
//
// Declared on the engine rather than at the call site because the guest-facing
// ABI has no room for a per-call argument: adding one means changing the host
// function signature and every SDK that binds it. The design doc's open
// question 9.1 prefers "both, with the call site winning"; this is the half
// that can be built without an ABI change, and nothing here forecloses the
// other half.
func WithWriteAheadIntentOps(ops ...string) EngineOption {
	return func(e *Engine) {
		if e.intentOps == nil {
			e.intentOps = make(map[string]bool, len(ops))
		}
		for _, op := range ops {
			if op = strings.TrimSpace(op); op != "" {
				e.intentOps[op] = true
			}
		}
	}
}

// callSemantics reports the guarantee declared for one operation.
func (e *Engine) callSemantics(service, operation string) CallSemantics {
	if len(e.intentOps) > 0 && e.intentOps[intentOpKey(service, operation)] {
		return WriteAheadIntent
	}
	return AtLeastOnce
}

// intentStore returns the store's write-ahead intent implementation, or an
// error explaining why this engine cannot honour the guarantee.
//
// It fails rather than falling back to AtLeastOnce. An operation declared
// WriteAheadIntent that quietly runs at-least-once is the precise failure mode
// this whole item exists to remove: a durability guarantee that is configured,
// believed, and absent. A loud failure on the first call is recoverable by
// fixing the configuration; a silent downgrade is discovered by a duplicate
// charge.
func (e *Engine) intentStore() (callIntentStore, error) {
	if e.db == nil && e.workflowStore == nil {
		return nil, fmt.Errorf("no store configured")
	}
	st, ok := e.workflowStore.(callIntentStore)
	if !ok {
		return nil, fmt.Errorf("store %T does not implement write-ahead call intent", e.workflowStore)
	}
	return st, nil
}

// isPendingIntent reports whether a replayed event was left mid-flight by a
// crash: the call was dispatched and the outcome never recorded.
//
// One source. Pending is read from intent_at and checksum by LoadEventHistory,
// which is what IMPROVEMENT-PLAN 1.4 phase D made the live representation.
//
// This used to also match a "__CLEAT_PENDING_INTENT__" sentinel in Err, the
// representation the deleted flushCallIntent would have written. Nothing in any
// deployment ever wrote it -- the write side was deleted rather than wired in,
// because every completion path guarded its upsert on `error IS NULL`, so a
// sentinel row could never be completed and would have stuck forever. The
// comment here said it should go when phase E retired the constant; E and F are
// both done, so it has (1.4 phase F tail).
func (r EventRecord) isPendingIntent() bool {
	return r.Pending
}

// ambiguousCall identifies the replayed call that was left mid-flight. It is
// the call an operator has to go and reconcile against the external service,
// so the fields here are the ones that name it in a support conversation.
type ambiguousCall struct {
	Step    int
	Service string
	Op      string
}

// recordAmbiguity notes that replay handed the guest an unresolved pending
// intent. First one wins: if a workflow hits several, the earliest is the one
// whose side effect is in doubt for the longest, and reporting a later call
// would point reconciliation at the wrong operation.
//
// This is deliberately separate from the "[AMBIGUOUS]" text written into the
// guest-visible result. That text is an English sentence, and until this
// existed it was the *only* record of the condition -- callers detected it by
// substring, so rewording the message silently disabled the detection.
func (s *execSession) recordAmbiguity(rec EventRecord) {
	if s.ambiguity == nil {
		s.ambiguity = &ambiguousCall{Step: rec.Step, Service: rec.Service, Op: rec.Op}
	}
}

// classifyFailure tags a failed execution with the reason only the host
// session saw. Today that is exactly one case: replay hit a pending intent
// that no resolver could settle, the guest turned it into a failure, and the
// resulting error would otherwise be stored as error_code='unknown' -- the
// same value as every ordinary bug, so the one class of failure that needs a
// human to check an external service could not be queried for.
//
// A workflow that catches the ambiguous call and completes anyway is not a
// failure and is not classified; err == nil passes straight through.
func (s *execSession) classifyFailure(err error) error {
	if err == nil || s.ambiguity == nil {
		return err
	}
	// Empty op and workflowID: this wrap carries the code and leaves the
	// message exactly as it was built. See CleatError.Error.
	return NewAmbiguousError("", "", err)
}

// freshCallWithIntent is freshCall for an operation declared WriteAheadIntent.
//
// The ordering is the entire feature:
//
//	commit intent  ->  dispatch  ->  commit outcome
//
// A crash between the first and third leaves a pending row, which replay
// reports as ambiguous instead of calling the service a second time.
//
// It deliberately does not go through recordEvent. recordEvent flushes through
// the adaptive flusher or flushEvent, and an event that reached both paths
// would be written twice with two different checksum chains. The design calls
// for a branch here and for asserting that the two paths are exclusive; this
// function is that branch, and recordEventPersisted is the bookkeeping half of
// recordEvent with the flush removed.
func (s *execSession) freshCallWithIntent(ctx context.Context, service, operation, requestJSON string, step int) (string, error) {
	st, err := s.engine.intentStore()
	if err != nil {
		// Non-retryable by construction: retrying cannot make the store
		// implement the interface, and the caller must not dispatch.
		return "", fmt.Errorf("call %s.%s is declared write-ahead-intent but this engine cannot honour it: %w",
			service, operation, err)
	}

	intent := EventRecord{
		Step:      step,
		EventType: EventTypeCall,
		Service:   service,
		Op:        operation,
		Request:   requestJSON,
	}
	if err := st.WriteCallIntent(ctx, s.workflowID, intent, s.engine.workerID, s.engine.generation); err != nil {
		// The call has NOT been dispatched. That is the correct outcome of a
		// failed intent write: without a durable intent, a crash mid-call
		// would be indistinguishable from a call that never happened, which
		// is the state this operation was declared to avoid.
		return "", fmt.Errorf("write-ahead intent for %s.%s at step %d: %w", service, operation, step, err)
	}

	resp, callErr := s.callService(ctx, service, operation, requestJSON, step)

	rec := intent
	rec.Response = resp
	if callErr != nil {
		rec.Err = callErr.Error()
	}
	// ErrNonRetryable is deliberately left unset, exactly as the AtLeastOnce
	// freshCall leaves it. Only the retry path has a policy to classify
	// against, and the governing constraint for this stream is that a fresh
	// run and its replay classify identically -- so this path must record what
	// the path it mirrors records, and nothing more.
	rec.TimestampMs = time.Now().UnixMilli()

	payload, _ := eventRecordToPayload(rec)
	checksum := computeEventChecksum(rec, s.lastChecksum)
	if err := st.CompleteCallIntent(ctx, s.workflowID, rec, payload, checksum, s.engine.workerID, s.engine.generation); err != nil {
		// The call HAS been dispatched and its outcome is not durable. Say so
		// rather than returning the response as though it were recorded: a
		// replay will find the pending row and report ambiguity, and a caller
		// that believed this succeeded would disagree with its own history.
		s.engine.log().ErrorContext(ctx, "call intent completion failed",
			"workflow_id", s.workflowID, "step", step, "service", service, "operation", operation, "error", err)
		return "", fmt.Errorf("completing write-ahead intent for %s.%s at step %d: %w", service, operation, step, err)
	}

	s.recordEventPersisted(rec, checksum)
	return resp, callErr
}

// recordEventPersisted is recordEvent's bookkeeping for an event that is
// already durable: it advances the session's history, step counter and
// checksum chain without flushing.
func (s *execSession) recordEventPersisted(rec EventRecord, checksum string) {
	if rec.TimestampMs == 0 {
		rec.TimestampMs = time.Now().UnixMilli()
	}
	s.nowMs = rec.TimestampMs
	s.history = append(s.history, rec)
	s.stepCount++
	atomic.AddInt64(&freshStepCount, 1)
	s.lastChecksum = checksum
}

// ---------------------------------------------------------------------------
// Resolution (IMPROVEMENT-PLAN 1.4 phase E)
// ---------------------------------------------------------------------------

// AmbiguityResolver answers the question a crash leaves open: did the call
// actually happen, and what did it return?
//
// Detection on its own converts a rare silent duplicate into a rare permanent
// failure, which for some workloads is worse. A workflow that learns its
// outcome is unknown, and has no way to find out, is stuck. This is the way
// out that costs nothing when it is not needed: most services that accept an
// idempotency key can also be asked what happened to one.
type AmbiguityResolver interface {
	// ResolveCall reports the outcome of the operation identified by
	// idempotencyKey, which is the key the original attempt sent.
	//
	// resolved=false means "cannot say" and is not an error: the service may
	// have no record, or no way to look one up. The engine reports the
	// ambiguity to the workflow, exactly as it does without a resolver.
	//
	// An error means the lookup itself failed. It is treated the same as
	// "cannot say" -- an unreachable resolver must not turn a recoverable
	// ambiguity into a different failure -- but it is logged, because a
	// resolver that always errors is indistinguishable from one that never
	// resolves anything.
	ResolveCall(ctx context.Context, service, operation, idempotencyKey string) (response string, resolved bool, err error)
}

// WithAmbiguityResolver sets the resolver consulted when replay finds a call
// that was dispatched but whose outcome was never recorded.
func WithAmbiguityResolver(r AmbiguityResolver) EngineOption {
	return func(e *Engine) { e.ambiguityResolver = r }
}

// resolveAmbiguity attempts to turn a pending intent row into a completed one.
//
// It returns the resolved response and true only when the resolver answered
// AND the outcome was durably recorded. A resolution that could not be
// persisted is deliberately not used: the workflow would proceed on it now and
// the next replay would find the row still pending and ask again, which is the
// determinism divergence this whole stream exists to prevent.
func (s *execSession) resolveAmbiguity(ctx context.Context, rec EventRecord) (string, bool) {
	r := s.engine.ambiguityResolver
	if r == nil {
		return "", false
	}

	key := DurableCallIdempotencyKey(s.workflowID, s.execRunID, rec.Step)
	resp, resolved, err := r.ResolveCall(ctx, rec.Service, rec.Op, key)
	if err != nil {
		// Not fatal: an unreachable resolver leaves the ambiguity exactly as
		// it was, which is the state this is trying to improve on and not a
		// worse one. Logged because a resolver that always fails looks
		// identical to one that never has an answer.
		s.engine.log().WarnContext(ctx, "ambiguity resolver failed",
			"workflow_id", s.workflowID, "step", rec.Step,
			"service", rec.Service, "operation", rec.Op, "error", err)
		return "", false
	}
	if !resolved {
		return "", false
	}

	store, ok := s.engine.workflowStore.(callIntentResolver)
	if !ok {
		s.engine.log().WarnContext(ctx, "ambiguity resolved but the store cannot record it; reporting ambiguity instead",
			"workflow_id", s.workflowID, "step", rec.Step, "store", fmt.Sprintf("%T", s.engine.workflowStore))
		return "", false
	}

	completed := rec
	completed.Response = resp
	completed.Err = ""
	completed.Pending = false
	if completed.TimestampMs == 0 {
		completed.TimestampMs = time.Now().UnixMilli()
	}
	payload, _ := eventRecordToPayload(completed)

	// The replay path usually resolves the last row, where chainRepairsAfter
	// returns nothing -- but not always: a signal delivered while the workflow
	// was down lands above the pending call, and then the chain needs the same
	// repair the operator path needs. IMPROVEMENT-PLAN 3.89.
	if err := store.ResolveCallIntent(ctx, s.workflowID, completed, payload,
		s.engine.workerID, s.engine.generation, chainRepairsAfter(s.history, rec.Step)); err != nil {
		s.engine.log().ErrorContext(ctx, "ambiguity was resolved but could not be recorded; reporting ambiguity instead",
			"workflow_id", s.workflowID, "step", rec.Step, "error", err)
		return "", false
	}

	s.engine.log().InfoContext(ctx, "ambiguous call resolved",
		"workflow_id", s.workflowID, "step", rec.Step,
		"service", rec.Service, "operation", rec.Op)
	return resp, true
}
