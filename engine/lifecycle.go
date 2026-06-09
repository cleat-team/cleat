package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
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
// It sets replayJustEnded so that the first DurableSleep after replay
// can detect the resume-from-sleep case and complete without suspending.

func (s *execSession) exitReplay() {
	s.isReplay = false
	s.replayJustEnded = true
}

// recordEvent timestamps a fresh event, advances the session clock,
// and appends it to the history. It must only be called during fresh
// execution (not replay).

func (s *execSession) recordEvent(rec EventRecord) {
	if rec.TimestampMs == 0 {
		rec.TimestampMs = time.Now().UnixMilli()
	}
	s.nowMs = rec.TimestampMs
	s.history = append(s.history, rec)
	s.stepCount++

	// Persist immediately so events survive worker crashes.
	if s.engine.db != nil && !s.isReplay {
		if flushErr := s.engine.flushEvent(context.Background(), s.workflowID, rec); flushErr != nil {
			s.engine.log().ErrorContext(context.Background(), "recordEvent flushEvent failed", "workflow_id", s.workflowID, "step", rec.Step, "event_type", rec.EventType, "error", flushErr)
		}
	}
}

func (s *execSession) Now(ctx context.Context) int64 {
	// During replay, read the timestamp from the last consumed event
	// to produce deterministic Now() values matching the original
	// execution. Before any event is consumed (stepCount==0), s.nowMs
	// is seeded from the first history event or wall clock.
	if s.stepCount > 0 && s.stepCount <= len(s.history) {
		if ts := s.history[s.stepCount-1].TimestampMs; ts > 0 {
			return ts
		}
	}
	return s.nowMs
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

	written, _ := s.writeResult(ctx, m, uuidPtr, uuidStr, uuidMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) SideEffect(ctx context.Context, m api.Module, computedResult string, respPtr, respMaxLen uint32) int64 {
	if s.isReplay {
		return s.replaySideEffect(ctx, m, computedResult, respPtr, respMaxLen)
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

	written, _ := s.writeResult(ctx, m, respPtr, computedResult, respMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) replaySideEffect(ctx context.Context, m api.Module, computedResult string, respPtr, respMaxLen uint32) int64 {
	if s.stepCount < len(s.history) {
		rec := s.history[s.stepCount]
		if !s.advanceReplayStep(ctx, &rec) {
			return 0
		}

		if rec.EventType != EventTypeSideEffect {
			replayFailuresTotal.Inc()
			errMsg := fmt.Sprintf("replay divergence at step %d: expected side_effect event, got %s", rec.Step, rec.EventType)
			written, _ := s.writeResult(ctx, m, respPtr, errMsg, respMaxLen)
			return packSimpleResult(1, written)
		}

		// Verify that the replayed SideEffect computedResult matches the
		// recorded value. A mismatch means the WASM module produced a
		// different result on replay — a non-determinism bug.
		if rec.SideEffectResult != computedResult {
			replayFailuresTotal.Inc()
			errMsg := fmt.Sprintf(
				"replay divergence at step %d: SideEffect produced %q but history recorded %q. "+
					"Your workflow may have a non-determinism bug (time.Now(), random values, "+
					"map iteration, goroutines). Run 'cleat vet' to check for common issues.",
				rec.Step, computedResult, rec.SideEffectResult,
			)
			written, _ := s.writeResult(ctx, m, respPtr, errMsg, respMaxLen)
			return packSimpleResult(1, written)
		}

		written, _ := s.writeResult(ctx, m, respPtr, rec.SideEffectResult, respMaxLen)
		return packSimpleResult(0, written)
	}

	s.exitReplay()
	return s.freshSideEffect(ctx, m, computedResult, respPtr, respMaxLen)
}

func (s *execSession) WorkflowID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {

	id := s.workflowID
	if id == "" {
		id = "unknown"
	}
	written, _ := s.writeResult(ctx, m, idPtr, id, idMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) RunID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {

	runID := s.execRunID
	if runID == "" {
		runID = "unknown"
	}
	written, _ := s.writeResult(ctx, m, idPtr, runID, idMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) RegisterQueryHandler(ctx context.Context, m api.Module, name string) int64 {
	// Query handlers are registered but don't produce event history entries.
	// They are invoked out-of-band by the worker, not during replay.
	s.queryHandlers = append(s.queryHandlers, name)
	return 0
}

// ---- Stream R host functions ----

func (s *execSession) SetState(ctx context.Context, m api.Module, key, value string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) {
				return 0
			}
			if rec.EventType != EventTypeStateMutation || rec.StateOp != "set" || rec.StateKey != key {
				return 1
			}
			s.mu.Lock()
			if s.stateStore == nil {
				s.stateStore = make(map[string]string)
			}
			s.stateStore[key] = rec.StateValue
			s.mu.Unlock()
			return 0
		}
		s.exitReplay()
	}

	s.mu.Lock()
	if s.stateStore == nil {
		s.stateStore = make(map[string]string)
	}
	s.stateStore[key] = value
	s.mu.Unlock()

	rec := EventRecord{
		Step:       s.stepCount,
		EventType:  EventTypeStateMutation,
		StateKey:   key,
		StateValue: value,
		StateOp:    "set",
	}
	s.recordEvent(rec)
	return 0
}

func (s *execSession) GetState(ctx context.Context, m api.Module, key string, valuePtr, valueMaxLen uint32) int64 {

	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) {
				return 0
			}
			if rec.EventType != EventTypeStateMutation || rec.StateOp != "get" || rec.StateKey != key {
				return packSimpleResult(1, 0)
			}
			written, _ := s.writeResult(ctx, m, valuePtr, rec.StateValue, valueMaxLen)
			return packSimpleResult(0, written)
		}
		s.exitReplay()
	}

	s.mu.Lock()
	value := ""
	if s.stateStore != nil {
		value = s.stateStore[key]
	}
	s.mu.Unlock()

	rec := EventRecord{
		Step:       s.stepCount,
		EventType:  EventTypeStateMutation,
		StateKey:   key,
		StateValue: value,
		StateOp:    "get",
	}
	s.recordEvent(rec)

	written, _ := s.writeResult(ctx, m, valuePtr, value, valueMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) DeleteState(ctx context.Context, m api.Module, key string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) {
				return 0
			}
			if rec.EventType != EventTypeStateMutation || rec.StateOp != "del" || rec.StateKey != key {
				return 1
			}
			s.mu.Lock()
			if s.stateStore != nil {
				delete(s.stateStore, key)
			}
			s.mu.Unlock()
			return 0
		}
		s.exitReplay()
	}

	s.mu.Lock()
	if s.stateStore != nil {
		delete(s.stateStore, key)
	}
	s.mu.Unlock()

	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeStateMutation,
		StateKey:  key,
		StateOp:   "del",
	}
	s.recordEvent(rec)
	return 0
}

