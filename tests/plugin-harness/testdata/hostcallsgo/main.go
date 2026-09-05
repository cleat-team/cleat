// Package hostcallsgo is the Go reference fixture for the host-call execution
// harness: it runs exactly ONE host call per invocation and reports what that
// call did.
//
// One call per invocation, rather than all 23 in one workflow, for two reasons
// that are both about not losing information:
//
//   - A host call may SUSPEND the workflow. Everything after it in the same
//     invocation then never runs, and the calls that did not run are
//     indistinguishable from calls that ran and returned nothing. The plugin
//     harness gets away with one workflow for all 17 of its calls because a
//     plugin call cannot suspend; a host call can.
//   - The harness must be able to redden on ONE call in ONE language. A single
//     fixture that fails as a unit localises nothing, and localisation is the
//     acceptance test for this design (2026-09-05 plan, A3).
//
// The cost is 23 workflow executions per language instead of one. The
// expensive part -- building the fixture -- still happens once.
package hostcallsgo

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/cleat-team/cleat/cleat"
)

// request is what the harness sends: the name of the single call to exercise.
type request struct {
	Call string `json:"call"`
}

// outcome is what the harness reads back. It records what the call DID, not
// whether the fixture liked it: a host call that returns an error in an
// environment with no backend is a correct outcome to record, and the expected
// -outcome table is what decides whether it is the right one.
type outcome struct {
	Call string `json:"call"`
	// Status is one of "ok", "error". A call that suspends the workflow
	// produces no outcome at all -- the harness detects that from the engine's
	// suspend result, which is why it must not use wasmtest.Execute.
	Status string `json:"status"`
	// Detail is the error text for "error", or a short rendering of the
	// returned value for "ok". The TEXT is the point: §3.200 was a guest that
	// decoded the host's error length and threw the message away, and a
	// harness that recorded only ok/error could not have seen it.
	Detail string `json:"detail"`
}

func ok(call, detail string) (string, error) { return emit(outcome{call, "ok", detail}) }
func bad(call string, err error) (string, error) {
	return emit(outcome{call, "error", fmt.Sprintf("%v", err)})
}

