package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero/api"
)

func (s *execSession) ContinueAsNew(ctx context.Context, m api.Module, newInputJSON string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeContinueAsNew {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}
				s.suspendErr = &SuspendError{
					Reason:   "continue_as_new",
					NewInput: rec.NewInput,
				}
				return 0
			}
		}
		s.exitReplay()
	}

	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeContinueAsNew,
		NewInput:  newInputJSON,
	}
	s.recordEvent(rec)

	s.suspendErr = &SuspendError{
		Reason:   "continue_as_new",
		NewInput: newInputJSON,
	}
	return 0
}

// ContinueAsNewWithVersion restarts the workflow with new input and optionally
// a new version. If newVersion is 0, uses the current version (same as ContinueAsNew).

func (s *execSession) ContinueAsNewWithVersion(ctx context.Context, m api.Module, newInputJSON string, newVersion int) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeContinueAsNew {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}
				s.suspendErr = &SuspendError{
					Reason:     "continue_as_new",
					NewInput:   rec.NewInput,
					NewVersion: rec.NewVersion,
				}
				return 0
			}
		}
		s.exitReplay()
	}

	rec := EventRecord{
		Step:       s.stepCount,
		EventType:  EventTypeContinueAsNew,
		NewInput:   newInputJSON,
		NewVersion: newVersion,
	}
	s.recordEvent(rec)

	s.suspendErr = &SuspendError{
		Reason:     "continue_as_new",
		NewInput:   newInputJSON,
		NewVersion: newVersion,
	}
	return 0
}

func (s *execSession) Version(ctx context.Context) int64 {
	if s.engine.state != nil {
		return int64(s.engine.state.Version())
	}
	return 1
}

func (s *execSession) MinVersion(ctx context.Context) int64 {
	if s.engine.state != nil {
		return int64(s.engine.state.MinVersion())
	}
	return 1
}

func (s *execSession) SetQueryState(ctx context.Context, m api.Module, key, value string) int64 {
	s.mu.Lock()
	if s.queryState == nil {
		s.queryState = make(map[string]string)
	}
	s.queryState[key] = value
	s.mu.Unlock()
	return 0
}

func (s *execSession) RegisterUpdateHandler(ctx context.Context, m api.Module, name string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeUpdateHandler {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}
				return 0
			}
		}
		s.exitReplay()
	}

	// Fresh execution: record the handler registration event.
	rec := EventRecord{
		Step:              s.stepCount,
		EventType:         EventTypeUpdateHandler,
		UpdateHandlerName: name,
	}
	s.recordEvent(rec)
	return 0
}

// exitReplay transitions from replay to forward execution.
//
// It used to also arm a replayJustEnded flag that DurableSleep consumed to
// detect resume-from-sleep. That flag is gone: sleep now decides from elapsed
// time, which does not require another call to have crossed the frontier
// first. See DurableSleep and IMPROVEMENT-PLAN 3.67.
func (s *execSession) exitReplay() {
	s.isReplay = false
}

// recordEvent timestamps a fresh event, advances the session clock,
// and appends it to the history. It must only be called during fresh
// execution (not replay).

// eventPersistence says what happened to an event's durable write, which is
// three outcomes and not two. cleat#1572.
//
// The distinction that matters is NOT ATTEMPTED versus ATTEMPTED AND FAILED.
// Both leave the row absent, and they mean opposite things:
//
//   - Not attempted is a configuration. This engine has no database, or the
//     operator set --no-per-step-flush and the events land at segment end.
//     Nothing is wrong and nothing is lost.
//   - Failed is a refusal. The commonest cause is ErrFenceLost: another worker
//     has taken this run, so THIS worker's events are not going to be in its
//     history at all. Anything shown to a user off the back of one is output
//     the system does not believe happened.
type eventPersistence int

const (
	// eventNotAttempted: no database, or per-step flush is off.
	eventNotAttempted eventPersistence = iota
	// eventPersisted: the row is in the database now.
	eventPersisted
	// eventFlushFailed: the write was attempted and refused or errored.
	eventFlushFailed
)

