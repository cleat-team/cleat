// SDK host demo — runs the durable workflow lifecycle using the SDK interface.
//
// Build & run:
//
//	GOTOOLCHAIN=local /localssd/go1.24.5/bin/go run ./wasm-demo/cmd/sdk-host/
//
// Or:
//
//	PATH="/localssd/go1.24.5/bin:$PATH" go run ./wasm-demo/cmd/sdk-host/

package main

import (
	"fmt"
	"strings"
	"time"
)

func main() {
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println("  Durable WASM Workflow — SDK Host Runtime Demo")
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println()
	fmt.Println("  Architecture:")
	fmt.Println("  +--------------------------------------------------+")
	fmt.Println("  |  Workflow code uses durable.HostCalls interface  |")
	fmt.Println("  |  All I/O via DurableCall / DurableCallTyped      |")
	fmt.Println("  |  Deterministic by construction                   |")
	fmt.Println("  +--------------------------+-----------------------+")
	fmt.Println("                             |  HostCalls")
	fmt.Println("                             v")
	fmt.Println("  +--------------------------------------------------+")
	fmt.Println("  |  Host Runtime (durableHost)                      |")
	fmt.Println("  |  . Records results in event history              |")
	fmt.Println("  |  . On replay: returns cached results             |")
	fmt.Println("  |  . Handles crashes, fencing, checkpoints         |")
	fmt.Println("  +--------------------------------------------------+")
	fmt.Println()

	cart := []cartItem{
		{SKU: "ABC-123", Quantity: 2},
		{SKU: "XYZ-789", Quantity: 1},
	}

	// ---- Phase 1: Full execution ----
	fmt.Println("=== Phase 1: Full execution on Node A ===")
	fmt.Println()

	dh1 := newDurableHost("order-12345")
	dh1.state.Input = `{"user_id":"order-12345","cart":[{"sku":"ABC-123","quantity":2},{"sku":"XYZ-789","quantity":1}]}`
	fmt.Println("  Executing workflow (fresh start)...")
	fmt.Println()

	result, err := runWorkflowSDK(dh1.H(), "order-12345", cart)
	if err != nil {
		fmt.Printf("\n  WORKFLOW ERROR: %v\n", err)
	} else {
		fmt.Printf("\n  Complete: trackingID=%s\n", result)
	}
	fmt.Printf("  API calls recorded: %d\n", len(dh1.state.Steps))
	fmt.Println()

	// ---- Phase 2: Execution with crash ----
	fmt.Println("=== Phase 2: Execution with crash mid-workflow ===")
	fmt.Println()

	dh2 := newDurableHost("order-67890")
	dh2.state.Input = `{"user_id":"order-67890","cart":[{"sku":"ABC-123","quantity":2},{"sku":"XYZ-789","quantity":1}]}`
	dh2.crashAt = 5 // crash before shipping.CreateShipment (after payment.Charge)
	fmt.Println("  Executing workflow (crashAt=5)...")
	fmt.Println()

	result2, err2 := runWorkflowSDK(dh2.H(), "order-67890", cart)
	if err2 != nil {
		fmt.Printf("\n  CRASH: %v\n", err2)
	} else {
		fmt.Printf("\n  (unexpected success: %s)\n", result2)
	}
	fmt.Printf("  API calls recorded before crash: %d\n", len(dh2.state.Steps))
	fmt.Println()
	fmt.Println("  Partial checkpoint:")
	fmt.Println("  " + strings.ReplaceAll(string(dh2.Checkpoint()), "\n", "\n  "))
	fmt.Println()

	// ---- Phase 3: Resume on Node B ----
	fmt.Println("=== Phase 3: Resume on Node B (different machine) ===")
	fmt.Println()

	replayedBeforeCrash := len(dh2.state.Steps)
	stateCopy := copyCheckpoint(dh2.state)
	dh3 := newDurableHostFromCheckpoint(stateCopy)
	fmt.Printf("  Loaded checkpoint: %d steps recorded\n", replayedBeforeCrash)
	fmt.Println("  Resuming workflow (replay mode)...")
	fmt.Println()

	result3, err3 := runWorkflowSDK(dh3.H(), "order-67890", cart)
	if err3 != nil {
		fmt.Printf("\n  RESUME FAILED: %v\n", err3)
	} else {
		fmt.Printf("\n  Complete after resume: trackingID=%s\n", result3)
	}
	freshCalls := len(dh3.state.Steps) - replayedBeforeCrash
	fmt.Printf("  Total steps: %d (%d replayed from cache, %d executed fresh)\n",
		len(dh3.state.Steps), replayedBeforeCrash, freshCalls)
	fmt.Println()

	// ---- Phase 4: Full replay from cold ----
	fmt.Println("=== Phase 4: Full replay from cold (Node C, disaster recovery) ===")
	fmt.Println()

	dh4 := newDurableHostFromCheckpoint(dh3.state)
	dh4.isReplay = true
	dh4.stepCount = 0
	fmt.Println("  Replaying entire workflow from event history...")
	fmt.Println("  (No real API calls - host returns cached results)")
	fmt.Println()

	result4, err4 := runWorkflowSDK(dh4.H(), "order-67890", cart)
	if err4 != nil {
		fmt.Printf("\n  REPLAY ERROR: %v\n", err4)
	} else {
		fmt.Printf("\n  Replay complete: trackingID=%s\n", result4)
	}
	fmt.Printf("  All %d API calls served from cache - zero network traffic\n", len(dh4.state.Steps))
	fmt.Println()

	// ---- Phase 5: Fencing ----
	fmt.Println("=== Phase 5: Fencing — worker loses ownership mid-execution ===")
	fmt.Println()

	dh5 := newDurableHost("order-54321")
	dh5.state.Input = `{"user_id":"order-54321","cart":[{"sku":"ABC-123","quantity":2},{"sku":"XYZ-789","quantity":1}]}`

	go func() {
		time.Sleep(120 * time.Millisecond)
		fmt.Println("  (background) DB heartbeat: 0 rows updated — another worker claimed this workflow")
		fmt.Println()
		dh5.Fence()
	}()

	fmt.Println("  Executing workflow (fence arrives mid-execution)...")
	fmt.Println()

	result5, err5 := runWorkflowSDK(dh5.H(), "order-54321", cart)
	stepsBeforeFence := len(dh5.state.Steps)
	if err5 != nil {
		if strings.Contains(err5.Error(), "fenced") {
			fmt.Printf("\n  WORKFLOW FENCED: %v\n", err5)
		} else {
			fmt.Printf("\n  WORKFLOW ERROR: %v\n", err5)
		}
	} else {
		fmt.Printf("\n  (fence not triggered in time): trackingID=%s\n", result5)
	}
	fmt.Printf("  Steps completed before fence: %d\n", stepsBeforeFence)
	fmt.Println()

	// ---- Summary ----
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println("  Results")
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println()
	fmt.Printf("  Phase 1 (Node A):  %2d API calls, full execution\n", len(dh1.state.Steps))
	fmt.Printf("  Phase 2 (Node A):  %2d API calls before crash\n", len(dh2.state.Steps))
	fmt.Printf("  Phase 3 (Node B):  %2d calls (%d replay + %d fresh) on resume\n",
		len(dh3.state.Steps), len(dh2.state.Steps), len(dh3.state.Steps)-len(dh2.state.Steps))
	fmt.Printf("  Phase 4 (Node C):  %2d calls, all from cache (full replay)\n", len(dh4.state.Steps))
	fmt.Printf("  Phase 5 (Fence):   %d steps, workflow fenced mid-execution\n", len(dh5.state.Steps))
	fmt.Println()
	fmt.Println("  Safety guarantees:")
	fmt.Println("  . Workflow code uses only durable.HostCalls (no direct I/O)")
	fmt.Println("  . All external calls recorded in event history")
	fmt.Println("  . Replay is deterministic: same inputs -> same sequence")
	fmt.Println("  . Fencing prevents split-brain after ownership loss")
	fmt.Println("  . Zero fmt.Sprintf JSON injection — all calls use DurableCallTyped")
	fmt.Println("  . SDK retry policies, structured errors, and Selector available")
	fmt.Println()
}
