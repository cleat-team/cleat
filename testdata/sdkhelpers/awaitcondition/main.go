// Package awaitcondition is a fixture for cleat#3803 / cleat#3805: cleat.AwaitCondition, a non-generic package-level function.
//
// It calls ONE SDK API and writes no HostCalls method of its own, so every import the build links for it comes
// from the SDK function's own body. A fixture that also wrote h.X itself could not tell "the SDK function was
// seen" from "the workflow happened to name the same call".
package awaitcondition

import (
	"time"

	"github.com/cleat-team/cleat/cleat"
)

//cleat:entry
func Run(h cleat.HostCalls, input string) (string, error) {
	if cleat.AwaitCondition(h, func() bool { return true }, time.Second, time.Minute) {
		return "met", nil
	}
	return "timed out", nil
}
