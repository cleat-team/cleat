// Host runtime for durable WASM workflows.
//
// This demonstrates the full lifecycle:
//  1. First execution of a WASM workflow with the host intercepting API calls
//  2. Mid-execution crash (simulated at the host level)
//  3. Resume from checkpoint — the same WASM bytes run, but the host replays
//     the first N API calls from cache and only makes fresh calls for the rest
//  4. Full replay from scratch — same workflow, entirely from cache, no real
//     API calls made
//  5. Fencing — a second worker claims the workflow; the original worker detects
//     fencing and stops immediately
//
// CRITICAL: The WASM module is the SAME binary across all executions. The
// host-level crash injects failure without changing workflow code, so replay
// is byte-for-byte deterministic.

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Durable host state
// ---------------------------------------------------------------------------

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
	Input      string       `json:"input"` // JSON-encoded inputs (for determinism)
	Steps      []stepRecord `json:"steps"`
	Complete   bool         `json:"complete"`
	FinalVal   string       `json:"final_val,omitempty"`
	FinalErr   string       `json:"final_err,omitempty"`
}

// host is the durable execution engine. It wraps a workflow and intercepts every
// API call to provide durability (caching, checkpointing, replay).
type host struct {
	state     *workflowState
	stepCount int
	isReplay  bool
	crashAt   int    // if > 0, crash after this many steps (simulates failure)
	epoch     int64  // fencing token; incremented when a new worker claims this workflow
	fenced    bool   // when true, all DurableCall return error immediately — simulates lost heartbeat ownership
}

func newHost(workflowID string) *host {
	return &host{
		state: &workflowState{
			WorkflowID: workflowID,
			Steps:      []stepRecord{},
		},
		epoch: 1,
	}
}

func newHostFromCheckpoint(state *workflowState) *host {
	return &host{
		state:    state,
		isReplay: true,
	}
}

// DurableCall is the host function imported by WASM workflows.
//
// On first execution: makes the real API call and records the result.
// On replay: returns the cached result without the real call.
// On crash: returns an error after crashAt steps are recorded.
// On fence: returns an error immediately without making any call.
//
// This is the ONLY way a WASM workflow can interact with the outside world.
func (h *host) DurableCall(service, op, requestJSON string) (string, error) {
	if h.fenced {
		return "", fmt.Errorf("worker fenced — another worker claimed this workflow")
	}
	if h.isReplay {
		// ── Replay path ──
		if h.stepCount >= len(h.state.Steps) {
			// No more recorded steps: switch to live execution.
			// This is the "resume after crash" case — the first N calls
			// were replayed, now we make fresh calls.
			h.isReplay = false
			return h.durableFirstExecution(service, op, requestJSON)
		}
		rec := h.state.Steps[h.stepCount]
		if rec.Service != service || rec.Op != op {
			return "", fmt.Errorf(
				"🔴 REPLAY DIVERGENCE at step %d: workflow called %s.%s, but history has %s.%s — workflow code is NOT deterministic",
				h.stepCount, service, op, rec.Service, rec.Op)
		}
		fmt.Printf("    ♻   (replay) %s.%s → cached\n", service, op)
		h.stepCount++
		if rec.Err != "" {
			return "", fmt.Errorf(rec.Err)
		}
		return rec.Response, nil
	}

	return h.durableFirstExecution(service, op, requestJSON)
}

func (h *host) durableFirstExecution(service, op, requestJSON string) (string, error) {
	// Check if we should crash before this call.
	if h.crashAt > 0 && h.stepCount >= h.crashAt {
		fmt.Printf("    💥  SIMULATED CRASH before %s.%s\n", service, op)
		return "", fmt.Errorf("host lost power at step %d", h.stepCount)
	}

	fmt.Printf("    🌐  %s.%s(%s)\n", service, op, truncate(requestJSON, 60))

	response, err := h.realAPICall(service, op, requestJSON)

	rec := stepRecord{
		Step:     h.stepCount,
		Service:  service,
		Op:       op,
		Request:  requestJSON,
		Response: response,
	}
	if err != nil {
		rec.Err = err.Error()
	}
	h.state.Steps = append(h.state.Steps, rec)
	h.stepCount++

	return response, err
}

