package saga

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/cleat/cleattest"
)

func TestTransferMoney_Success(t *testing.T) {
	env := cleattest.NewTestEnv()

	// All three steps succeed.
	env.OnCall("banking", "Withdraw", nil).Return("", nil)
	env.OnCall("banking", "Deposit", nil).Return("", nil)
	env.OnCall("banking", "StepWithError", nil).Return("", nil)

	err := TransferMoney(env.H(), TransferDetails{
		Amount:      100.00,
		FromAccount: "checking",
		ToAccount:   "savings",
		ReferenceID: "ref-001",
	})
	if err != nil {
		t.Fatalf("TransferMoney failed: %v", err)
	}

	// All three forward steps were called.
	env.AssertCalled(t, "banking", "Withdraw")
	env.AssertCalled(t, "banking", "Deposit")
	env.AssertCalled(t, "banking", "StepWithError")

	// No compensation calls were made.
	env.AssertNotCalled(t, "banking", "WithdrawCompensation")
	env.AssertNotCalled(t, "banking", "DepositCompensation")
}

func TestTransferMoney_WithdrawFails(t *testing.T) {
	env := cleattest.NewTestEnv()

	// Withdraw fails immediately — no compensations needed.
	env.OnCall("banking", "Withdraw", nil).Return("", fmt.Errorf("insufficient funds"))

	err := TransferMoney(env.H(), TransferDetails{
		Amount:      100.00,
		FromAccount: "checking",
		ToAccount:   "savings",
		ReferenceID: "ref-001",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "insufficient funds") {
		t.Errorf("expected error containing 'insufficient funds', got: %v", err)
	}

	// Only the failed step was called.
	env.AssertCalled(t, "banking", "Withdraw")
	env.AssertNotCalled(t, "banking", "Deposit")
	env.AssertNotCalled(t, "banking", "StepWithError")
	env.AssertNotCalled(t, "banking", "WithdrawCompensation")
	env.AssertNotCalled(t, "banking", "DepositCompensation")
}

func TestTransferMoney_StepWithErrorFiresCompensations(t *testing.T) {
	env := cleattest.NewTestEnv()

	// Steps 1 and 2 succeed.
	env.OnCall("banking", "Withdraw", nil).Return("", nil)
	env.OnCall("banking", "Deposit", nil).Return("", nil)
	// Step 3 fails.
	env.OnCall("banking", "StepWithError", nil).Return("", fmt.Errorf("some error"))

	// Compensation stubs for the Saga's reverse-order compensation.
	env.OnCall("banking", "DepositCompensation", nil).Return("", nil)
	env.OnCall("banking", "WithdrawCompensation", nil).Return("", nil)

	err := TransferMoney(env.H(), TransferDetails{
		Amount:      100.00,
		FromAccount: "checking",
		ToAccount:   "savings",
		ReferenceID: "ref-001",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "some error") {
		t.Errorf("expected error containing 'some error', got: %v", err)
	}

	history := env.CallHistory()

	// We expect 5 calls in this order:
	//   Withdraw, Deposit, StepWithError, DepositCompensation, WithdrawCompensation
	if len(history) != 5 {
		t.Fatalf("expected 5 calls in history, got %d: %+v", len(history), history)
	}

	type callCheck struct {
		idx       int
		operation string
	}
	checks := []callCheck{
		{0, "Withdraw"},
		{1, "Deposit"},
		{2, "StepWithError"},
		{3, "DepositCompensation"},
		{4, "WithdrawCompensation"},
	}
	for _, c := range checks {
		if history[c.idx].Operation != c.operation {
			t.Errorf("call[%d].Operation = %q, want %q", c.idx, history[c.idx].Operation, c.operation)
		}
	}
}

// TestTransferMoney_CompensationErrorsContinue tests that compensation
// continues even if one compensation step fails. Note: the current Saga
// implementation ignores compensation errors (Compensate returns void),
// so this test verifies the existing behavior.
func TestTransferMoney_CompensationErrorsContinue(t *testing.T) {
	env := cleattest.NewTestEnv()

	env.OnCall("banking", "Withdraw", nil).Return("", nil)
	env.OnCall("banking", "Deposit", nil).Return("", nil)
	env.OnCall("banking", "StepWithError", nil).Return("", fmt.Errorf("some error"))

	// DepositCompensation fails, but WithdrawCompensation should still run.
	env.OnCall("banking", "DepositCompensation", nil).Return("", fmt.Errorf("compensation failed"))
	env.OnCall("banking", "WithdrawCompensation", nil).Return("", nil)

	err := TransferMoney(env.H(), TransferDetails{
		Amount:      100.00,
		FromAccount: "checking",
		ToAccount:   "savings",
		ReferenceID: "ref-001",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	history := env.CallHistory()
	if len(history) < 5 {
		t.Fatalf("expected at least 5 calls, got %d", len(history))
	}

	// Both compensations should have been attempted even though the
	// first one (DepositCompensation) failed. This verifies the Saga
	// continues compensating all completed steps in reverse order.
	env.AssertCalled(t, "banking", "Withdraw")
	env.AssertCalled(t, "banking", "Deposit")
	env.AssertCalled(t, "banking", "StepWithError")
	env.AssertCalled(t, "banking", "DepositCompensation")
	env.AssertCalled(t, "banking", "WithdrawCompensation")
}
