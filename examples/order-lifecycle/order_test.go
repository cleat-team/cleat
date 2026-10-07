package orderlifecycle

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat/cleattest"
	"github.com/cleat-team/cleat/cleat/pluginclients/email"
)

// setupEnv creates a test environment and wires it into the package-level h so
// that both the entry-point parameter and the helpers' references work.
func setupEnv() *cleattest.TestEnv {
	env := cleattest.NewTestEnv()
	h = env.H()
	return env
}

// stubPlugins answers the two BUNDLED plugins this workflow calls. They are real
// plugins — shipped in the worker — which is why they are stubbed by name here
// while the rope-side steps need no stub at all: they are placeholders.
func stubPlugins(env *cleattest.TestEnv) {
	env.OnPluginCall("webhook-ingest", "await_webhook").
		Return(`{"found":true,"event_type":"payment.succeeded","payload":{}}`, nil)
	env.OnPluginCall("email-notify", "send").Return(`{"id":"msg_1"}`, nil)
}

// run drives PlaceOrder to completion while advancing the simulated clock.
//
// It has to: every rope-side step is a DurableSleep, and a durable sleep waits
// on SIMULATED time — real timers do not move it, so a test that simply called
// PlaceOrder would block until the deadline. The deadline is what turns "the
// workflow never woke" into a failure rather than a hung suite.
func run(t *testing.T, env *cleattest.TestEnv, input OrderInput) (string, error) {
	t.Helper()
	type result struct {
		out string
		err error
	}
	// The entry point takes the input as a JSON string, not as an OrderInput —
	// see PlaceOrder's doc comment for why the signature is that shape. Marshal
	// here so the test crosses the same boundary the worker does.
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	done := make(chan result, 1)
	go func() {
		out, err := PlaceOrder(env.H(), string(raw))
		done <- result{out, err}
	}()

	deadline := time.After(10 * time.Second)
	for {
		select {
		case r := <-done:
			return r.out, r.err
		case <-deadline:
			t.Fatal("PlaceOrder did not return within 10s of real time")
			return "", nil
		default:
			// An HOUR per tick, not a millisecond: the approval window is 24h of
			// simulated time, so a fine-grained tick needs 864,000 iterations
			// and the real-time deadline fires first. Measured: 100ms ticks
			// time out at 10s having advanced two minutes of simulated time.
			env.AdvanceTime(time.Hour)
			time.Sleep(time.Millisecond)
		}
	}
}

// statusesSeen samples the workflow's published `status` until stop is closed,
// returning every distinct non-empty value it observed.
//
// Sampled rather than asserted on the finished state, because the value under
// test is only wrong DURING the run: an approved order ends at "done" whether
// or not the gate cleared its own status, so reading the finished state cannot
// see the difference. Only the in-flight value can.
func statusesSeen(env *cleattest.TestEnv, stop <-chan struct{}) map[string]bool {
	seen := map[string]bool{}
	for {
		select {
		case <-stop:
			return seen
		default:
		}
		if s, ok := env.QueryState("status"); ok && s != "" {
			seen[s] = true
		}
		time.Sleep(time.Millisecond)
	}
}

func smallOrder() OrderInput {
	return OrderInput{
		OrderID:    "ord-1001",
		CustomerID: "cus-1",
		Email:      "buyer@example.com",
		SourceID:   "11111111-1111-1111-1111-111111111111",
		Items: []OrderItem{
			{SKU: "widget", Quantity: 2, PriceCents: 2500},
		},
	}
}

