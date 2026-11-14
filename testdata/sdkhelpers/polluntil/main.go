// Package polluntil is a fixture for cleat#3803 / cleat#3805: cleat.PollUntil, a generic package-level function.
//
// It calls ONE SDK API and writes no HostCalls method of its own, so every import the build links for it comes
// from the SDK function's own body. A fixture that also wrote h.X itself could not tell "the SDK function was
// seen" from "the workflow happened to name the same call".
package polluntil

import (
	"time"

	"github.com/cleat-team/cleat/cleat"
)

//cleat:entry
func Run(h cleat.HostCalls, input string) (string, error) {
	return cleat.PollUntil[string](h, time.Second, time.Minute,
		func() (string, error) { return input, nil },
		func(s string) bool { return s != "" })
}
