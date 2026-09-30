package b2bsaascontrolplane

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat/cleattest"
	"github.com/cleat-team/cleat/cleat/pluginclients/auditlog"
	"github.com/cleat-team/cleat/cleat/pluginclients/email"
)

// setupEnv creates a test environment and wires it into the package-level h,
// matching examples/order-lifecycle/order_test.go's own pattern.
func setupEnv() *cleattest.TestEnv {
	env := cleattest.NewTestEnv()
	h = env.H()
	return env
}

// stubPlugins answers the two bundled plugins this workflow calls.
func stubPlugins(env *cleattest.TestEnv) {
	env.OnPluginCall("audit-log", "record_event").
		Return(`{"recorded":true}`, nil)
	env.OnPluginCall("email-notify", "send").Return(`{"message_id":"msg_1","status":"queued"}`, nil)
}

// run drives ProvisionTenant to completion while advancing the simulated
// clock -- the rope-side steps are DurableSleeps, which wait on simulated
// time, matching examples/order-lifecycle/order_test.go's own run helper.
func run(t *testing.T, env *cleattest.TestEnv, input ProvisionInput) (string, error) {
	t.Helper()
	type result struct {
		out string
		err error
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	done := make(chan result, 1)
	go func() {
		out, err := ProvisionTenant(env.H(), string(raw))
		done <- result{out, err}
	}()

	deadline := time.After(10 * time.Second)
	for {
		select {
		case r := <-done:
			return r.out, r.err
		case <-deadline:
			t.Fatal("ProvisionTenant did not return within 10s of real time")
			return "", nil
		default:
			env.AdvanceTime(time.Second)
			time.Sleep(time.Millisecond)
		}
	}
}

func smallTenant() ProvisionInput {
	return ProvisionInput{
		TenantID:     "11111111-1111-1111-1111-111111111111",
		BusinessName: "Acme Corp",
		AdminEmail:   "admin@acme.example",
		Plan:         "starter",
	}
}

func TestProvisionTenant_Completes(t *testing.T) {
	env := setupEnv()
	stubPlugins(env)

	resultJSON, err := run(t, env, smallTenant())
	if err != nil {
		t.Fatalf("ProvisionTenant failed: %v", err)
	}

	var result ProvisionResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatalf("ProvisionTenant returned unparseable JSON %q: %v", resultJSON, err)
	}
	if result.Status != "active" {
		t.Errorf("status = %q, want active", result.Status)
	}
	if result.TenantID != smallTenant().TenantID {
		t.Errorf("tenant_id = %q, want %q", result.TenantID, smallTenant().TenantID)
	}

	if got, ok := env.QueryState("status"); !ok || got != "active" {
		t.Errorf("query status = %q (ok=%v), want active", got, ok)
	}
}

// TestProvisionTenant_RecordsEveryMilestone is the regression this scenario
// exists to carry: each provisioning step must call record_event with the
// exact event_type the README documents, through the TYPED client
// (cleat#2626/#2681) -- asserted on the wire request cleattest recorded, the
// only way to see a hand-rolled JSON regression the way
// order-lifecycle's own TestPlaceOrder_NotifyCustomerSendsARealBody does for
// email-notify.
func TestProvisionTenant_RecordsEveryMilestone(t *testing.T) {
	env := setupEnv()
	stubPlugins(env)

	if _, err := run(t, env, smallTenant()); err != nil {
		t.Fatalf("ProvisionTenant failed: %v", err)
	}

	var eventTypes []string
	for _, rec := range env.CallHistory() {
		if rec.Service != "audit-log" || rec.Operation != "record_event" {
			continue
		}
		var req auditlog.RecordEventInput
		if err := json.Unmarshal([]byte(rec.Request), &req); err != nil {
			t.Fatalf("record_event request is not valid JSON: %v (%q)", err, rec.Request)
		}
		eventTypes = append(eventTypes, req.EventType)
	}

	want := []string{
		"tenant.provisioning_started",
		"tenant.workspace_provisioned",
		"tenant.welcome_email_sent",
		"tenant.provisioning_completed",
	}
	if len(eventTypes) != len(want) {
		t.Fatalf("record_event called with event types %v, want %v", eventTypes, want)
	}
	for i, w := range want {
		if eventTypes[i] != w {
			t.Errorf("record_event call %d: event_type = %q, want %q", i, eventTypes[i], w)
		}
	}
}

