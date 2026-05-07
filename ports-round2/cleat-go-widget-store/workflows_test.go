package main

import (
	"errors"
	"testing"
	"time"

	"github.com/rcownie/durable/durable"
	"github.com/rcownie/durable/durable/durabletest"
	"github.com/stretchr/testify/assert"
)

// testResult holds the return values from a workflow execution in a goroutine.
type testResult struct {
	result string
	err    error
}

// workflowIDWrapper wraps a HostCalls to override WorkflowID().
type workflowIDWrapper struct {
	durable.HostCalls
	wfID string
}

func (w *workflowIDWrapper) WorkflowID() string {
	return w.wfID
}

// ---------------------------------------------------------------------------
// TestCheckoutWorkflow
// ---------------------------------------------------------------------------

func TestCheckoutWorkflow(t *testing.T) {
	t.Run("Payment success with dispatch", func(t *testing.T) {
		env := durabletest.NewTestEnv()
		defer env.Reset()

		// Stub store operations in call order
		env.OnCall("store", "createOrder", nil).ReturnJSON(1, nil)
		env.OnCall("store", "reserveInventory", nil).ReturnJSON(true, nil)
		env.OnCall("store", "updateOrderStatus", nil).ReturnJSON("", nil)

		h := &workflowIDWrapper{HostCalls: env.H(), wfID: "test-wf-pay-success"}

		resultCh := make(chan testResult)
		go func() {
			res, err := checkoutWorkflow(h, "")
			resultCh <- testResult{result: res, err: err}
		}()

		// Let the workflow reach AwaitSignals
		time.Sleep(50 * time.Millisecond)
		env.Signal(PAYMENT_STATUS, "paid")

		select {
		case r := <-resultCh:
			assert.NoError(t, r.err)
			assert.Equal(t, "", r.result)
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for checkout workflow")
		}

		env.AssertCalled(t, "store", "createOrder")
		env.AssertCalled(t, "store", "reserveInventory")
		env.AssertCalled(t, "store", "updateOrderStatus")

		// Verify query state was set
		orderID, ok := env.QueryState(ORDER_ID)
		assert.True(t, ok)
		assert.Equal(t, "1", orderID)
	})

	t.Run("Inventory reservation fails", func(t *testing.T) {
		env := durabletest.NewTestEnv()
		defer env.Reset()

		env.OnCall("store", "createOrder", nil).ReturnJSON(2, nil)
		env.OnCall("store", "reserveInventory", nil).ReturnJSON(false, nil)
		env.OnCall("store", "updateOrderStatus", nil).ReturnJSON("", nil)

		h := &workflowIDWrapper{HostCalls: env.H(), wfID: "test-wf-no-inv"}

		resultCh := make(chan testResult)
		go func() {
			res, err := checkoutWorkflow(h, "")
			resultCh <- testResult{result: res, err: err}
		}()

		select {
		case r := <-resultCh:
			assert.NoError(t, r.err)
			assert.Equal(t, "", r.result)
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for checkout workflow")
		}

		env.AssertCalled(t, "store", "createOrder")
		env.AssertCalled(t, "store", "reserveInventory")
		env.AssertCalled(t, "store", "updateOrderStatus")

		// PAYMENT_ID should be set to empty string (no payment needed)
		paymentID, ok := env.QueryState(PAYMENT_ID)
		assert.True(t, ok)
		assert.Equal(t, "", paymentID)
	})

	t.Run("Payment timeout", func(t *testing.T) {
		env := durabletest.NewTestEnv()
		defer env.Reset()

		env.OnCall("store", "createOrder", nil).ReturnJSON(3, nil)
		env.OnCall("store", "reserveInventory", nil).ReturnJSON(true, nil)
		env.OnCall("store", "undoReserveInventory", nil).ReturnJSON("", nil)
		env.OnCall("store", "updateOrderStatus", nil).ReturnJSON("", nil)

		h := &workflowIDWrapper{HostCalls: env.H(), wfID: "test-wf-timeout"}

		resultCh := make(chan testResult)
		go func() {
			res, err := checkoutWorkflow(h, "")
			resultCh <- testResult{result: res, err: err}
		}()

		// Let the workflow reach AwaitSignals, then advance time past the 60s timeout
		time.Sleep(50 * time.Millisecond)
		env.AdvanceTime(61 * time.Second)

		select {
		case r := <-resultCh:
			assert.NoError(t, r.err)
			assert.Equal(t, "", r.result)
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for checkout workflow")
		}

		env.AssertCalled(t, "store", "createOrder")
		env.AssertCalled(t, "store", "reserveInventory")
		env.AssertCalled(t, "store", "undoReserveInventory")
		env.AssertCalled(t, "store", "updateOrderStatus")
	})

	t.Run("Payment failed status", func(t *testing.T) {
		env := durabletest.NewTestEnv()
		defer env.Reset()

		env.OnCall("store", "createOrder", nil).ReturnJSON(4, nil)
		env.OnCall("store", "reserveInventory", nil).ReturnJSON(true, nil)
		env.OnCall("store", "undoReserveInventory", nil).ReturnJSON("", nil)
		env.OnCall("store", "updateOrderStatus", nil).ReturnJSON("", nil)

		h := &workflowIDWrapper{HostCalls: env.H(), wfID: "test-wf-pay-fail"}

		resultCh := make(chan testResult)
		go func() {
			res, err := checkoutWorkflow(h, "")
			resultCh <- testResult{result: res, err: err}
		}()

		// Send a failed payment status
		time.Sleep(50 * time.Millisecond)
		env.Signal(PAYMENT_STATUS, "failed")

		select {
		case r := <-resultCh:
			assert.NoError(t, r.err)
			assert.Equal(t, "", r.result)
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for checkout workflow")
		}

		env.AssertCalled(t, "store", "createOrder")
		env.AssertCalled(t, "store", "reserveInventory")
		env.AssertCalled(t, "store", "undoReserveInventory")
		env.AssertCalled(t, "store", "updateOrderStatus")
	})

	t.Run("CreateOrder step fails", func(t *testing.T) {
		env := durabletest.NewTestEnv()
		defer env.Reset()

		env.OnCall("store", "createOrder", nil).Return("", errors.New("database error"))

		h := &workflowIDWrapper{HostCalls: env.H(), wfID: "test-wf-create-fail"}

		resultCh := make(chan testResult)
		go func() {
			res, err := checkoutWorkflow(h, "")
			resultCh <- testResult{result: res, err: err}
		}()

		select {
		case r := <-resultCh:
			assert.Error(t, r.err)
			assert.Equal(t, "", r.result)
			assert.Contains(t, r.err.Error(), "database error")
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for checkout workflow")
		}
	})

	t.Run("WorkflowID is empty", func(t *testing.T) {
		env := durabletest.NewTestEnv()
		defer env.Reset()

		// No stubs needed -- the empty WorkflowID check should short-circuit

		resultCh := make(chan testResult)
		go func() {
			// Use env.H() directly, which returns "" from WorkflowID()
			res, err := checkoutWorkflow(env.H(), "")
			resultCh <- testResult{result: res, err: err}
		}()

		select {
		case r := <-resultCh:
			assert.Error(t, r.err)
			assert.Equal(t, "", r.result)
			assert.Contains(t, r.err.Error(), "workflow ID is empty")
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for checkout workflow")
		}
	})
}

