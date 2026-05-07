// Host runtime for durable WASM workflows — SDK-integrated version.
//
// This demonstrates the same lifecycle as main.go but uses the
// cleat.HostCalls interface from the SDK instead of a raw *host type:
//  1. First execution with the host intercepting API calls
//  2. Mid-execution crash (simulated at the host level)
//  3. Resume from checkpoint via replay
//  4. Full replay from cold (all calls cached)
//  5. Fencing — second worker claims the workflow

package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/rcownie/cleat/cleat"
)

// ---- Durable host state ----

type stepRecord struct {
	Step     int    `json:"step"`
	Service  string `json:"service"`
	Op       string `json:"op"`
	Request  string `json:"request"`
	Response string `json:"response"`
	Err      string `json:"err,omitempty"`
}

type workflowState struct {
	WorkflowID string       `json:"workflow_id"`
	Input      string       `json:"input"`
	Steps      []stepRecord `json:"steps"`
	Complete   bool         `json:"complete"`
	FinalVal   string       `json:"final_val,omitempty"`
	FinalErr   string       `json:"final_err,omitempty"`
}

// durableHost wraps a workflowState and provides a cleat.HostCalls
// that records calls, replays from cache, and handles crashes/fencing.
type durableHost struct {
	state     *workflowState
	stepCount int
	isReplay  bool
	crashAt   int
	fenced    bool
	epoch     int64
	now       time.Time
}

