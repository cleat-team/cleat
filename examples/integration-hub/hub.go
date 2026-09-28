// Integration hub — per-tenant connector dispatch, with the rope end stubbed.
//
// This is the reference implementation behind docs/playbooks/integration-hub.md:
// the CLEAT-SIDE half of an event-driven integration hub. Real and runnable end
// to end on a fresh checkout with nothing installed.
//
// **The rope half is the connector's far end.** The scenario stands up a local
// HTTP sink and registers it as a webhook; the dispatch is a genuine durable
// call to the bundled notifications plugin, which delivers to that sink. What
// you do not get is the CRM, the warehouse or the ERP — replacing the sink with
// your customer's system is the reader's work, and the README says how.
//
// UNLIKE examples/order-lifecycle, THIS SCENARIO NEEDS NO PLACEHOLDER FOR ITS
// DISPATCH, and the difference is worth a reader's attention. order-lifecycle's
// PSP, inventory system and 3PL have no bundled plugin, so each rope-side step
// is a recorded DurableSleep standing where the round trip goes. Here the
// dispatch resolves to a plugin that ships in the worker, so the call is real
// and the only stub is the thing at the other end of it.
//
// What this exists to show, and it is one property rather than four:
//
//   - **A host call is recorded in event history and replayed deterministically.**
//     `notifications.send_webhook` is registered `Idempotent: false`, so if the
//     call were NOT recorded, a worker that died after making it would make it
//     again on resume and the customer's CRM would receive the same event twice.
//     The scenario asserts that it does not, by killing the worker mid-run and
//     counting deliveries — see run-integration-hub-scenario.sh.
//
// The four hitch points (docs/playbooks/integration-hub.md, "The assembly"):
//
//   - **routes** — the ingest endpoint, `POST /ingest/{source_id}`
//   - **host functions** — the connector dispatch, `send_webhook`
//   - **edge middleware** — `ratelimiter`, on every request
//   - **background loop** — webhook-ingest's retry sweep, which runs
//     `AcrossAllTenants` by name and is where an undelivered event goes
//
// Build:
//
//	cleat build -o /tmp/out ./examples/integration-hub/
package integrationhub

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cleat-team/cleat/cleat"
)

// h is the package-level context object. The transformer auto-threads it into
// every function in the durable closure that references it — which is why the
// helpers below take domain values and not a HostCalls.
var h cleat.HostCalls

// SettleDelayMs is the window the scenario kills the worker inside.
//
// IT IS NOT DECORATION AND IT IS NOT A RETRY DELAY. A crash-resume assertion
// needs a deterministic moment at which to crash, and without a durable step
// between the connector call and the end of the run there is no window wide
// enough to hit reliably -- the run would finish first. A durable sleep makes
// the window explicit and the assertion reproducible; it is also a real pattern
// (a connector that must not be hammered settles before the next event).
const SettleDelayMs = 30_000

// ---- Domain types ----

type SyncInput struct {
	// SourceID names the webhook_sources row the CUSTOMER's system delivers
	// into. Created once per deployment; see the README.
	SourceID string `json:"source_id"`

	// WebhookID names the notifications webhook the CONNECTOR dispatches to.
	// This is the rope end: in a real deployment its URL is the customer's CRM.
	WebhookID string `json:"webhook_id"`

	CustomerID string `json:"customer_id"`

	// EventType is the name of the thing that happened, and it is what the
	// connector's subscription is keyed on.
	EventType string `json:"event_type"`

	// Payload is the event body, forwarded to the connector verbatim.
	Payload json.RawMessage `json:"payload"`

	// TenantStepName, when set, names a workflow definition the CALLING
	// TENANT has uploaded through POST /api/definitions -- the wedge
	// docs/playbooks/integration-hub.md describes ("The wedge: the tenant's
	// own step, not yours"). Optional and empty by default, so every
	// existing scenario that does not set it is unaffected: the dispatch
	// still forwards the inbound payload verbatim, exactly as before.
	TenantStepName string `json:"tenant_step_name"`
}

type SyncResult struct {
	CustomerID  string `json:"customer_id"`
	EventType   string `json:"event_type"`
	DeliveryID  string `json:"delivery_id"`
	Status      string `json:"status"`
	InboundSeen bool   `json:"inbound_seen"`
	// TenantStepRan is true only when TenantStepName was set AND its child
	// workflow completed successfully. Distinguishes "no tenant step was
	// asked for" from "one was asked for and ran" for a caller inspecting
	// the published result rather than the request it sent.
	TenantStepRan bool `json:"tenant_step_ran,omitempty"`
}

// ---- Entry point ----

