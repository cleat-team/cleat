package eventtriggers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/plugin"
)

// cleat#2114. A subscription's entry_point column was accepted, stored, and
// read back into sub.EntryPoint (routes.go, publish.go) but never reached
// the workflow start: plugin.StartRequest had no field to carry one, so a
// matching event always started with implicit entry-point resolution no
// matter what the subscription named.
//
// This proves the field is now THREADED, not merely that publishing an
// event no longer errors -- the issue's own test plan calls out that a
// weaker assertion ("does not error") would not have caught the original
// bug, since the original code did not error either; it just silently
// ignored sub.EntryPoint.
func TestASubscriptionsEntryPointReachesTheStartRequest(t *testing.T) {
	db := newRecordingDB(t)
	db.subscriptions = []fakeSubscription{{
		id:         uuid.New(),
		defName:    "multi-entry-workflow",
		tmpl:       json.RawMessage(`{}`),
		enabled:    true,
		entryPoint: "handle_refund",
	}}

	var captured plugin.StartRequest
	var started int
	env := &plugin.Environment{
		Dialect: plugin.DialectPostgres,
		StartWorkflow: func(_ context.Context, req plugin.StartRequest) (string, error) {
			captured, started = req, started+1
			return "run-1", nil
		},
	}
	matched, err := triggerMatchingWorkflows(context.Background(), db, quietLogger(),
		env, uuid.New(), uuid.New(), "order.refunded", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("triggerMatchingWorkflows: %v", err)
	}
	if started != 1 || matched != 1 {
		t.Fatalf("UNMEASURED: expected 1 workflow start, got started=%d matched=%d", started, matched)
	}
	if captured.EntryPoint != "handle_refund" {
		t.Errorf("plugin.StartRequest.EntryPoint = %q, want %q -- the subscription's entry_point "+
			"must reach the start request, not just be read back from the row",
			captured.EntryPoint, "handle_refund")
	}
}

// The known-negative from the issue's test plan: a subscription with no
// entry_point set, on a workflow that auto-resolves its single entry point,
// keeps today's behaviour unchanged -- an empty EntryPoint on the request.
func TestASubscriptionWithNoEntryPointKeepsImplicitResolution(t *testing.T) {
	db := newRecordingDB(t)
	db.subscriptions = []fakeSubscription{{
		id:      uuid.New(),
		defName: "single-entry-workflow",
		tmpl:    json.RawMessage(`{}`),
		enabled: true,
		// entryPoint deliberately left unset (""), the common case: most
		// subscriptions target a single-entry-point workflow and never set
		// one.
	}}

	var captured plugin.StartRequest
	var started int
	env := &plugin.Environment{
		Dialect: plugin.DialectPostgres,
		StartWorkflow: func(_ context.Context, req plugin.StartRequest) (string, error) {
			captured, started = req, started+1
			return "run-1", nil
		},
	}
	matched, err := triggerMatchingWorkflows(context.Background(), db, quietLogger(),
		env, uuid.New(), uuid.New(), "order.created", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("triggerMatchingWorkflows: %v", err)
	}
	if started != 1 || matched != 1 {
		t.Fatalf("UNMEASURED: expected 1 workflow start, got started=%d matched=%d", started, matched)
	}
	if captured.EntryPoint != "" {
		t.Errorf("plugin.StartRequest.EntryPoint = %q, want \"\" (implicit resolution) "+
			"for a subscription with no entry_point set", captured.EntryPoint)
	}
}
