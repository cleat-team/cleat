package integrationhub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat/cleattest"
)

// setupEnv creates a test environment and wires its HostCalls into the
// package-level h, which the helpers below reach through rather than taking a
// HostCalls parameter. Without this line `h` is nil and the first PluginCall
// panics -- see cleat/runtime.go, and note that the panic names the runtime
// rather than the test.
func setupEnv() *cleattest.TestEnv {
	env := cleattest.NewTestEnv()
	h = env.H()
	return env
}

// stubPlugins answers the two BUNDLED plugins this workflow calls. Both are
// real plugins shipped in the worker, so they are stubbed by name here while
// the connector's far end needs no stub at all: it is a URL, and the scenario
// points it at a local sink.
func stubPlugins(env *cleattest.TestEnv) {
	env.OnPluginCall("webhook-ingest", "await_webhook").
		Return(`{"found":true,"event_type":"contact.updated","payload":{"id":"c-1"}}`, nil)
	env.OnPluginCall("notifications", "send_webhook").
		Return(`{"delivery_id":"11111111-1111-1111-1111-111111111111"}`, nil)
}

// run drives SyncCustomer to completion while advancing the simulated clock.
//
// It has to: the settle step is a DurableSleep, and a durable sleep waits on
// SIMULATED time -- real timers do not move it, so a test that simply called
// SyncCustomer would block until the deadline.
func run(t *testing.T, env *cleattest.TestEnv, input SyncInput) (string, error) {
	t.Helper()

	// The entry point takes the input as a JSON string, not as a SyncInput --
	// see SyncCustomer's doc comment for why the signature is that shape.
	// Marshal here so the test crosses the same boundary the worker does.
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := SyncCustomer(env.H(), string(raw))
		done <- result{out, err}
	}()

	deadline := time.After(10 * time.Second)
	for {
		select {
		case r := <-done:
			return r.out, r.err
		case <-deadline:
			t.Fatal("SyncCustomer did not return within 10s of real time")
			return "", nil
		default:
			// Generous per tick for the same reason order-lifecycle's helper
			// gives: the wait windows here are tens of seconds of simulated
			// time, and a fine-grained tick spends its budget on iterations.
			env.AdvanceTime(time.Hour)
			time.Sleep(time.Millisecond)
		}
	}
}

func syncInput() SyncInput {
	return SyncInput{
		SourceID:   "22222222-2222-2222-2222-222222222222",
		WebhookID:  "33333333-3333-3333-3333-333333333333",
		CustomerID: "cus-1",
		EventType:  "contact.updated",
		Payload:    json.RawMessage(`{"id":"c-1"}`),
	}
}

func TestSyncCustomer_DispatchesToTheConnector(t *testing.T) {
	env := setupEnv()
	stubPlugins(env)

	resultJSON, err := run(t, env, syncInput())
	if err != nil {
		t.Fatalf("SyncCustomer failed: %v", err)
	}

	var result SyncResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatalf("SyncCustomer returned unparseable JSON %q: %v", resultJSON, err)
	}
	if result.Status != "done" {
		t.Errorf("status = %q, want done", result.Status)
	}
	if !result.InboundSeen {
		t.Error("InboundSeen = false: the run completed without observing the inbound event")
	}

	// THE DELIVERY ID IS THE WHOLE POINT OF THE ENTRY POINT'S RESULT.
	//
	// The scenario's crash-resume assertion counts delivery ROWS, and this is
	// the value a resumed run must still be holding: if the engine did not
	// record the call, the resumed run would dispatch again and the connector
	// would receive the customer's event twice.
	if result.DeliveryID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("delivery_id = %q, want the id the plugin returned", result.DeliveryID)
	}
	if got, ok := env.QueryState("delivery_id"); !ok || got != result.DeliveryID {
		t.Errorf("published delivery_id = %q (ok=%v), want it to match the result", got, ok)
	}

	// NOTE: `env.AssertCalled(t, "notifications", "send_webhook")` WOULD FAIL
	// HERE, WHETHER OR NOT THE CALL WAS MADE. cleattest's AssertCalled reads
	// `callHistory`, which only durable `Call`s are appended to --
	// `pluginCallImpl` records to a separate replay map and never touches it
	// (cleattest.go, `func (e *TestEnv) pluginCallImpl`). So the assertion above
	// is the observable one instead: the delivery id in the RESULT is the id
	// the stub returned, which is only obtainable by having made the call.
	//
	// The direction matters: AssertCalled fails loudly (t.Fatalf), so using it
	// wrongly is merely a broken test. AssertNotCalled over a plugin call is the
	// dangerous one -- it always passes. See the issue filed against cleattest.
}

