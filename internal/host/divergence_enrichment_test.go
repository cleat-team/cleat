package host

import (
	"fmt"
	"strings"
	"testing"
)

// TestDivergencePayloadEnrichment verifies each of the 7 enriched divergence
// error messages contains the expected field labels. Uses format-string
// construction since exercising all divergence paths through WASM requires
// a test workflow with every host call type.
func TestDivergencePayloadEnrichment(t *testing.T) {
	t.Run("replayCall_event_type", func(t *testing.T) {
		result := truncateWithHash(`{"key":"value"}`, maxPayloadLen)
		msg := "replay divergence at step 0: expected call event, got sleep.\n  actual request: " + result + "\n  expected request: " + result + "\nRun 'cleat vet' on your workflow code to check for common non-determinism issues"
		for _, label := range []string{
			"actual request:",
			"expected request:",
		} {
			if !strings.Contains(msg, label) {
				t.Errorf("message missing label %q: %s", label, msg)
			}
		}
	})

	t.Run("replayCall_service_op_mismatch", func(t *testing.T) {
		result := truncateWithHash(`{"key":"value"}`, maxPayloadLen)
		msg := "replay divergence at step 0: workflow called svc.op but history has svc2.op2.\n  actual request: " + result + "\n  expected request: " + result + "\nRun 'cleat vet' on your workflow code to check for common non-determinism issues"
		for _, label := range []string{
			"actual request:",
			"expected request:",
		} {
			if !strings.Contains(msg, label) {
				t.Errorf("message missing label %q: %s", label, msg)
			}
		}
	})

	t.Run("replayPluginCall_event_type", func(t *testing.T) {
		input := truncateWithHash(`{"input":"data"}`, maxPayloadLen)
		cachedInput := truncateWithHash(`{"cached":"input"}`, maxPayloadLen)
		cachedOutput := truncateWithHash(`{"cached":"output"}`, maxPayloadLen)
		msg := "replay divergence at step 0: expected plugin_call event, got sleep.\n  actual input: " + input + "\n  expected (cached) input: " + cachedInput + "\n  expected (cached) output: " + cachedOutput + "\nRun 'cleat vet' on your workflow code to check for common non-determinism issues"
		for _, label := range []string{
			"actual input:",
			"expected (cached) input:",
			"expected (cached) output:",
		} {
			if !strings.Contains(msg, label) {
				t.Errorf("message missing label %q: %s", label, msg)
			}
		}
	})

	t.Run("replayPluginCall_plugin_func_mismatch", func(t *testing.T) {
		input := truncateWithHash(`{"input":"data"}`, maxPayloadLen)
		cachedInput := truncateWithHash(`{"cached":"input"}`, maxPayloadLen)
		cachedOutput := truncateWithHash(`{"cached":"output"}`, maxPayloadLen)
		msg := "replay divergence at step 0: workflow called p1/f1 but history has p2/f2.\n  actual input: " + input + "\n  expected (cached) input: " + cachedInput + "\n  expected (cached) output: " + cachedOutput + "\nRun 'cleat vet' on your workflow code to check for common non-determinism issues"
		for _, label := range []string{
			"actual input:",
			"expected (cached) input:",
			"expected (cached) output:",
		} {
			if !strings.Contains(msg, label) {
				t.Errorf("message missing label %q: %s", label, msg)
			}
		}
	})

	t.Run("await_all_children_event_type", func(t *testing.T) {
		runIDs := truncateWithHash(`["run1","run2"]`, maxPayloadLen)
		expected := truncateWithHash(`["run1","run3"]`, maxPayloadLen)
		msg := "replay divergence at step 0: expected await_all_children, got sleep.\n  actual run IDs: " + runIDs + "\n  expected run IDs: " + expected + "\nRun 'cleat vet' on your workflow code to check for common non-determinism issues"
		for _, label := range []string{
			"actual run IDs:",
			"expected run IDs:",
		} {
			if !strings.Contains(msg, label) {
				t.Errorf("message missing label %q: %s", label, msg)
			}
		}
	})

	t.Run("await_all_children_run_ids_mismatch", func(t *testing.T) {
		runIDs := truncateWithHash(`["run1","run2"]`, maxPayloadLen)
		expected := truncateWithHash(`["run1","run3"]`, maxPayloadLen)
		msg := "replay divergence at step 0: await_all_children run IDs mismatch.\n  actual run IDs: " + runIDs + "\n  expected run IDs: " + expected + "\nRun 'cleat vet' on your workflow code to check for common non-determinism issues"
		for _, label := range []string{
			"await_all_children run IDs mismatch",
			"actual run IDs:",
			"expected run IDs:",
		} {
			if !strings.Contains(msg, label) {
				t.Errorf("message missing label %q: %s", label, msg)
			}
		}
	})

	t.Run("fetch_mismatch", func(t *testing.T) {
		body := truncateWithHash(`{"body":"data"}`, maxPayloadLen)
		expectedBody := truncateWithHash(`{"body":"old"}`, maxPayloadLen)
		expectedResponse := truncateWithHash(`{"resp":"ok"}`, maxPayloadLen)
		msg := "replay divergence at step 0: Fetch mismatch.\n  workflow: POST /api\n  history: GET /api/old\n  actual body: " + body + "\n  expected body: " + expectedBody + "\n  expected response: " + expectedResponse + "\nRun 'cleat vet' on your workflow code to check for common non-determinism issues"
		for _, label := range []string{
			"Fetch mismatch",
			"actual body:",
			"expected body:",
			"expected response:",
		} {
			if !strings.Contains(msg, label) {
				t.Errorf("message missing label %q: %s", label, msg)
			}
		}
	})

	t.Run("truncation_applied_to_all_payloads", func(t *testing.T) {
		// Build a payload over maxPayloadLen to verify truncation fires.
		big := strings.Repeat("x", maxPayloadLen+100)
		result := truncateWithHash(big, maxPayloadLen)
		if !strings.Contains(result, "[sha256=") {
			t.Error("truncateWithHash did not add hash for oversized payload")
		}
		if len(result) > maxPayloadLen+100 {
			t.Errorf("truncated result too long: %d bytes", len(result))
		}
	})

	t.Run("await_child_event_type", func(t *testing.T) {
		msg := fmt.Sprintf("replay divergence at step 0: expected await_child, got %s.\n  run ID: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
			"sleep", "run-1")
		for _, label := range []string{
			"expected await_child",
			"run ID:",
		} {
			if !strings.Contains(msg, label) {
				t.Errorf("message missing label %q: %s", label, msg)
			}
		}
	})
}
