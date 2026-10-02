// Order lifecycle — a saga over a payment provider, with compensation.
//
// This is the reference implementation behind docs/playbooks/order-lifecycle.md:
// the CLEAT-SIDE half of an order pipeline, real and runnable end to end on a
// fresh checkout with nothing installed.
//
// **It is workflow-only, and the rope side is a placeholder.** Your PSP, your
// inventory system and your 3PL are not called. A durable call that reaches an
// external system resolves BY NAME to a plugin (engine/app.go), and this is one
// example directory rather than a plugin set; the SDK's generic outbound-HTTP
// call is embedder-only and cannot serve a stock cleat-worker (ABI.md 2.48).
// So each rope-side step is a recorded DurableSleep standing where the round
// trip goes — the same shape cmd/cleat/templates/fullstack uses, and for the
// same reason. The seam is marked in each function.
//
// What is REAL, and it is the part the playbook is about:
//
//   - Saga compensation (cleat.NewSaga) — each step declares its undo, and a
//     later failure unwinds the completed ones in reverse. A step that fails
//     internally unwinds the saga exactly as an external failure would, which
//     is why no plugin is needed to demonstrate it.
//   - await_webhook against the bundled webhook-ingest plugin — a real plugin,
//     shipped in the worker, so this half genuinely runs.
//   - email-notify — bundled, but "not configured, disabled" on a stock worker
//     unless the deployment sets email_enabled and a SendGrid key. The
//     notification step is therefore best-effort, and says why where it is
//     declared.
//   - A human-approval signal above a threshold.
//   - Query state, including WHICH steps were undone, so a UI can show what
//     happened without reading the event history.
//   - Idempotency at the start path, which is the worker's rather than this
//     file's.
//
// Build:
//
//	cleat build -o /tmp/out ./examples/order-lifecycle/
package orderlifecycle

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cleat-team/cleat/cleat"
	"github.com/cleat-team/cleat/cleat/pluginclients/email"
	"github.com/cleat-team/cleat/cleat/pluginclients/webhookingest"
)

// h is the package-level context object. The transformer auto-threads it into
// every function in the durable closure that references it — which is why the
// helpers below take domain values and not a HostCalls.
var h cleat.HostCalls

// ApprovalThresholdCents is the order value above which a human has to approve
// before the card is charged. Deliberately low enough that the UI can reach it
// with two items, so the approval path is demonstrable rather than described.
const ApprovalThresholdCents = 50_000

// ApprovalTimeout is how long an order waits for its approval signal before
// giving up. A real deployment would use hours; this is short so the timeout
// path is testable.
const ApprovalTimeout = 24 * time.Hour

// placeholderRoundTripMs stands where a network round trip goes, in each of the
// rope-side steps. It is a `DurableSleep` rather than a `time.Sleep` for the
// reason that matters: a durable sleep is RECORDED, so a worker that dies
// during it resumes past it rather than repeating it. Swapping this for a
// `time.Sleep` in a real step would quietly give up the property the step is
// there to demonstrate.
const placeholderRoundTripMs = 120

// ---- Domain types ----

type OrderItem struct {
	SKU        string `json:"sku"`
	Quantity   int    `json:"quantity"`
	PriceCents int    `json:"price_cents"`
}

type OrderInput struct {
	OrderID    string      `json:"order_id"`
	CustomerID string      `json:"customer_id"`
	Email      string      `json:"email"`
	Items      []OrderItem `json:"items"`

	// SourceID names the webhook_sources row the PSP's payment confirmation
	// arrives through. It is created once per deployment, not per order — see
	// the README, which documents the commands, and which is also where the
	// awkwardness of that setup is recorded rather than smoothed over.
	SourceID string `json:"source_id"`

	// SimulatePaymentFailure makes the PSP placeholder decline. It exists so
	// the COMPENSATING path is reachable from the browser: a reader can watch
	// the inventory release happen, which is the whole argument for declaring
	// compensation rather than orchestrating it.
	SimulatePaymentFailure bool `json:"simulate_payment_failure"`

	// SimulateCompensationFailure makes the inventory release FAIL. It exists
	// because "the compensation ran" and "the compensation worked" are different
	// claims, and the second is the one an operator acts on: a refund that fails
	// leaves the order in the state the saga existed to avoid. The playbook's
	// own failure modes name it, and without a trigger it is unreachable.
	SimulateCompensationFailure bool `json:"simulate_compensation_failure"`

	// SimulateFulfilmentFailure fails the step AFTER the charge, which is the
	// case the saga exists for and the one a single failure flag cannot show:
	// the charge completed, so the refund is a real unwind rather than an
	// unwind of nothing. Without a second trigger, the only reachable failure
	// is the first spending step, where compensation does the least.
	SimulateFulfilmentFailure bool `json:"simulate_fulfilment_failure"`
}

