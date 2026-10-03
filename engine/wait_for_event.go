package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// eventWaiter is the narrow interface the cleat_wait_for_event export needs.
// *execSession satisfies it, and it is deliberately NOT folded into
// HostHandler.
//
// HostHandler is implemented by six or more test doubles whose whole purpose is
// to exercise OTHER calls. Adding a method to it makes every one of them grow a
// stub for a call none of them makes -- churn in test files this change has no
// business touching, and a needless collision surface while other streams are
// editing the same package. The compile-time guarantee an interface method
// would buy is a guarantee about test doubles, not about production hosts: the
// only production implementation is execSession. A host that does not implement
// this reports the call as unsupported instead of panicking, which is the
// honest outcome for a double that has no opinion about waiting.
type eventWaiter interface {
	WaitForEvent(ctx context.Context, m api.Module, pluginName, functionName, inputJSON, signalNames string, timeoutMs int64, outPtr, outMaxLen uint32) int64
}

// eventRecheckMs is the longest a single re-claim sleeps before looking again.
//
// The wake is usually the signal the registration itself triggers, so this is
// not the wait's expected duration -- it is the bound on a *spurious* wake.
// A publish's INSERT and its
// signalAwaiters call are two separate steps, so a wake can arrive for an event
// an earlier wake of this same awaiter already claimed; the loop re-checks the
// condition after each wake, and this is how long it waits before doing so
// unprompted.
//
// It does NOT need to be recorded for replay: each iteration's wait goes
// through DurableAwaitSignals, which writes its own TimeoutMs into the
// await_signals record, so a replay uses the recorded value and a change here
// cannot move an existing history's deadline.
const eventRecheckMs = 6000

