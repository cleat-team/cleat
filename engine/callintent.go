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
		// Stamped at CONSTRUCTION, not in recordEventPersisted like the
		// non-intent path, because this path computes the payload and the
		// checksum from `rec` before persisting it. Setting the flag after
		// that would leave the stored row and the in-memory event disagreeing
		// about a field the checksum covers -- and the intent written ahead of
		// the call has to carry it too, or a crash between the intent and its
		// completion would resume with the flag lost. cleat#1155.
		InDeferPhase: s.inDeferPhase,
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

// AmbiguityOutcome is what a resolver learned about a call whose outcome a
// crash left unrecorded.
//
// THREE STATES, NOT A BOOL. An earlier version of AmbiguityResolver answered
// with (response string, resolved bool), which can say "here is the
// response" or "cannot say" but has no way to say "the call never reached
// the service, it is safe to retry" -- a resolver that confirmed that had to
// either lie (claim resolved=true with an empty response, which a workflow
// would read as a real answer) or waste the information (report
// resolved=false, identical to a resolver that found nothing at all).
// cleat#1984.
type AmbiguityOutcome int

const (
	// AmbiguityCannotSay is the zero value: the resolver has no answer. The
	// engine reports the ambiguity to the workflow exactly as it does without
	// a resolver. Also what a resolver error degrades to -- see ResolveCall's
	// doc comment on err.
	AmbiguityCannotSay AmbiguityOutcome = iota
	// AmbiguityResolved means the call happened and response is its real
	// outcome, to be recorded and handed to the workflow as though the
	// original call had returned it.
	AmbiguityResolved
	// AmbiguityNotSent means the service has no record of the call: it never
	// reached the service, so nothing to deduplicate against exists and
	// retrying is safe. The engine records a retryable failure (the same
	// classification -- and the same guest-visible CallError.Retryable()==true
	// -- as an ordinary fresh call failure) rather than a response, and the
	// workflow's own retry handling takes it from there. response is ignored
	// for this outcome.
	AmbiguityNotSent
)

func (o AmbiguityOutcome) String() string {
	switch o {
	case AmbiguityResolved:
		return "resolved"
	case AmbiguityNotSent:
		return "not_sent"
	default:
		return "cannot_say"
	}
}

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
	// outcome=AmbiguityCannotSay is not an error: the service may have no
	// record, or no way to look one up. The engine reports the ambiguity to
	// the workflow, exactly as it does without a resolver.
	//
	// An error means the lookup itself failed. It is treated the same as
	// AmbiguityCannotSay -- an unreachable resolver must not turn a
	// recoverable ambiguity into a different failure -- but it is logged,
	// because a resolver that always errors is indistinguishable from one
	// that never resolves anything.
	ResolveCall(ctx context.Context, service, operation, idempotencyKey string) (response string, outcome AmbiguityOutcome, err error)
}

// WithAmbiguityResolver sets the resolver consulted when replay finds a call
// that was dispatched but whose outcome was never recorded.
//
// EMBEDDER API. `cleat-worker` never calls this -- confirmed by
// `grep -rln WithAmbiguityResolver --include='*.go' . | grep -v _test`, which
// returns only this file -- and that is the designed default, not a gap: a
// nil resolver makes resolveAmbiguity return ("", false) without calling
// anything, leaving the ambiguity exactly as it was and reported as such,
// "not a worse one" than the state it started in (see resolveAmbiguity's own
// comment on the same phrase, in the resolver-error branch). A worker
// deployment relies on the always-available manual path instead --
// `POST /api/admin/instances/{id}/steps/{step}/resolve` (`engine.ResolveStep`)
// followed by `engine.ReReplay` -- which needs no resolver configured.
//
// Automatic resolution is an extension point for an embedder that links the
// engine directly and can supply one: most services that accept an
// idempotency key can also answer "what happened to this one", turning most
// ambiguities into non-events instead of an operator's manual check. See
// `AmbiguityResolver`'s doc comment for the guarantee it must uphold.
//
// Recorded because it was found stale twice: two independent sessions read
// the same zero-non-test-callers grep and concluded, wrongly, that automatic
// resolution was simply unbuilt (cleat#1778). The decision already existed --
// `engine_option_reachability_test.go`'s exemption table, "EMBEDDER API: nil
// is the designed default; degrades to no-op" -- but a reachability guard's
// exemption list is not somewhere an API's own reader consults. cleat#1871.
func WithAmbiguityResolver(r AmbiguityResolver) EngineOption {
	return func(e *Engine) { e.ambiguityResolver = r }
}

