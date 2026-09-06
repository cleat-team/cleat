// Package signalworkflow is the fixture for
// TestSignalWorkflowWiresItsImport. It calls the one method whose absence from
// the wiring tables IMPROVEMENT-PLAN 3.224 is about, and nothing else, so the
// assertion is about that method rather than about whatever else a larger
// fixture happens to use.
package signalworkflow

import "github.com/cleat-team/cleat/cleat"

//cleat:entry
func Send(h cleat.HostCalls, input string) (string, error) {
	if err := h.SignalWorkflow("00000000-0000-0000-0000-000000000001", "ping", input); err != nil {
		return "", err
	}
	return "sent", nil
}
