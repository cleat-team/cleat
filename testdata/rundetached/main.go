// Package rundetached is the fixture for the RunDetached row of
// TestEachRewiredMethodWiresItsImport. It calls exactly one host call, so a
// failure names that call rather than whatever else a larger fixture uses.
package rundetached

import "github.com/cleat-team/cleat/cleat"

//cleat:entry
func Detach(h cleat.HostCalls, input string) (string, error) {
	if err := h.RunDetached("child-workflow", input); err != nil {
		return "", err
	}
	return "detached", nil
}
