// Package calloptsonly is the fixture for a workflow whose ONLY durable call
// is DurableCallWithOptions.
//
// It exists as a separate package from calloptstimeout because the defect it
// pins is a property of the whole compiled module, not of one invocation: the
// closure analysis decides what to bind from the calls it can see in the
// workflow, and a fixture that also calls DurableCall anywhere has already
// changed the answer. One package per binding state is the only way to hold
// both.
package calloptsonly

import (
	"encoding/json"
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

type result struct {
	Err  string `json:"err"`
	Resp string `json:"resp"`
}

// CallWithOptionsOnly makes exactly one durable call, through
// DurableCallWithOptions, and reports what came back.
//
// Entry point: call_with_options_only
func CallWithOptionsOnly(h cleat.HostCalls, input string) (string, error) {
	resp, err := h.DurableCallWithOptions(
		cleat.CallOptions{},
		"harness-service", "harness-op", `{}`)
	out := result{Resp: resp}
	if err != nil {
		out.Err = fmt.Sprintf("%v", err)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