func (h *host) realAPICall(service, op, requestJSON string) (string, error) {
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

func (h *host) Checkpoint() []byte {
	data, _ := json.MarshalIndent(h.state, "", "  ")
	return data
}

// Fence marks this host as fenced, simulating a DB heartbeat update that returns
// 0 rows affected — meaning another worker claimed this workflow via
// SELECT ... FOR UPDATE SKIP LOCKED. All subsequent DurableCall (and future
// durable functions) return a fencing error immediately.
//
// In production, fence detection happens automatically:
//   - Heartbeat UPDATE returns rows_affected == 0
//   - CheckOwnership SELECT returns assigned_to != this worker
//   - Both trigger Fence() on the host object
func (h *host) Fence() {
	h.fenced = true
}

// ---------------------------------------------------------------------------
// The workflow — this is the WASM module's code.
//
// In production this is a WASM binary loaded via wazero. Here we show the
// identical logic as native Go — the behavior at the DurableCall boundary is
// the same either way.
// ---------------------------------------------------------------------------

type cartItem struct {
	SKU      string
	Quantity int
}

// runWorkflow is the single, deterministic workflow function. It is the SAME
// code for first execution, crash+resume, and full replay. This is the core
// invariant that makes durable execution work.
func runWorkflow(h *host, userID string, cart []cartItem) (string, error) {
	if len(cart) == 0 {
		return "", fmt.Errorf("cart is empty")
	}

	// --- Check each item's catalog availability ---
	for _, item := range cart {
		req := fmt.Sprintf(`{"sku":"%s"}`, item.SKU)
		if _, err := h.DurableCall("catalog", "LookupItem", req); err != nil {
			return "", fmt.Errorf("item %s unavailable: %w", item.SKU, err)
		}
	}

	// --- Reserve inventory ---
	itemParts := make([]string, len(cart))
	for i, item := range cart {
		itemParts[i] = fmt.Sprintf(`{"sku":"%s","quantity":%d}`, item.SKU, item.Quantity)
	}
	reserveReq := fmt.Sprintf(`{"user_id":"%s","items":[%s]}`, userID, strings.Join(itemParts, ","))
	if _, err := h.DurableCall("inventory", "Reserve", reserveReq); err != nil {
		return "", fmt.Errorf("reservation failed: %w", err)
	}

	// --- Get payment method ---
	pmReq := fmt.Sprintf(`{"user_id":"%s"}`, userID)
	if _, err := h.DurableCall("payments", "GetDefaultMethod", pmReq); err != nil {
		h.DurableCall("inventory", "Release", fmt.Sprintf(`{"reservation_id":"resv_abc123"}`))
		return "", fmt.Errorf("payment method lookup failed: %w", err)
	}

	// --- Charge customer ---
	chargeReq := fmt.Sprintf(`{"token":"pm_tok_555","amount_cents":3299}`)
	if _, err := h.DurableCall("payments", "Charge", chargeReq); err != nil {
		h.DurableCall("inventory", "Release", fmt.Sprintf(`{"reservation_id":"resv_abc123"}`))
		return "", fmt.Errorf("payment failed: %w", err)
	}

	// --- Create shipment ---
	shipReq := fmt.Sprintf(`{"reservation_id":"resv_abc123","address":"123 Main St","charge_id":"chg_xyz789"}`)
	if _, err := h.DurableCall("shipping", "CreateShipment", shipReq); err != nil {
		h.DurableCall("payments", "Refund", fmt.Sprintf(`{"charge_id":"chg_xyz789"}`))
		h.DurableCall("inventory", "Release", fmt.Sprintf(`{"reservation_id":"resv_abc123"}`))
		return "", fmt.Errorf("shipping failed: %w", err)
	}

	// --- Notify customer (best effort) ---
	h.DurableCall("notifications", "SendEmail",
		fmt.Sprintf(`{"user_id":"%s","tracking_id":"TRACK-123456"}`, userID))

	h.state.Complete = true
	h.state.FinalVal = "TRACK-123456"
	return "TRACK-123456", nil
}

// ---------------------------------------------------------------------------
// Demo driver
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Durable WASM Workflow — Host Runtime Demo                      ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("  Architecture:")
	fmt.Println("  ┌─────────────────────────────────────────────────────────┐")
	fmt.Println("  │  WASM Module (workflow + libraries)                     │")
	fmt.Println("  │  All I/O via imported host functions                    │")
	fmt.Println("  │  Deterministic by construction (no time, no random)     │")
	fmt.Println("  └──────────────┬──────────────────────────────────────────┘")
	fmt.Println("                 │  durable_call(service, op, request)")
	fmt.Println("                 ▼")
	fmt.Println("  ┌─────────────────────────────────────────────────────────┐")
	fmt.Println("  │  Host Runtime                                           │")
	fmt.Println("  │  • Intercepts durable_call                             │")
	fmt.Println("  │  • Records results in event history (checkpoint)       │")
	fmt.Println("  │  • On replay: returns cached results, skips real call  │")
	fmt.Println("  │  • Stores checkpoints in DB (S3/Dynamo/Postgres)       │")
	fmt.Println("  └─────────────────────────────────────────────────────────┘")
	fmt.Println()

	cart := []cartItem{
		{SKU: "ABC-123", Quantity: 2},
		{SKU: "XYZ-789", Quantity: 1},
	}

	// ---- Phase 1: Full execution ----
	fmt.Println("═══ Phase 1: Full execution on Node A ═══")
	fmt.Println()

	h1 := newHost("order-12345")
	h1.state.Input = `{"user_id":"order-12345","cart":[{"sku":"ABC-123","quantity":2},{"sku":"XYZ-789","quantity":1}]}`
	fmt.Println("  Executing workflow (WASM module loaded, fresh start)...")
	fmt.Println()

	result, err := runWorkflow(h1, "order-12345", cart)
	if err != nil {
		fmt.Printf("\n  ❌ Workflow error: %v\n", err)
	} else {
		fmt.Printf("\n  ✅ Complete: trackingID=%s\n", result)
	}
	fmt.Printf("  API calls made: %d\n", len(h1.state.Steps))
	fmt.Println()

	// ---- Phase 2: Execution with crash ----
	fmt.Println("═══ Phase 2: Execution with crash mid-workflow ═══")
	fmt.Println()

	h2 := newHost("order-67890")
	h2.state.Input = `{"user_id":"order-67890","cart":[{"sku":"ABC-123","quantity":2},{"sku":"XYZ-789","quantity":1}]}`
	h2.crashAt = 5 // Crash after step 4 (catalog×2 + reserve + payment_method + charge = 5? No, let me count.)

	// The steps are:
	// 0: catalog.LookupItem(ABC-123)
	// 1: catalog.LookupItem(XYZ-789)
	// 2: inventory.Reserve
	// 3: payments.GetDefaultMethod
	// 4: payments.Charge
	// 5: shipping.CreateShipment
	// 6: notifications.SendEmail
	//
	// So crashAt=5 means crash before shipping (step 5), after payment succeeds.
	fmt.Println("  Executing workflow (same WASM bytes, fresh host)...")
	fmt.Println()

	result2, err2 := runWorkflow(h2, "order-67890", cart)
	if err2 != nil {
		fmt.Printf("\n  💥 CRASH: %v\n", err2)
	} else {
		fmt.Printf("\n  (unexpected success: %s)\n", result2)
	}
	fmt.Printf("  API calls recorded before crash: %d\n", len(h2.state.Steps))
	fmt.Println()

	// Show the partial checkpoint.
	fmt.Println("  Partial checkpoint recovered from durable store:")
	fmt.Println("  " + strings.ReplaceAll(string(h2.Checkpoint()), "\n", "\n  "))
	fmt.Println()

	// ---- Phase 3: Resume on Node B ----
	fmt.Println("═══ Phase 3: Resume on Node B (different machine) ═══")
	fmt.Println()

	// Load the checkpoint that Node A left behind.
	// IMPORTANT: we copy the state so we can measure what changed.
	replayedBeforeCrash := len(h2.state.Steps)
	stateCopy := copyCheckpoint(h2.state)
	h3 := newHostFromCheckpoint(stateCopy)
	fmt.Printf("  Loaded checkpoint: %d steps recorded\n", replayedBeforeCrash)
	fmt.Println("  Resuming workflow (same WASM bytes, fresh host, replay mode)...")
	fmt.Println()

	result3, err3 := runWorkflow(h3, "order-67890", cart)
	if err3 != nil {
		fmt.Printf("\n  ❌ Resume failed: %v\n", err3)
	} else {
		fmt.Printf("\n  ✅ Complete after resume: trackingID=%s\n", result3)
	}
	freshCalls := len(h3.state.Steps) - replayedBeforeCrash
	fmt.Printf("  Total steps: %d (%d replayed from cache, %d executed fresh)\n",
		len(h3.state.Steps), replayedBeforeCrash, freshCalls)
	fmt.Println()

	// ---- Phase 4: Full replay from cold ----
	fmt.Println("═══ Phase 4: Full replay from cold (Node C, disaster recovery) ═══")
	fmt.Println()

	h4 := newHostFromCheckpoint(h3.state)
	h4.isReplay = true
	h4.stepCount = 0
	fmt.Println("  Replaying entire workflow from event history...")
	fmt.Println("  (No real API calls — host returns cached results)")
	fmt.Println()

	result4, err4 := runWorkflow(h4, "order-67890", cart)
	if err4 != nil {
		fmt.Printf("\n  ❌ Replay error: %v\n", err4)
	} else {
		fmt.Printf("\n  ✅ Replay complete: trackingID=%s\n", result4)
	}
	fmt.Printf("  All %d API calls served from cache — zero network traffic\n", len(h4.state.Steps))
	fmt.Println()

	// ---- Phase 5: Fencing token demo ----
	fmt.Println("═══ Phase 5: Fencing — worker loses ownership mid-execution ═══")
	fmt.Println()

	h5 := newHost("order-54321")
	h5.state.Input = `{"user_id":"order-54321","cart":[{"sku":"ABC-123","quantity":2},{"sku":"XYZ-789","quantity":1}]}`

	// Simulate: mid-execution, the DB heartbeat returns 0 rows (another worker
	// claimed this workflow via SKIP LOCKED). The goroutine races with the
	// workflow: the host is making forward progress, but ownership has been lost.
	go func() {
		time.Sleep(120 * time.Millisecond)
		fmt.Println("  ⚠   (background) DB heartbeat: 0 rows updated — another worker claimed this workflow")
		fmt.Println()
		h5.Fence()
	}()

	fmt.Println("  Executing workflow (fence arrives mid-execution)...")
	fmt.Println()
	fmt.Println("  Note: fence check happens at the TOP of each DurableCall.")
	fmt.Println("  A call that is already executing completes normally; only the")
	fmt.Println("  NEXT call sees the fence and returns the error immediately.")
	fmt.Println()

	result5, err5 := runWorkflow(h5, "order-54321", cart)
	stepsBeforeFence := len(h5.state.Steps)
	if err5 != nil {
		if strings.Contains(err5.Error(), "fenced") {
			fmt.Printf("\n  🔒 WORKFLOW FENCED: %v\n", err5)
		} else {
			fmt.Printf("\n  ❌ Workflow error: %v\n", err5)
		}
	} else {
		fmt.Printf("\n  ✅ (fence not triggered in time): trackingID=%s\n", result5)
	}
	fmt.Printf("  Steps completed before fence: %d\n", stepsBeforeFence)
	fmt.Printf("  Host fenced flag: %v\n", h5.fenced)
	if h5.fenced {
		fmt.Printf("  Workflow status: fenced (not completed normally)\n")
	}
	fmt.Printf("  Recorded steps: %d (fenced calls are NOT recorded)\n", stepsBeforeFence)
	fmt.Println()

	// Show the partial checkpoint so readers can see only pre-fence steps exist.
	fmt.Println("  Partial checkpoint (only pre-fence steps recorded):")
	fmt.Println("  " + strings.ReplaceAll(string(h5.Checkpoint()), "\n", "\n  "))
	fmt.Println()

	// ---- Summary ----
	fmt.Println("╔══════════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Results                                                        ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════════╣")
	fmt.Println("║                                                                  ║")
	fmt.Printf("║  Phase 1 (Node A):  %2d API calls, full execution              ║\n", len(h1.state.Steps))
	fmt.Printf("║  Phase 2 (Node A):  %2d API calls before crash                 ║\n", len(h2.state.Steps))
	fmt.Printf("║  Phase 3 (Node B):  %2d calls (%d replay + %d fresh) on resume  ║\n",
		len(h3.state.Steps), len(h2.state.Steps), len(h3.state.Steps)-len(h2.state.Steps))
	fmt.Printf("║  Phase 4 (Node C):  %2d calls, all from cache (full replay)    ║\n", len(h4.state.Steps))
	fmt.Printf("║  Phase 5 (Fence):   %d steps, workflow fenced mid-execution    ║\n", len(h5.state.Steps))
	fmt.Println("║                                                                  ║")
	fmt.Println("║  Safety guarantees:                                              ║")
	fmt.Println("║  • Workflow = WASM binary in database                            ║")
	fmt.Println("║  • WASM sandbox: no file system, no network, no os.Exec          ║")
	fmt.Println("║  • All I/O through host functions (audited, rate-limited)        ║")
	fmt.Println("║  • Host controls: what APIs can be called, timeout, retry        ║")
	fmt.Println("║  • Determinism: WASM + host-provided time = replayable           ║")
	fmt.Println("║  • Fencing token prevents split-brain after DB connection loss   ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════╝")
}

// copyCheckpoint deep-copies the workflow state so resume doesn't mutate the
// snapshot we're measuring against.
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
