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
// Two sources, deliberately. Pending is the live one, read from intent_at and
// checksum by LoadEventHistory. pendingSentinel is the representation the
// deleted flushCallIntent would have written; nothing in any deployment ever
// wrote it, but the detector for it predates this work, tests/integrity
// exercises it directly, and keeping it costs one comparison. It should go when
// phase E retires the constant.
func (r EventRecord) isPendingIntent() bool {
	return r.Pending || r.Err == pendingSentinel
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
	if err := st.WriteCallIntent(ctx, s.workflowID, intent); err != nil {
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
	if err := st.CompleteCallIntent(ctx, s.workflowID, rec, payload, checksum); err != nil {
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