// recordEvent appends an event to the session history and persists it.
//
// It returns what became of the durable write. EXISTING CALLERS IGNORE IT and
// still compile, which is deliberate: the return exists for the one caller
// that must not show a user a token the database refused, and adding it should
// not touch the several dozen sites that have no such question.
func (s *execSession) recordEvent(rec EventRecord) eventPersistence {
	if rec.TimestampMs == 0 {
		rec.TimestampMs = time.Now().UnixMilli()
	}
	// Stamped here rather than at each call site, because "was the guest
	// draining its defer table when this happened" is a property of the
	// session at the moment of recording, and there are too many call sites
	// for any of them to be the place that remembers. cleat#1155.
	if s.inDeferPhase {
		rec.InDeferPhase = true
	}
	// The durable clock must not go backwards (cleat#944).
	//
	// Two clock domains feed Now(). Before any event is recorded it is the
	// seed -- the workflow row's created_at, which is the DATABASE's clock
	// (see Engine.seedNowMs, and note why created_at has to stay the seed: it
	// is the only value identical on the original run and the replay). The
	// first recorded event's timestamp is the WORKER's time.Now(). Nothing
	// reconciled them, so the step between them was the offset between two
	// machines' clocks, in whichever direction they happened to differ.
	//
	// Measured on the samples-go port: PostgreSQL 6 of 8 runs backwards, worst
	// -26ms; MySQL 4 of 5, worst -118ms. Both databases were containers against
	// a host worker. The magnitude tracks the DEPLOYMENT, not the dialect --
	// two clocks on one laptop are close, a worker and a database in different
	// availability zones have no such bound.
	//
	// A workflow computing Now().Sub(start) across its first durable call got a
	// negative duration.
	//
	// CLAMPING HERE RATHER THAN AT THE READ IS WHAT KEEPS REPLAY EXACT, and the
	// reason is sharper than "history would hold a different number".
	//
	// A read-side clamp would make the guest see a value that was never
	// recorded. The original run and the replay would then agree only by both
	// applying the same clamp to the same stale input -- which holds until
	// someone changes the clamp, and nothing would fail at the moment they did.
	// Writing the adjusted value means the recorded number IS the number: it is
	// what the checksum covers, replay sets s.nowMs straight from it
	// (engine/replayer.go), and there is nothing left to recompute or to keep
	// in agreement.
	//
	// (That framing is rcownie-ef's, from the review of this change.)
	//
	// This is a floor, not a rewrite: once the worker clock passes the seed --
	// which it does within the offset, tens of milliseconds -- the branch stops
	// firing and every later timestamp is the worker's own. DurableSleep is
	// unaffected either way, because it sets TimestampMs to anchor+duration,
	// which is already >= s.nowMs.
	if rec.TimestampMs < s.nowMs {
		rec.TimestampMs = s.nowMs
	}
	s.nowMs = rec.TimestampMs
	s.history = append(s.history, rec)
	s.stepCount++
	atomic.AddInt64(&freshStepCount, 1)

	// Persist immediately so events survive worker crashes.
	outcome := eventNotAttempted
	if s.engine.db != nil && !s.isReplay {
		checksum := computeEventChecksum(rec, s.lastChecksum)
		flushed := false
		af := s.engine.getAdaptiveFlusher()
		if af != nil {
			done, useBatch := af.Flush(context.Background(), s.workflowID, rec, checksum, s.engine.workerID, s.engine.generation)
			if useBatch {
				// A blocking receive. It was a select with one case and no
				// default, which is the same thing spelled in a way that
				// suggests a second case was once intended or is coming.
				if err := <-done; err != nil {
					if errors.Is(err, ErrFenceLost) {
						// Expected and normal under reaping (B4): this
						// worker's claim on the workflow was lost, so the
						// event was not written. Not logged as an error --
						// see engine/flush.go's flushEvent doc for why the
						// session is not aborted here either.
						s.engine.log().DebugContext(context.Background(), "adaptive flush: fence lost, workflow reassigned to another worker", "workflow_id", s.workflowID, "step", rec.Step)
					} else {
						s.engine.log().ErrorContext(context.Background(), "adaptive flush failed", "workflow_id", s.workflowID, "step", rec.Step, "error", err)
					}
					outcome = eventFlushFailed
				} else {
					s.lastChecksum = checksum
					outcome = eventPersisted
				}
				flushed = true
			}
		}
		if !flushed {
			// Direct flush (low-rate mode or batch/adaptive flushers disabled)
			//
			// RETRIED, which it was not. Of the six routes to
			// eventFlushFailed this is the one a low-rate workflow takes on
			// every step, and it made exactly one attempt: a database that
			// dropped a connection lost the event outright, while the same
			// event at a higher step rate would have been retried by the batch
			// path. The asymmetry was not a decision, it was where the retry
			// happened to be written. cleat#1717.
			//
			// RETRYING IS SAFE HERE BECAUSE A RE-INSERT IS NOT A DOUBLE WRITE.
			// insertEventSQL is ON CONFLICT (tenant_id, workflow_id, step) DO UPDATE
			// with a WHERE that cannot fire (see its doc), so an attempt that
			// committed and lost its acknowledgement replays as zero rows
			// affected -- and afterFencedInsert answers zero rows by asking
			// Heartbeat whether the fence still holds, which it does, so the
			// retry returns nil rather than mistaking its own earlier success
			// for a lost fence. TestARetryOfAnAlreadyCommittedFlushIsNotAFenceLoss
			// measures that rather than trusting this paragraph.
			flushErr := retryFlushUntilDeadline(context.Background(), s.engine.flushRetryWindow, "direct flush", func() error {
				return s.engine.flushEvent(context.Background(), s.workflowID, rec, s.lastChecksum)
			})
			if flushErr != nil {
				if errors.Is(flushErr, ErrFenceLost) {
					s.engine.log().DebugContext(context.Background(), "flushEvent: fence lost, workflow reassigned to another worker", "workflow_id", s.workflowID, "step", rec.Step)
				} else {
					s.engine.log().ErrorContext(context.Background(), "recordEvent flushEvent failed", "workflow_id", s.workflowID, "step", rec.Step, "event_type", rec.EventType, "error", flushErr)
				}
				outcome = eventFlushFailed
			} else {
				s.lastChecksum = checksum
				// flushEvent returns nil WITHOUT WRITING when per-step flush
				// is disabled, so a nil error is not on its own evidence that
				// the row exists.
				if !s.engine.noPerStepFlush {
					outcome = eventPersisted
				}
			}
		}
	}
	return outcome
}