type OrderResult struct {
	OrderID    string `json:"order_id"`
	TotalCents int    `json:"total_cents"`
	Status     string `json:"status"`
	Steps      int    `json:"steps_completed"`
}

// ---- Entry point ----

// PlaceOrder runs one order to completion, or unwinds it.
//
// The result is a JSON string rather than a struct: a WASM entry point hands
// back bytes, and string is the one shape every language SDK expresses
// identically (IMPROVEMENT-PLAN 3.228).
//
// THE INPUT IS A STRING, NOT AN OrderInput, and the binding rule is why
// (`wasm/exports.go`; cleat#824):
//
//   - An entry point with exactly ONE `string` parameter receives the WHOLE
//     input JSON verbatim. That is the shape for a workflow whose input is an
//     opaque payload, which an order is here.
//   - Every OTHER shape binds by Go parameter NAME -- a parameter called
//     `input` binds the key `"input"` of the input object. So `input
//     OrderInput` would require a caller to start a run with
//     `{"input": {"input": {…}}}`, the outer object being the request body's
//     `input` field and the inner one the key the parameter binds.
//
// The second form is the one to arrive at by accident, and it is worth knowing
// what it looks like, because the failure does not name the binding: the run
// dies at the export with `unmarshal input: unexpected end of JSON input` and
// no line of this function ever runs. Measured against a real worker, having
// written the struct parameter first.
//
// Three other examples declare a struct entry-point parameter
// (`examples/dag`, `examples/fooddash`, `examples/saga-temporal-port`). They
// are correct under the same rule; none of them is deployed by anything, so the
// wrapping it requires has never been exercised.
//
// Taking the string keeps the typed struct everywhere it is useful -- every
// helper below still takes an OrderInput -- and puts the wire format somewhere
// a reader can see it.
func PlaceOrder(h cleat.HostCalls, input string) (string, error) {
	var in OrderInput
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return "", fmt.Errorf("decode order input: %w", err)
	}

	if len(in.Items) == 0 {
		return "", fmt.Errorf("order %s: must contain at least one item", in.OrderID)
	}
	if in.OrderID == "" {
		return "", fmt.Errorf("order_id is required")
	}

	total := orderTotal(in.Items)

	// Published before anything can fail, so a UI polling this order has
	// something to show from the first moment rather than 404-ing.
	h.SetQueryState("order_id", in.OrderID)
	h.SetQueryState("status", "validated")
	h.SetQueryState("total_cents", fmt.Sprintf("%d", total))

	// ---- Human approval, above a threshold ----
	//
	// A signal rather than a step in the saga: the order is not unwinding
	// anything at this point, it is waiting for a decision, and an order that
	// is never approved should leave no compensation trail — it never spent
	// anything.
	if total > ApprovalThresholdCents {
		h.SetQueryState("status", "awaiting_approval")
		h.Log("order awaiting approval", "order_id", in.OrderID, "total_cents", total)

		sr := h.AwaitSignals([]string{"order_approved", "order_rejected"}, ApprovalTimeout)
		if sr.Err != nil {
			return "", fmt.Errorf("order %s: approval signal: %w", in.OrderID, sr.Err)
		}
		if sr.TimedOut {
			h.SetQueryState("status", "rejected")
			h.SetQueryState("rejection_reason", "no approval within the window")
			return "", fmt.Errorf("order %s: no approval within %s", in.OrderID, ApprovalTimeout)
		}
		if sr.Name == "order_rejected" {
			h.SetQueryState("status", "rejected")
			h.SetQueryState("rejection_reason", reasonOr(sr.Payload, "rejected without a reason"))
			return "", fmt.Errorf("order %s: rejected", in.OrderID)
		}

		// The decision is in and the order is no longer waiting on one, so this
		// has to be published. It is app state -- the saga cannot know the order
		// was gated -- and without it the published status stays
		// "awaiting_approval" for the WHOLE saga, because the saga reports its
		// progress as current_step and only writes status again at the end.
		// Two things then read wrong: any poller sees an order that is being
		// charged described as still awaiting a decision, and the demo UI, which
		// offers the Approve/Reject controls whenever it sees that value, leaves
		// them on screen for an order already approved. On develop the saga's
		// own status writes ("charging", "dispatching") moved it on; cleat#2627
		// removed those, so the app has to say so itself.
		h.SetQueryState("status", "approved")
	}

	// ---- The saga ----
	//
	// NO BOOKKEEPING HERE. The three lists this block used to maintain --
	// completed, unwound, unwindFailed -- are now what Saga.RunWithResult
	// returns and publishes itself, so the per-step appends and the step-boundary
	// SetQueryState calls are gone (cleat#2627). See SagaResult's doc comment
	// for why they are three lists rather than two, and why "ran" is not
	// "succeeded": that reasoning did not change, it just moved to where the
	// mechanism that needs it lives.
	//
	// What is left here is the app-specific state only: the order id, the
	// total, the human-approval branch, and notify_failed.

	s := cleat.NewSaga()

	s.AddStep("reserve_inventory",
		func(h cleat.HostCalls) (string, error) {
			return "", reserveInventory(in)
		},
		func(h cleat.HostCalls) error {
			// Returned, not only recorded: the saga joins compensation errors and
			// the run has to know the unwind did not complete. Dropping it here
			// would make a failed release indistinguishable from a successful one.
			// Which list the step lands in is the saga's own bookkeeping now --
			// see SagaResult.
			return releaseInventory(in)
		},
	)

	s.AddStep("charge_psp",
		func(h cleat.HostCalls) (string, error) {
			return "", chargePSP(in, total)
		},
		func(h cleat.HostCalls) error {
			return refundPSP(in.OrderID, total)
		},
	)

	// No compensation, and that is a decision rather than an omission: the
	// webhook has already been CONSUMED by the time this returns, so there is
	// nothing to hand back. Undoing the charge is what unwinds the payment, and
	// that is the previous step's job.
	s.AddStep("await_payment_confirmation",
		func(h cleat.HostCalls) (string, error) {
			return "", awaitPaymentConfirmation(in.SourceID, in.OrderID)
		},
		nil,
	)

	s.AddStep("dispatch_fulfilment",
		func(h cleat.HostCalls) (string, error) {
			return "", dispatchFulfilment(in)
		},
		func(h cleat.HostCalls) error {
			return cancelDispatch(in.OrderID)
		},
	)

	// BEST-EFFORT, AND THAT IS A DECISION THE ENGINE FORCES.
	//
	// This step fails on a stock worker: `email-notify` is bundled but reports
	// "plugin not configured, disabled" unless the deployment sets
	// `email_enabled` in --plugin-config AND supplies a provider key -- it builds
	// SendGrid mail. So a workflow that treats the notification as a saga step
	// unwinds a charged, dispatched, settled order because an email did not go
	// out, which is exactly the wrong trade.
	//
	// Measured, not assumed: this step was written to propagate, and run 2 of the
	// scenario failed at it with four completed steps and nothing wrong with the
	// order. A notification is not a spending step; it is the last thing that
	// happens to an order rather than part of whether the order happened.
	//
	// So the failure is recorded and the order stands. `notify_failed` is
	// published rather than the error being swallowed, because "the customer was
	// not told" is something an operator may need to act on.
	s.AddStep("notify_customer",
		func(h cleat.HostCalls) (string, error) {
			if err := notifyCustomer(in, total); err != nil {
				// No err.Error(): a call through an interface is unresolvable
				// dispatch to the analyzer (E008), and `error` is an interface.
				h.Log("order placed, but the notification could not be sent",
					"order_id", in.OrderID)
				h.SetQueryState("notify_failed", "true")
			}
			// Returning nil either way is the whole of "best-effort": the step is
			// COMPLETED as far as the saga is concerned, so it is never compensated.
			return "", nil
		},
		nil,
	)

	res, err := s.RunWithResult(h)
	if err != nil {
		// The saga has already run the compensations, in reverse, for exactly
		// the steps that completed, and has published the READ itself: which
		// steps were there to unwind, newest first -- the order they were undone
		// in. There is nothing to publish here; this is the app-specific log.
		//
		// No err.Error() here, deliberately: the analyzer rejects a call through
		// an interface as unresolvable dispatch (E008), and `error` is an
		// interface. The text is not lost -- it is in the error returned below,
		// which the engine records against the run.
		h.Log("order failed",
			"order_id", in.OrderID,
			"unwound", strings.Join(res.Unwound, ","),
			"unwind_failed", strings.Join(res.UnwindFailed, ","),
		)

		// THE TRAIL IS IN THE ERROR AS WELL AS THE QUERY STATE.
		//
		// That used to be a necessity rather than belt-and-braces, and the
		// comment here said so: a run ending `failed` had its query state
		// DISCARDED, because `writeTerminalFailure` passed nil for it while the
		// success path handed the harvested map to FinalizeWorkflowSegment. So
		// for exactly the runs that compensate, the state a UI would poll did
		// not exist, and the run's `error` field was the only surface left.
		//
		// cleat#2520 fixed that: writeTerminalFailure now takes the harvested
		// map, so a failed run's published state survives and a poller sees the
		// unwind trail in the same place it sees everything else. The error
		// still carries it because it is genuinely useful there -- an operator
		// reading one failed run should not have to go and query for it -- not
		// because the published copy is missing.
		return "", fmt.Errorf("order %s not completed (unwound: [%s]; could not unwind: [%s]): %w",
			in.OrderID,
			strings.Join(res.Unwound, " "),
			strings.Join(res.UnwindFailed, " "),
			err)
	}

	// status=done and the cleared compensation fields are published by the saga
	// itself -- see RunWithResult's doc comment for the full contract.
	h.Log("order complete", "order_id", in.OrderID, "total_cents", total)

	out, err := json.Marshal(OrderResult{
		OrderID:    in.OrderID,
		TotalCents: total,
		Status:     "done",
		Steps:      len(res.Completed),
	})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ---- Steps: the cleat side is the orchestration; the rope side is a placeholder ----
