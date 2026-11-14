// Package sagatyped is a fixture for cleat#3803 / cleat#3805: cleat.SagaTyped.Run, a method on a generic type.
//
// It calls ONE SDK API and writes no HostCalls method of its own, so every import the build links for it comes
// from the SDK function's own body. A fixture that also wrote h.X itself could not tell "the SDK function was
// seen" from "the workflow happened to name the same call".
package sagatyped

import (
	"github.com/cleat-team/cleat/cleat"
)

//cleat:entry
func Run(h cleat.HostCalls, input string) (string, error) {
	out, err := cleat.NewSagaTyped[string]().
		AddStep("only", func(cleat.HostCalls) (string, error) { return "done", nil }, nil).
		Run(h)
	if err != nil || len(out) == 0 {
		return "", err
	}
	return out[0], nil
}