func (s *execSession) Now(ctx context.Context) int64 {
	// The virtual clock is the LATER of two deterministic anchors, never just
	// the recorded one.
	//
	// During replay, the last consumed event's timestamp reproduces what the
	// original execution saw. But a sleep records no event and advances s.nowMs
	// to anchor+duration (see DurableSleep), and stepCount does not move -- so
	// reading only the history timestamp handed the guest a PRE-sleep instant
	// after a sleep had completed. Measured: 163ms of apparent elapsed time
	// across a 3000ms sleep, until the next event happened to be recorded and
	// re-anchored the clock.
	//
	// Both values are deterministic, so taking the later of them is too:
	// history timestamps are recorded, and s.nowMs is anchor+duration where the
	// anchor is itself one of these. Replay recomputes the same sleeps from the
	// same anchors and arrives at the same number.
	//
	// max, not assignment, for the reason DurableSleep gives for its own max:
	// the clock must never run backwards, and either source can be the larger
	// one depending on where the workflow is.
	now := s.nowMs
	if s.stepCount > 0 && s.stepCount <= len(s.history) {
		if ts := s.history[s.stepCount-1].TimestampMs; ts > now {
			now = ts
		}
	}
	return now
}

func (s *execSession) Random(ctx context.Context) int64 {
	// Deterministic random: seeded from workflow ID and step count.
	// On replay, stepCount is the same for each call, so Random()
	// always returns the same sequence.
	data := fmt.Sprintf("%s:%d:%d", s.workflowID, s.stepCount, s.randomSeq)
	s.randomSeq++
	hash := sha256.Sum256([]byte(data))
	return int64(binary.BigEndian.Uint64(hash[:8]))
}

func (s *execSession) UUID(ctx context.Context, m api.Module, seed string, uuidPtr, uuidMaxLen uint32) int64 {

	wfID := s.workflowID
	if wfID == "" {
		wfID = "unknown"
	}
	data := wfID + ":" + seed
	hash := sha256.Sum256([]byte(data))
	// Format as UUIDv5-like value (first 16 bytes of SHA-256, version bits set).
	hash[6] = (hash[6] & 0x0f) | 0x50 // Version 5
	hash[8] = (hash[8] & 0x3f) | 0x80 // Variant 1
	uuidStr := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		hash[0:4], hash[4:6], hash[6:8], hash[8:10], hash[10:16])

	written, writtenEC := s.writeOut(ctx, m, uuidPtr, uuidStr, uuidMaxLen)
	return packSimpleResult(writtenEC, written)
}

func (s *execSession) SideEffect(ctx context.Context, m api.Module, computedResult string, respPtr, respMaxLen uint32) int64 {
	if s.isReplay {
		return s.replaySideEffect(ctx, m, computedResult, respPtr, respMaxLen)
	}
	// A fresh side_effect is new work: it records a non-deterministic value into
	// the history of a workflow that has already terminated, and any later
	// replay would take that value as authoritative.
	if s.stopBeforeNewWork() {
		return callSuspendSentinel
	}
	return s.freshSideEffect(ctx, m, computedResult, respPtr, respMaxLen)
}

