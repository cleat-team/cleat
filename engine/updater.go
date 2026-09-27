package engine

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tetratelabs/wazero/api"
)

// Workflow updates: a request/reply call into a running workflow that can both
// change its state and return a value to the caller.
//
// # Why delivery is an event and not just a table read
//
// workflow_update_requests is the queue -- it gives arrival and crash
// survival. It does not give determinism. Replay re-executes the guest from
// the top and matches each host call against history[stepCount] in order, so
// for a handler that mutates workflow state to be reproduced, the delivery has
// to happen at the same PROGRAM POSITION on every run. That is what
// EventTypeUpdateReceived records.
//
// The constraint is hard rather than stylistic: recordEvent APPENDS
// (`s.history = append(s.history, rec)`), so an event can only ever land at the
// frontier. There is no way to insert a delivery in the middle of an existing
// history, which is why an update cannot be dispatched at, say, handler
// registration time -- on every segment after the first, registration is
// replayed from deep inside history that is already written.
//
// So the guest polls at fixed program positions (the SDK does this before each
// suspension, and exposes DispatchUpdates for authors who want more), and:
//
//   - replaying: return history[stepCount] if it is an update_received, else
//     report nothing found. The table is NOT consulted -- a request that
//     arrived later must not be delivered at an earlier step.
//   - fresh: read the table, record update_received, return it.
//
// This is DurableAwaitSignals' shape, for the same reason.
//
// # The handler re-runs on every replay, and that is the point
//
// The durable facts are the handler's input and its output, not its
// execution. Re-running it is what rebuilds the state it mutated. Durable
// calls the handler itself makes append between the received and completed
// events and replay in order like any others.

// updateDelivery is the JSON envelope cleat_poll_update writes into the guest's
// buffer. One buffer rather than three out-params: three lengths plus a found
// flag do not fit an i64 result alongside each other, and every SDK already has
// a JSON decoder because the ABI is JSON-carrying throughout.
type updateDelivery struct {
	Name    string `json:"name"`
	Payload string `json:"payload"`
	// RequestID here is the composite KEY from updateRequestKey, not the
	// request_id COLUMN cleat#1416 added -- the two are different things
	// wearing one name, and this comment is the only place that says so.
	//
	// The key packs the update name, the row's request_id and the promise id
	// into one opaque string, because the guest hands back exactly what it was
	// given and the host needs all three to settle the request. No SDK parses
	// it; splitUpdateRequestKey is the only reader. Renaming the JSON field
	// would be an ABI change across five SDKs for a clarification, which is why
	// this is a comment.
	RequestID string `json:"request_id"`
}

// DurablePollUpdate delivers the next pending update request, or reports that
// there is none.
//
// The result is packed like PollSignal's: bytes written in the high 32 bits,
// flags in the low 32, with 0x0100 meaning found. A guest that gets found=false
// must not call DurableCompleteUpdate.
func (s *execSession) DurablePollUpdate(ctx context.Context, m api.Module, outPtr, outMaxLen uint32) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeUpdateReceived {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}
				return s.writeUpdateDelivery(ctx, m, outPtr, outMaxLen, updateDelivery{
					Name:      rec.UpdateHandlerName,
					Payload:   rec.UpdatePayload,
					RequestID: rec.UpdateRequestID,
				})
			}
			// Anything else at this step means no update was delivered here on
			// the original run. Report nothing found WITHOUT advancing and
			// WITHOUT leaving replay: the guest polls at every suspension, so
			// most polls legitimately find nothing, and treating that as a
			// history mismatch would end replay at the first one.
			return 0
		}
		s.exitReplay()
	}

	if s.engine.updateStore == nil {
		return 0
	}

	// A fresh delivery is new work: it runs guest code that can start calls,
	// children and timers. A defer segment exists to run a terminated
	// workflow's cleanup, not to service new requests. Same reasoning as
	// DurableAwaitSignals and IMPROVEMENT-PLAN 3.84.
	if s.stopBeforeNewWork() {
		return callSuspendSentinel
	}

	pending, err := s.engine.updateStore.GetPendingUpdateRequests(ctx, s.engine.workflowID)
	if err != nil {
		s.engine.log().ErrorContext(ctx, "poll_update: reading pending update requests",
			"workflow_id", s.engine.workflowID, "error", err)
		return 0
	}
	if len(pending) == 0 {
		return 0
	}
	upd := pending[0]

	// Record BEFORE the guest runs the handler, and before anything settles.
	// A crash after this point leaves the event in history and the row still
	// pending, so the next replay finds the delivery here and never re-reads
	// the table -- at-least-once delivery with idempotent replay. Recording
	// after would lose the update entirely on a crash, and the caller would
	// wait forever on a promise nothing settles.
	s.recordEvent(EventRecord{
		Step:              s.stepCount,
		EventType:         EventTypeUpdateReceived,
		UpdateHandlerName: upd.UpdateName,
		UpdatePayload:     upd.Payload,
		UpdateRequestID:   updateRequestKey(upd),
	})

	return s.writeUpdateDelivery(ctx, m, outPtr, outMaxLen, updateDelivery{
		Name:      upd.UpdateName,
		Payload:   upd.Payload,
		RequestID: updateRequestKey(upd),
	})
}

