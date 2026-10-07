// Tenant provisioning for a B2B SaaS control plane.
//
// This is the reference implementation behind cleat#2534/#2681: the CLEAT-SIDE
// half of "a new business signs up and gets a working tenant." It runs AS the
// new tenant, not as an operator -- see the README's "Which tenant does this
// run as" section for why that is load-bearing rather than incidental.
//
// **It is workflow-only, and the rope side is a placeholder.** Your workspace
// provisioning (seeding default projects, warming a cache, whatever "a new
// tenant is ready to use the product" means for your product) is not called.
// The same DurableSleep-as-seam shape examples/order-lifecycle/order.go uses,
// for the same reason: a durable call that reaches an external system
// resolves BY NAME to a plugin (engine/app.go), and this is one example
// directory rather than a plugin set.
//
// What is REAL, and it is the part the playbook is about:
//
//   - A durable provisioning sequence, with published status at every step.
//   - record_event, the Stage 2 audit host function
//     (plugins/auditlog, cleat#2616), called through its TYPED client
//     (cleat/pluginclients/auditlog, cleat#2626/#2681) rather than hand-rolled
//     PluginCall JSON -- every provisioning milestone lands in the new
//     tenant's own hash-chained audit trail.
//   - email-notify — bundled, but "not configured, disabled" on a stock
//     worker unless the deployment sets email_enabled and a SendGrid key. The
//     welcome-email step is therefore best-effort, exactly like
//     order-lifecycle's notify_customer, and for the same reason: an email
//     that does not go out should not unwind a tenant that otherwise
//     provisioned cleanly.
//   - Query state, so a UI (or the backend's own polling) can show
//     provisioning progress without reading the event history.
//
// What this file does NOT do, on purpose: create the tenant, mint its API
// key, or set its trial expiry. Those all happen BEFORE this workflow ever
// starts -- see the README's "Which tenant does this run as" section. A
// plugin has no grant to create a tenant (cleat#2534 Stage 1's own review
// found a real hazard in giving it one under --tenant-isolation=role; see
// that stage's PR), and a workflow setting its OWN trial expiry would let a
// tenant indefinitely postpone the sweep meant to constrain it
// (plugins/tenantlifecycle/plugin.go's own doc comment). Both are operator
// actions the BACKEND performs with its own database connection, the same
// way `cleat-worker --create-tenant`/`--generate-api-key` and
// `cleatctl set-tenant-trial` already do.
//
// Build:
//
//	cleat build -o /tmp/out ./examples/b2b-saas-control-plane/
package b2bsaascontrolplane

import (
	"encoding/json"
	"fmt"

	"github.com/cleat-team/cleat/cleat"
	"github.com/cleat-team/cleat/cleat/pluginclients/auditlog"
	"github.com/cleat-team/cleat/cleat/pluginclients/email"
)

// h is the package-level context object. The transformer auto-threads it into
// every function in the durable closure that references it -- the same
// convention examples/order-lifecycle/order.go documents and uses.
var h cleat.HostCalls

// placeholderRoundTripMs stands where the rope-side provisioning call goes --
// seeding a default workspace, a starter project, whatever "ready to use"
// means for your product. A DurableSleep, not a time.Sleep, for the reason
// that matters: it is RECORDED, so a worker that dies mid-provisioning
// resumes past it rather than repeating it.
const placeholderRoundTripMs = 120

// ---- Domain types ----

type ProvisionInput struct {
	// TenantID is the id the backend already created (via
	// auth.TenantStore.CreateTenant, before this run was started) and it is
	// supplied rather than re-derived: the workflow already runs AS this
	// tenant -- every host call it makes is scoped to it by the engine -- so
	// this field is used only to publish it in query state and audit details,
	// never to authorize anything. Confirming it against the caller's own
	// identity is not this file's job; there is nothing here that reads a
	// DIFFERENT tenant's data for this value to gate.
	TenantID string `json:"tenant_id"`

	BusinessName string `json:"business_name"`
	AdminEmail   string `json:"admin_email"`
	Plan         string `json:"plan"`

	// SimulateWorkspaceFailure makes the workspace-provisioning placeholder
	// fail. It exists so the failure path is reachable from the browser, the
	// same reason order-lifecycle's SimulatePaymentFailure exists: a scenario
	// that can only show its happy path never needed durability to begin
	// with.
	SimulateWorkspaceFailure bool `json:"simulate_workspace_failure"`
}

type ProvisionResult struct {
	TenantID string `json:"tenant_id"`
	Status   string `json:"status"`
}

// ---- Entry point ----

