// Subscription billing — recurring payment workflow with retry and grace period.
//
// Demonstrates:
//   - DurableCallWithRetry for payment retries with exponential backoff
//   - DurableSleep for monthly billing cycle and grace periods
//   - PollCancellation to handle cancellation signals mid-workflow
//   - ContinueAsNew for clean monthly recurrence
//   - SetQueryState for external subscription status queries
//
// Build:
//
//	durable build -o /tmp/out ./examples/subscription/
package subscription

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/rcownie/durable/durable"
)

var h durable.HostCalls

// ---- Domain types ----

type SubscriptionInput struct {
	UserID    string `json:"user_id"`
	PlanID    string `json:"plan_id"`
	AmountUSD int    `json:"amount_usd"`
}

type ChargeResult struct {
	ChargeID   string `json:"charge_id"`
	Success    bool   `json:"success"`
	DeclineReason string `json:"decline_reason,omitempty"`
}

// ---- Entry point ----

func ManageSubscription(h durable.HostCalls, input SubscriptionInput) (string, error) {
	if input.UserID == "" || input.PlanID == "" {
		return "", fmt.Errorf("user_id and plan_id are required")
	}

	// --- First charge (with retry) ---
	h.SetQueryState("status", "active")
	h.SetQueryState("plan", input.PlanID)

	if err := chargeWithRetry(h, input); err != nil {
		// Payment failed after retries — enter grace period.
		return enterGracePeriod(h, input)
	}

	h.DurableLog(fmt.Sprintf("Subscription active: user=%s plan=%s", input.UserID, input.PlanID))

	// --- Monthly billing loop ---
	// Wait one month, then continue as new to keep event history bounded.
	h.DurableSleep(30 * 24 * time.Hour)

	// ContinueAsNew restarts with fresh history, carrying forward the input.
	if err := h.ContinueAsNew(toJSON(input)); err != nil {
		return "", fmt.Errorf("continue_as_new failed: %w", err)
	}
	return "", nil // unreachable — ContinueAsNew panics-suspends
}

// chargeWithRetry attempts payment with server-side retry.
func chargeWithRetry(h durable.HostCalls, input SubscriptionInput) error {
	req := toJSON(map[string]interface{}{
		"user_id":    input.UserID,
		"plan_id":    input.PlanID,
		"amount_usd": input.AmountUSD,
	})

	resp, err := h.DurableCallWithRetry(durable.CallOptions{
		RetryPolicy: &durable.RetryPolicy{
			MaxAttempts:        4,
			InitialInterval:    1 * time.Second,
			BackoffCoefficient: 2.0,
			MaxInterval:        30 * time.Second,
		},
	}, "billing", "Charge", req)

	if err != nil {
		return fmt.Errorf("charge failed after retries: %w", err)
	}

	var cr ChargeResult
	if err := json.Unmarshal([]byte(resp), &cr); err != nil {
		return fmt.Errorf("bad charge response: %w", err)
	}
	if !cr.Success {
		return fmt.Errorf("charge declined: %s", cr.DeclineReason)
	}

	// Record successful payment.
	h.SetQueryState("last_charge_id", cr.ChargeID)

	// Send invoice (best-effort, non-retryable).
	h.DurableCall("billing", "SendInvoice", toJSON(map[string]string{
		"user_id":   input.UserID,
		"charge_id": cr.ChargeID,
	}))
	return nil
}

// enterGracePeriod gives the user time to fix payment before canceling.
func enterGracePeriod(h durable.HostCalls, input SubscriptionInput) (string, error) {
	h.DurableLog(fmt.Sprintf("Entering grace period: user=%s", input.UserID))
	h.SetQueryState("status", "past_due")

	graceStart := h.Now()

	// Notify user to update payment method.
	h.DurableCall("notifications", "SendEmail", toJSON(map[string]string{
		"user_id":  input.UserID,
		"template": "payment_failed",
	}))

	// Wait 3 days, checking for cancellation daily.
	for i := 0; i < 3; i++ {
		h.DurableSleep(24 * time.Hour)
		if cancelled, reason := h.PollCancellation(); cancelled {
			h.SetQueryState("status", "canceled")
			h.SetQueryState("cancel_reason", reason)
			h.DurableLog(fmt.Sprintf("Subscription canceled during grace: %s", reason))
			h.DurableCall("billing", "DeactivateSubscription", toJSON(map[string]string{
				"user_id": input.UserID,
			}))
			h.DurableCall("notifications", "SendEmail", toJSON(map[string]string{
				"user_id":  input.UserID,
				"template": "subscription_canceled",
			}))
			return fmt.Sprintf("canceled: %s", reason), nil
		}

		// Retry charge each day.
		if err := chargeWithRetry(h, input); err == nil {
			h.SetQueryState("status", "active")
			h.SetQueryState("grace_exited_at", h.Now().String())
			h.DurableLog(fmt.Sprintf("Payment recovered after %.0fh in grace",
				float64(h.Now().Sub(graceStart).Hours())))

			h.DurableSleep(30*24*time.Hour - h.Now().Sub(graceStart))
			return h.ContinueAsNew(toJSON(input))
		}
	}

	// Grace period exhausted — cancel.
	h.SetQueryState("status", "canceled")
	h.DurableLog(fmt.Sprintf("Grace period exhausted, canceling: user=%s", input.UserID))
	h.DurableCall("billing", "DeactivateSubscription", toJSON(map[string]string{
		"user_id": input.UserID,
	}))
	h.DurableCall("notifications", "SendEmail", toJSON(map[string]string{
		"user_id":  input.UserID,
		"template": "subscription_expired",
	}))
	return "canceled: grace period exhausted", nil
}

func toJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
