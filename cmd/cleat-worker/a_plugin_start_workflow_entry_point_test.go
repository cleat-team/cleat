package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// cleat#2114. event-triggers subscriptions stored an entry_point field and
// read it back (plugins/eventtriggers/publish.go) but never used it: every
// event-triggered start resolved its entry point implicitly no matter what
// the subscription named. plugin.StartRequest had no field to carry one at
// all, so this was not fixable in publish.go alone.
//
// startPluginWorkflow is the seam plugin.Environment.StartWorkflow resolves
// to for the worker's own store (extracted out of a closure literal in
// main() so it is testable here at all -- it had zero test coverage
// before). It must flat-merge req.EntryPoint into req.Input the same way
// handleStartWorkflow (server.go) merges the public start API's entry_point
// field for cleat#2108 -- both call plugin.MergeEntryPoint, so this proves
// the shared helper is actually wired in here, not just that it exists.
func TestStartPluginWorkflowMergesEntryPointFlat(t *testing.T) {
	var captured json.RawMessage
	st := &mockStore{
		startNewRunFn: func(_ context.Context, _ string, _ string, _ int, input json.RawMessage, _ string, _ string, _ int) (string, bool, error) {
			captured = input
			return "run-1", false, nil
		},
	}

	req := plugin.StartRequest{
		DefName:        "d",
		Input:          json.RawMessage(`{"user_id":"u1","cart":[{"sku":"widget"}]}`),
		IdempotencyKey: "eventtrigger:evt-1:sub-1",
		TenantID:       "11111111-1111-1111-1111-111111111111",
		EntryPoint:     "place_order",
	}
	runID, err := startPluginWorkflow(context.Background(), st, req)
	if err != nil {
		t.Fatalf("startPluginWorkflow: %v", err)
	}
	if runID != "run-1" {
		t.Errorf("runID = %q, want run-1", runID)
	}

	var stored map[string]any
	if err := json.Unmarshal(captured, &stored); err != nil {
		t.Fatalf("stored input did not parse as JSON: %v: %s", err, captured)
	}

	// THE BUG: an "input" key present at the top level would mean req.Input
	// got WRAPPED rather than merged into -- the same shape cleat#2108 found
	// in the REST handler, which corrupts the entry's own fields.
	if _, wrapped := stored["input"]; wrapped {
		t.Fatalf("stored input is WRAPPED under an \"input\" key -- the entry's own fields "+
			"(user_id, cart) are nested and will not deserialize as the entry's typed parameter: %s", captured)
	}
	if stored["__entry_point"] != "place_order" {
		t.Errorf("stored input's __entry_point = %v, want \"place_order\": %s", stored["__entry_point"], captured)
	}
	if stored["user_id"] != "u1" {
		t.Errorf("stored input's user_id = %v, want \"u1\" (flat, a sibling of __entry_point): %s", stored["user_id"], captured)
	}
	if _, ok := stored["cart"]; !ok {
		t.Errorf("stored input has no top-level \"cart\" field: %s", captured)
	}
}

// The known-negative: no EntryPoint means today's implicit resolution is
// unchanged -- req.Input reaches the store byte-for-byte, no __entry_point
// field added.
func TestStartPluginWorkflowWithNoEntryPointPassesInputThrough(t *testing.T) {
	var captured json.RawMessage
	st := &mockStore{
		startNewRunFn: func(_ context.Context, _ string, _ string, _ int, input json.RawMessage, _ string, _ string, _ int) (string, bool, error) {
			captured = input
			return "run-1", false, nil
		},
	}

	req := plugin.StartRequest{
		DefName:        "d",
		Input:          json.RawMessage(`{"user_id":"u1"}`),
		IdempotencyKey: "eventtrigger:evt-1:sub-1",
		TenantID:       "11111111-1111-1111-1111-111111111111",
	}
	if _, err := startPluginWorkflow(context.Background(), st, req); err != nil {
		t.Fatalf("startPluginWorkflow: %v", err)
	}
	if strings.TrimSpace(string(captured)) != `{"user_id":"u1"}` {
		t.Errorf("with no EntryPoint, input should pass through untouched, got: %s", captured)
	}
}

// An EntryPoint against a non-object Input is refused rather than silently
// discarding the caller's input -- the same refusal handleStartWorkflow
// makes for the REST API, arrived at through plugin.MergeEntryPoint.
func TestStartPluginWorkflowEntryPointAgainstNonObjectInputIsRefused(t *testing.T) {
	st := &mockStore{}

	req := plugin.StartRequest{
		DefName:        "d",
		Input:          json.RawMessage(`"just a string"`),
		IdempotencyKey: "k",
		TenantID:       "11111111-1111-1111-1111-111111111111",
		EntryPoint:     "place_order",
	}
	_, err := startPluginWorkflow(context.Background(), st, req)
	if err == nil {
		t.Fatal("expected a refusal for entry_point against a non-object input")
	}
	if !strings.Contains(err.Error(), "JSON object") {
		t.Errorf("refusal does not explain why: %v", err)
	}
}

// cleat#1555/#1580 had no direct test either, before this extraction. Both
// still apply: neither field may be defaulted, because a default silently
// restores exactly the failure each was added to close.
func TestStartPluginWorkflowRequiresIdempotencyKeyAndTenantID(t *testing.T) {
	st := &mockStore{}

	if _, err := startPluginWorkflow(context.Background(), st, plugin.StartRequest{
		DefName: "d", Input: json.RawMessage(`{}`), TenantID: "t",
	}); err == nil || !strings.Contains(err.Error(), "idempotency key is required") {
		t.Errorf("empty IdempotencyKey: got err=%v, want a refusal naming it required", err)
	}

	if _, err := startPluginWorkflow(context.Background(), st, plugin.StartRequest{
		DefName: "d", Input: json.RawMessage(`{}`), IdempotencyKey: "k",
	}); err == nil || !strings.Contains(err.Error(), "tenant id is required") {
		t.Errorf("empty TenantID: got err=%v, want a refusal naming it required", err)
	}
}