// ---------------------------------------------------------------------------
// Idempotency-key replay (cleat#2897, decision (c) on cleat#1984)
// ---------------------------------------------------------------------------

// IdempotencyReplayOutcome is what re-dispatching a pending call under its
// original idempotency key found.
type IdempotencyReplayOutcome int

const (
	// IdempotencyReplayCannotSay is the zero value: the replay attempt did
	// not produce a definite answer (a transport error, a timeout, or any
	// status other than the ones below). The engine falls back to today's
	// [AMBIGUOUS] report, exactly as if no replayer were configured.
	IdempotencyReplayCannotSay IdempotencyReplayOutcome = iota
	// IdempotencyReplayResolved means the service answered with a definite
	// result -- its own key table caught the duplicate and returned the
	// original outcome, or (far less often) the retried request is itself
	// what the service executed. Either way response is the real outcome,
	// recorded and handed to the workflow exactly as resolveAmbiguity's
	// AmbiguityResolved does.
	IdempotencyReplayResolved
	// IdempotencyReplayRetryLater means the service answered 409: a request
	// under this exact key is still being processed. This is not a failure
	// -- it is the mechanism working as designed, catching the window where
	// the original attempt has not finished yet. See
	// replayUnderOriginalKey's retry loop for the bound on how long the
	// engine waits before giving up and reporting [AMBIGUOUS].
	IdempotencyReplayRetryLater
)

func (o IdempotencyReplayOutcome) String() string {
	switch o {
	case IdempotencyReplayResolved:
		return "resolved"
	case IdempotencyReplayRetryLater:
		return "retry_later"
	default:
		return "cannot_say"
	}
}

// IdempotencyKeyReplayer is implemented by a ServiceCaller that can re-issue
// an operation under its ORIGINAL idempotency key and read the service's own
// verdict directly off the response -- as opposed to AmbiguityResolver, which
// asks a SEPARATE lookup operation "what happened to this key". This is the
// mechanism decision (c) on cleat#1984 asks for: the service's own key table
// is what resolves the ambiguity, not a second query against it.
//
// Reading the status directly (rather than through ServiceCaller.Call's
// classified error) is deliberate and matches dbServiceCaller.ResolveCall's
// own reasoning: the contract here IS the status code -- 409 means something
// specific (IdempotencyReplayRetryLater) that a generic retryable/permanent
// classification would collapse into "retryable", indistinguishable from an
// ordinary 503.
type IdempotencyKeyReplayer interface {
	// ReplayUnderOriginalKey re-issues service.operation with the SAME
	// requestJSON and idempotencyKey the original (now-ambiguous) attempt
	// used. err is a transport/plumbing failure, not a service-level
	// disagreement -- the service's own verdict is outcome, exactly as
	// AmbiguityResolver.ResolveCall's err doc comment distinguishes.
	ReplayUnderOriginalKey(ctx context.Context, service, operation, requestJSON, idempotencyKey string) (response string, outcome IdempotencyReplayOutcome, err error)
}

// WithIdempotencyKeyReplayer sets the replayer consulted, before
// ambiguityResolver, when replay finds a call whose outcome was never
// recorded and whose operation is declared to support it (see
// WithIdempotencyKeyOps). `cleat-worker` wires one through
// --idempotency-key-ops; an embedder may supply its own. Nil (the default)
// means this path is never tried.
func WithIdempotencyKeyReplayer(r IdempotencyKeyReplayer) EngineOption {
	return func(e *Engine) { e.idempotencyKeyReplayer = r }
}

