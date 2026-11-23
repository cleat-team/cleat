package engine

import (
	"context"
	"strings"
	"testing"
)

// TestDivergencePayloadEnrichment verifies that each call whose divergence message carries a payload still prints the labels its operator-facing documentation
// shows (docs/troubleshooting.md), by DRIVING THE REAL CALL against a history that diverges and reading what the call wrote to the guest.
//
// Its first version built each message with a format string in the test and asserted that the string it had just written contained the labels it had just
// written. It could not fail while the file compiled, which is how the request detail went missing from six calls when they moved onto the step dispatcher
// (cleat#4322, found by reading the commits rather than by this test). It also asserted first-line labels ("Fetch mismatch", "await_all_children run IDs
// mismatch") that no producer prints any more, and nothing noticed that either.
//
// The exact old text of each moved call is pinned literally by a_divergence_keeps_the_detail_its_call_printed_before_the_dispatcher_test.go; this is the
// label-level check over every call that prints a payload, including the ones that have not moved. A call that moves is not exempt: it is driven here the same way.
func TestDivergencePayloadEnrichment(t *testing.T) {
	other := func(typ EventType, rec EventRecord) []EventRecord {
		rec.Step, rec.EventType = 0, typ
		return []EventRecord{rec}
	}

	for _, tc := range []struct {
		name   string
		call   func(ctx context.Context, s *execSession) int64
		hist   []EventRecord
		labels []string
	}{
		{"replayCall_event_type", func(ctx context.Context, s *execSession) int64 {
			return s.DurableCall(ctx, nil, "svc", "op", `{"key":"value"}`, 0, 16384)
		}, other(EventTypeSleep, EventRecord{Request: `{"key":"old"}`}), []string{"actual request:", "expected request:"}},
		{"replayCall_service_op_mismatch", func(ctx context.Context, s *execSession) int64 {
			return s.DurableCall(ctx, nil, "svc", "op", `{"key":"value"}`, 0, 16384)
		}, other(EventTypeCall, EventRecord{Service: "svc2", Op: "op2", Request: `{"key":"old"}`}), []string{"actual request:", "expected request:"}},
		{"replayPluginCall_event_type", func(ctx context.Context, s *execSession) int64 {
			return s.PluginCall(ctx, nil, "p", "f", `{"key":"value"}`, 0, 16384)
		}, other(EventTypeSleep, EventRecord{PluginInput: `{"key":"old"}`, PluginOutput: `{"o":1}`}), []string{"actual input:", "expected (cached) input:", "expected (cached) output:"}},
		{"replayPluginCall_plugin_func_mismatch", func(ctx context.Context, s *execSession) int64 {
			return s.PluginCall(ctx, nil, "p", "f", `{"key":"value"}`, 0, 16384)
		}, other(EventTypePluginCall, EventRecord{PluginName: "q", PluginFunc: "g", PluginInput: `{"key":"old"}`, PluginOutput: `{"o":1}`}), []string{"actual input:", "expected (cached) input:", "expected (cached) output:"}},
		{"await_all_children_event_type", func(ctx context.Context, s *execSession) int64 {
			return s.AwaitAllChildren(ctx, nil, `["run1","run2"]`, 0, 16384)
		}, other(EventTypeSleep, EventRecord{Request: `["run1","run3"]`}), []string{"actual run IDs:", "expected run IDs:"}},
		{"await_all_children_run_ids_mismatch", func(ctx context.Context, s *execSession) int64 {
			return s.AwaitAllChildren(ctx, nil, `["run1","run2"]`, 0, 16384)
		}, other(EventTypeAwaitAllChildren, EventRecord{Request: `["run1","run3"]`}), []string{"actual run IDs:", "expected run IDs:"}},
		{"fetch_mismatch", func(ctx context.Context, s *execSession) int64 {
			return s.Fetch(ctx, nil, "POST", "/api", "{}", `{"body":"data"}`, 0, 16384)
		}, other(EventTypeFetch, EventRecord{FetchMethod: "GET", FetchURL: "/api/old", FetchBody: `{"body":"old"}`, FetchResponse: `{"resp":"ok"}`}),
			[]string{"workflow:", "history:", "actual body:", "expected body:", "expected response:"}},
		{"await_child_event_type", func(ctx context.Context, s *execSession) int64 {
			return s.AwaitChild(ctx, nil, "run-1", 0, 16384)
		}, other(EventTypeSleep, EventRecord{}), []string{"run ID:"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := g3fMessage(tc.call, tc.hist)
			// The control: this is a divergence message the call wrote, so a missing label below is the call's and not the harness's.
			if !strings.HasPrefix(msg, "replay divergence at step 0: ") {
				t.Fatalf("PRECONDITION FAILED: the call did not report a divergence, so this row measures nothing: %q", msg)
			}
			for _, label := range tc.labels {
				if !strings.Contains(msg, label) {
					t.Errorf("the divergence the call wrote is missing label %q: %s", label, msg)
				}
			}
		})
	}

	// A long request, through the real call: it is named by its WHOLE size and a hash, and none of it is printed, cut or whole (cleat#4321; it was cut at the
	// limit and followed by a hash before).
	t.Run("a_long_payload_is_named_and_not_printed", func(t *testing.T) {
		big := strings.Repeat("x", maxPayloadLen+100)
		msg := g3fMessage(func(ctx context.Context, s *execSession) int64 {
			return s.DurableCall(ctx, nil, "svc", "op", big, 0, 16384)
		}, other(EventTypeSleep, EventRecord{Request: "short"}))
		if !strings.HasPrefix(msg, "replay divergence at step 0: ") {
			t.Fatalf("PRECONDITION FAILED: the call did not report a divergence: %.120q", msg)
		}
		if !strings.Contains(msg, "actual request: [redacted: 4196 bytes, sha256=") {
			t.Errorf("the divergence does not name the %d-byte payload by its whole size and a hash: %.200q", len(big), msg)
		}
		if strings.Contains(msg, strings.Repeat("x", 100)) {
			t.Errorf("the divergence prints part of the %d-byte payload", len(big))
		}
	})
}