// DurableCompleteUpdate records the handler's outcome and settles the caller's
// promise.
//
// errMsg being non-empty is what distinguishes a rejection from a result; a
// handler that returns an empty string and no error completes successfully with
// an empty result, which is a legitimate outcome and not a missing one.
func (s *execSession) DurableCompleteUpdate(ctx context.Context, m api.Module, requestID, result, errMsg string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if rec.EventType == EventTypeUpdateCompleted {
				if !s.advanceReplayStep(ctx, &rec) {
					return 0
				}
				// Deliberately does NOT settle again. The promise was settled
				// on the original run; re-settling would report not-found
				// (#818) and turn every replay into an error.
				return 0
			}
		}
		s.exitReplay()
	}

	if s.stopBeforeNewWork() {
		return callSuspendSentinel
	}

	name, rowID, promiseID := splitUpdateRequestKey(requestID)
	if rowID == "" {
		// A key written before cleat#1416, replayed across the upgrade. The
		// migrations backfilled request_id from update_name, so the name
		// addresses exactly the row this key was written for.
		rowID = name
	}

	s.recordEvent(EventRecord{
		Step:              s.stepCount,
		EventType:         EventTypeUpdateCompleted,
		UpdateHandlerName: name,
		UpdateRequestID:   requestID,
		UpdateResponse:    result,
		UpdateError:       errMsg,
	})

	if s.engine.updateStore == nil {
		return 0
	}

	if err := s.engine.updateStore.CompleteUpdateRequest(ctx, s.engine.workflowID, rowID, result, errMsg); err != nil {
		s.engine.log().ErrorContext(ctx, "complete_update: recording the outcome on the request row",
			"workflow_id", s.engine.workflowID, "update_name", name, "error", err)
	}

	// A request with no promise has no caller blocked on it -- the promise_id
	// column is nullable and the HTTP API is not the only way a request can be
	// made. Completing the row is still right; settling nothing is too.
	if promiseID == "" {
		return 0
	}
	var settleErr error
	if errMsg != "" {
		settleErr = s.engine.updateStore.RejectPromise(ctx, promiseID, errMsg)
	} else {
		settleErr = s.engine.updateStore.ResolvePromise(ctx, promiseID, result)
	}
	if settleErr != nil {
		s.engine.log().ErrorContext(ctx, "complete_update: settling the caller's promise",
			"workflow_id", s.engine.workflowID, "update_name", name,
			"promise_id", promiseID, "error", settleErr)
	}
	return 0
}

func (s *execSession) writeUpdateDelivery(ctx context.Context, m api.Module, outPtr, outMaxLen uint32, d updateDelivery) int64 {
	b, err := json.Marshal(d)
	if err != nil {
		// Unreachable for three strings, but a silent truncation here would
		// hand the guest a half-written envelope it would then parse.
		s.engine.log().ErrorContext(ctx, "poll_update: encoding the delivery envelope",
			"workflow_id", s.engine.workflowID, "error", err)
		return 0
	}
	written, _ := s.writeResult(ctx, m, outPtr, string(b), outMaxLen)
	return int64(uint64(written)<<32 | uint64(updateFoundFlag))
}

// updateFoundFlag mirrors PollSignal's found bit so the two calls unpack the
// same way in every SDK.
const updateFoundFlag = uint32(0x0100)