//
// THE THREE PLACEHOLDERS, and why they are placeholders rather than calls.
//
// Your PSP, your inventory system and your 3PL are reached from a workflow by
// a durable call that resolves BY NAME to a plugin (`engine/app.go`), and this
// scenario ships none — it is one example directory, not a plugin set. The
// cleat SDK's generic outbound-HTTP call, `DurableCall("http", "fetch", …)`, is
// **embedder-only** and cannot serve a stock `cleat-worker` (ABI.md 2.48). So
// the honest shape is what `cmd/cleat/templates/fullstack` already does for the
// same reason: a recorded `DurableSleep` standing where the round trip goes.
//
// Each placeholder is:
//
//   - **recorded**, via DurableSleep, so it replays deterministically and the
//     step is durable in the way that matters — the workflow resumes past it
//     rather than repeating it;
//   - **the seam**, marked so that "write your Stripe call here" is a location
//     rather than an instruction;
//   - **able to fail**, so the compensating path is reachable.
//
// A step that cannot fail cannot demonstrate a saga. That is why
// SimulatePaymentFailure exists on the input rather than being a test-only
// branch: the browser needs to reach the unwinding path as well as the happy
// one, or the example only shows the half that never needed durability.

func reserveInventory(input OrderInput) error {
	h.DurableSleepMs(placeholderRoundTripMs)
	return nil
}

