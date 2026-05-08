// Package saga is a cleat port of the Temporal samples-go saga (money transfer)
// example. It validates completeness of cleat's Go SDK for sagas.
package saga

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/rcownie/cleat/cleat"
)

// TransferMoneyTaskQueue is included for compatibility with the original
// Temporal example. In cleat, the entry-point function name serves the
// same routing purpose.
const TransferMoneyTaskQueue = "TRANSFER_MONEY_TASK_QUEUE"

// TransferDetails describes a money transfer between two accounts.
type TransferDetails struct {
	Amount      float32 `json:"amount"`
	FromAccount string  `json:"from_account"`
	ToAccount   string  `json:"to_account"`
	ReferenceID string  `json:"reference_id"`
}

// transferCallOptions configures the retry policy for all transfer
// activities, matching the original Temporal example's RetryPolicy.
var transferCallOptions = cleat.CallOptions{
	Retry: &cleat.RetryPolicy{
		InitialInterval:    1 * time.Second,
		BackoffCoefficient: 2.0,
		MaxInterval:        1 * time.Minute,
		MaxAttempts:        3,
	},
}

// TransferMoney orchestrates a money transfer with Saga-based
// compensation. Steps execute sequentially; if any step fails,
// previously completed steps are compensated in reverse order.
func TransferMoney(h cleat.HostCalls, details TransferDetails) error {
	s := cleat.NewSaga()

	// Step 1: Withdraw from the source account.
	// Compensation: reverse the withdrawal.
	s.AddStep("withdraw",
		func(h cleat.HostCalls) (string, error) {
			return "", callWithRetry(h, "Withdraw", details)
		},
		func(h cleat.HostCalls) error {
			_ = h.DurableCallTyped("banking", "WithdrawCompensation", details, nil)
			return nil
		},
	)

	// Step 2: Deposit into the destination account.
	// Compensation: reverse the deposit.
	s.AddStep("deposit",
		func(h cleat.HostCalls) (string, error) {
			return "", callWithRetry(h, "Deposit", details)
		},
		func(h cleat.HostCalls) error {
			_ = h.DurableCallTyped("banking", "DepositCompensation", details, nil)
			return nil
		},
	)

	// Step 3: Unreliable step that triggers compensation for testing.
	// No compensation needed since this step always fails by design.
	s.AddStep("step_with_error",
		func(h cleat.HostCalls) (string, error) {
			return "", callWithRetry(h, "StepWithError", details)
		},
		nil, // no compensation
	)

	return s.Run(h)
}

// callWithRetry makes a typed durable call with the configured retry
// policy. Note: there is no DurableCallTypedWithOptions in the current
// cleat API, so we manually marshal to JSON and use DurableCallWithOptions.
func callWithRetry(h cleat.HostCalls, operation string, details TransferDetails) error {
	reqJSON, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("marshaling request for banking.%s: %w", operation, err)
	}
	_, err = h.DurableCallWithOptions(transferCallOptions, "banking", operation, string(reqJSON))
	return err
}
