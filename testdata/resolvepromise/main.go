// Package resolvepromise is the fixture for the ResolvePromise row of
// TestEachRewiredMethodWiresItsImport. It calls exactly one host call, so a
// failure names that call rather than whatever else a larger fixture uses.
package resolvepromise

import "github.com/cleat-team/cleat/cleat"

//cleat:entry
func Resolve(h cleat.HostCalls, input string) (string, error) {
	if err := h.ResolvePromise(input, "{}"); err != nil {
		return "", err
	}
	return "resolved", nil
}