func releaseInventory(input OrderInput) error {
	h.DurableSleepMs(placeholderRoundTripMs)
	if input.SimulateCompensationFailure {
		return fmt.Errorf("placeholder inventory will not release order %s", input.OrderID)
	}
	return nil
}

// chargePSP is the seam where your PSP goes, and the one step whose failure the
// example has to be able to cause on purpose.
//
// THE TWO THINGS IT IS NOT, both of which a reader will otherwise assume:
//
//   - It is not idempotent, and it cannot be made so here. A real charge needs
//     an idempotency key passed DOWNSTREAM, to the PSP — a different thing from
//     the key on the start path. Cleat's key stops a duplicate ORDER; the PSP's
//     key stops a duplicate CHARGE when this step is retried after a timeout.
//     A PSP that never saw the first request cannot deduplicate it for you, and
//     that is the failure this placeholder cannot show.
//   - It is not a durable call, because a durable call here would have to
//     resolve to a plugin this example does not ship. See the note above.
func chargePSP(input OrderInput, totalCents int) error {
	h.DurableSleepMs(placeholderRoundTripMs)
	if input.SimulatePaymentFailure {
		return fmt.Errorf("placeholder PSP declined %d cents for order %s", totalCents, input.OrderID)
	}
	return nil
}

// refundPSP is the charge's compensation. It sleeps for the same reason the
// charge does, and it is the step that runs when a LATER step fails — which is
// the whole of what a saga buys you.
func refundPSP(orderID string, totalCents int) error {
	h.DurableSleepMs(placeholderRoundTripMs)
	return nil
}

