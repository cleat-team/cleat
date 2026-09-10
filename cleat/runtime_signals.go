package cleat

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type SignalResult struct {
	Name    string
	Payload string
	// ReplyTo is the address to answer this signal at, and is non-empty
	// only when the sender used SendSignalAndWait and is suspended waiting
	// for a reply. Pass it to ReplyToSignal. A signal sent with
	// SignalWorkflow leaves it empty, which is how a receiver tells a
	// request that wants an answer from a one-way notification.
	ReplyTo  string
	TimedOut bool
	Err      error
}

// SendSignalAndWait sends a signal to another workflow and suspends until
// that workflow replies or the timeout elapses.
//
// It is composed from three durable primitives rather than being a host call
// of its own: a promise is the reply channel, its ID is the correlation ID,
// and answering is resolving it. That composition is the whole of
// IMPROVEMENT-PLAN 3.220, and it is possible only because #813/#821 made a
// promise ID a globally unique token any holder can settle, whose settlement
// wakes the creator and reports ErrPromiseNotFound rather than silently
// succeeding when it matches nothing.
//
// The three implementations this replaces disagreed with each other: the
// host call was inert engine-side, cleattest routed replies over an
// in-memory Go channel that cannot cross a process, and embedded returned a
// canned {"status":"delivered"} without waiting for anything -- so a test
// asserting a reply passed there for no reason. Building from primitives
// that already work identically in all three environments is what removes
// that divergence, not a fourth implementation.
//
// Each step is separately durable, so a crash between them replays correctly:
// the promise is created once, the signal is sent once, and the await
// resumes.
func (h *HostCallsImpl) SendSignalAndWait(targetRunID, signalName, payload string, timeout time.Duration) (string, error) {
	replyTo, err := h.CreatePromise("__reply:" + signalName)
	if err != nil {
		return "", fmt.Errorf("durable: SendSignalAndWait: create reply promise: %w", err)
	}
	envelope, err := encodeSignalEnvelope(replyTo, payload)
	if err != nil {
		return "", fmt.Errorf("durable: SendSignalAndWait: encode envelope: %w", err)
	}
	if err := h.SignalWorkflow(targetRunID, signalName, envelope); err != nil {
		return "", fmt.Errorf("durable: SendSignalAndWait: send signal %q to %q: %w", signalName, targetRunID, err)
	}
	response, timedOut, err := h.AwaitPromise(replyTo, timeout)
	if err != nil {
		return "", fmt.Errorf("durable: SendSignalAndWait: await reply to signal %q: %w", signalName, err)
	}
	// Returning an error on timedOut is correct even though AwaitPromise
	// reports timedOut = true for a SUSPENSION as well as for a real timeout.
	// The host distinguishes them and this code does not have to: when the
	// await suspends, engine/promises.go sets session.suspendErr before
	// returning, and engine/executor.go:264 treats a workflow error as a
	// failure only when suspendErr is nil -- ":315 deliberately lets a
	// suspension win over the error that accompanied it". So this error
	// surfaces on a genuine timeout and is discarded on a suspension.
	//
	// Do not "fix" this into a suspension check. There is nothing in the
	// returned triple to check: the engine signals suspension host-side, not
	// through a sentinel, so the guest cannot tell the two apart and must not
	// try.
	if timedOut {
		return "", fmt.Errorf("durable: SendSignalAndWait: no reply to signal %q from workflow %q within %v", signalName, targetRunID, timeout)
	}
	return response, nil
}

// ReplyToSignal answers a signal sent with SendSignalAndWait, waking the
// sender with response.
//
// correlationID is SignalResult.ReplyTo, which is the reply promise's ID, so
// replying is resolving that promise. An ID that matches no promise reports
// ErrPromiseNotFound rather than reporting success, which is what makes a
// stale or wrong address a visible failure instead of a sender that hangs
// until its timeout.
func (h *HostCallsImpl) ReplyToSignal(correlationID, response string) error {
	if correlationID == "" {
		return errors.New("durable: ReplyToSignal: empty correlation ID. Pass SignalResult.ReplyTo from the signal being answered; it is empty when the sender used SignalWorkflow and is not waiting for a reply.")
	}
	if err := h.ResolvePromise(correlationID, response); err != nil {
		return fmt.Errorf("durable: ReplyToSignal(%q): %w", correlationID, err)
	}
	return nil
}