func newDurableHost(workflowID string) *durableHost {
	return &durableHost{
		state: &workflowState{
			WorkflowID: workflowID,
			Steps:      []stepRecord{},
		},
		epoch: 1,
		now:   time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func newDurableHostFromCheckpoint(state *workflowState) *durableHost {
	return &durableHost{
		state:    state,
		isReplay: true,
		now:      time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// H returns a cleat.HostCalls backed by this host.
func (dh *durableHost) H() cleat.HostCalls {
	return cleat.NewHostCalls(cleat.HostCallsOptions{
		DurableCall: func(service, operation, requestJSON string) (string, error) {
			if dh.fenced {
				return "", fmt.Errorf("worker fenced — another worker claimed this workflow")
			}
			if dh.isReplay {
				if dh.stepCount >= len(dh.state.Steps) {
					dh.isReplay = false
					return dh.durableFirstExecution(service, operation, requestJSON)
				}
				rec := dh.state.Steps[dh.stepCount]
				if rec.Service != service || rec.Op != operation {
					return "", fmt.Errorf(
						"REPLAY DIVERGENCE at step %d: workflow called %s.%s, but history has %s.%s — workflow code is NOT deterministic",
						dh.stepCount, service, operation, rec.Service, rec.Op)
				}
				fmt.Printf("    (replay) %s.%s -> cached\n", service, operation)
				dh.stepCount++
				if rec.Err != "" {
					return "", fmt.Errorf(rec.Err)
				}
				return rec.Response, nil
			}
			return dh.durableFirstExecution(service, operation, requestJSON)
		},

		DurableSleep: func(ms int64) {
			fmt.Printf("    sleep %dms\n", ms)
			dh.now = dh.now.Add(time.Duration(ms) * time.Millisecond)
		},

		DurableAwaitSignals: func(signalNames []string, timeoutMs int64) (string, string, bool, error) {
			// Demo: always time out after the timeout.
			dh.now = dh.now.Add(time.Duration(timeoutMs) * time.Millisecond)
			return "", "", true, nil
		},

		DurableLog: func(message string) {
			fmt.Printf("    [log] %s\n", truncate(message, 120))
		},

		Now: func() int64 {
			return dh.now.UnixMilli()
		},

		Version: func() int {
			return 1
		},

		MinVersion: func() int {
			return 1
		},

		PollCancellation: func() (bool, string) {
			return false, ""
		},

		PollSignal: func(signalName string) (string, bool, error) {
			return "", false, nil
		},

		ContinueAsNew: func(newInputJSON string) error {
			fmt.Printf("    continue-as-new: %s\n", truncate(newInputJSON, 80))
			return nil
		},

		ChildWorkflow: func(name, inputJSON string) (string, error) {
			return "child_run_1", nil
		},

		AwaitChild: func(runID string) (string, error) {
			return `{"status":"completed"}`, nil
		},

		DurableDefer: func(description string) (string, error) {
			return "defer_1", nil
		},

		SetQueryState: func(key, value string) {
			fmt.Printf("    query_state: %s = %s\n", key, value)
		},

		Random: func() int64 {
			return 42
		},
	})
}

func (dh *durableHost) durableFirstExecution(service, op, requestJSON string) (string, error) {
	if dh.crashAt > 0 && dh.stepCount >= dh.crashAt {
		fmt.Printf("    SIMULATED CRASH before %s.%s\n", service, op)
		return "", fmt.Errorf("host lost power at step %d", dh.stepCount)
	}

	fmt.Printf("    %s.%s(%s)\n", service, op, truncate(requestJSON, 60))

	response, err := realAPICall(service, op, requestJSON)

	rec := stepRecord{
		Step:     dh.stepCount,
		Service:  service,
		Op:       op,
		Request:  requestJSON,
		Response: response,
	}
	if err != nil {
		rec.Err = err.Error()
	}
	dh.state.Steps = append(dh.state.Steps, rec)
	dh.stepCount++

	return response, err
}

func (dh *durableHost) Checkpoint() []byte {
	data, _ := json.MarshalIndent(dh.state, "", "  ")
	return data
}

func (dh *durableHost) Fence() {
	dh.fenced = true
}

func realAPICall(service, op, requestJSON string) (string, error) {
	time.Sleep(50 * time.Millisecond)

	switch service + "." + op {
	case "catalog.LookupItem":
		return `{"sku":"ABC-123","name":"Widget","price_cents":999,"found":true}`, nil
	case "inventory.Reserve":
		return `{"reservation_id":"resv_abc123","status":"reserved","total_cents":3299}`, nil
	case "inventory.Release":
		return `{"status":"released"}`, nil
	case "payments.GetDefaultMethod":
		return `{"token":"pm_tok_555","type":"card","last_four":"4242"}`, nil
	case "payments.Charge":
		return `{"charge_id":"chg_xyz789","status":"captured"}`, nil
	case "payments.Refund":
		return `{"status":"refunded"}`, nil
	case "shipping.CreateShipment":
		return `{"tracking_id":"TRACK-123456","status":"label_created"}`, nil
	case "notifications.SendEmail":
		return `{"status":"sent"}`, nil
	default:
		return `{}`, nil
	}
}

// ---- The workflow — same logic, now using cleat.HostCalls ----

type cartItem struct {
	SKU      string
	Quantity int
}

// runWorkflowSDK is the durable workflow using the SDK interface.
// All external calls go through h.DurableCall / h.DurableCallTyped.
func runWorkflowSDK(h cleat.HostCalls, userID string, cart []cartItem) (string, error) {
	if len(cart) == 0 {
		return "", fmt.Errorf("cart is empty")
	}

	// Check each item's catalog availability.
	for _, item := range cart {
		type lookupReq struct {
			SKU string `json:"sku"`
		}
		var result struct {
			SKU       string `json:"sku"`
			Name      string `json:"name"`
			PriceCents int   `json:"price_cents"`
			Found     bool   `json:"found"`
		}
		if err := h.DurableCallTyped("catalog", "LookupItem", lookupReq{SKU: item.SKU}, &result); err != nil {
			return "", fmt.Errorf("item %s unavailable: %w", item.SKU, err)
		}
	}

	// Reserve inventory.
	type reserveReq struct {
		UserID string       `json:"user_id"`
		Items  []cartItem   `json:"items"`
	}
	_, err := h.DurableCall("inventory", "Reserve",
		mustMarshal(reserveReq{UserID: userID, Items: cart}))
	if err != nil {
		return "", fmt.Errorf("reservation failed: %w", err)
	}

	// Get payment method.
	type pmReq struct {
		UserID string `json:"user_id"`
	}
	_, err = h.DurableCall("payments", "GetDefaultMethod",
		mustMarshal(pmReq{UserID: userID}))
	if err != nil {
		h.DurableCall("inventory", "Release", `{"reservation_id":"resv_abc123"}`)
		return "", fmt.Errorf("payment method lookup failed: %w", err)
	}

	// Charge customer.
	type chargeReq struct {
		Token       string `json:"token"`
		AmountCents int    `json:"amount_cents"`
	}
	_, err = h.DurableCall("payments", "Charge",
		mustMarshal(chargeReq{Token: "pm_tok_555", AmountCents: 3299}))
	if err != nil {
		h.DurableCall("inventory", "Release", `{"reservation_id":"resv_abc123"}`)
		return "", fmt.Errorf("payment failed: %w", err)
	}

	// Create shipment.
	type shipReq struct {
		ReservationID string `json:"reservation_id"`
		Address       string `json:"address"`
		ChargeID      string `json:"charge_id"`
	}
	_, err = h.DurableCall("shipping", "CreateShipment",
		mustMarshal(shipReq{
			ReservationID: "resv_abc123",
			Address:       "123 Main St",
			ChargeID:      "chg_xyz789",
		}))
	if err != nil {
		h.DurableCall("payments", "Refund", `{"charge_id":"chg_xyz789"}`)
		h.DurableCall("inventory", "Release", `{"reservation_id":"resv_abc123"}`)
		return "", fmt.Errorf("shipping failed: %w", err)
	}

	// Notify customer (best effort).
	type notifyReq struct {
		UserID     string `json:"user_id"`
		TrackingID string `json:"tracking_id"`
	}
	h.DurableCall("notifications", "SendEmail",
		mustMarshal(notifyReq{UserID: userID, TrackingID: "TRACK-123456"}))

	return "TRACK-123456", nil
}

func mustMarshal(v interface{}) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("json.Marshal: %v", err))
	}
	return string(data)
}

func copyCheckpoint(state *workflowState) *workflowState {
	steps := make([]stepRecord, len(state.Steps))
	copy(steps, state.Steps)
	return &workflowState{
		WorkflowID: state.WorkflowID,
		Input:      state.Input,
		Steps:      steps,
		Complete:   state.Complete,
		FinalVal:   state.FinalVal,
		FinalErr:   state.FinalErr,
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