func TestPlaceOrder_Completes(t *testing.T) {
	env := setupEnv()
	stubPlugins(env)

	resultJSON, err := run(t, env, smallOrder())
	if err != nil {
		t.Fatalf("PlaceOrder failed: %v", err)
	}

	// The entry point returns JSON, not a struct — an entry point's result must
	// be a string (IMPROVEMENT-PLAN 3.228). Unmarshal it back so the test checks
	// the bytes the host would actually receive.
	var result OrderResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatalf("PlaceOrder returned unparseable JSON %q: %v", resultJSON, err)
	}
	if result.Status != "done" {
		t.Errorf("status = %q, want done", result.Status)
	}
	if result.TotalCents != 5000 {
		t.Errorf("total = %d, want 5000", result.TotalCents)
	}
	if result.Steps != 5 {
		t.Errorf("steps completed = %d, want all 5", result.Steps)
	}

	// Nothing was undone, and the query state has to say so rather than leaving
	// a UI to distinguish "no compensations" from "not reported".
	if got, ok := env.QueryState("status"); !ok || got != "done" {
		t.Errorf("query status = %q (ok=%v), want done", got, ok)
	}
	if got, _ := env.QueryState("compensated"); got != "" {
		t.Errorf("compensated = %q, want empty on a completed order", got)
	}
}

// TestPlaceOrder_NotifyCustomerSendsARealBody is the regression cleat-review
// asked for on cleat#2626. notifyCustomer used to build its email-notify.send
// payload as a hand-written map[string]string{"body": ...} -- a key
// plugins/email's SendInput (host_functions.go) has never had. json.Unmarshal
// silently drops an unknown field, so BodyHTML stayed "" and every real send
// failed with "email: body_html is required" -- with nothing here catching it,
// because stubPlugins answers "email-notify"/"send" unconditionally and no
// earlier test inspected what notifyCustomer actually sent, only that the call
// did not return an error.
//
// This asserts on the wire request cleattest recorded, not on PlaceOrder's own
// result, which is the only way to see the bug: PlaceOrder succeeds either way,
// because the stub answers before anything checks BodyHTML.
func TestPlaceOrder_NotifyCustomerSendsARealBody(t *testing.T) {
	env := setupEnv()
	stubPlugins(env)

	order := smallOrder()
	if _, err := run(t, env, order); err != nil {
		t.Fatalf("PlaceOrder failed: %v", err)
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
		t.Fatal("notifyCustomer never called email-notify.send")
	}

	if sendReq.To != order.Email {
		t.Errorf("SendInput.To = %q, want %q", sendReq.To, order.Email)
	}
	if sendReq.BodyHTML == "" {
		t.Fatal("SendInput.BodyHTML is empty -- the confirmation text is not reaching the field plugins/email actually reads")
	}
	if !strings.Contains(sendReq.BodyHTML, order.OrderID) {
		t.Errorf("SendInput.BodyHTML = %q, want it to mention the order id %q", sendReq.BodyHTML, order.OrderID)
	}
}

// THE TEST THAT MATTERS. A failure at the FIRST spending step must unwind what
// came before it and touch nothing that came after — and the difference between
// those two is what a saga gets right and a hand-written rollback gets wrong.
func TestPlaceOrder_CompensatesTheCompletedStepsOnly(t *testing.T) {
	env := setupEnv()
	stubPlugins(env)

	input := smallOrder()
	input.SimulatePaymentFailure = true

	if _, err := run(t, env, input); err == nil {
		t.Fatal("expected the order to fail when the charge is declined")
	}

	// The reservation completed, so its compensation ran.
	compensated, ok := env.QueryState("compensated")
	if !ok {
		t.Fatal("query state 'compensated' was not published; a UI cannot show what was undone")
	}
	if !strings.Contains(compensated, "reserve_inventory") {
		t.Errorf("compensated = %q, want it to name reserve_inventory", compensated)
	}

	// The FAILED step is NOT compensated, and that is correct rather than an
	// omission — a step whose forward never completed has nothing to undo, and
	// "refund a charge that did not happen" is its own incident. This is also
	// why a real chargePSP must pass an idempotency key DOWNSTREAM: the case
	// where the charge actually succeeded and the step only LOOKS failed is a
	// timeout, and the retry is deduplicated by the PSP, not by the saga.
	if strings.Contains(compensated, "charge_psp") {
		t.Errorf("compensated = %q names charge_psp, whose forward never completed", compensated)
	}

	// Never reached, so never undone.
	if strings.Contains(compensated, "dispatch_fulfilment") {
		t.Errorf("compensated = %q names dispatch_fulfilment, which never ran", compensated)
	}

	if got, _ := env.QueryState("failed_step"); got != "charge_psp" {
		t.Errorf("failed_step = %q, want charge_psp", got)
	}
	if got, _ := env.QueryState("status"); got != "failed" {
		t.Errorf("status = %q, want failed", got)
	}
}

