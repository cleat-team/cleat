package cleat

import (
	"encoding/json"
	"errors"
	"fmt"
)

// updateDelivery is the envelope cleat_poll_update writes. It mirrors
// engine/updater.go's struct of the same name; the two are the same wire
// format read from opposite sides.
type updateDelivery struct {
	Name      string `json:"name"`
	Payload   string `json:"payload"`
	RequestID string `json:"request_id"`
}

// PollUpdate returns the next pending update as a JSON envelope
// {"name","payload","request_id"}, or found=false when there is none.
//
// Low-level: prefer DispatchUpdates. Delivery is durable, so an update returned
// here is recorded as having been delivered whether or not you complete it --
// see engine/updater.go.
func (h *HostCallsImpl) PollUpdate() (string, bool, error) {
	if h.pollUpdate == nil {
		return "", false, errors.New("durable: PollUpdate can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.pollUpdate()
}

// CompleteUpdate records an update handler's outcome and settles the caller's
// promise. A non-empty errMsg rejects; an empty one resolves.
//
// Low-level: prefer DispatchUpdates, which cannot forget to call this.
func (h *HostCallsImpl) CompleteUpdate(requestID, resultJSON, errMsg string) error {
	if h.completeUpdate == nil {
		return errors.New("durable: CompleteUpdate can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.completeUpdate(requestID, resultJSON, errMsg)
}

// DispatchUpdates delivers and runs every update request currently pending for
// this workflow, in the order the store returns them.
//
// # What a "dispatch point" is
//
// It is a call to this function, and nothing more. The SDK places one
// immediately before each suspension -- DurableSleep, AwaitSignals,
// AwaitPromise, AwaitChild -- and exports this so a workflow can add its own.
//
// It has to work that way because an update handler is a CLOSURE IN GUEST
// MEMORY. Only guest code can call it, so an arriving update cannot interrupt
// the workflow; something in the guest has to ask, and this is the asking.
//
// # Why the position matters more than the timing
//
// Replay re-executes the workflow from the top and matches each host call
// against the recorded history in order. So the guarantee the engine needs is
// that the Nth host call the guest makes is the same call on every run. Putting
// this immediately before a suspension gives exactly that: the cleat_poll_update
// lands at the same step index every time, which is what lets the recorded
// update_received event be found there and the handler be re-run at the same
// point in the program.
//
// A consequence worth stating plainly: an update is handled at the next
// dispatch point, not the instant it arrives. A workflow in a tight loop of
// durable calls with no suspension will not service updates until it suspends.
//
// # The handler re-runs on every replay
//
// That is how the state it mutated is rebuilt. The durable facts are its input
// (recorded by the poll) and its output (recorded by the completion), not its
// execution. See engine/updater.go.
func (h *HostCallsImpl) DispatchUpdates() {
	// Reentrancy guard. Every dispatch point is a suspension point, and an
	// update handler is ordinary workflow code that may sleep, await a promise
	// or await a child -- so without this a handler doing any of those would
	// re-enter here and recurse until the stack ran out. Nesting would also be
	// wrong even if it terminated: the inner dispatch would interleave a second
	// update's events inside the first one's, and the received/completed pair
	// would no longer bracket the handler that produced it.
	if h.dispatchingUpdates {
		return
	}
	if h.pollUpdate == nil || h.completeUpdate == nil {
		// No binding: this guest was compiled without the update host calls,
		// which is the state every guest was in before updates were
		// implemented. Doing nothing is right -- there is no queue to drain.
		return
	}
	h.dispatchingUpdates = true
	defer func() { h.dispatchingUpdates = false }()

	for {
		envelope, found, err := h.pollUpdate()
		if err != nil {
			h.DurableLog(fmt.Sprintf("cleat: poll_update failed, updates not dispatched: %v", err))
			return
		}
		if !found {
			return
		}
		var d updateDelivery
		if err := json.Unmarshal([]byte(envelope), &d); err != nil {
			// The envelope is written by the host, so this is not a caller
			// error. Returning rather than continuing avoids spinning on a
			// delivery that will decode the same way next time.
			h.DurableLog(fmt.Sprintf("cleat: poll_update returned an envelope this SDK cannot decode: %v", err))
			return
		}
		h.runUpdate(d)
	}
}

// runUpdate applies one delivered update and reports the outcome.
//
// Every path completes the request. A handler that is not registered, a
// validator that refuses and a handler that errors are all answers the caller
// is entitled to -- leaving any of them uncompleted would leave the caller
// holding a promise nothing settles, which is the defect this whole feature
// was built to stop being.
func (h *HostCallsImpl) runUpdate(d updateDelivery) {
	entry, ok := h.updateHandlers[d.Name]
	if !ok {
		h.completeOrLog(d, "",
			fmt.Sprintf("cleat: no update handler registered for %q", d.Name))
		return
	}

	// The validator runs first and is read-only, so a refusal costs nothing
	// beyond the completion event -- no state change, no durable work.
	if entry.validator != nil {
		if err := entry.validator(d.Payload); err != nil {
			h.completeOrLog(d, "", err.Error())
			return
		}
	}

	result, err := entry.handler(d.Payload)
	if err != nil {
		h.completeOrLog(d, "", err.Error())
		return
	}
	h.completeOrLog(d, result, "")
}

// completeOrLog completes the request and says so when it cannot.
//
// These four calls discarded their error with `_ =` until IMPROVEMENT-PLAN
// 3.245, which is why the defect that section describes was invisible: every
// failing update passed an empty result, the store rejected "" as JSON, the
// request stayed `pending`, the caller's promise was never settled -- and the
// guest carried on as though it had answered.
//
// A failure here cannot be recovered from inside the guest: the row is the
// host's, and the next poll will redeliver the same request. Logging is what
// the guest can do, and it turns a silent hang into something a worker log
// shows. The caller still waits, and that is the host's problem to fix.
func (h *HostCallsImpl) completeOrLog(d updateDelivery, result, errMsg string) {
	if err := h.completeUpdate(d.RequestID, result, errMsg); err != nil {
		h.DurableLog(fmt.Sprintf(
			"cleat: completing update %q (request %s) failed: %v -- the request stays pending "+
				"and the caller's promise is unsettled", d.Name, d.RequestID, err))
	}
}