// IncrState atomically increments a numeric state value.  It is NOT safe for
// concurrent access from multiple WASM modules.  The engine serialises all
// host calls within a single workflow execution, so this is never called
// concurrently in practice — speculative parallelism MUST NOT be introduced
// without adding synchronisation to IncrState.

func (s *execSession) IncrState(ctx context.Context, m api.Module, key string, delta int64) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) {
				return 0
			}
			if rec.EventType != EventTypeStateMutation || rec.StateOp != "incr" || rec.StateKey != key {
				return 0
			}
			s.mu.Lock()
			if s.stateStore == nil {
				s.stateStore = make(map[string]string)
			}
			s.stateStore[key] = rec.StateValue
			s.mu.Unlock()
			newVal, _ := strconv.ParseInt(rec.StateValue, 10, 64)
			return newVal
		}
		s.exitReplay()
	}

	s.mu.Lock()
	if s.stateStore == nil {
		s.stateStore = make(map[string]string)
	}

	current := int64(0)
	if v, ok := s.stateStore[key]; ok {
		current, _ = strconv.ParseInt(v, 10, 64)
	}
	newVal := current + delta
	s.stateStore[key] = fmt.Sprintf("%d", newVal)
	s.mu.Unlock()

	rec := EventRecord{
		Step:       s.stepCount,
		EventType:  EventTypeStateMutation,
		StateKey:   key,
		StateValue: fmt.Sprintf("%d", newVal),
		StateDelta: delta,
		StateOp:    "incr",
	}
	s.recordEvent(rec)
	return newVal
}