// webhookEventType is the event_type handleIngestWebhook assigns a PSP
// callback that sets neither X-Github-Event nor X-Event-Type -- which the
// README's curl example does not, so every payment webhook this workflow
// receives carries this value. It has to agree with webhookingest's own
// default (plugins/webhookingest/routes.go's defaultWebhookEventType) for
// AwaitSignals below to ever see the signal a matching webhook fires: the
// claim eventtriggers.ClaimOrRegisterAwaiter performs is keyed on an EXACT
// event_type, not "whatever came in".
const webhookEventType = "webhook"

// awaitPaymentConfirmation waits for the PSP's webhook as a STEP.
//
// This is the shape worth copying: the workflow does not register a handler and
// the handler does not look up an order. The ingress route verifies the HMAC,
// stores the event, and this call — which is already parked on the right order
// — picks it up. Correlation is the engine's.
//
// A REAL SUSPEND, NOT A POLL -- cleat#2649. Before this, a found:false
// result from await_webhook was a real answer, not an error (its own doc
// said so), and "the workflow engine will retry according to its retry
// policy" was not true in practice: a call that SUCCEEDS is never retried,
// whatever it returned, so the wait had to be a loop over DurableSleep --
// 30 iterations, a full second apart, burning a durable-call round trip on
// every attempt regardless of whether anything happened. await_webhook now
// claims through the same key-slot correlation await_event uses
// (eventtriggers.ClaimOrRegisterAwaiter): a not-found result registers this
// workflow as an awaiter for (sourceID, orderID), and the publish handler
// signals it directly the moment a matching webhook arrives
// (plugins/eventtriggers/publish.go's signalAwaiters, "__evt:"+event_type) --
// so the loop below is a genuine suspend, no worker held, woken by the one
// event that matches rather than by the next tick of a timer -- it is a
// bounded loop of suspends, not a poll, because a wake is not proof of a
// claimable event (see the loop's own comment) and the ordinary case exits
// on the first iteration.
func awaitPaymentConfirmation(sourceID, orderID string) error {
	if sourceID == "" {
		return fmt.Errorf("no webhook source configured; see the README's setup step")
	}

	// Keys: []string{orderID} -- key1 is always this source's own id
	// (handleIngestWebhook adds it automatically), so this is key2: the
	// value the README's source setup step tells the PSP to echo back as
	// "order_id" in its webhook body, which handleIngestWebhook extracts via
	// the source's configured correlation_key_field. Without it, this call
	// would correlate on sourceID alone -- fine for a source used by exactly
	// one order at a time, wrong the moment two orders share it, which is
	// the ordinary case this README's source is deliberately set up to show.
	got, err := webhookingest.AwaitWebhook.Call(h, webhookingest.AwaitWebhookInput{
		SourceID: sourceID,
		Keys:     []string{orderID},
	})
	if err != nil {
		return err
	}
	if got.Found {
		return nil
	}

	// Not found: the call above already registered this workflow as an
	// awaiter for exactly (sourceID, orderID). Suspend on the signal that
	// registration wakes, rather than polling.
	//
	// A WAKE IS NOT PROOF OF A CLAIMABLE EVENT -- cleat-review's finding on
	// this PR. A publish's INSERT and its signalAwaiters call are two
	// separate steps (plugins/eventtriggers/publish.go), so a signal can
	// arrive for an event a DIFFERENT, earlier wake of this same awaiter (or
	// eventtriggers.ClaimOrRegisterAwaiter's own internal re-check, which
	// closes the race this signal exists to cover -- see claim.go) already
	// claimed. Treating one wake as authoritative and erroring when the
	// re-check finds nothing turns an ordinary spurious wakeup -- the same
	// hazard any condition variable has -- into a false failure. So this
	// loops on the CONDITION (found a claimable event) rather than the
	// EVENT (a signal arrived), across a fixed number of attempts rather
	// than tracking wall-clock time: plain Go code between host calls is
	// replayed, so a real time.Now() read here would not be.
	const attempts = 5
	const perAttemptWait = 6 * time.Second // attempts * perAttemptWait = 30s total, same budget as before this PR.

	for i := 0; i < attempts; i++ {
		h.AwaitSignals([]string{"__evt:" + webhookEventType}, perAttemptWait)

		// The signal only wakes the workflow; it is not the claim. Re-call
		// so the atomic claim/mark-consumed still happens exactly once,
		// through the same mechanism, regardless of how many awaiters a
		// given publish woke, or whether this particular wake was spurious.
		got, err = webhookingest.AwaitWebhook.Call(h, webhookingest.AwaitWebhookInput{
			SourceID: sourceID,
			Keys:     []string{orderID},
		})
		if err != nil {
			return err
		}
		if got.Found {
			return nil
		}
	}
	return fmt.Errorf("no payment confirmation from source %s within %s",
		sourceID, attempts*perAttemptWait)
}