// WithIdempotencyKeyOps declares which "service.operation" pairs support
// idempotency-key replay: re-dispatching the SAME call under the SAME key on
// an unresolved ambiguity, rather than reporting [AMBIGUOUS] or consulting a
// separate lookup operation. Disjoint from WithAmbiguityResolver's
// configured operations by construction -- cleat-worker's boot validation
// refuses an operation declared for both, because they are different
// capabilities a service either has or does not, not a precedence to choose
// between per call.
func WithIdempotencyKeyOps(ops map[string]bool) EngineOption {
	return func(e *Engine) { e.idempotencyKeyOps = ops }
}

// DefaultIdempotencyKeyRetention is the bound applied when
// WithIdempotencyKeyRetention is not set: 24 hours, matching Stripe's own
// idempotency-key retention window -- the shortest of the providers cleat#1984
// surveyed (Adyen holds >=7 days), and so the safer default where an operator
// has not stated their service's own bound.
const DefaultIdempotencyKeyRetention = 24 * time.Hour

// WithIdempotencyKeyRetention bounds how long after the original call's
// dispatch replayUnderOriginalKey will still attempt a resend under its
// original key. Beyond a service's own key-retention window, the key has
// been forgotten and a resend is a NEW call, not a dedupe candidate -- the
// exact hazard cleat#1984's work item 3 names. Zero (including never calling
// this option) means DefaultIdempotencyKeyRetention.
func WithIdempotencyKeyRetention(d time.Duration) EngineOption {
	return func(e *Engine) { e.idempotencyKeyRetention = d }
}

// idempotencyReplayMaxAttempts bounds how many times replayUnderOriginalKey
// retries a 409 before giving up and falling back to [AMBIGUOUS]. This runs
// synchronously inside replay, on a worker goroutine a workflow instance is
// waiting on, so the bound is small and the backoff is fixed rather than
// exponential -- a 409 means "still in flight", not "back off harder", and a
// caller who needs longer than this should rely on --ambiguity-lookup or the
// manual ResolveStep path instead of blocking replay on it.
const (
	idempotencyReplayMaxAttempts = 3
	idempotencyReplayBackoff     = 2 * time.Second
)

// replayUnderOriginalKey attempts cleat#2897's mechanism (c): re-dispatch a
// pending call under its ORIGINAL idempotency key, for an operation declared
// in WithIdempotencyKeyOps, and let the service's own key table resolve it.
//
// Returns IdempotencyReplayResolved with the response only when the service
// gave a definite answer. Every other outcome -- not configured for this
// operation, retention window exceeded, a transport error, 409 exhausted its
// retry budget -- returns IdempotencyReplayCannotSay, which the caller
// (resolveAmbiguity) treats exactly like no replayer being configured at
// all: falls through to ambiguityResolver (if any) and then to [AMBIGUOUS].
func (s *execSession) replayUnderOriginalKey(ctx context.Context, rec EventRecord) (string, IdempotencyReplayOutcome) {
	r := s.engine.idempotencyKeyReplayer
	if r == nil {
		return "", IdempotencyReplayCannotSay
	}
	if !s.engine.idempotencyKeyOps[rec.Service+"."+rec.Op] {
		return "", IdempotencyReplayCannotSay
	}

	// Retention bound (cleat#1984 item 3). rec.CreatedAt is the row's
	// INSERT time -- set once, at WriteCallIntent, and never touched by
	// CompleteCallIntent -- so for a still-pending row it is exactly the
	// original dispatch time, with no new column needed: applyCreatedAt
	// (store_events.go) already loads it for every dialect.
	retention := s.engine.idempotencyKeyRetention
	if retention <= 0 {
		retention = DefaultIdempotencyKeyRetention
	}
	if !rec.CreatedAt.IsZero() {
		if age := time.Since(rec.CreatedAt); age > retention {
			s.engine.log().WarnContext(ctx, "idempotency-key replay skipped: retention window exceeded",
				"workflow_id", s.workflowID, "step", rec.Step, "service", rec.Service, "operation", rec.Op,
				"age", age, "retention", retention)
			return "", IdempotencyReplayCannotSay
		}
	}

	key := DurableCallIdempotencyKey(s.workflowID, s.execRunID, rec.Step)

	for attempt := 1; attempt <= idempotencyReplayMaxAttempts; attempt++ {
		resp, outcome, err := r.ReplayUnderOriginalKey(ctx, rec.Service, rec.Op, rec.Request, key)
		if err != nil {
			// A transport failure answers nothing about the call's outcome --
			// cannot say, not an error the caller must propagate. Same
			// reasoning as AmbiguityResolver.ResolveCall's own err doc
			// comment: logged, because a replayer that always errors looks
			// identical to one with nothing to say.
			s.engine.log().WarnContext(ctx, "idempotency-key replay failed",
				"workflow_id", s.workflowID, "step", rec.Step,
				"service", rec.Service, "operation", rec.Op, "error", err)
			return "", IdempotencyReplayCannotSay
		}
		if outcome == IdempotencyReplayResolved {
			return resp, IdempotencyReplayResolved
		}
		if outcome != IdempotencyReplayRetryLater {
			return "", IdempotencyReplayCannotSay
		}
		if attempt == idempotencyReplayMaxAttempts {
			return "", IdempotencyReplayCannotSay
		}
		select {
		case <-ctx.Done():
			return "", IdempotencyReplayCannotSay
		case <-time.After(idempotencyReplayBackoff):
		}
	}
	return "", IdempotencyReplayCannotSay
}

