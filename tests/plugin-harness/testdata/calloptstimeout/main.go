// Package calloptstimeout is the fixture for CallOptions.Timeout at the WASM
// boundary.
//
// cleat/runtime_test.go already covers the timeout logic, but it runs NATIVE:
// a real OS thread, a real scheduler, and a caller that returns on another
// goroutine. None of those are facts about a wasip1 guest, and the enforcement
// path is a goroutine racing time.After -- so the native tests and the guest
// exercise the same source over different runtimes. This fixture is the half
// that was missing.
package calloptstimeout

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/cleat-team/cleat/cleat"
)

type request struct {
	Case string `json:"case"`
}

type result struct {
	Case      string `json:"case"`
	Err       string `json:"err"`
	IsTimeout bool   `json:"is_timeout"`
	Resp      string `json:"resp"`
}

// ProbeCallTimeout runs one CallOptions timeout case against harness-service,
// which the test makes slow.
//
// Entry point: probe_call_timeout
func ProbeCallTimeout(h cleat.HostCalls, input string) (string, error) {
	var req request
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		return "", fmt.Errorf("fixture: undecodable input %q: %w", input, err)
	}

	// CONTROL, and the one that decides what this fixture is measuring: the
	// SAME service, the SAME operation, through the plain method. If this
	// fails too, the fixture is broken; if it succeeds where the WithOptions
	// cases do not, the difference is the method.
	if req.Case == "plain-call" {
		resp, err := h.DurableCall("harness-service", "harness-op", `{}`)
		out := result{Case: req.Case, Resp: resp}
		if err != nil {
			out.Err = fmt.Sprintf("%v", err)
		}
		b, err := json.Marshal(out)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}

	var opts cleat.CallOptions
	switch req.Case {
	case "short-timeout":
		// Shorter than the caller's delay: this is the case under test.
		opts = cleat.CallOptions{Timeout: 50 * time.Millisecond}
	case "long-timeout":
		// CONTROL. Longer than the delay, so it must succeed. Without it, a
		// build where every call failed would look like a working timeout.
		opts = cleat.CallOptions{Timeout: 30 * time.Second}
	case "no-timeout":
		// CONTROL. No timeout at all, so it must succeed and must not be
		// reported as a timeout.
	default:
		return "", fmt.Errorf("fixture: unknown case %q", req.Case)
	}

	resp, err := h.DurableCallWithOptions(opts, "harness-service", "harness-op", `{}`)
	out := result{Case: req.Case, Resp: resp}
	if err != nil {
		// fmt.Sprintf, not err.Error(): calling Error() on an error value is
		// interface dispatch and the analyzer refuses it (E008). fmt.Sprintf
		// reaches the same string and is accepted.
		out.Err = fmt.Sprintf("%v", err)
		// The TYPE, not a substring: a message that happens to contain
		// "timeout" is not the same claim as the SDK's own error.
		//
		// A direct type assertion. errors.As would also work -- measured
		// 2026-09-08 across six idioms, and only err.Error() is refused
		// (E008, "calls through interfaces cannot be statically resolved").
		// errors.As, errors.Is, a type assertion and sentinel equality are
		// all accepted, so a workflow CAN recognise this error; the
		// assertion is just the smallest thing that answers the question.
		_, isTimeout := err.(*cleat.CallTimeoutError)
		out.IsTimeout = isTimeout
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