func emit(o outcome) (string, error) {
	b, err := json.Marshal(o)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ExerciseHostCall runs the one call named in the input.
//
// Entry point: exercise_host_call
func ExerciseHostCall(h cleat.HostCalls, input string) (string, error) {
	var req request
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		return "", fmt.Errorf("fixture: undecodable input %q: %w", input, err)
	}

	switch req.Call {

	// ---- children ----
	case "ChildWorkflow":
		r, err := h.ChildWorkflow("child-workflow", `{}`)
		if err != nil {
			return bad(req.Call, err)
		}
		return ok(req.Call, r)

	case "ChildWorkflowWithOptions":
		r, err := h.ChildWorkflowWithOptions("child-workflow", `{}`, cleat.ChildWorkflowOptions{Version: 1})
		if err != nil {
			return bad(req.Call, err)
		}
		return ok(req.Call, r)

	case "AwaitChild":
		r, err := h.AwaitChild("00000000-0000-0000-0000-000000000001")
		if err != nil {
			return bad(req.Call, err)
		}
		return ok(req.Call, r)

	case "AwaitAllChildren":
		rs, err := h.AwaitAllChildren([]string{"00000000-0000-0000-0000-000000000001"})
		if err != nil {
			return bad(req.Call, err)
		}
		return ok(req.Call, fmt.Sprintf("%d child result(s)", len(rs)))

	case "AwaitAnyChild":
		runID, r, err := h.AwaitAnyChild([]string{"00000000-0000-0000-0000-000000000001"})
		if err != nil {
			return bad(req.Call, err)
		}
		return ok(req.Call, runID+" "+r)

	case "PollChild":
		status, r, err := h.PollChild("00000000-0000-0000-0000-000000000001")
		if err != nil {
			return bad(req.Call, err)
		}
		return ok(req.Call, status+" "+r)

	// ---- promises ----
	case "CreatePromise":
		id, err := h.CreatePromise("harness-promise")
		if err != nil {
			return bad(req.Call, err)
		}
		return ok(req.Call, id)

	case "AwaitPromise":
		r, timedOut, err := h.AwaitPromise("00000000-0000-0000-0000-000000000002", 10*time.Millisecond)
		if err != nil {
			return bad(req.Call, err)
		}
		return ok(req.Call, fmt.Sprintf("timedOut=%v %s", timedOut, r))

	// ---- signals ----
	case "DurableAwaitSignals":
		name, payload, timedOut, err := h.DurableAwaitSignals([]string{"harness-signal"}, 10)
		if err != nil {
			return bad(req.Call, err)
		}
		return ok(req.Call, fmt.Sprintf("timedOut=%v %s %s", timedOut, name, payload))

	case "PollSignal":
		payload, present, err := h.PollSignal("harness-signal")
		if err != nil {
			return bad(req.Call, err)
		}
		return ok(req.Call, fmt.Sprintf("present=%v %s", present, payload))

	// ---- durable calls ----
	case "DurableCall":
		r, err := h.DurableCall("harness-service", "harness-op", `{}`)
		if err != nil {
			return bad(req.Call, err)
		}
		return ok(req.Call, r)

	case "DurableCallWithHeartbeat":
		r, err := h.DurableCallWithHeartbeat("harness-service", "harness-op", `{}`,
			time.Second, func(string) {})
		if err != nil {
			return bad(req.Call, err)
		}
		return ok(req.Call, r)

	case "DurableCallWithRetry":
		r, err := h.DurableCallWithRetry("harness-service", "harness-op", `{}`, cleat.RetryPolicy{
			MaxAttempts:        1,
			InitialInterval:    time.Millisecond,
			BackoffCoefficient: 1,
			MaxInterval:        time.Millisecond,
		})
		if err != nil {
			return bad(req.Call, err)
		}
		return ok(req.Call, r)

	// ---- defers ----
	case "DurableDefer":
		id, err := h.DurableDefer("harness defer")
		if err != nil {
			return bad(req.Call, err)
		}
		return ok(req.Call, id)

	case "DurableDeferFunc":
		id, err := h.DurableDeferFunc(func() {})
		if err != nil {
			return bad(req.Call, err)
		}
		return ok(req.Call, id)

	// ---- crons ----
	case "ScheduleCron":
		id, err := h.ScheduleCron("harness-workflow", "0 0 * * *", "UTC", `{}`)
		if err != nil {
			return bad(req.Call, err)
		}
		return ok(req.Call, id)

	case "ListCrons":
		r, err := h.ListCrons()
		if err != nil {
			return bad(req.Call, err)
		}
		return ok(req.Call, r)

	// ---- plugins ----
	case "PluginCall":
		// Both paths, in one row, deliberately.
		//
		// llm.list_models is the one plugin call that works with no tenant, so
		// on its own it exercises only the success path -- and §3.200, the
		// defect this harness is accepted against, lived entirely on the
		// ERROR path: the guest decoded the host's error length and threw the
		// message away. A row that only ever succeeded could not have caught
		// it, which is what the first version of this fixture did.
		//
		// blobstore.put fails with the host's "no tenant context", so the
		// detail below carries the host's own text and a guest that discards
		// it changes this row and nothing else.
		r, err := h.PluginCall("llm", "list_models", `{}`)
		if err != nil {
			return bad(req.Call, err)
		}
		_, errPath := h.PluginCall("blobstore", "put", `{"key":"k","data":"aGk="}`)
		if errPath == nil {
			return ok(req.Call, "ok="+r+" err=<none: blobstore.put unexpectedly succeeded>")
		}
		return ok(req.Call, fmt.Sprintf("ok=%s err=%v", r, errPath))

	case "PluginCallStreaming":
		ch, err := h.PluginCallStreaming("llm", "chat_stream", `{"messages":[{"role":"user","content":"hi"}]}`)
		if err != nil {
			return bad(req.Call, err)
		}
		n := 0
		for range ch {
			n++
		}
		return ok(req.Call, fmt.Sprintf("%d event(s)", n))

	// ---- locks ----
	case "AcquireLock":
		// In wave 1 rather than wave 2, on WS-3's C1 finding. The split rule
		// was "does the error path read a buffer", and AcquireLock's does not
		// -- but its SUCCESS path decodes a bit the host computed:
		//
		//     acquired := uint32((uint64(result) >> 8) & 0x1) != 0
		//
		// A guest that returned a constant true would compile, pass every
		// compile-coverage check, and be silently wrong about whether it holds
		// a lock. That is the §3.200 defect class, so the rule is really "does
		// the guest have to decode something the host computed".
		// Twice, for the same reason PluginCall exercises both paths: a row
		// that only ever sees `true` cannot tell a decoded bit from a
		// hardcoded one.
		first, err := h.AcquireLock("harness-lock", 60000)
		if err != nil {
			return bad(req.Call, err)
		}
		second, err := h.AcquireLock("harness-lock", 60000)
		if err != nil {
			return bad(req.Call, err)
		}
		return ok(req.Call, fmt.Sprintf("first=%v second=%v", first, second))

	// ---- identity and control ----
	case "WorkflowID":
		return ok(req.Call, h.WorkflowID())

	case "RunID":
		return ok(req.Call, h.RunID())

	case "PollCancellation":
		cancelled, reason := h.PollCancellation()
		return ok(req.Call, fmt.Sprintf("cancelled=%v %s", cancelled, reason))

	case "SideEffect":
		r, err := h.SideEffect(func() (string, error) { return "side-effect-value", nil })
		if err != nil {
			return bad(req.Call, err)
		}
		return ok(req.Call, r)
	}

	// An unknown name is a hard error, not an "error" outcome. The harness
	// drives this fixture from the same list it asserts against, so a name it
	// does not recognise means the two have drifted -- and an outcome row
	// saying "unknown call" would be recorded as a legitimate expected
	// failure, which is the exact shape of a guard that stops guarding.
	return "", fmt.Errorf("fixture: no case for host call %q", req.Call)
}
