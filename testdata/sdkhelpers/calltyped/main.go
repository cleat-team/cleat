// Package calltyped is a fixture for cleat#3803 / cleat#3805: cleat.CallTyped, a generic package-level function.
//
// It calls ONE SDK API and writes no HostCalls method of its own, so every import the build links for it comes
// from the SDK function's own body. A fixture that also wrote h.X itself could not tell "the SDK function was
// seen" from "the workflow happened to name the same call".
package calltyped

import (
	"github.com/cleat-team/cleat/cleat"
)

//cleat:entry
func Run(h cleat.HostCalls, input string) (string, error) {
	return cleat.CallTyped[string](h, "svc", "op", map[string]string{"k": input})
}