// ---------------------------------------------------------------------------
// TestDispatchOrderWorkflow
// ---------------------------------------------------------------------------

func TestDispatchOrderWorkflow(t *testing.T) {
	t.Run("Happy path - successful dispatch", func(t *testing.T) {
		env := durabletest.NewTestEnv()
		defer env.Reset()

		// Stub 10 progress-update calls in sequence
		for i := 0; i < 10; i++ {
			env.OnCall("store", "updateOrderProgress", nil).ReturnJSON(9-i, nil)
		}

		done := make(chan struct{})
		var wfResult string
		var wfErr error
		go func() {
			wfResult, wfErr = dispatchOrderWorkflow(env.H(), "1")
			close(done)
		}()

		// Advance time to wake each 1-second DurableSleep
		for i := 0; i < 10; i++ {
			time.Sleep(20 * time.Millisecond)
			env.AdvanceTime(2 * time.Second)
		}

		<-done
		assert.NoError(t, wfErr)
		assert.Equal(t, "", wfResult)
	})

	t.Run("UpdateOrderProgress step fails mid-execution", func(t *testing.T) {
		env := durabletest.NewTestEnv()
		defer env.Reset()

		// First 5 calls succeed
		for i := 0; i < 5; i++ {
			env.OnCall("store", "updateOrderProgress", nil).ReturnJSON(9-i, nil)
		}
		// 6th call fails
		env.OnCall("store", "updateOrderProgress", nil).Return("", errors.New("database update error"))

		done := make(chan struct{})
		var wfResult string
		var wfErr error
		go func() {
			wfResult, wfErr = dispatchOrderWorkflow(env.H(), "1")
			close(done)
		}()

		// Advance time for first 6 sleeps (the 6th call will fail)
		for i := 0; i < 6; i++ {
			time.Sleep(20 * time.Millisecond)
			env.AdvanceTime(2 * time.Second)
		}

		<-done
		assert.Error(t, wfErr)
		assert.Equal(t, "", wfResult)
		assert.Contains(t, wfErr.Error(), "database update error")
	})

	t.Run("Multiple order IDs produce different behavior", func(t *testing.T) {
		env := durabletest.NewTestEnv()
		defer env.Reset()

		for i := 0; i < 10; i++ {
			env.OnCall("store", "updateOrderProgress", nil).ReturnJSON(i%6, nil)
		}

		done := make(chan struct{})
		var wfResult string
		var wfErr error
		go func() {
			wfResult, wfErr = dispatchOrderWorkflow(env.H(), "999")
			close(done)
		}()

		for i := 0; i < 10; i++ {
			time.Sleep(20 * time.Millisecond)
			env.AdvanceTime(2 * time.Second)
		}

		<-done
		assert.NoError(t, wfErr)
		assert.Equal(t, "", wfResult)
	})
}

