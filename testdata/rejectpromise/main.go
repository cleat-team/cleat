// Package rejectpromise is the fixture for the RejectPromise row of
// TestEachRewiredMethodWiresItsImport. It calls exactly one host call, so a
// failure names that call rather than whatever else a larger fixture uses.
package rejectpromise

import "github.com/cleat-team/cleat/cleat"

//cleat:entry
func Reject(h cleat.HostCalls, input string) (string, error) {
	if err := h.RejectPromise(input, "nope"); err != nil {
		return "", err
	}
	return "rejected", nil
}
