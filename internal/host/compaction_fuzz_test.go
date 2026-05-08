package host

import (
	"fmt"
	"testing"
)

// ---------------------------------------------------------------------------
// Fuzz test: Compaction equivalence
//
// Property: compacting an event history and then reconstructing it from the
// compaction state must produce a functionally equivalent event sequence.
//
// The fuzzer feeds arbitrary byte sequences that are deterministically parsed
// into EventRecord slices. The test then compacts at three different split
// points (compact one event, compact half, compact all) and verifies that
// buildFullHistoryFromCompaction(extractCompactionState(prefix), suffix)
// matches the original events field-for-field.
// ---------------------------------------------------------------------------

// FuzzCompactionEquivalence verifies that compaction + reconstruction of a
// workflow event history produces events identical to the original for every
// event type and field.
func FuzzCompactionEquivalence(f *testing.F) {
	// ---- seed corpus ----

	// Single Call event with realistic fields.
	f.Add(fuzzSeed(EventCodeCall, []string{"catalog", "LookupItem", `{"sku":"ABC"}`, `{"price":999}`, ""}))
	// Single Sleep.
	f.Add(fuzzSeed(EventCodeSleep, nil, 42))
	// Single AwaitSignals.
	f.Add(fuzzSeed(EventCodeAwaitSignals, []string{"payment,shipping"}, 30000))
	// Single SignalReceived.
	f.Add(fuzzSeed(EventCodeSignalReceived, []string{"payment", `{"amount":500}`}, []int64{}...))
	// Single Defer (DeferID + DeferDescription).
	f.Add(fuzzSeed(EventCodeDefer, []string{"defer-0", "cleanup resources"}, []int64{}...))
	// Single ChildWorkflow.
	f.Add(fuzzSeed(EventCodeChildWorkflow, []string{"order-worker", `{"items":[]}`, "run-child-001"}, []int64{}...))
	// Single AwaitChild (RunID + Response + Error).
	f.Add(fuzzSeed(EventCodeAwaitChild, []string{"run-child-001", `{"status":"done"}`, ""}, []int64{}...))
	// Single ContinueAsNew.
	f.Add(fuzzSeed(EventCodeContinueAsNew, []string{`{"page":2}`}, []int64{}...))
	// Single Heartbeat.
	f.Add(fuzzSeed(EventCodeHeartbeat, []string{"inventory", "Reserve"}, []int64{}...))
	// Single AwaitAllChildren.
	f.Add(fuzzSeed(EventCodeAwaitAllChildren, []string{`[{"run_id":"c1","result":"ok"}]`}, []int64{}...))
	// Single PluginCall.
	f.Add(fuzzSeed(EventCodePluginCall, []string{"s3", "GetObject", `{"key":"x"}`, `{"body":"..."}`, ""}, []int64{}...))
	// Single CreatePromise.
	f.Add(fuzzSeed(EventCodeCreatePromise, []string{"payment-auth", "prom-001"}, []int64{}...))
	// Single AwaitPromise.
	f.Add(fuzzSeed(EventCodeAwaitPromise, []string{"prom-001"}, []int64{}...))
	// Single PromiseResolved.
	f.Add(fuzzSeed(EventCodePromiseResolved, []string{"prom-001", `{"status":"ok"}`}, []int64{}...))
	// Single PromiseRejected.
	f.Add(fuzzSeed(EventCodePromiseRejected, []string{"prom-001", "card declined"}, []int64{}...))
	// Single UpdateHandler.
	f.Add(fuzzSeed(EventCodeUpdateHandler, []string{"update-inventory"}, []int64{}...))
	// Single StateMutation.
	f.Add(fuzzSeed(EventCodeStateMutation, []string{"stock", "42", "set"}, 5))
	// Single RunDetached (no fields).
	f.Add(fuzzSeed(EventCodeRunDetached, nil, []int64{}...))
	// Single PluginCallStreamChunk.
	f.Add(fuzzSeed(EventCodePluginCallStreamChunk, []string{"s3", "GetObject", `{"key":"x"}`, `{"body":"..."}`, ""}, []int64{}...))

	// Pair: Call + Sleep (tests split at 1-halfway).
	f.Add(append(
		fuzzSeed(EventCodeCall, []string{"svc", "op", `{}`, `{}`, ""}),
		fuzzSeed(EventCodeSleep, nil, 99)...,
	))

	// Pair: ChildWorkflow + child completing AwaitChild (tests open-children
	// tracking and completion).
	f.Add(append(
		fuzzSeed(EventCodeChildWorkflow, []string{"child-wf", `{"x":1}`, "run-c1"}, []int64{}...),
		fuzzSeed(EventCodeAwaitChild, []string{"run-c1", `{"y":2}`, ""}, []int64{}...)...,
	))

	// Triplet: Defer, ChildWorkflow, AwaitChild with pending child (tests
	// pending defer and open-children book-keeping).
	f.Add(append(
		append(
			fuzzSeed(EventCodeDefer, []string{"def-1", "rollback"}, []int64{}...),
			fuzzSeed(EventCodeChildWorkflow, []string{"wf", `{}`, "run-open"}, []int64{}...)...,
		),
		fuzzSeed(EventCodeAwaitChild, []string{"run-open", "", ""}, []int64{}...)...,
	))

	// ---- fuzz target ----
	f.Fuzz(func(t *testing.T, data []byte) {
		events := parseFuzzEvents(data)
		if len(events) < 2 {
			// Need at least two events for a meaningful compaction split.
			return
		}

		// Test three split points:
		//   1              -- compact only the first event.
		//   len(events)/2  -- compact roughly half.
		//   len(events)    -- compact everything (empty tail).
		//
		// keepStep == 0 is deliberately omitted: it produces an empty
		// CompactionState with zero compacted events, which is a trivial
		// pass-through that adds little coverage.
		splits := []int{1, len(events) / 2, len(events)}
		seen := map[int]bool{}
		for _, sp := range splits {
			if sp < 1 || sp > len(events) || seen[sp] {
				continue
			}
			seen[sp] = true

			compacted := events[:sp]
			tail := events[sp:]

			cs := extractCompactionState(compacted)
			reconstructed := buildFullHistoryFromCompaction(tail, cs)

			if len(reconstructed) != len(events) {
				t.Errorf("split=%d: length mismatch: got %d events, want %d",
					sp, len(reconstructed), len(events))
				continue
			}

			var firstBad int
			ok := true
			for i := range events {
				if !eventFieldsMatch(events[i], reconstructed[i]) {
					if ok {
						firstBad = i
						ok = false
					}
				}
			}
			if !ok {
				t.Errorf("split=%d: first mismatch at event %d (%s)",
					sp, firstBad, events[firstBad].EventType)
				dumpEventDiff(t, events[firstBad], reconstructed[firstBad])
				// Also dump surrounding context.
				for d := -2; d <= 2; d++ {
					idx := firstBad + d
					if idx >= 0 && idx < len(events) {
						t.Logf("  [%d] orig: %s  rec: %s",
							idx, eventSummary(events[idx]), eventSummary(reconstructed[idx]))
					}
				}
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Fuzz-data encoding helpers (used for seed-corpus construction)
// ---------------------------------------------------------------------------

// fuzzSeed encodes a single event into a byte slice for the fuzz corpus.
// Strings are length-prefixed; int64 values are written as a single byte
// (clamped to [0, 255]).
func fuzzSeed(typeCode byte, strs []string, ints ...int64) []byte {
	b := []byte{typeCode}
	for _, s := range strs {
		n := len(s)
		if n > 255 {
			n = 255
		}
		b = append(b, byte(n))
		b = append(b, []byte(s[:n])...)
	}
	for _, v := range ints {
		if v < 0 {
			v = 0
		}
		if v > 255 {
			v = 255
		}
		b = append(b, byte(v))
	}
	return b
}

// ---------------------------------------------------------------------------
// Fuzz-data parsing
// ---------------------------------------------------------------------------

// parseFuzzEvents interprets a byte slice as a sequence of length-prefixed
// events and returns the resulting EventRecord slice. Every byte sequence
// produces a valid (possibly empty) result. This deterministic mapping allows
// Go's fuzzer to explore the event space through byte-level mutations.
func parseFuzzEvents(data []byte) []EventRecord {
	var events []EventRecord
	r := &byteReader{data: data}

	for step := 0; r.remaining() > 0; step++ {
		typeCode := int(r.readByte()) % 19 // clamp to [0, 18]

		ev := EventRecord{
			Step:      step,
			EventType: codeToEventType[typeCode],
		}

		switch typeCode {
		case EventCodeCall: // svc, op, req, resp, err (5 strings)
			ev.Service = r.readString()
			ev.Op = r.readString()
			ev.Request = r.readString()
			ev.Response = r.readString()
			ev.Err = r.readString()
		case EventCodeSleep: // durMs (1 int64)
			ev.DurationMs = r.readInt64()
		case EventCodeAwaitSignals: // sigNames (string), timeoutMs (int64)
			ev.SignalNames = r.readString()
			ev.TimeoutMs = r.readInt64()
		case EventCodeSignalReceived: // sigName, sigPayload (2 strings)
			ev.SignalName = r.readString()
			ev.SignalPayload = r.readString()
		case EventCodeDefer: // deferID, deferDesc (2 strings)
			ev.DeferID = r.readString()
			ev.DeferDescription = r.readString()
		case EventCodeChildWorkflow: // childName, childInput, runID (3 strings)
			ev.ChildName = r.readString()
			ev.ChildInput = r.readString()
			ev.RunID = r.readString()
		case EventCodeAwaitChild: // runID, resp, errMsg (3 strings)
			ev.RunID = r.readString()
			ev.Response = r.readString()
			ev.Err = r.readString()
		case EventCodeContinueAsNew: // newInput (1 string)
			ev.NewInput = r.readString()
		case EventCodeHeartbeat: // svc, op (2 strings)
			ev.Service = r.readString()
			ev.Op = r.readString()
		case EventCodeAwaitAllChildren: // resp (1 string)
			ev.Response = r.readString()
		case EventCodePluginCall: // pluginName, pluginFunc, input, output, err (5 strings)
			ev.PluginName = r.readString()
			ev.PluginFunc = r.readString()
			ev.PluginInput = r.readString()
			ev.PluginOutput = r.readString()
			ev.PluginError = r.readString()
		case EventCodeCreatePromise: // promiseName, promiseID (2 strings)
			ev.PromiseName = r.readString()
			ev.PromiseID = r.readString()
		case EventCodeAwaitPromise: // promiseID (1 string)
			ev.PromiseID = r.readString()
		case EventCodePromiseResolved: // promiseID, result (2 strings)
			ev.PromiseID = r.readString()
			ev.PromiseResult = r.readString()
		case EventCodePromiseRejected: // promiseID, err (2 strings)
			ev.PromiseID = r.readString()
			ev.PromiseError = r.readString()
		case EventCodeUpdateHandler: // handlerName (1 string)
			ev.UpdateHandlerName = r.readString()
		case EventCodeStateMutation: // key, value, op (3 strings), delta (1 int64)
			ev.StateKey = r.readString()
			ev.StateValue = r.readString()
			ev.StateOp = r.readString()
			ev.StateDelta = r.readInt64()
		case EventCodeRunDetached: // no string fields
		case EventCodePluginCallStreamChunk: // pluginName, pluginFunc, input, output, err (5 strings)
			ev.PluginName = r.readString()
			ev.PluginFunc = r.readString()
			ev.PluginInput = r.readString()
			ev.PluginOutput = r.readString()
			ev.PluginError = r.readString()
		}

		events = append(events, ev)
	}
	return events
}

// byteReader wraps a []byte with sequential read helpers.
type byteReader struct {
	data []byte
	pos  int
}

func (r *byteReader) remaining() int { return len(r.data) - r.pos }

func (r *byteReader) readByte() byte {
	if r.pos >= len(r.data) {
		return 0
	}
	b := r.data[r.pos]
	r.pos++
	return b
}

func (r *byteReader) readString() string {
	n := int(r.readByte())
	if n <= 0 {
		return ""
	}
	if n > r.remaining() {
		n = r.remaining()
	}
	s := string(r.data[r.pos : r.pos+n])
	r.pos += n
	return s
}

func (r *byteReader) readInt64() int64 {
	return int64(r.readByte())
}

// ---------------------------------------------------------------------------
// Equivalence checking
// ---------------------------------------------------------------------------

// eventFieldsMatch compares all round-tripped fields of two EventRecords and
// returns true if they are equivalent. The comparison is type-specific: only
// fields that are preserved through the compaction round-trip are checked.
func eventFieldsMatch(a, b EventRecord) bool {
	if a.Step != b.Step || a.EventType != b.EventType {
		return false
	}
	switch a.EventType {
	case EventTypeCall:
		return a.Service == b.Service &&
			a.Op == b.Op &&
			a.Request == b.Request &&
			a.Response == b.Response &&
			a.Err == b.Err

	case EventTypeSleep:
		return a.DurationMs == b.DurationMs

	case EventTypeAwaitSignals:
		return a.SignalNames == b.SignalNames &&
			a.TimeoutMs == b.TimeoutMs

	case EventTypeSignalReceived:
		return a.SignalName == b.SignalName &&
			a.SignalPayload == b.SignalPayload

	case EventTypeDefer:
		return a.DeferID == b.DeferID &&
			a.DeferDescription == b.DeferDescription

	case EventTypeChildWorkflow:
		return a.ChildName == b.ChildName &&
			a.ChildInput == b.ChildInput &&
			a.RunID == b.RunID

	case EventTypeAwaitChild:
		return a.RunID == b.RunID &&
			a.Response == b.Response &&
			a.Err == b.Err

	case EventTypeContinueAsNew:
		return a.NewInput == b.NewInput

	case EventTypeHeartbeat:
		return a.Service == b.Service &&
			a.Op == b.Op

	case EventTypeAwaitAllChildren:
		// The Request field (runIDs JSON) is not preserved through
		// compaction; only Response is needed for deterministic replay.
		return a.Response == b.Response

	case EventTypePluginCall:
		return a.PluginName == b.PluginName &&
			a.PluginFunc == b.PluginFunc &&
			a.PluginInput == b.PluginInput &&
			a.PluginOutput == b.PluginOutput &&
			a.PluginError == b.PluginError

	case EventTypeSideEffect:
		return a.SideEffectResult == b.SideEffectResult

	case EventTypeCreatePromise:
		return a.PromiseName == b.PromiseName &&
			a.PromiseID == b.PromiseID

	case EventTypeAwaitPromise:
		return a.PromiseID == b.PromiseID

	case EventTypePromiseResolved:
		return a.PromiseID == b.PromiseID &&
			a.PromiseResult == b.PromiseResult

	case EventTypePromiseRejected:
		return a.PromiseID == b.PromiseID &&
			a.PromiseError == b.PromiseError

	case EventTypeUpdateHandler:
		return a.UpdateHandlerName == b.UpdateHandlerName

	case EventTypeStateMutation:
		return a.StateKey == b.StateKey &&
			a.StateValue == b.StateValue &&
			a.StateDelta == b.StateDelta &&
			a.StateOp == b.StateOp

	case EventTypeRunDetached:
		// RunDetached stores no compacted fields.
		return true
	case EventTypePluginCallStreamChunk:
		return a.PluginName == b.PluginName &&
			a.PluginFunc == b.PluginFunc &&
			a.PluginInput == b.PluginInput &&
			a.PluginOutput == b.PluginOutput &&
			a.PluginError == b.PluginError

	case EventTypeAcquireLock:
		return a.LockKey == b.LockKey &&
			a.LockTTLMs == b.LockTTLMs &&
			a.LockAcquired == b.LockAcquired

	case EventTypeReleaseLock:
		return a.LockKey == b.LockKey

	case EventTypeScopeAcquired:
		return a.ScopeKey == b.ScopeKey
	}
	return false
}

// ---------------------------------------------------------------------------
// Diagnostics
// ---------------------------------------------------------------------------

// eventSummary returns a compact one-line representation of an event for
// diagnostic output.
func eventSummary(ev EventRecord) string {
	switch ev.EventType {
	case EventTypeCall:
		return fmt.Sprintf("Call{svc=%s op=%s req=%s resp=%s err=%q}",
			trunc(ev.Service, 12), trunc(ev.Op, 12),
			trunc(ev.Request, 12), trunc(ev.Response, 12), ev.Err)
	case EventTypeSleep:
		return fmt.Sprintf("Sleep{dur=%d}", ev.DurationMs)
	case EventTypeAwaitSignals:
		return fmt.Sprintf("AwaitSignals{sigs=%s timeout=%d}", trunc(ev.SignalNames, 16), ev.TimeoutMs)
	case EventTypeSignalReceived:
		return fmt.Sprintf("SignalReceived{name=%s payload=%s}", trunc(ev.SignalName, 12), trunc(ev.SignalPayload, 12))
	case EventTypeDefer:
		return fmt.Sprintf("Defer{id=%s desc=%s}", trunc(ev.DeferID, 12), trunc(ev.DeferDescription, 16))
	case EventTypeChildWorkflow:
		return fmt.Sprintf("ChildWf{name=%s input=%s runID=%s}", trunc(ev.ChildName, 12), trunc(ev.ChildInput, 12), trunc(ev.RunID, 12))
	case EventTypeAwaitChild:
		return fmt.Sprintf("AwaitChild{runID=%s resp=%s err=%q}", trunc(ev.RunID, 12), trunc(ev.Response, 12), ev.Err)
	case EventTypeContinueAsNew:
		return fmt.Sprintf("CAN{input=%s}", trunc(ev.NewInput, 20))
	case EventTypeHeartbeat:
		return fmt.Sprintf("Heartbeat{svc=%s op=%s}", trunc(ev.Service, 12), trunc(ev.Op, 12))
	case EventTypeAwaitAllChildren:
		return fmt.Sprintf("AwaitAll{resp=%s}", trunc(ev.Response, 20))
	case EventTypePluginCall:
		return fmt.Sprintf("PluginCall{name=%s fn=%s}", trunc(ev.PluginName, 12), trunc(ev.PluginFunc, 12))
	case EventTypeCreatePromise:
		return fmt.Sprintf("CreatePromise{name=%s id=%s}", trunc(ev.PromiseName, 12), trunc(ev.PromiseID, 12))
	case EventTypeAwaitPromise:
		return fmt.Sprintf("AwaitPromise{id=%s}", trunc(ev.PromiseID, 12))
	case EventTypePromiseResolved:
		return fmt.Sprintf("PromiseResolved{id=%s result=%s}", trunc(ev.PromiseID, 12), trunc(ev.PromiseResult, 12))
	case EventTypePromiseRejected:
		return fmt.Sprintf("PromiseRejected{id=%s err=%q}", trunc(ev.PromiseID, 12), ev.PromiseError)
	case EventTypeUpdateHandler:
		return fmt.Sprintf("UpdateHandler{name=%s}", trunc(ev.UpdateHandlerName, 16))
	case EventTypeStateMutation:
		return fmt.Sprintf("StateMutation{key=%s value=%s op=%s delta=%d}", trunc(ev.StateKey, 8), trunc(ev.StateValue, 8), trunc(ev.StateOp, 8), ev.StateDelta)
	case EventTypeRunDetached:
		return "RunDetached{}"
	case EventTypePluginCallStreamChunk:
		return fmt.Sprintf("PluginCallStreamChunk{name=%s fn=%s}", trunc(ev.PluginName, 12), trunc(ev.PluginFunc, 12))
	default:
		return fmt.Sprintf("Unknown{%s}", ev.EventType)
	}
}

// dumpEventDiff prints the differing fields between two events on t.Log.
func dumpEventDiff(t *testing.T, a, b EventRecord) {
	t.Helper()
	// Always print Step and Type.
	if a.Step != b.Step {
		t.Logf("  Step: %d vs %d", a.Step, b.Step)
	}
	if a.EventType != b.EventType {
		t.Logf("  EventType: %s vs %s", a.EventType, b.EventType)
		return
	}
	switch a.EventType {
	case EventTypeCall:
		mismatchStr("Service", a.Service, b.Service, t)
		mismatchStr("Op", a.Op, b.Op, t)
		mismatchStr("Request", a.Request, b.Request, t)
		mismatchStr("Response", a.Response, b.Response, t)
		mismatchStr("Err", a.Err, b.Err, t)
	case EventTypeSleep:
		mismatchInt("DurationMs", a.DurationMs, b.DurationMs, t)
	case EventTypeAwaitSignals:
		mismatchStr("SignalNames", a.SignalNames, b.SignalNames, t)
		mismatchInt("TimeoutMs", a.TimeoutMs, b.TimeoutMs, t)
	case EventTypeSignalReceived:
		mismatchStr("SignalName", a.SignalName, b.SignalName, t)
		mismatchStr("SignalPayload", a.SignalPayload, b.SignalPayload, t)
	case EventTypeDefer:
		mismatchStr("DeferID", a.DeferID, b.DeferID, t)
		mismatchStr("DeferDescription", a.DeferDescription, b.DeferDescription, t)
	case EventTypeChildWorkflow:
		mismatchStr("ChildName", a.ChildName, b.ChildName, t)
		mismatchStr("ChildInput", a.ChildInput, b.ChildInput, t)
		mismatchStr("RunID", a.RunID, b.RunID, t)
	case EventTypeAwaitChild:
		mismatchStr("RunID", a.RunID, b.RunID, t)
		mismatchStr("Response", a.Response, b.Response, t)
		mismatchStr("Err", a.Err, b.Err, t)
	case EventTypeContinueAsNew:
		mismatchStr("NewInput", a.NewInput, b.NewInput, t)
	case EventTypeHeartbeat:
		mismatchStr("Service", a.Service, b.Service, t)
		mismatchStr("Op", a.Op, b.Op, t)
	case EventTypeAwaitAllChildren:
		mismatchStr("Response", a.Response, b.Response, t)
	case EventTypePluginCall:
		mismatchStr("PluginName", a.PluginName, b.PluginName, t)
		mismatchStr("PluginFunc", a.PluginFunc, b.PluginFunc, t)
		mismatchStr("PluginInput", a.PluginInput, b.PluginInput, t)
		mismatchStr("PluginOutput", a.PluginOutput, b.PluginOutput, t)
		mismatchStr("PluginError", a.PluginError, b.PluginError, t)
	case EventTypeCreatePromise:
		mismatchStr("PromiseName", a.PromiseName, b.PromiseName, t)
		mismatchStr("PromiseID", a.PromiseID, b.PromiseID, t)
	case EventTypeAwaitPromise:
		mismatchStr("PromiseID", a.PromiseID, b.PromiseID, t)
	case EventTypePromiseResolved:
		mismatchStr("PromiseID", a.PromiseID, b.PromiseID, t)
		mismatchStr("PromiseResult", a.PromiseResult, b.PromiseResult, t)
	case EventTypePromiseRejected:
		mismatchStr("PromiseID", a.PromiseID, b.PromiseID, t)
		mismatchStr("PromiseError", a.PromiseError, b.PromiseError, t)
	case EventTypeUpdateHandler:
		mismatchStr("UpdateHandlerName", a.UpdateHandlerName, b.UpdateHandlerName, t)
	case EventTypeStateMutation:
		mismatchStr("StateKey", a.StateKey, b.StateKey, t)
		mismatchStr("StateValue", a.StateValue, b.StateValue, t)
		mismatchInt("StateDelta", a.StateDelta, b.StateDelta, t)
		mismatchStr("StateOp", a.StateOp, b.StateOp, t)
	case EventTypeRunDetached:
		// No compacted fields.
	case EventTypePluginCallStreamChunk:
		mismatchStr("PluginName", a.PluginName, b.PluginName, t)
		mismatchStr("PluginFunc", a.PluginFunc, b.PluginFunc, t)
		mismatchStr("PluginInput", a.PluginInput, b.PluginInput, t)
		mismatchStr("PluginOutput", a.PluginOutput, b.PluginOutput, t)
		mismatchStr("PluginError", a.PluginError, b.PluginError, t)
	}
}

func mismatchStr(name, a, b string, t *testing.T) {
	if a != b {
		t.Logf("  %s: %q vs %q", name, a, b)
	}
}

func mismatchInt(name string, a, b int64, t *testing.T) {
	if a != b {
		t.Logf("  %s: %d vs %d", name, a, b)
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