// ProvisionTenant runs one tenant's provisioning to completion, or fails it.
//
// The result is a JSON string rather than a struct, and the input is a
// string rather than a ProvisionInput, for the same reason and under the
// same binding rule order-lifecycle's PlaceOrder documents in full
// (wasm/exports.go; cleat#824): an entry point with exactly one `string`
// parameter receives the whole input JSON verbatim, which is the right shape
// for an opaque payload like this one.
func ProvisionTenant(h cleat.HostCalls, input string) (string, error) {
	var in ProvisionInput
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return "", fmt.Errorf("decode provisioning input: %w", err)
	}
	if in.TenantID == "" {
		return "", fmt.Errorf("tenant_id is required")
	}
	if in.BusinessName == "" {
		return "", fmt.Errorf("business_name is required")
	}

	h.SetQueryState("tenant_id", in.TenantID)
	h.SetQueryState("business_name", in.BusinessName)
	h.SetQueryState("plan", in.Plan)
	h.SetQueryState("status", "provisioning")

	if err := recordMilestone("tenant.provisioning_started",
		fmt.Sprintf(`{"plan":%q}`, in.Plan)); err != nil {
		return "", fmt.Errorf("tenant %s: %w", in.TenantID, err)
	}

	// ---- Workspace provisioning: the rope-side seam ----
	//
	// Your default project, starter data, warmed caches -- whatever "ready to
	// use" means for your product goes here, reached through a durable call
	// that resolves by name to a plugin this example does not ship. See the
	// package doc comment.
	h.SetQueryState("status", "provisioning_workspace")
	h.DurableSleepMs(placeholderRoundTripMs)
	if in.SimulateWorkspaceFailure {
		h.SetQueryState("status", "failed")
		h.SetQueryState("failed_step", "provision_workspace")
		h.Log("tenant provisioning failed", "tenant_id", in.TenantID, "step", "provision_workspace")
		if rerr := recordMilestone("tenant.provisioning_failed",
			`{"step":"provision_workspace"}`); rerr != nil {
			// The provisioning failure is the one that matters to the caller;
			// a failure to also RECORD that failure is logged rather than
			// returned, so it does not mask the real error.
			h.Log("tenant provisioning: failed to record the failure event", "tenant_id", in.TenantID)
		}
		return "", fmt.Errorf("tenant %s: workspace provisioning failed", in.TenantID)
	}

	if err := recordMilestone("tenant.workspace_provisioned", ""); err != nil {
		return "", fmt.Errorf("tenant %s: %w", in.TenantID, err)
	}

	// ---- Welcome email: BEST-EFFORT, and that is a decision the engine forces ----
	//
	// Exactly order-lifecycle's notify_customer reasoning: email-notify is
	// bundled but reports "plugin not configured, disabled" on a stock
	// worker unless the deployment sets email_enabled and a provider key.
	// A tenant that provisioned cleanly should not be unwound -- there is
	// nothing TO unwind here, since nothing spent anything -- because a
	// welcome email did not go out.
	h.SetQueryState("status", "notifying")
	if err := sendWelcomeEmail(in); err != nil {
		h.Log("tenant provisioned, but the welcome email could not be sent", "tenant_id", in.TenantID)
		h.SetQueryState("notify_failed", "true")
	} else if err := recordMilestone("tenant.welcome_email_sent", ""); err != nil {
		return "", fmt.Errorf("tenant %s: %w", in.TenantID, err)
	}

	h.SetQueryState("status", "active")
	h.Log("tenant provisioning complete", "tenant_id", in.TenantID, "business_name", in.BusinessName)
	if err := recordMilestone("tenant.provisioning_completed", ""); err != nil {
		return "", fmt.Errorf("tenant %s: %w", in.TenantID, err)
	}

	out, err := json.Marshal(ProvisionResult{TenantID: in.TenantID, Status: "active"})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ---- Helpers ----

// recordMilestone appends one row to the calling tenant's own audit chain
// through the typed record_event client -- the hitch point cleat#2534's
// Stage 2 and cleat#2681 both ask for exercised, not merely described.
//
// details is passed through verbatim as the JSON object body, or omitted --
// record_event refuses anything that is not a JSON object (cleat#2589/#2616),
// so an empty string here (rather than "{}") relies on
// auditlog.RecordEventInput's own `omitempty` to leave Details unset, which
// record_event defaults to "{}" itself.
func recordMilestone(eventType, details string) error {
	in := auditlog.RecordEventInput{EventType: eventType}
	if details != "" {
		in.Details = json.RawMessage(details)
	}
	_, err := auditlog.RecordEvent.Call(h, in)
	if err != nil {
		return fmt.Errorf("record %s: %w", eventType, err)
	}
	return nil
}

// sendWelcomeEmail goes to a plugin rather than a service, matching
// order-lifecycle's notifyCustomer: email is a first-class cleat surface
// with its own delivery tracking.
func sendWelcomeEmail(in ProvisionInput) error {
	if in.AdminEmail == "" {
		return nil
	}
	_, err := email.Send.Call(h, email.SendInput{
		To:      in.AdminEmail,
		Subject: fmt.Sprintf("Welcome to %s", in.BusinessName),
		BodyHTML: fmt.Sprintf(
			"Your workspace for %s is ready. Plan: %s.\n", in.BusinessName, in.Plan),
	})
	return err
}