func (s *execSession) freshSideEffect(ctx context.Context, m api.Module, computedResult string, respPtr, respMaxLen uint32) int64 {

	rec := EventRecord{
		Step:             s.stepCount,
		EventType:        EventTypeSideEffect,
		SideEffectResult: computedResult,
	}
	s.recordEvent(rec)

	written, writtenEC := s.writeOut(ctx, m, respPtr, computedResult, respMaxLen)
	return packSimpleResult(writtenEC, written)
}

func (s *execSession) replaySideEffect(ctx context.Context, m api.Module, computedResult string, respPtr, respMaxLen uint32) int64 {
	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if !s.advanceReplayStep(ctx, &rec) {
			return 0
		}

		if rec.EventType != EventTypeSideEffect {
			if s.engine.Metrics != nil {
				s.engine.Metrics.RecordReplayFailure(ctx)
			}
			errMsg := fmt.Sprintf("replay divergence at step %d: expected side_effect event, got %s", rec.Step, rec.EventType)
			written, _ := s.writeResult(ctx, m, respPtr, errMsg, respMaxLen)
			return packSimpleResult(1, written)
		}

		// Verify that the replayed SideEffect computedResult matches the
		// recorded value. A mismatch means the WASM module produced a
		// different result on replay — a non-determinism bug.
		if rec.SideEffectResult != computedResult {
			if s.engine.Metrics != nil {
				s.engine.Metrics.RecordReplayFailure(ctx)
			}
			errMsg := fmt.Sprintf(
				"replay divergence at step %d: SideEffect produced %q but history recorded %q. "+
					"Your workflow may have a non-determinism bug (time.Now(), random values, "+
					"map iteration, goroutines). Run 'cleat vet' to check for common issues.",
				rec.Step, computedResult, rec.SideEffectResult,
			)
			written, _ := s.writeResult(ctx, m, respPtr, errMsg, respMaxLen)
			return packSimpleResult(1, written)
		}

		written, writtenEC := s.writeOut(ctx, m, respPtr, rec.SideEffectResult, respMaxLen)
		return packSimpleResult(writtenEC, written)
	}

	s.exitReplay()
	return s.freshSideEffect(ctx, m, computedResult, respPtr, respMaxLen)
}

func (s *execSession) WorkflowID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {

	id := s.workflowID
	if id == "" {
		id = "unknown"
	}
	written, writtenEC := s.writeOut(ctx, m, idPtr, id, idMaxLen)
	return packSimpleResult(writtenEC, written)
}

func (s *execSession) RunID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {

	runID := s.execRunID
	if runID == "" {
		runID = "unknown"
	}
	written, writtenEC := s.writeOut(ctx, m, idPtr, runID, idMaxLen)
	return packSimpleResult(writtenEC, written)
}

func (s *execSession) RegisterQueryHandler(ctx context.Context, m api.Module, name string) int64 {
	// Retained as a harmless ABI-compatibility no-op only -- see the
	// RegisterQueryHandler doc comment on the HostHandler interface in
	// imports.go. Nothing reads s.queryHandlers back out to dispatch a
	// query to it; "invoked out-of-band by the worker" was never true.
	s.queryHandlers = append(s.queryHandlers, name)
	return 0
}

// ---- Stream R host functions ----

// IncrState atomically increments a numeric state value.  It is NOT safe for
// concurrent access from multiple WASM modules.  The engine serialises all
// host calls within a single workflow execution, so this is never called
// concurrently in practice — speculative parallelism MUST NOT be introduced
// without adding synchronisation to IncrState.