// ambiguityNotSentMessage is the Err text recorded (and surfaced to the
// guest) when a resolver confirms a call never reached the service. Not an
// AMBIGUOUS message: this is a definite, retryable failure, same shape as any
// other transient call error.
const ambiguityNotSentMessage = "call outcome resolved: the service has no record of this call -- it never arrived, and retrying is safe"

// resolveAmbiguity attempts to turn a pending intent row into a completed one.
//
// It returns the response and AmbiguityResolved only when the resolver found
// a real outcome AND it was durably recorded; AmbiguityNotSent only when the
// resolver confirmed the call never arrived AND that was durably recorded;
// AmbiguityCannotSay otherwise. A resolution that could not be persisted is
// deliberately not used: the workflow would proceed on it now and the next
// replay would find the row still pending and ask again, which is the
// determinism divergence this whole stream exists to prevent.
func (s *execSession) resolveAmbiguity(ctx context.Context, rec EventRecord) (string, AmbiguityOutcome) {
	r := s.engine.ambiguityResolver
	if r == nil {
		return "", AmbiguityCannotSay
	}

	key := DurableCallIdempotencyKey(s.workflowID, s.execRunID, rec.Step)
	resp, outcome, err := r.ResolveCall(ctx, rec.Service, rec.Op, key)
	if err != nil {
		// Not fatal: an unreachable resolver leaves the ambiguity exactly as
		// it was, which is the state this is trying to improve on and not a
		// worse one. Logged because a resolver that always fails looks
		// identical to one that never has an answer.
		s.engine.log().WarnContext(ctx, "ambiguity resolver failed",
			"workflow_id", s.workflowID, "step", rec.Step,
			"service", rec.Service, "operation", rec.Op, "error", err)
		return "", AmbiguityCannotSay
	}

	switch outcome {
	case AmbiguityResolved:
		if response, ok := s.persistAmbiguityResolution(ctx, rec, resp, "", "", false, outcome.String()); ok {
			return response, AmbiguityResolved
		}
	case AmbiguityNotSent:
		// Response stays empty: there is no real outcome to hand the
		// workflow, only the engine's own classification of why this attempt
		// failed. nonRetryable=false is what makes durablecalls.go's
		// replay path (recordedFailureCode) reproduce the same retryable
		// classification as a fresh call failure on every future replay of
		// this now-completed row.
		//
		// THE RETRY THIS ENABLES USES A DIFFERENT IDEMPOTENCY KEY THAN THE
		// ORIGINAL ATTEMPT (coordinator + cleat-review, cleat#1984 round 1).
		// `key` above is DurableCallIdempotencyKey(workflow, run, rec.Step)
		// -- this STEP's key. The workflow's retry is a NEW DurableCall, a
		// NEW step, and therefore a NEW key, by the exact construction
		// durablecalls.go's own comment on retryStep relies on to make the
		// engine's OWN internal attempt loop safe ("every attempt carries
		// the same idempotency key... without this the key changes at
		// exactly the moment a duplicate is most likely"). This path does
		// not have that protection: it hands the retry decision to the
		// GUEST, which cannot reuse rec.Step. So if the resolver's `404`
		// answered "not sent" because the original request was merely SLOW
		// rather than lost, a late arrival under the OLD key and the
		// retry's request under the NEW key can both execute. See
		// docs/durable-calls.md's "A 404 answer is a PROMISE" paragraph for
		// the contract requirement this places on the lookup operation.
		//
		// cleat#2897: this hazard is exactly what mechanism (c) --
		// replayUnderOriginalKey, tried before this resolver -- exists to
		// avoid for a service declared to support it: the retry there reuses
		// THIS step's key instead of handing a new one to the guest.
		if response, ok := s.persistAmbiguityResolution(ctx, rec, "", ambiguityNotSentMessage, ErrTransient.String(), false, outcome.String()); ok {
			return response, AmbiguityNotSent
		}
	}
	return "", AmbiguityCannotSay
}