func (h *HostCallsImpl) AwaitSignalsWithQuorum(signalNames []string, minCount int, maxRejections int, timeout time.Duration) ([]SignalResult, error) {
	if h.awaitSignalsWithQuorum != nil {
		results, err := h.awaitSignalsWithQuorum(signalNames, minCount, maxRejections, timeout)
		for i := range results {
			results[i] = unwrapSignalResult(results[i])
		}
		return results, err
	}
	// A quorum of N over a set of M names is unsatisfiable when N > M, and it
	// used to spin to the timeout and report "got k/N signals" -- a message
	// that describes a slow sender rather than a caller asking for something
	// arithmetic forbids. Once names narrow it is guaranteed to fail, so it is
	// refused here as the programming error it is.
	if minCount > len(signalNames) {
		return nil, fmt.Errorf(
			"durable: AwaitSignalsWithQuorum: quorum of %d over %d name(s) %v is unsatisfiable; "+
				"a quorum counts DISTINCT names, so it cannot exceed the size of the set",
			minCount, len(signalNames), signalNames)
	}

	// Fallback: poll-based loop using DurableAwaitSignals.
	deadline := time.Now().Add(timeout)
	var results []SignalResult
	rejectionCount := 0

	// COPIED, not aliased. remaining is narrowed below, and signalNames belongs
	// to the caller -- narrowing it in place would edit a slice the workflow may
	// still be holding.
	remaining := append([]string(nil), signalNames...)

	for len(results) < minCount {
		remainingTime := time.Until(deadline)
		if remainingTime <= 0 {
			return results, fmt.Errorf("durable: quorum timeout after %v: got %d/%d signals", timeout, len(results), minCount)
		}
		result := h.AwaitSignals(remaining, remainingTime)
		if result.TimedOut {
			return results, fmt.Errorf("durable: quorum timeout after %v: got %d/%d signals", timeout, len(results), minCount)
		}
		if result.Err != nil {
			return results, fmt.Errorf("durable: quorum signal error: %w", result.Err)
		}
		// A name outside the requested set has not been asked for, and counting
		// it would reinstate the defect through the other door: narrowing what
		// we ASK for is only half the fix if we accept whatever arrives.
		//
		// A well-behaved host returns one of the names it was given, so this is
		// unreachable through the engine's own DurableAwaitSignals. It is here
		// because the fix must not rest on that politeness -- the invariant the
		// quorum depends on is "each result is a distinct member of the set",
		// and this is the only place that can enforce it.
		if !containsName(remaining, result.Name) {
			return results, fmt.Errorf(
				"durable: AwaitSignalsWithQuorum: awaited %v and received %q, which is "+
					"not among them; a quorum counts distinct names and cannot count this one",
				remaining, result.Name)
		}

		results = append(results, result)

		// Narrow the set: this name has voted, and a quorum counts VOTERS.
		//
		// Without this, `remaining` was assigned once and passed unchanged to
		// every await, so three deliveries of one name satisfied a quorum of
		// three with the other two never sent (cleat#1132). The variable was
		// already named for a narrowing that had never landed.
		//
		// A rejection narrows too. A voter that votes no has voted, and leaving
		// it in the set would let one rejector trip maxRejections alone -- which
		// is the same defect wearing the other outcome.
		remaining = withoutName(remaining, result.Name)

		// Check for rejection if maxRejections >= 0.
		if maxRejections >= 0 {
			var payloadMap map[string]interface{}
			if err := json.Unmarshal([]byte(result.Payload), &payloadMap); err == nil {
				if rejected, ok := payloadMap["rejected"].(bool); ok && rejected {
					rejectionCount++
					if rejectionCount > maxRejections {
						return results, fmt.Errorf("durable: quorum exceeded max rejections (%d)", maxRejections)
					}
				}
			}
		}
	}
	return results, nil
}

func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// withoutName returns names with the first occurrence of name removed.
//
// Returns a new slice rather than compacting in place: the caller's signalNames
// is copied once at the top of the quorum loop, and keeping this non-mutating
// means a future caller that passes a shared slice cannot be surprised.
func withoutName(names []string, name string) []string {
	out := make([]string, 0, len(names))
	dropped := false
	for _, n := range names {
		if !dropped && n == name {
			dropped = true
			continue
		}
		out = append(out, n)
	}
	return out
}

func (h *HostCallsImpl) SignalWorkflow(targetRunID, signalName, payload string) error {
	if h.signalWorkflow == nil {
		return errors.New("durable: SignalWorkflow can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.signalWorkflow(targetRunID, signalName, payload)
}

func (h *HostCallsImpl) AwaitSignals(signalNames []string, timeout time.Duration) SignalResult {
	h.DispatchUpdates() // dispatch point; see DispatchUpdates
	if timeout <= 0 {
		return SignalResult{
			TimedOut: true,
			Err:      errors.New("AwaitSignals requires a positive timeout. Use PollSignals() for non-blocking signal checks."),
		}
	}
	name, payload, timedOut, err := h.DurableAwaitSignals(signalNames, timeout.Milliseconds())
	return unwrapSignalResult(SignalResult{
		Name:     name,
		Payload:  payload,
		TimedOut: timedOut,
		Err:      err,
	})
}

func (h *HostCallsImpl) PollSignals(names []string) SignalResult {
	for _, name := range names {
		payload, found, err := h.PollSignal(name)
		if err != nil {
			return SignalResult{Err: err}
		}
		if found {
			return unwrapSignalResult(SignalResult{Name: name, Payload: payload})
		}
	}
	return SignalResult{TimedOut: true}
}

func (h *HostCallsImpl) DurableAwaitSignals(signalNames []string, timeoutMs int64) (string, string, bool, error) {
	if h.durableAwaitSignals == nil {
		return "", "", false, errors.New("durable: DurableAwaitSignals can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.durableAwaitSignals(signalNames, timeoutMs)
}

func (h *HostCallsImpl) PollSignal(signalName string) (string, bool, error) {
	if h.pollSignal == nil {
		return "", false, errors.New("durable: PollSignal can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.pollSignal(signalName)
}