func (s *execSession) Fetch(ctx context.Context, m api.Module, method, url, headersJSON, body string, responsePtr, responseMaxLen uint32) int64 {

	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) {
				return 0
			}
			if rec.EventType != EventTypeFetch || rec.FetchMethod != method || rec.FetchURL != url || rec.FetchBody != body {
				if s.engine.Metrics != nil {
					s.engine.Metrics.RecordReplayFailure(ctx)
				}
				errMsg := fmt.Sprintf("replay divergence at step %d: Fetch mismatch.\n  workflow: %s %s\n  history: %s %s\n  actual body: %s\n  expected body: %s\n  expected response: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
					rec.Step,
					method, url,
					rec.FetchMethod, rec.FetchURL,
					truncateWithHash(body, maxPayloadLen),
					truncateWithHash(rec.FetchBody, maxPayloadLen),
					truncateWithHash(rec.FetchResponse, maxPayloadLen))
				written, _ := s.writeResult(ctx, m, responsePtr, errMsg, responseMaxLen)
				return packSimpleResult(1, written)
			}
			if rec.Err != "" {
				written, _ := s.writeResult(ctx, m, responsePtr, rec.Err, responseMaxLen)
				return packSimpleResult(1, written)
			}
			written, writtenEC := s.writeOut(ctx, m, responsePtr, rec.FetchResponse, responseMaxLen)
			return packSimpleResult(writtenEC, written)
		}
		s.exitReplay()
	}

	// Past the frontier in a defer segment: an outbound HTTP request is new
	// work, and the most externally visible kind there is -- it leaves a side
	// effect on someone else's server that no amount of unwinding takes back.
	//
	// IMPROVEMENT-PLAN 3.84 guarded six fresh paths and this is the seventh it
	// did not list. Its table was built by reading the entry points a guest
	// uses to reach a *service*, and Fetch reaches one without going through
	// the durable-call family, so it was not in the inventory to begin with.
	//
	// packSimpleResult, which is what every return below uses, has bit 31 free
	// -- TestStopSentinelBitsAcrossEveryLayout measures it as free=ff000000ffffff00
	// -- so the same universal sentinel works here with no new layout question.
	if s.stopBeforeNewWork() {
		return callSuspendSentinel
	}

	var response string
	var fetchErr error
	if s.engine.fetcher != nil {
		response, fetchErr = s.engine.fetcher.Fetch(ctx, method, url, headersJSON, body)
	} else {
		// NOT a misconfiguration an operator can fix, and the old text --
		// "no fetcher configured" -- read like one. cleat ships no default
		// Fetcher and cleat-worker sets none, so this branch is taken on
		// EVERY cleat_fetch from a stock worker (IMPROVEMENT-PLAN 3.317).
		// Say who can supply one, so the reader stops looking for a flag.
		fetchErr = fmt.Errorf(
			"cleat_fetch is unavailable: this engine has no Fetcher, and cleat "+
				"ships no default one. cleat_fetch works only when the engine is "+
				"embedded and the host supplies engine.WithFetcher(...); a stock "+
				"cleat-worker cannot serve it. Workflow %s attempted %s %s",
			s.engine.workflowID, method, url)
	}

	rec := EventRecord{
		Step:          s.stepCount,
		EventType:     EventTypeFetch,
		FetchMethod:   method,
		FetchURL:      url,
		FetchHeaders:  headersJSON,
		FetchBody:     body,
		FetchResponse: response,
	}
	if fetchErr != nil {
		rec.Err = fetchErr.Error()
	}
	s.recordEvent(rec)

	if fetchErr != nil {
		written, _ := s.writeResult(ctx, m, responsePtr, fetchErr.Error(), responseMaxLen)
		return packSimpleResult(1, written)
	}

	written, writtenEC := s.writeOut(ctx, m, responsePtr, response, responseMaxLen)
	return packSimpleResult(writtenEC, written)
}

// JsonParse validates and canonicalises input using the host's encoding/json.
//
// input arrives already decoded, and output goes through writeResult. Both
// matter: this handler used to take raw (ptr, len) and call m.Memory()
// directly, which made it the only host function reading its own input out of
// guest memory. The wasmtime backend passes a nil api.Module by design -- the
// memory is handed over in the context instead (see writeResult) -- so
// m.Memory() was a nil dereference and every cleat_json_parse call from a
// guest crashed the execution on the primary backend. See
// IMPROVEMENT-PLAN.md 2.14.
func (s *execSession) JsonParse(ctx context.Context, m api.Module, input string, outPtr, outMaxLen uint32) int64 {
	var v any
	if err := json.Unmarshal([]byte(input), &v); err != nil {
		return packSimpleResult(1)
	}
	normalized, err := json.Marshal(v)
	if err != nil {
		return packSimpleResult(1)
	}
	written, err := s.writeResult(ctx, m, outPtr, string(normalized), outMaxLen)
	if err != nil {
		return packSimpleResult(1)
	}
	return packSimpleResult(0, written)
}

// JsonStringify re-serialises input via the host's encoding/json. See
// JsonParse for why input is a string rather than a (ptr, len) pair.
func (s *execSession) JsonStringify(ctx context.Context, m api.Module, input string, outPtr, outMaxLen uint32) int64 {
	var v any
	if err := json.Unmarshal([]byte(input), &v); err != nil {
		return packSimpleResult(1)
	}
	serialized, err := json.Marshal(v)
	if err != nil {
		return packSimpleResult(1)
	}
	written, err := s.writeResult(ctx, m, outPtr, string(serialized), outMaxLen)
	if err != nil {
		return packSimpleResult(1)
	}
	return packSimpleResult(0, written)
}