func (s *execSession) HasState(ctx context.Context, m api.Module, key string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) {
				return 0
			}
			if rec.EventType != EventTypeStateMutation || rec.StateOp != "has" || rec.StateKey != key {
				return 0
			}
			if rec.StateValue == "1" {
				return 1
			}
			return 0
		}
		s.exitReplay()
	}

	s.mu.Lock()
	exists := int64(0)
	if s.stateStore != nil {
		if _, ok := s.stateStore[key]; ok {
			exists = 1
		}
	}
	s.mu.Unlock()

	rec := EventRecord{
		Step:       s.stepCount,
		EventType:  EventTypeStateMutation,
		StateKey:   key,
		StateValue: fmt.Sprintf("%d", exists),
		StateOp:    "has",
	}
	s.recordEvent(rec)
	return exists
}

func (s *execSession) ListState(ctx context.Context, m api.Module, prefix string, keysPtr, keysMaxLen uint32) int64 {

	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) {
				return 0
			}
			if rec.EventType != EventTypeStateMutation || rec.StateOp != "list" || rec.StateKey != prefix {
				return packSimpleResult(1, 0)
			}
			written, _ := s.writeResult(ctx, m, keysPtr, rec.StateKeys, keysMaxLen)
			return packSimpleResult(0, written)
		}
		s.exitReplay()
	}

	s.mu.Lock()
	var keys []string
	if s.stateStore != nil {
		for k := range s.stateStore {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
	}
	s.mu.Unlock()
	sort.Strings(keys)
	keysJSON, _ := json.Marshal(keys)

	rec := EventRecord{
		Step:      s.stepCount,
		EventType: EventTypeStateMutation,
		StateKey:  prefix,
		StateKeys: string(keysJSON),
		StateOp:   "list",
	}
	s.recordEvent(rec)

	written, _ := s.writeResult(ctx, m, keysPtr, string(keysJSON), keysMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) Fetch(ctx context.Context, m api.Module, method, url, headersJSON, body string, responsePtr, responseMaxLen uint32) int64 {

	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) {
				return 0
			}
			if rec.EventType != EventTypeFetch || rec.FetchMethod != method || rec.FetchURL != url || rec.FetchBody != body {
				replayFailuresTotal.Inc()
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
			written, _ := s.writeResult(ctx, m, responsePtr, rec.FetchResponse, responseMaxLen)
			return packSimpleResult(0, written)
		}
		s.exitReplay()
	}

	var response string
	var fetchErr error
	if s.engine.fetcher != nil {
		response, fetchErr = s.engine.fetcher.Fetch(ctx, method, url, headersJSON, body)
	} else {
		fetchErr = fmt.Errorf("no fetcher configured: workflow %s attempted %s %s", s.engine.workflowID, method, url)
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

	written, _ := s.writeResult(ctx, m, responsePtr, response, responseMaxLen)
	return packSimpleResult(0, written)
}

func (s *execSession) JsonParse(ctx context.Context, m api.Module, jsonPtr, jsonLen, outPtr, outMaxLen uint32) int64 {
	mem := m.Memory()
	input, ok := readWasmStringValidated(mem, jsonPtr, jsonLen, MaxWasmStringLen)
	if !ok {
		return packSimpleResult(1)
	}
	var v any
	if err := json.Unmarshal([]byte(input), &v); err != nil {
		return packSimpleResult(1)
	}
	normalized, err := json.Marshal(v)
	if err != nil {
		return packSimpleResult(1)
	}
	written, err := writeWasmString(mem, outPtr, string(normalized), outMaxLen)
	if err != nil {
		return packSimpleResult(1)
	}
	return packSimpleResult(0, written)
}

func (s *execSession) JsonStringify(ctx context.Context, m api.Module, ptr, length, outPtr, outMaxLen uint32) int64 {
	mem := m.Memory()
	input, ok := readWasmStringValidated(mem, ptr, length, MaxWasmStringLen)
	if !ok {
		return packSimpleResult(1)
	}
	var v any
	if err := json.Unmarshal([]byte(input), &v); err != nil {
		return packSimpleResult(1)
	}
	serialized, err := json.Marshal(v)
	if err != nil {
		return packSimpleResult(1)
	}
	written, err := writeWasmString(mem, outPtr, string(serialized), outMaxLen)
	if err != nil {
		return packSimpleResult(1)
	}
	return packSimpleResult(0, written)
}