// SyncCustomer runs one integration event to completion.
//
// The result is a JSON string rather than a struct: a WASM entry point hands
// back bytes, and string is the one shape every language SDK expresses
// identically (IMPROVEMENT-PLAN 3.228).
//
// THE INPUT IS A STRING, NOT A SyncInput, and the binding rule is why
// (`wasm/exports.go`; cleat#824): an entry point with exactly one `string`
// parameter receives the whole input JSON verbatim, and every other shape binds
// by Go parameter NAME. See examples/order-lifecycle/order.go for the long form
// and for what the wrong shape looks like from a real worker.
func SyncCustomer(h cleat.HostCalls, input string) (string, error) {
	var in SyncInput
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return "", fmt.Errorf("decode sync input: %w", err)
	}
	if in.CustomerID == "" {
		return "", fmt.Errorf("customer_id is required")
	}
	if in.EventType == "" {
		return "", fmt.Errorf("event_type is required")
	}
	if in.WebhookID == "" {
		return "", fmt.Errorf("webhook_id is required; the connector has nowhere to dispatch to")
	}

	h.SetQueryState("customer_id", in.CustomerID)
	h.SetQueryState("event_type", in.EventType)
	h.SetQueryState("status", "waiting_for_event")

	// ---- 1. The customer's system delivers an event ----
	//
	// A real plugin, shipped in the worker, reached through the ingest route.
	// The call parks the run until an event arrives for this source, which is
	// the point: the workflow does not poll and does not need the event's body
	// to have arrived before it started.
	inbound, err := awaitInboundEvent(in.SourceID, in.EventType)
	if err != nil {
		h.SetQueryState("status", "failed")
		h.SetQueryState("failed_step", "await_inbound_event")
		return "", fmt.Errorf("customer %s: waiting for %s: %w", in.CustomerID, in.EventType, err)
	}

	h.Log("inbound event received",
		"customer_id", in.CustomerID,
		"event_type", inbound.EventType,
	)

	// ---- 1.5. The tenant's own step, not yours ----
	//
	// docs/playbooks/integration-hub.md, "The wedge": a customer uploads its
	// own transform through POST /api/definitions, and THIS workflow -- yours,
	// not theirs -- invokes it as a child, handing it the raw inbound payload.
	// Optional: every scenario that never sets TenantStepName dispatches the
	// payload verbatim, exactly as before this existed.
	tenantStepRan := false
	payload := inbound.Payload
	if in.TenantStepName != "" {
		h.SetQueryState("status", "running_tenant_step")
		runID, err := h.ChildWorkflow(in.TenantStepName, string(inbound.Payload))
		if err != nil {
			h.SetQueryState("status", "failed")
			h.SetQueryState("failed_step", "tenant_step")
			return "", fmt.Errorf("customer %s: starting tenant step %q: %w", in.CustomerID, in.TenantStepName, err)
		}
		transformed, err := h.AwaitChild(runID)
		if err != nil {
			// The tenant's own step failed or was refused -- including by the
			// sandbox itself (engine/wasi_policy.go), if it attempted
			// something a tenant-supplied step must not be able to do. Either
			// way this is OUR workflow's failure to report, not a silent
			// pass-through: a malicious or broken tenant step must not read
			// as "delivered".
			h.SetQueryState("status", "failed")
			h.SetQueryState("failed_step", "tenant_step")
			return "", fmt.Errorf("customer %s: tenant step %q: %w", in.CustomerID, in.TenantStepName, err)
		}
		payload = json.RawMessage(transformed)
		tenantStepRan = true
		h.Log("tenant step completed",
			"customer_id", in.CustomerID,
			"tenant_step_name", in.TenantStepName,
		)
	}

	h.SetQueryState("status", "dispatching")

	// ---- 2. The connector dispatch — THE RECORDED CALL ----
	//
	// This is the one the scenario crashes on. `send_webhook` is registered
	// `Idempotent: false` (plugins/notifications/host_functions.go), so its
	// result is only safe to skip on replay if the engine recorded it. The
	// scenario kills the worker after this returns and asserts, on resume, that
	// exactly ONE delivery row exists.
	//
	// It returns a delivery id rather than delivering: the plugin writes a
	// 'pending' row and its own background loop performs the HTTP call. That is
	// what makes the assertion countable — a re-executed call would be a second
	// row, and the row is visible through GET /webhooks/{id}/deliveries.
	deliveryID, err := dispatchToConnector(in.WebhookID, in.EventType, payload)
	if err != nil {
		h.SetQueryState("status", "failed")
		h.SetQueryState("failed_step", "dispatch_to_connector")
		return "", fmt.Errorf("customer %s: dispatch: %w", in.CustomerID, err)
	}

	h.SetQueryState("status", "dispatched")
	h.SetQueryState("delivery_id", deliveryID)
	h.Log("dispatched to connector",
		"customer_id", in.CustomerID,
		"delivery_id", deliveryID,
	)

	// ---- 3. Settle ----
	//
	// A durable sleep, so the crash window exists and so the resume point is
	// unambiguous. Recorded, so a worker that dies here resumes PAST it rather
	// than repeating it -- the same property the dispatch relies on, on a step
	// whose repetition would be merely slow rather than a duplicate delivery.
	h.DurableSleepMs(SettleDelayMs)

	h.SetQueryState("status", "done")
	return mustJSON(SyncResult{
		CustomerID:    in.CustomerID,
		EventType:     in.EventType,
		DeliveryID:    deliveryID,
		Status:        "done",
		InboundSeen:   true,
		TenantStepRan: tenantStepRan,
	})
}