// A connector that refuses the dispatch must fail the run at THAT step, and say
// so. Without this, a dispatch that never happened is indistinguishable from
// one that did: both leave a run that is not `done`, and the operator has no
// way to tell which half of the integration broke.
func TestSyncCustomer_ADispatchThatFailsNamesItsStep(t *testing.T) {
	env := setupEnv()
	env.OnPluginCall("webhook-ingest", "await_webhook").
		Return(`{"found":true,"event_type":"contact.updated","payload":{}}`, nil)
	env.OnPluginCall("notifications", "send_webhook").
		Return("", errTest("connector refused"))

	if _, err := run(t, env, syncInput()); err == nil {
		t.Fatal("expected the run to fail when the connector refuses the dispatch")
	}

	if got, _ := env.QueryState("failed_step"); got != "dispatch_to_connector" {
		t.Errorf("failed_step = %q, want dispatch_to_connector", got)
	}
	if got, _ := env.QueryState("status"); got != "failed" {
		t.Errorf("status = %q, want failed", got)
	}
}

// A sync with no connector is refused BEFORE the run waits for anything.
//
// The ordering matters and is the assertion: a run that parked on the inbound
// event first would hold a slot for up to the wait window and then fail for a
// reason it knew at start time.
func TestSyncCustomer_NoConnectorIsRefusedBeforeWaiting(t *testing.T) {
	env := setupEnv()
	stubPlugins(env)

	in := syncInput()
	in.WebhookID = ""

	_, err := run(t, env, in)
	if err == nil {
		t.Fatal("expected a sync with no webhook_id to be refused")
	}
	if !strings.Contains(err.Error(), "webhook_id") {
		t.Errorf("error = %v, want it to name webhook_id", err)
	}

	// THE REFUSAL IS BEFORE THE FIRST HOST CALL, and asserting that needs care.
	//
	// `env.AssertNotCalled(t, "notifications", "send_webhook")` reads correctly
	// and is worth nothing: cleattest never records plugin calls in
	// `callHistory`, so that assertion passes whether the call was made or not.
	// Verified with a known positive -- it passes over a plugin call that
	// demonstrably happened.
	//
	// So the assertion is on the thing that can actually differ: a run that got
	// past validation would have published a stage, and a run refused up front
	// publishes nothing at all. If the validation moved after the first
	// SetQueryState, this goes red.
	if got, ok := env.QueryState("status"); ok {
		t.Errorf("status = %q: the run published state, so it got past the "+
			"validation that should have refused it before it did anything", got)
	}
}

// A tenant-uploaded step, invoked as a child, transforms the payload before
// dispatch. docs/playbooks/integration-hub.md, "The wedge: the tenant's own
// step, not yours".
func TestSyncCustomer_RunsTheTenantsOwnStep(t *testing.T) {
	env := setupEnv()
	env.OnPluginCall("webhook-ingest", "await_webhook").
		Return(`{"found":true,"event_type":"contact.updated","payload":{"id":"c-1"}}`, nil)
	env.OnChildWorkflow("normalize-order").
		Return(`{"id":"c-1","normalized":true}`, nil)
	env.OnPluginCall("notifications", "send_webhook").
		Return(`{"delivery_id":"11111111-1111-1111-1111-111111111111"}`, nil)

	in := syncInput()
	in.TenantStepName = "normalize-order"

	resultJSON, err := run(t, env, in)
	if err != nil {
		t.Fatalf("SyncCustomer failed: %v", err)
	}

	var result SyncResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatalf("SyncCustomer returned unparseable JSON %q: %v", resultJSON, err)
	}
	if result.Status != "done" {
		t.Errorf("status = %q, want done", result.Status)
	}
	if !result.TenantStepRan {
		t.Error("TenantStepRan = false: the tenant step was asked for and stubbed to succeed")
	}

	calls := env.ChildWorkflowCallHistory()
	if len(calls) != 1 || calls[0].Name != "normalize-order" {
		t.Errorf("child workflow calls = %+v, want exactly one call to normalize-order", calls)
	}
}

// A tenant step that fails -- including one the SANDBOX refuses -- must fail
// THIS workflow, distinctly, rather than reading as a successful dispatch.
// The failing tenant step is exactly what a malicious or broken upload looks
// like from the caller's side of ChildWorkflow/AwaitChild; see
// examples/integration-hub/tenant-steps/malicious-read-host-file for what a
// real WASI-policy refusal looks like end to end.
func TestSyncCustomer_ATenantStepThatFailsNamesItsStep(t *testing.T) {
	env := setupEnv()
	env.OnPluginCall("webhook-ingest", "await_webhook").
		Return(`{"found":true,"event_type":"contact.updated","payload":{"id":"c-1"}}`, nil)
	env.OnChildWorkflow("malicious-read-host-file").
		Return("", errTest(`cleat refuses the WASI call "path_open"`))

	in := syncInput()
	in.TenantStepName = "malicious-read-host-file"

	if _, err := run(t, env, in); err == nil {
		t.Fatal("expected the run to fail when the tenant step fails")
	}

	if got, _ := env.QueryState("failed_step"); got != "tenant_step" {
		t.Errorf("failed_step = %q, want tenant_step", got)
	}
	if got, _ := env.QueryState("status"); got != "failed" {
		t.Errorf("status = %q, want failed", got)
	}

	// The connector must never see a payload from a step that never
	// completed -- a malicious step failing must not read as "delivered".
	if got, ok := env.QueryState("delivery_id"); ok {
		t.Errorf("delivery_id = %q: dispatch ran despite the tenant step failing", got)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
