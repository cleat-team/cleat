// Package sideeffectgo is the Go guest for cleat#4113's end-to-end test: one side effect whose value is NOT reproducible, and a count of how
// many times its function ran in THIS execution.
//
// Every execution is a fresh WASM instance, so the package-level counter starts at zero each time. The first run therefore reports ran=1, and a
// replay that returns the recorded value without calling the function reports ran=0. A replay that called the function again would report
// ran=1, and one that failed the replay would report nothing.
package sideeffectgo

import (
	"encoding/json"
	"strconv"

	"github.com/cleat-team/cleat/cleat"
)

var ran int

type result struct {
	Value string `json:"value"`
	Ran   int    `json:"ran"`
}

// SideEffectOnce takes one side effect and reports its value and how often its function ran.
//
// Entry point: side_effect_once
func SideEffectOnce(h cleat.HostCalls, input string) (string, error) {
	v, err := h.SideEffect(func() (string, error) {
		ran++
		// The value is not the evidence: the count below is. (A value from the outside world, time.Now or crypto/rand, is refused by the
		// build even inside a side effect, E003 and E017; see the PR.)
		return "computed-in-run-" + strconv.Itoa(ran), nil
	})
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(result{Value: v, Ran: ran})
	if err != nil {
		return "", err
	}
	return string(b), nil
}