// WaitForEvent blocks until the workflow's claim on an external event
// succeeds, or until the total timeout expires.
//
// # WHAT IT REPLACES, AND WHY THAT IS A PLATFORM JOB
//
// Waiting for an external event used to cost every app author this loop
// (examples/order-lifecycle/order.go): attempt the claim; if it fails, suspend
// on the signal the registration wakes; then attempt the claim again, because
// A WAKE IS NOT PROOF OF A CLAIMABLE EVENT. The invariant that makes the loop
// correct -- a publish's INSERT and its signalAwaiters call are two separate
// steps, so a wake can be spurious -- is a property of THIS mechanism, and
// spelling it in application code means either exporting the internals into
// every app or shipping a workflow that intermittently misses its own event.
// The claim/register/re-claim loop belongs here.
//
// # THE AWAITED FUNCTION IS CLAIM-SHAPED
//
// The primitive is generic over plugins and so cannot know a webhook's
// semantics. It requires the function's JSON output to carry a boolean
// `found`: true returns the output to the caller; false means register and
// wait. await_webhook's AwaitWebhookOutput has that shape, and so does
// eventtriggers.await_event.
//
// A MISSING `found` KEY IS AN ERROR, NOT A NOT-FOUND. Treating a malformed
// output as "no event yet" would loop until the deadline and report a timeout
// for what is really a programming mistake in the plugin -- the same silent
// failure the webhook plugin's own Keys/correlation_key_field guard exists to
// prevent (plugins/webhookingest/host_functions.go). It is reported as a
// failure rather than skipped because a plugin that never answers the
// question would otherwise burn the caller's whole budget in a loop.
//
// REPLAY: NO NEW MECHANISM, AND THE LOOP NEVER CHOOSES
//
// Every attempt routes through s.PluginCall, which IS the fresh/replay
// dispatcher, and every wait routes through s.DurableAwaitSignals, whose
// replay arm already reads history positionally. So this loop writes the same
// record sequence the application loop wrote by hand --
//
//	plugin_call, await_signals, signal_received, plugin_call, ...
//
// -- and it does not decide which arm of the plugin path to take: it calls the
// thing that decides. That matters beyond tidiness. The re-invocation policy
// is asked ONLY in replayPluginCall, so a loop that reached the fresh arm
// during replay would make no such decision: it would claim live and get
// found=false where the record says true, diverging at the next step. Here
// there is no path to the fresh arm during replay, because the loop has no
// branch that could take it. await_webhook sets neither Idempotent nor
// SameValueOnReplay, so MayReInvokeOnReplay is false and replay serves the
// RECORDED output (cleat#1318) -- the divergence cannot arise, and the
// "same number of plugin calls on replay" property is enforced by the
// registry rather than by this function being careful.
//
// # THE DEADLINE IS DERIVED FROM THE DURABLE CLOCK, NOT THE WALL CLOCK
//
// deadline is s.nowMs + timeoutMs, and s.nowMs is the session's durable clock,
// so the same computation yields the same deadline on every replay. This is
// the rule DurableAwaitSignals states for its own deadline ("deriving it from
// the current clock would push the timeout further out on every replay and the
// await would never expire"). A real clock read here would be neither
// replayed nor recorded, which is the class of non-determinism `cleat vet`
// exists to catch.
//
// A TIMEOUT IS REPORTED THROUGH THE EXISTING ERROR PATH, deliberately. The
// application loop returned an error on exhaustion, so this does too, and the
// success case is the event in outPtr. That avoids adding a packed result:
// engine/abi_pack_property_test.go asserts the packing regions are free and
// that callSuspendSentinel sits outside them, so a new packed shape is a
// change to a PROPERTY TEST, not a local addition. Reusing
// packDurableCallResult keeps that file untouched.
func (s *execSession) WaitForEvent(ctx context.Context, m api.Module,
	pluginName, functionName, inputJSON, signalNames string, timeoutMs int64,
	outPtr, outMaxLen uint32) int64 {

	// A fresh WaitForEvent is new work, exactly as a fresh await is: it
	// registers an awaiter and suspends. The guard is on the fresh path only,
	// mirroring DurableAwaitSignals -- during replay the records decide.
	if !s.isReplay && s.stopBeforeNewWork() {
		return callSuspendSentinel
	}

	// Durable clock, so replay recomputes the same deadline. See the doc above.
	deadline := s.nowMs + timeoutMs

	for {
		// ---- 1. attempt the claim, through the recorded plugin path ----
		//
		// s.PluginCall is the dispatcher (fresh vs replay); calling it rather
		// than either arm is what keeps the re-invocation policy in the one
		// place that asks it. The output lands in the caller's own buffer, so
		// the last successful attempt is already where the caller will read it.
		res := s.PluginCall(ctx, m, pluginName, functionName, inputJSON, outPtr, outMaxLen)
		if res == callSuspendSentinel {
			return res
		}

		written := uint32(uint64(res) >> 40)
		if callErr := byte((res >> 8) & 0xFF); callErr != 0 {
			// The plugin failed and the message is already in outPtr. Not a
			// wait outcome: surfacing it is what the application loop did with
			// the error it got back.
			return res
		}

		// readOutBufString, not readWasmStringValidated(m.Memory(), ...): the
		// wasmtime registration passes m == nil and routes memory through the
		// context, so asking the module for what the plugin call just wrote
		// would panic there. See its doc comment.
		out, ok := readOutBufString(ctx, m, outPtr, written, MaxWasmStringLen)
		if !ok {
			return s.waitForEventFailure(ctx, m, outPtr, outMaxLen,
				fmt.Sprintf("wait_for_event: %s/%s returned an unreadable output (%d bytes)",
					pluginName, functionName, written))
		}

		found, wellFormed := waitForEventClaimFound(out)
		if !wellFormed {
			return s.waitForEventFailure(ctx, m, outPtr, outMaxLen,
				fmt.Sprintf("wait_for_event: %s/%s output has no boolean \"found\" field, so this "+
					"call cannot tell a claimed event from an unclaimed one. The awaited function must be "+
					"claim-shaped -- see WaitForEvent's doc comment. Output was: %s",
					pluginName, functionName, truncateWithHash(out, maxPayloadLen)))
		}
		if found {
			// Claimed. outPtr holds the event, and res carries the length the
			// guest needs.
			return res
		}

		// ---- 2. not claimed: register, then wait to be woken ----
		remaining := deadline - s.nowMs
		if timeoutMs <= 0 || remaining <= 0 {
			return s.waitForEventTimeout(ctx, m, outPtr, outMaxLen, pluginName, functionName, timeoutMs)
		}
		waitMs := int64(eventRecheckMs)
		if remaining < waitMs {
			waitMs = remaining
		}

		// The signal-name and payload buffers are scratch: this loop does not
		// read the wake, it only needs to know the segment may end. The await
		// RECORDS its own timeout, so a replay of this iteration uses the
		// recorded value rather than anything computed here.
		aw := s.DurableAwaitSignals(ctx, m, signalNames, waitMs, 0, 0, 0, 0)
		if aw == callSuspendSentinel {
			// DurableAwaitSignals refused the wait outright -- a defer segment, or
			// a session that has already suspended. Propagate the refusal
			// unchanged: the guest's suspend check decodes it as a stop.
			return aw
		}
		if s.suspendErr != nil {
			// The await suspended the run, so this segment ends here -- and the
			// guest must be TOLD that, not handed a value it reads as success.
			//
			// Returning a decodable zero (responseLen=0, callErrorCode=0,
			// errCode=0) reads as an empty SUCCESSFUL event: the caller runs on
			// with an event that never arrived and records whatever it did with
			// it. That is cleat#933 exactly, in the signaller's own words --
			// "the guest cannot tell the two apart, so it runs on ... the fault
			// is that the history was written" -- and the remedy the signaller
			// already applies to a second await is the same one used here: hand
			// back callSuspendSentinel, which the guest's suspend check reads as
			// a stop. On the wake, replay serves the recorded plugin_call and
			// this loop returns the real event.
			return callSuspendSentinel
		}
		// No suspend and no signal means this iteration's wait genuinely
		// expired, so the condition is re-checked -- which is the whole point
		// of looping on the CONDITION rather than on the wake. The total
		// budget is enforced by `remaining` at the top of the next iteration.
	}
}