// The same shape one step deeper, and it is the case the saga exists for: the
// charge COMPLETED, so the refund is a real unwind rather than an unwind of
// nothing. A single failure flag cannot reach this — failing the first spending
// step is where compensation does the least — which is why the input carries a
// second trigger.
func TestPlaceOrder_CompensatesPastTheWebhookStep(t *testing.T) {
	env := setupEnv()
	stubPlugins(env)

	input := smallOrder()
	input.SimulateFulfilmentFailure = true

	if _, err := run(t, env, input); err == nil {
		t.Fatal("expected the order to fail when dispatch fails")
	}

	compensated, _ := env.QueryState("compensated")
	// The refund is the part that matters, and it is the half a reader most
	// wants to see: the webhook step has nothing to hand back, and it is easy
	// to write an unwinder that stops there.
	for _, want := range []string{"charge_psp", "reserve_inventory"} {
		if !strings.Contains(compensated, want) {
			t.Errorf("compensated = %q, want it to name %s", compensated, want)
		}
	}
	// Dispatch never completed, so there is nothing to cancel.
	if strings.Contains(compensated, "dispatch_fulfilment") {
		t.Errorf("compensated = %q names dispatch_fulfilment, whose forward failed", compensated)
	}

	// Newest first: the order they were undone in, which is the reverse of the
	// order they ran.
	if i, j := strings.Index(compensated, "charge_psp"), strings.Index(compensated, "reserve_inventory"); i > j {
		t.Errorf("compensated = %q; the charge was undone before the reservation was, "+
			"but they are listed in the other order", compensated)
	}
}

// A compensation that RUNS AND FAILS is not a compensated step.
//
// This is the distinction the playbook's own failure modes name — "a
// compensation that fails leaves you in the state the saga was trying to avoid"
// — and it is the one an operator has to act on. Without it, an order whose
// refund failed would publish as compensated, and the two are the same word
// until someone needs them to be different.
func TestPlaceOrder_ACompensationThatFailsIsNotReportedAsUnwound(t *testing.T) {
	env := setupEnv()
	stubPlugins(env)

	input := smallOrder()
	input.SimulatePaymentFailure = true      // fail at the charge, so the reservation unwinds
	input.SimulateCompensationFailure = true // and the release itself fails

	if _, err := run(t, env, input); err == nil {
		t.Fatal("expected the order to fail")
	}

	unwound, _ := env.QueryState("compensated")
	if strings.Contains(unwound, "reserve_inventory") {
		t.Errorf("compensated = %q names reserve_inventory, but its compensation FAILED — "+
			"a step whose unwind did not complete is not compensated", unwound)
	}

	failed, ok := env.QueryState("unwind_failed")
	if !ok {
		t.Fatal("query state 'unwind_failed' was not published; a failed compensation is " +
			"indistinguishable from one that never ran, and it is the line an operator must act on")
	}
	if !strings.Contains(failed, "reserve_inventory") {
		t.Errorf("unwind_failed = %q, want it to name reserve_inventory", failed)
	}
	if got, _ := env.QueryState("unwind_failed_count"); got != "1" {
		t.Errorf("unwind_failed_count = %q, want 1", got)
	}
}

func TestPlaceOrder_RejectsAnEmptyOrderBeforeSpendingAnything(t *testing.T) {
	env := setupEnv()
	stubPlugins(env)

	input := smallOrder()
	input.Items = nil

	if _, err := run(t, env, input); err == nil {
		t.Fatal("expected an error for an order with no items")
	}
	if got, _ := env.QueryState("compensated"); got != "" {
		t.Errorf("compensated = %q, want empty: nothing was spent, so nothing needed undoing", got)
	}
}