// ---- Steps ----

// awaitInboundEvent waits for the customer's system to deliver an event.
//
// THE WAIT IS A LOOP, and it is not obvious. await_webhook's own doc says a
// found:false result "is a real answer, not an error" and that "the workflow
// engine will retry according to its retry policy"
// (plugins/webhookingest/host_functions.go). Measured against a real worker it
// does not, and the reason is worth knowing: a call that SUCCEEDS is not
// retried, whatever it returned -- the retry policy is on the durable call, and
// the durable call succeeded. Returning an error to force the wait fails the
// run instead, which is what happened the first time this was written in
// examples/order-lifecycle.
//
// So the loop waits, on DurableSleep, which is recorded: the worker parks
// between attempts and a restart resumes past the sleeps rather than repeating
// them.
func awaitInboundEvent(sourceID, eventType string) (inboundEvent, error) {
	if sourceID == "" {
		return inboundEvent{}, fmt.Errorf("no webhook source configured; see the README's setup step")
	}

	payload, err := json.Marshal(map[string]string{
		"source_id":  sourceID,
		"event_type": eventType,
	})
	if err != nil {
		return inboundEvent{}, err
	}

	const attempts = 30
	const waitMs = 1000

	for i := 0; i < attempts; i++ {
		raw, err := h.PluginCall("webhook-ingest", "await_webhook", string(payload))
		if err != nil {
			return inboundEvent{}, err
		}

		var got struct {
			Found     bool            `json:"found"`
			EventType string          `json:"event_type"`
			Payload   json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			return inboundEvent{}, fmt.Errorf("decode await_webhook response: %w", err)
		}
		if got.Found {
			return inboundEvent{EventType: got.EventType, Payload: got.Payload}, nil
		}
		if i < attempts-1 {
			h.DurableSleepMs(waitMs)
		}
	}
	// The message names the likely cause and not only the symptom, because the
	// symptom points at the wrong side of the system.
	//
	// Measured: an ingress POST carrying `{"event_type":"contact.updated"}` in
	// its BODY and no header stores the event as `webhook` -- the type comes
	// from `X-Github-Event` or `X-Event-Type`, falling back to that literal
	// (plugins/webhookingest/routes.go). The event is in the table the whole
	// time; `await_webhook` simply filters on a name nothing was stored under,
	// and the run fails a full wait-window later saying no event arrived.
	return inboundEvent{}, fmt.Errorf(
		"no %q event from source %s within %ds (an event whose type was set only "+
			"in the request body is stored as %q: the type comes from the "+
			"X-Github-Event or X-Event-Type header)",
		eventType, sourceID, attempts*waitMs/1000, "webhook")
}

type inboundEvent struct {
	EventType string
	Payload   json.RawMessage
}

// dispatchToConnector is the seam where your customer's CRM goes.
//
// THE CALL IS REAL. It resolves to the bundled notifications plugin, which
// writes a delivery row and hands it to its own retry loop; the HTTP request
// that eventually leaves the worker goes to whatever URL the webhook was
// registered with. Point that at your customer's system and this example is
// their integration.
//
// What it is NOT: it is not the customer's system itself. The scenario
// registers a local sink, because an example cannot ship a CRM -- and the
// README names that as the rope end rather than leaving a reader to discover
// the sink is a stub.
func dispatchToConnector(webhookID, eventType string, payload json.RawMessage) (string, error) {
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	req, err := json.Marshal(map[string]any{
		"webhook_id": webhookID,
		"event_type": eventType,
		"payload":    payload,
	})
	if err != nil {
		return "", err
	}

	raw, err := h.PluginCall("notifications", "send_webhook", string(req))
	if err != nil {
		return "", err
	}

	var out struct {
		DeliveryID string `json:"delivery_id"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return "", fmt.Errorf("decode send_webhook response: %w", err)
	}
	if out.DeliveryID == "" {
		return "", fmt.Errorf("notifications returned no delivery_id")
	}
	return out.DeliveryID, nil
}

// ---- Helpers ----

func mustJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