// waitForEventClaimFound reports whether a claim-shaped output carries
// found=true, and whether it carries a boolean found field at all.
//
// A POINTER distinguishes the two, and that is the reason for it: an absent
// key and an explicit false both unmarshal to the zero value of a plain bool,
// so a struct with `Found bool` cannot tell a plugin that answered "not yet"
// from one that did not answer the question. See WaitForEvent's doc comment
// for why that distinction is a failure rather than a longer wait.
func waitForEventClaimFound(out string) (found bool, wellFormed bool) {
	var v struct {
		Found *bool `json:"found"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return false, false
	}
	if v.Found == nil {
		return false, false
	}
	return *v.Found, true
}

// waitForEventFailure writes msg into the caller's buffer and reports a failed
// call, the same shape freshPluginCallInternal uses for its own refusals.
func (s *execSession) waitForEventFailure(ctx context.Context, m api.Module, outPtr, outMaxLen uint32, msg string) int64 {
	written, _ := s.writeResult(ctx, m, outPtr, msg, outMaxLen)
	return packDurableCallResult(int(written), callFailureCode, 1)
}

// waitForEventTimeout is the exhaustion path -- what the application loop
// returned when its attempts ran out, and the "or time out" half of the
// contract. It is an error rather than a distinct packed flag; see
// WaitForEvent's doc comment.
func (s *execSession) waitForEventTimeout(ctx context.Context, m api.Module, outPtr, outMaxLen uint32,
	pluginName, functionName string, timeoutMs int64) int64 {
	written, _ := s.writeResult(ctx, m, outPtr,
		fmt.Sprintf("wait_for_event: no event for %s/%s within %dms", pluginName, functionName, timeoutMs),
		outMaxLen)
	return packDurableCallResult(int(written), callFailureCode, 1)
}
