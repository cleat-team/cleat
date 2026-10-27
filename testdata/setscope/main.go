// Package setscope is the fixture for the SetScope row of
// TestEachRewiredMethodWiresItsImport. It calls exactly one host call, so a
// failure names that call rather than whatever else a larger fixture uses.
package setscope

import "github.com/cleat-team/cleat/cleat"

//cleat:entry
func Scope(h cleat.HostCalls, input string) (string, error) {
	return h.SetScope("order", input), nil
}