func dispatchFulfilment(input OrderInput) error {
	h.DurableSleepMs(placeholderRoundTripMs)
	if input.SimulateFulfilmentFailure {
		return fmt.Errorf("placeholder 3PL cannot dispatch order %s", input.OrderID)
	}
	return nil
}

func cancelDispatch(orderID string) error {
	h.DurableSleepMs(placeholderRoundTripMs)
	return nil
}

// notifyCustomer goes to a plugin rather than a service, because email is a
// first-class cleat surface with its own delivery tracking — send returns a
// message id that check_status can be asked about later.
func notifyCustomer(input OrderInput, totalCents int) error {
	if input.Email == "" {
		return nil
	}
	// The generated SendInput has no "body" field -- only
	// BodyHTML and BodyText -- which is what surfaced this: the hand-rolled
	// JSON this replaced sent "body", a key plugins/email's SendInput
	// (host_functions.go) has never had. json.Unmarshal silently drops an
	// unknown field, so BodyHTML stayed empty and every real send failed
	// with "email: body_html is required" -- the exact kind of manifest/
	// reality drift cleat#2626 exists to make a compile error instead.
	_, err := email.Send.Call(h, email.SendInput{
		To:      input.Email,
		Subject: fmt.Sprintf("Order %s confirmed", input.OrderID),
		BodyHTML: fmt.Sprintf(
			"Your order %s has been confirmed. Total: %s.\n",
			input.OrderID, formatCents(totalCents)),
	})
	return err
}

// ---- Helpers ----

func orderTotal(items []OrderItem) int {
	total := 0
	for _, it := range items {
		total += it.PriceCents * it.Quantity
	}
	return total
}

func reasonOr(payload, fallback string) string {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return fallback
	}
	return payload
}

func formatCents(c int) string {
	return fmt.Sprintf("%d.%02d", c/100, c%100)
}