func TestProvisionTenant_SendsARealWelcomeEmail(t *testing.T) {
	env := setupEnv()
	stubPlugins(env)

	in := smallTenant()
	if _, err := run(t, env, in); err != nil {
		t.Fatalf("ProvisionTenant failed: %v", err)
	}

	var sendReq *email.SendInput
	for _, rec := range env.CallHistory() {
		if rec.Service != "email-notify" || rec.Operation != "send" {
			continue
		}
		var req email.SendInput
		if err := json.Unmarshal([]byte(rec.Request), &req); err != nil {
			t.Fatalf("email-notify.send request is not valid JSON: %v (%q)", err, rec.Request)
		}
		sendReq = &req
	}
	if sendReq == nil {
		t.Fatal("sendWelcomeEmail never called email-notify.send")
	}
	if sendReq.To != in.AdminEmail {
		t.Errorf("SendInput.To = %q, want %q", sendReq.To, in.AdminEmail)
	}
	if sendReq.BodyHTML == "" {
		t.Fatal("SendInput.BodyHTML is empty")
	}
	if !strings.Contains(sendReq.BodyHTML, in.BusinessName) {
		t.Errorf("SendInput.BodyHTML = %q, want it to mention %q", sendReq.BodyHTML, in.BusinessName)
	}
}

// A failed welcome email must not fail (or unwind) an otherwise-successful
// provisioning run -- there is nothing to unwind, since nothing was spent,
// and order-lifecycle's own notify_customer makes the identical decision for
// the identical reason.
func TestProvisionTenant_ANotifyFailureDoesNotFailProvisioning(t *testing.T) {
	env := setupEnv()
	env.OnPluginCall("audit-log", "record_event").Return(`{"recorded":true}`, nil)
	env.OnPluginCall("email-notify", "send").Return("", fmt.Errorf("plugin not configured, disabled"))

	resultJSON, err := run(t, env, smallTenant())
	if err != nil {
		t.Fatalf("ProvisionTenant failed on a notify error: %v", err)
	}
	var result ProvisionResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatalf("unparseable result: %v", err)
	}
	if result.Status != "active" {
		t.Errorf("status = %q, want active despite the notify failure", result.Status)
	}
	if got, _ := env.QueryState("notify_failed"); got != "true" {
		t.Errorf("notify_failed = %q, want true", got)
	}

	// And no welcome_email_sent milestone -- the workflow must not claim to
	// have sent an email it did not.
	for _, rec := range env.CallHistory() {
		if rec.Service != "audit-log" || rec.Operation != "record_event" {
			continue
		}
		var req auditlog.RecordEventInput
		_ = json.Unmarshal([]byte(rec.Request), &req)
		if req.EventType == "tenant.welcome_email_sent" {
			t.Errorf("recorded tenant.welcome_email_sent despite the send failing")
		}
	}
}

func TestProvisionTenant_WorkspaceFailureRecordsAFailureEventAndFails(t *testing.T) {
	env := setupEnv()
	stubPlugins(env)

	in := smallTenant()
	in.SimulateWorkspaceFailure = true

	if _, err := run(t, env, in); err == nil {
		t.Fatal("expected provisioning to fail when the workspace step fails")
	}
	if got, _ := env.QueryState("status"); got != "failed" {
		t.Errorf("status = %q, want failed", got)
	}
	if got, _ := env.QueryState("failed_step"); got != "provision_workspace" {
		t.Errorf("failed_step = %q, want provision_workspace", got)
	}

	var sawFailureEvent bool
	for _, rec := range env.CallHistory() {
		if rec.Service != "audit-log" || rec.Operation != "record_event" {
			continue
		}
		var req auditlog.RecordEventInput
		_ = json.Unmarshal([]byte(rec.Request), &req)
		if req.EventType == "tenant.provisioning_failed" {
			sawFailureEvent = true
		}
		// The failure means nothing past the failed step ran.
		if req.EventType == "tenant.workspace_provisioned" || req.EventType == "tenant.provisioning_completed" {
			t.Errorf("recorded %q despite the workspace step failing", req.EventType)
		}
	}
	if !sawFailureEvent {
		t.Error("never recorded tenant.provisioning_failed")
	}
}

func TestProvisionTenant_RejectsMissingTenantID(t *testing.T) {
	env := setupEnv()
	in := smallTenant()
	in.TenantID = ""
	if _, err := run(t, env, in); err == nil {
		t.Fatal("expected an error for a missing tenant_id")
	}
}

func TestProvisionTenant_RejectsMissingBusinessName(t *testing.T) {
	env := setupEnv()
	in := smallTenant()
	in.BusinessName = ""
	if _, err := run(t, env, in); err == nil {
		t.Fatal("expected an error for a missing business_name")
	}
}
