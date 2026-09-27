// Package durablesend is the fixture for the DurableSend row of
// TestEachRewiredMethodWiresItsImport. It calls exactly one host call, so a
// failure names that call rather than whatever else a larger fixture uses.
package durablesend

import "github.com/cleat-team/cleat/cleat"

//cleat:entry
func Send(h cleat.HostCalls, input string) (string, error) {
	if err := h.DurableSend("svc", "op", input); err != nil {
		return "", err
	}
	return "sent", nil
}