// updateRequestKey identifies a request to the guest and back.
//
// workflow_update_requests has no single-column identifier the store exposes:
// CompleteUpdateRequest is keyed by (workflow_id, update_name) and the promise
// is keyed by promise_id, so the guest has to hand both back. They travel as
// one opaque string because the guest must not have to know the shape -- it
// receives this and returns it unchanged.
//
// # Length-prefixed, because there is no safe separator
//
// This joined the halves with a literal NUL until IMPROVEMENT-PLAN 3.247, and
// the key is recorded as UpdateRequestID on the update_received event, which
// store_events.go puts in `event_history.payload` -- JSONB on PostgreSQL. A NUL
// is legal in a JSON string and PostgreSQL refuses it anyway:
//
//	pq: unsupported Unicode escape sequence (22P05)
//
// so finalizing the segment failed for EVERY update that reached a dispatch
// point. The harmful part was the ordering: runUpdate settles the caller's
// promise before the segment finalizes, so the caller was told `resolved` and
// then the workflow failed and the handler's state was discarded (#914).
//
// Measured 2026-09-07, and it is dialect-divergent -- which is why the fix is
// not "pick a different control character":
//
//	postgres   ERROR: unsupported Unicode escape sequence (22P05)
//	mysql      accepted, stored as \u0000
//	mssql      accepted, ISJSON() = 1
//
// **No separator is safe**, because UpdateName is chosen by the workflow author
// and can contain anything. A rarer delimiter moves the collision rather than
// removing it. The length prefix is unambiguous for every possible input:
//
//	"add" + "p-1"    ->  "3:addp-1"
//	"a:b" + ""       ->  "3:a:b"
//	"" + "p-1"       ->  "0:p-1"
//
// splitUpdateRequestKey is the only reader, so the encoding is free to change.
//
// It changed for cleat#1416, which made an update name reusable: the key now
// carries the request's own identity as well, because the name no longer
// identifies a row. Two length-prefixed fields, and a "u3:" marker so the two
// older forms can be told apart WITHOUT GUESSING.
//
// The marker is not decoration. A v2 key is `len:name` followed by the promise
// id, and a v3 key is `len:name` followed by `len:requestID` followed by the
// promise id -- so distinguishing them by looking for digits-then-colon after
// the name means asking whether a PROMISE ID happens to start that way. Promise
// ids are `upd-<hex>` in the worker but arbitrary in the store's contract, and
// the tests alone use "prom-1", "promise-a" and "". A prefix costs three bytes
// and removes the question.
func updateRequestKey(u UpdateRequestInfo) string {
	return "u3:" + strconv.Itoa(len(u.UpdateName)) + ":" + u.UpdateName +
		strconv.Itoa(len(u.RequestID)) + ":" + u.RequestID + u.PromiseID
}

// splitUpdateRequestKey reverses updateRequestKey.
//
// It reads all three forms this function has written. A workflow suspended
// mid-update across an upgrade replays a key written by the previous version,
// so none of them can be dropped:
//
//	u3:3:add5:ureq-x…p-1   current -- name, request id, promise id
//	3:addp-1               cleat#3.247's, no request id
//	add\0p-1               the original NUL-separated form
//
// A missing request id is returned EMPTY rather than guessed at. The caller
// supplies the fallback, and there is exactly one: migrations/postgres/068 and
// its two siblings backfill request_id from update_name, so for any row written
// before the upgrade the name IS the request id. That is why the backfill was
// chosen to be the name rather than a generated value.
//
// The NUL form cannot exist on PostgreSQL -- the write that would have
// persisted one is the write that failed -- but MySQL and SQL Server accepted
// them.
func splitUpdateRequestKey(key string) (updateName, requestID, promiseID string) {
	// Legacy NUL form first: neither a length prefix nor the "u3:" marker can
	// contain a NUL, so the forms cannot be confused.
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return key[:i], "", key[i+1:]
		}
	}

	if rest, ok := strings.CutPrefix(key, "u3:"); ok {
		name, after, ok := cutLengthPrefixed(rest)
		if !ok {
			return key, "", ""
		}
		rid, after, ok := cutLengthPrefixed(after)
		if !ok {
			return key, "", ""
		}
		return name, rid, after
	}

	name, after, ok := cutLengthPrefixed(key)
	if !ok {
		// Not a form this function wrote. Returning the whole key as the name
		// preserves the pre-3.247 behaviour for a malformed key rather than
		// inventing a new failure mode here.
		return key, "", ""
	}
	return name, "", after
}

// cutLengthPrefixed reads one `<len>:<value>` field and returns the rest.
func cutLengthPrefixed(s string) (value, rest string, ok bool) {
	colon := strings.IndexByte(s, ':')
	if colon < 0 {
		return "", "", false
	}
	n, err := strconv.Atoi(s[:colon])
	if err != nil || n < 0 || colon+1+n > len(s) {
		return "", "", false
	}
	return s[colon+1 : colon+1+n], s[colon+1+n:], true
}