// ---- The approval branch ----

func TestPlaceOrder_WaitsForApprovalAboveTheThreshold(t *testing.T) {
	env := setupEnv()
	stubPlugins(env)
	env.Signal("order_approved", `{"by":"ops@example.com"}`)

	input := smallOrder()
	input.Items = []OrderItem{{SKU: "server", Quantity: 1, PriceCents: ApprovalThresholdCents + 1}}

	stop := make(chan struct{})
	statuses := make(chan map[string]bool, 1)
	go func() { statuses <- statusesSeen(env, stop) }()

	if _, err := run(t, env, input); err != nil {
		t.Fatalf("PlaceOrder failed: %v", err)
	}
	close(stop)
	seen := <-statuses

	if got, _ := env.QueryState("status"); got != "done" {
		t.Errorf("status = %q, want done", got)
	}
	// The gate has to clear its own status. The saga reports its progress as
	// current_step and writes `status` only at the END, so without the write in
	// order.go an approved order reads "awaiting_approval" for the whole saga --
	// and the demo UI offers the Approve/Reject controls whenever it sees that
	// value, for an order it is already charging. cleat#2627.
	if !seen["approved"] {
		t.Errorf("status was %v during the run, never \"approved\"; an approved order must stop "+
			"reading \"awaiting_approval\" once the decision is made", seen)
	}
}

// A rejection must cost nothing. The approval gate is before the first spending
// step, so there is no compensation trail — and an implementation that put the
// gate after the charge would show up here as a non-empty `compensated`.
func TestPlaceOrder_ARejectedOrderSpendsNothing(t *testing.T) {
	env := setupEnv()
	env.Signal("order_rejected", "over budget")

	input := smallOrder()
	input.Items = []OrderItem{{SKU: "server", Quantity: 1, PriceCents: ApprovalThresholdCents + 1}}

	if _, err := run(t, env, input); err == nil {
		t.Fatal("expected the order to fail when it is rejected")
	}
	if got, _ := env.QueryState("compensated"); got != "" {
		t.Errorf("compensated = %q, want empty: a rejected order never spent anything", got)
	}
	if got, _ := env.QueryState("status"); got != "rejected" {
		t.Errorf("status = %q, want rejected", got)
	}
	if got, _ := env.QueryState("rejection_reason"); got != "over budget" {
		t.Errorf("rejection_reason = %q, want the payload the signal carried", got)
	}
}

// The gate has teeth only if an UNAPPROVED order does not proceed — and the
// test above cannot show that, because an implementation with no gate at all
// also completes and also passes it. Measured: removing `total > threshold`
// leaves the test above GREEN. This one goes red.
func TestPlaceOrder_AboveTheThresholdWithoutApprovalDoesNotCharge(t *testing.T) {
	env := setupEnv()
	stubPlugins(env)
	// Nothing is queued on the signal queue, so the wait has to time out.

	input := smallOrder()
	input.Items = []OrderItem{{SKU: "server", Quantity: 1, PriceCents: ApprovalThresholdCents + 1}}

	if _, err := run(t, env, input); err == nil {
		t.Fatal("an unapproved order completed; it must give up, not proceed")
	}
	if got, _ := env.QueryState("compensated"); got != "" {
		t.Errorf("compensated = %q, want empty: the gate is before the first spending step", got)
	}
	if got, _ := env.QueryState("status"); got != "rejected" {
		t.Errorf("status = %q, want rejected after the approval window", got)
	}
}

func TestPlaceOrder_BelowTheThresholdChargesWithoutApproval(t *testing.T) {
	env := setupEnv()
	stubPlugins(env)

	// No approval signal is queued at all: if the workflow waited for one it
	// would block, so this also asserts the gate is not applied unconditionally.
	if _, err := run(t, env, smallOrder()); err != nil {
		t.Fatalf("PlaceOrder failed: %v", err)
	}
	if got, _ := env.QueryState("status"); got != "done" {
		t.Errorf("status = %q, want done", got)
	}
}
