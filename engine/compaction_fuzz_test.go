package engine

import (
	"fmt"
	"reflect"
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
	f.Add(fuzzSeed(EventCodeCall, []string{"svc", "op", `{}`, `{}`, ""}))
	// Pair: ChildWorkflow + child completing AwaitChild (tests open-children
	// tracking and completion).
	f.Add(append(
		fuzzSeed(EventCodeChildWorkflow, []string{"child-wf", `{"x":1}`, "run-c1"}, []int64{}...),
		fuzzSeed(EventCodeAwaitChild, []string{"run-c1", `{"y":2}`, ""}, []int64{}...)...,
	))

	// Single SideEffect.
	f.Add(fuzzSeed(EventCodeSideEffect, []string{`"result"`}, []int64{}...))
	// Single ScopeAcquired.
	f.Add(fuzzSeed(EventCodeScopeAcquired, []string{"my-scope"}, []int64{}...))
	// Single AcquireLock.
	f.Add(fuzzSeed(EventCodeAcquireLock, []string{"lock-key-1"}, 5000))
	// Single ReleaseLock.
	f.Add(fuzzSeed(EventCodeReleaseLock, []string{"lock-key-1"}, []int64{}...))
	// Single Fetch.
	f.Add(fuzzSeed(EventCodeFetch, []string{"GET", "https://example.com", `{"Auth":"x"}`, "", `{"status":200}`, ""}, []int64{}...))
	// Single DurableLog.
	f.Add(fuzzSeed(EventCodeDurableLog, []string{"hello world", "info", "key=val"}, []int64{}...))
	// Single DurableSend.
	f.Add(fuzzSeed(EventCodeDurableSend, []string{"svc", "op", `{}`}, []int64{}...))
	// Single DurableScheduleInvoke.
	f.Add(fuzzSeed(EventCodeDurableScheduleInvoke, []string{"svc", "op", `{}`}, 5000))
	// Single ScheduleCron/DeleteCron/ListCrons -- unreachable before the type
	// byte's modulus was corrected from 27 to 30; see the comment on
	// parseFuzzEvents' typeCode computation.
	f.Add(fuzzSeed(EventCodeScheduleCron, []string{"order-cleanup", "0 0 * * *", "UTC", `{}`, "sched-1", `{"ok":true}`, ""}, []int64{}...))
	f.Add(fuzzSeed(EventCodeDeleteCron, []string{"", "", "", "", "sched-1", "", ""}, []int64{}...))
	f.Add(fuzzSeed(EventCodeListCrons, []string{"", "", "", "", "", `["sched-1"]`, ""}, []int64{}...))

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
	// The parser reads one int64 byte for TimestampMs immediately after the
	// type code and before any type-specific field, since every event
	// carries a timestamp in production regardless of type (see the
	// TimestampMs assignment in parseFuzzEvents). Seeds default it to 0;
	// go test -fuzz mutates this byte like any other, so it still gets
	// explored over time even though no seed below sets it explicitly.
	b := []byte{typeCode, 0}
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
		// Clamp to [0, 29] -- the full EventCode* range (compaction.go). This
		// was `% 27` (clamping to [0, 26]) until 2026-08-09, which meant
		// EventCodeScheduleCron (27), EventCodeDeleteCron (28), and
		// EventCodeListCrons (29) could never be generated: the fuzzer's type
		// byte can never land on them, so three of the ~27 compacted event
		// types went unfuzzed regardless of how long -fuzz ran. Found while
		// auditing coverage for this same stream's compaction fix.
		typeCode := int(r.readByte()) % 30

		ev := EventRecord{
			Step:      step,
			EventType: codeToEventType[typeCode],
			// TimestampMs is set on every EventRecord in production
			// (execSession.recordEvent, lifecycle.go) regardless of event
			// type, and is now preserved for every type through compaction
			// (see the field doc on CompactedEvent.TimestampMs). Reading it
			// here unconditionally, rather than per-type, is what lets the
			// fuzzer explore it for every event kind instead of just the
			// ones a case below happens to touch.
			TimestampMs: r.readInt64(),
		}
		if ev.EventType == "" {
			ev.EventType = EventTypeCall // unknown code, default to call
		}

		switch typeCode {
		case EventCodeCall: // svc, op, req, resp, err (5 strings), nonRetryable (1 byte)
			ev.Service = r.readString()
			ev.Op = r.readString()
			ev.Request = r.readString()
			ev.Response = r.readString()
			ev.Err = r.readString()
			ev.ErrNonRetryable = r.readByte()%2 == 1
		case EventCodeAwaitSignals: // sigNames (string), timeoutMs (int64)
			ev.SignalNames = r.readString()
			ev.TimeoutMs = r.readInt64()
		case EventCodeSignalReceived: // sigName, sigPayload (2 strings)
			ev.SignalName = r.readString()
			ev.SignalPayload = r.readString()
		case EventCodeDefer: // deferID, deferDesc (2 strings)
			ev.DeferID = r.readString()
			ev.DeferDescription = r.readString()
		case EventCodeChildWorkflow: // childName, childInput, runID, parentWorkflowID, parentClosePolicy (5 strings)
			ev.ChildName = r.readString()
			ev.ChildInput = r.readString()
			ev.RunID = r.readString()
			ev.ParentWorkflowID = r.readString()
			ev.ParentClosePolicy = r.readString()
		case EventCodeAwaitChild: // runID, resp, errMsg (3 strings)
			ev.RunID = r.readString()
			ev.Response = r.readString()
			ev.Err = r.readString()
		case EventCodeContinueAsNew: // newInput (1 string), newVersion (1 int64)
			ev.NewInput = r.readString()
			ev.NewVersion = int(r.readInt64())
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
		case EventCodeStateMutation: // key, value, op, stateKeys (4 strings), delta (1 int64)
			ev.StateKey = r.readString()
			ev.StateValue = r.readString()
			ev.StateOp = r.readString()
			ev.StateDelta = r.readInt64()
			ev.StateKeys = r.readString()
		case EventCodeRunDetached: // no string fields
		case EventCodeFetch: // method, url, headers, body, response, err (6 strings)
			ev.FetchMethod = r.readString()
			ev.FetchURL = r.readString()
			ev.FetchHeaders = r.readString()
			ev.FetchBody = r.readString()
			ev.FetchResponse = r.readString()
			ev.Err = r.readString()
		case EventCodeScheduleCron, EventCodeDeleteCron, EventCodeListCrons:
			// workflowName, expr, timezone, input, scheduleID, result, err (7 strings)
			ev.CronWorkflowName = r.readString()
			ev.CronExpr = r.readString()
			ev.CronTimezone = r.readString()
			ev.CronInput = r.readString()
			ev.CronScheduleID = r.readString()
			ev.CronResult = r.readString()
			ev.Err = r.readString()
		case EventCodePluginCallStreamChunk: // pluginName, pluginFunc, input, output, err (5 strings), chunkIndex (1 int64), finish (1 byte)
			ev.PluginName = r.readString()
			ev.PluginFunc = r.readString()
			ev.PluginInput = r.readString()
			ev.PluginOutput = r.readString()
			ev.PluginError = r.readString()
			ev.StreamChunkIndex = int(r.readInt64())
			ev.StreamFinish = r.readByte()%2 == 1
		case EventCodeSideEffect: // sideEffectResult (1 string)
			ev.SideEffectResult = r.readString()
		case EventCodeScopeAcquired: // scopeKey (1 string)
			ev.ScopeKey = r.readString()
		case EventCodeDurableLog: // message, level, kv (3 strings)
			ev.Message = r.readString()
			ev.LogLevel = r.readString()
			ev.LogKV = r.readString()
		case EventCodeDurableSend: // svc, op, req (3 strings)
			ev.Service = r.readString()
			ev.Op = r.readString()
			ev.Request = r.readString()
		case EventCodeDurableScheduleInvoke: // svc, op, req (3 strings), delay (int64)
			ev.Service = r.readString()
			ev.Op = r.readString()
			ev.Request = r.readString()
			ev.DurationMs = r.readInt64()

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
//
// eventFieldsMatch used to be a hand-maintained switch, one field list per
// EventType, checking only the fields its author knew a given event type
// carried. That is exactly how S4a went undetected: ErrNonRetryable was
// added to EventRecord and wired into the EventTypeCall constructors
// (durablecalls.go, heartbeats.go), but nobody updated this switch's
// EventTypeCall case to compare it, so a round trip that silently dropped
// the bit still reported a match. TestCompactionRoundTripThenReplay's
// EventTypeCall cases had run green throughout.
//
// It is now reflection-based instead: every exported EventRecord field is
// compared for every event, unless the field name is listed in
// compactionExemptFields with a reason. Adding a new field to EventRecord
// and populating it from some event constructor is caught automatically the
// next time a test event sets that field to a non-zero value -- no update to
// this file is needed unless the new field genuinely cannot survive
// compaction, in which case it must be added to compactionExemptFields
// alongside a reason, the same way the fields below were audited.

// compactionExemptFields lists EventRecord fields allowed to NOT survive a
// compaction round-trip, each with the reason it cannot (or need not). Every
// field is individually audited here as of 2026-08-09; re-derive with:
//
//	go doc -all github.com/cleat-team/cleat/engine.EventRecord
//
// and cross-check each new field against extractCompactionState /
// buildFullHistoryFromCompaction (engine/compaction.go) before adding it.
var compactionExemptFields = map[string]string{
	"Step": "structural: assigned by reconstruction position (the loop index " +
		"in buildFullHistoryFromCompaction), not carried inside CompactedEvent; " +
		"compared explicitly by the a.Step != b.Step check below",
	"EventType": "structural: recovered via codeToEventType, not a plain " +
		"copied field; compared explicitly by the a.EventType != b.EventType " +
		"check below",
	"CreatedAt": "wall-clock time the row was inserted, documented on " +
		"EventRecord (types.go) as being for admin timeline visualization " +
		"only and may already be zero for events loaded without timestamps -- " +
		"not required for deterministic replay",
	"Pending": "derived read (json:\"-\"): computed from the intent_at/" +
		"checksum columns at load time (store_intent.go), never itself a " +
		"persisted value, so there is nothing for a JSONB compaction snapshot " +
		"to carry",
	"Idempotent": "not persisted to the event_history table in any store " +
		"backend as of 2026-08-09 (no such column in store_event_write.go, " +
		"mysql_events.go, or mssql_events.go: grep -rn '\"idempotent\"' " +
		"engine/store_event_write.go engine/mysql_events.go engine/mssql_events.go " +
		"returns nothing). The bit is already false by the time " +
		"extractCompactionState receives the event -- it does not survive an " +
		"ordinary restart-replay either, independent of compaction. plugins.go " +
		"falls back to a live registry lookup for exactly this reason.",
	"UpdatePayload": "declared on EventRecord but never assigned anywhere in " +
		"the engine as of 2026-08-09 (grep -rn 'UpdatePayload:' engine/*.go " +
		"outside types.go returns nothing) -- a dead field with nothing to " +
		"lose. RegisterUpdateHandler (lifecycle.go) only ever sets " +
		"UpdateHandlerName.",
	"UpdateResponse": "dead field, see UpdatePayload",
	"UpdateError":    "dead field, see UpdatePayload",
}

// eventFieldsMatch reports whether every non-exempt field of a and b is
// equal.
func eventFieldsMatch(a, b EventRecord) bool {
	if a.Step != b.Step || a.EventType != b.EventType {
		return false
	}
	if _, known := eventTypeToCode[a.EventType]; !known {
		// An EventType extractCompactionState has no case for (see
		// eventTypeToCode, compaction.go) cannot be claimed equivalent just
		// because its fields happen to agree -- nothing here actually
		// exercised how that type round-trips, since compaction does not
		// know how to compact it. Preserves the pre-reflection behaviour's
		// same refusal (TestEventFieldsMatch_AllEventTypes/Unknown_type).
		return false
	}
	return len(diffEventFields(a, b)) == 0
}

// diffEventFields returns the names of every non-exempt EventRecord field
// that differs between a and b.
func diffEventFields(a, b EventRecord) []string {
	var diffs []string
	av := reflect.ValueOf(a)
	bv := reflect.ValueOf(b)
	rt := av.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		if _, exempt := compactionExemptFields[f.Name]; exempt {
			continue
		}
		if !reflect.DeepEqual(av.Field(i).Interface(), bv.Field(i).Interface()) {
			diffs = append(diffs, f.Name)
		}
	}
	return diffs
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
	case EventTypeFetch:
		return fmt.Sprintf("Fetch{method=%s url=%s}", trunc(ev.FetchMethod, 8), trunc(ev.FetchURL, 20))
	case EventTypePluginCallStreamChunk:
		return fmt.Sprintf("PluginCallStreamChunk{name=%s fn=%s}", trunc(ev.PluginName, 12), trunc(ev.PluginFunc, 12))
	case EventTypeSideEffect:
		return fmt.Sprintf("SideEffect{result=%s}", trunc(ev.SideEffectResult, 20))
	case EventTypeScopeAcquired:
		return fmt.Sprintf("ScopeAcquired{key=%s}", trunc(ev.ScopeKey, 20))
	case EventTypeAcquireLock:
		return fmt.Sprintf("AcquireLock{key=%s ttl=%d acquired=%d}", trunc(ev.LockKey, 12), ev.LockTTLMs, ev.LockAcquired)
	case EventTypeReleaseLock:
		return fmt.Sprintf("ReleaseLock{key=%s}", trunc(ev.LockKey, 12))
	case EventTypeDurableLog:
		return fmt.Sprintf("DurableLog{msg=%s level=%s}", trunc(ev.Message, 20), trunc(ev.LogLevel, 8))
	case EventTypeDurableSend:
		return fmt.Sprintf("DurableSend{svc=%s op=%s}", trunc(ev.Service, 12), trunc(ev.Op, 12))
	case EventTypeDurableScheduleInvoke:
		return fmt.Sprintf("DurableScheduleInvoke{svc=%s op=%s delay=%d}", trunc(ev.Service, 12), trunc(ev.Op, 12), ev.DurationMs)
	default:
		return fmt.Sprintf("Unknown{%s}", ev.EventType)
	}
}

// dumpEventDiff prints every differing non-exempt field between two events
// on t.Log, using the same reflection-based walk as diffEventFields so the
// diagnostic output can never omit a field the pass/fail decision considered.
func dumpEventDiff(t *testing.T, a, b EventRecord) {
	t.Helper()
	if a.Step != b.Step {
		t.Logf("  Step: %d vs %d", a.Step, b.Step)
	}
	if a.EventType != b.EventType {
		t.Logf("  EventType: %s vs %s", a.EventType, b.EventType)
		return
	}
	av := reflect.ValueOf(a)
	bv := reflect.ValueOf(b)
	rt := av.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		if _, exempt := compactionExemptFields[f.Name]; exempt {
			continue
		}
		af := av.Field(i).Interface()
		bf := bv.Field(i).Interface()
		if !reflect.DeepEqual(af, bf) {
			t.Logf("  %s: %v vs %v", f.Name, af, bf)
		}
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
