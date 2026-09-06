// Package scheduleinvoke is the fixture for the ScheduleInvoke row of
// TestEachRewiredMethodWiresItsImport. It calls exactly one host call, so a
// failure names that call rather than whatever else a larger fixture uses.
package scheduleinvoke

import "github.com/cleat-team/cleat/cleat"

//cleat:entry
func Schedule(h cleat.HostCalls, input string) (string, error) {
	if err := h.ScheduleInvoke("svc", "op", input, 1000); err != nil {
		return "", err
	}
	return "scheduled", nil
}