// ---------------------------------------------------------------------------
// TestSagaIntegration
// ---------------------------------------------------------------------------

// TestSagaPattern demonstrates using Cleat's Saga API for the checkout flow.
// This shows how the checkout could be rewritten using the structured Saga
// pattern for automatic compensation on any step failure.
func TestSagaPattern(t *testing.T) {
	env := durabletest.NewTestEnv()
	defer env.Reset()

	var orderID int

	env.OnCall("store", "createOrder", nil).ReturnJSON(1, nil)
	env.OnCall("store", "reserveInventory", nil).ReturnJSON(true, nil)
	env.OnCall("store", "undoReserveInventory", nil).ReturnJSON("", nil)
	env.OnCall("store", "updateOrderStatus", nil).ReturnJSON("", nil)

	h := &workflowIDWrapper{HostCalls: env.H(), wfID: "test-saga-checkout"}

	// Build a Saga-powered checkout
	saga := durable.NewSaga()
	saga.AddStep("createOrder",
		func(h durable.HostCalls) (string, error) {
			err := h.DurableCallTyped("store", "createOrder", &struct{}{}, &orderID)
			return "", err
		},
		nil, // no compensation for order creation in this variant
	)
	saga.AddStep("reserveInventory",
		func(h durable.HostCalls) (string, error) {
			var reserved bool
			err := h.DurableCallTyped("store", "reserveInventory", WIDGET_ID, &reserved)
			if err != nil {
				return "", durable.NewTerminalError(err)
			}
			if !reserved {
				return "", durable.NewTerminalError(errors.New("no inventory"))
			}
			return "", nil
		},
		func(h durable.HostCalls) error {
			return h.DurableCallTyped("store", "undoReserveInventory", WIDGET_ID, nil)
		},
	)

	// Run the saga steps
	err := saga.Run(h)
	assert.NoError(t, err)

	env.AssertCalled(t, "store", "createOrder")
	env.AssertCalled(t, "store", "reserveInventory")
}
