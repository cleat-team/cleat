// Package getscope is the fixture for the GetScope row of
// TestEachRewiredMethodWiresItsImport. It calls exactly one host call, so a
// failure names that call rather than whatever else a larger fixture uses.
package getscope

import "github.com/cleat-team/cleat/cleat"

//cleat:entry
func Scope(h cleat.HostCalls, input string) (string, error) {
	objectType, instanceKey := h.GetScope()
	return objectType + "/" + instanceKey, nil
}
