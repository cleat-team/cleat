package cleat

import (
	"strings"
	"testing"
	"time"
)

// cleat#4390, the in-process half of what cleat#4321 did to the engine. When the awaited function's output is not claim-shaped, WaitForEvent's fallback (the
// loop cleattest, localdev and embedded run; a worker uses the engine's export) printed that output into the error: whatever the plugin produced, in a sentence
// that goes to the guest and from there to a stored run error and the logs. It now names the output by its size and a hash.
//
// The expected text is written out literally; its size and hash were computed outside Go (hashlib), not by the helper under test.
func TestWaitForEventFallbackNamesAnUnclaimableOutputAndDoesNotPrintIt(t *testing.T) {
	const output = `{"payload":"MARKER-WFE-4390"}`
	h := NewHostCalls(HostCallsOptions{
		PluginCall: func(string, string, string) (string, error) { return output, nil },
	})
	_, err := h.WaitForEvent("webhook-ingest", "await_webhook", "{}", []string{"__evt:payment"}, time.Second)
	if err == nil {
		t.Fatal("PRECONDITION FAILED: an output with no boolean found was accepted, so this measures nothing")
	}
	msg := err.Error()
	if !strings.Contains(msg, `returned an output with no boolean "found" field`) {
		t.Fatalf("PRECONDITION FAILED: not the malformed-output error, so this row measures nothing: %q", msg)
	}
	if strings.Contains(msg, "MARKER-WFE-4390") || strings.Contains(msg, "payload") {
		t.Errorf("the error prints the plugin's output: %q", msg)
	}
	if want := "Output was: [redacted: 29 bytes, sha256=ed976a923b79e6e5]"; !strings.HasSuffix(msg, want) {
		t.Errorf("the error does not end by naming the output.\n  got:  %q\n  want it to end with: %q", msg, want)
	}
}