// persistAmbiguityResolution records a pending row as completed with the
// given outcome, shared by resolveAmbiguity (the --ambiguity-lookup path)
// and resolveAmbiguityViaKeyReplay (cleat#2897's same-key path) so the two
// mechanisms, which answer the same question through different means, write
// it down the same way.
//
// Returns (response, true) only when the resolution was durably recorded. A
// resolution that could not be persisted is deliberately not used: the
// workflow would proceed on it now and the next replay would find the row
// still pending and ask again, which is the determinism divergence this
// whole stream exists to prevent.
func (s *execSession) persistAmbiguityResolution(ctx context.Context, rec EventRecord, response, errMsg, errCode string, nonRetryable bool, logOutcome string) (string, bool) {
	store, ok := s.engine.workflowStore.(callIntentResolver)
	if !ok {
		s.engine.log().WarnContext(ctx, "ambiguity resolved but the store cannot record it; reporting ambiguity instead",
			"workflow_id", s.workflowID, "step", rec.Step, "store", fmt.Sprintf("%T", s.engine.workflowStore))
		return "", false
	}

	completed := rec
	completed.Pending = false
	if completed.TimestampMs == 0 {
		completed.TimestampMs = time.Now().UnixMilli()
	}
	completed.Response = response
	completed.Err = errMsg
	completed.ErrCode = errCode
	completed.ErrNonRetryable = nonRetryable

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
		"service", rec.Service, "operation", rec.Op, "outcome", logOutcome)
	return completed.Response, true
}

// resolveAmbiguityViaKeyReplay attempts cleat#2897's mechanism (c) and
// persists a positive result the same way resolveAmbiguity does. Returns
// (response, true) only when replayUnderOriginalKey got a definite answer
// AND it was durably recorded; (", false) otherwise, in which case the
// caller falls through to resolveAmbiguity (the --ambiguity-lookup path, if
// configured) and then to [AMBIGUOUS] -- exactly as if this mechanism were
// not configured at all.
func (s *execSession) resolveAmbiguityViaKeyReplay(ctx context.Context, rec EventRecord) (string, bool) {
	resp, outcome := s.replayUnderOriginalKey(ctx, rec)
	if outcome != IdempotencyReplayResolved {
		return "", false
	}
	return s.persistAmbiguityResolution(ctx, rec, resp, "", "", false, outcome.String())
}
