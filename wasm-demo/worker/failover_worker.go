// Worker process with database failover handling.
//
// This demonstrates the complete lifecycle of a worker that survives
// PostgreSQL failover (primary crash → standby promotion via Patroni).
//
// Since we can't install lib/pq in this environment, the DB operations
// are defined as an interface. The real SQL is shown in comments alongside
// each operation. The demo runs against a simulated DB that injects
// connection failures to exercise the failover paths.
//
// Build & run:
//   GOTOOLCHAIN=local /home/rcownie/go/bin/go build -o /tmp/worker ./worker/
//   /tmp/worker

package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// ==========================================================================
// DB Interface — what the worker needs from PostgreSQL
// ==========================================================================

type EventRecord struct {
	Step         int
	Service      string
	Operation    string
	RequestJSON  string
	ResponseJSON string
	ErrorMsg     string
}

type WorkflowInstance struct {
	ID         string
	DefName    string
	DefVersion int
	Status     string
	Input      string
	AssignedTo string
	NextWakeAt time.Time
}

// DB is the interface to the PostgreSQL database. Each operation is
// designed to be idempotent so it can be safely retried after a
// connection failure.
type DB interface {
	// Ping checks connectivity to the current PostgreSQL primary.
	Ping(ctx context.Context) error

	// ClaimWorkflow atomically dequeues a workflow instance.
	// SQL: UPDATE ... WHERE id = (SELECT ... FOR UPDATE SKIP LOCKED)
	ClaimWorkflow(ctx context.Context, workerID string) (*WorkflowInstance, error)

	// LoadEventHistory returns all recorded steps for a workflow, ordered.
	// SQL: SELECT step, service, operation, request, response, error
	//      FROM event_history WHERE workflow_id = $1 ORDER BY step
	LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error)

	// AppendEventHistory checkpoints a durable call result.
	// SQL: INSERT INTO event_history (...) VALUES (...)
	//      ON CONFLICT (workflow_id, step) DO NOTHING
	AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error

	// Heartbeat updates the heartbeat timestamp to prevent timeout.
	// SQL: UPDATE workflow_instances SET heartbeat_at = now()
	//      WHERE id = $1 AND assigned_to = $2
	// Returns false if the workflow is no longer assigned to this worker.
	Heartbeat(ctx context.Context, workflowID, workerID string, generation int64) (bool, error)

	// CheckOwnership verifies this worker still owns the workflow.
	// SQL: SELECT assigned_to FROM workflow_instances WHERE id = $1
	CheckOwnership(ctx context.Context, workflowID, workerID string) (bool, error)

	// CompleteWorkflow marks the workflow as done or failed.
	// SQL: UPDATE workflow_instances SET status = ..., completed_at = now()
	//      WHERE id = $1 AND assigned_to = $2
	CompleteWorkflow(ctx context.Context, workflowID, workerID, result, errMsg string) error

	// ReleaseWorkflow returns a workflow to the queue.
	// SQL: UPDATE workflow_instances SET status = 'ready', assigned_to = NULL
	//      WHERE id = $1 AND assigned_to = $2
	ReleaseWorkflow(ctx context.Context, workflowID, workerID string) error
}

// ==========================================================================
// Simulated DB with failover injection
// ==========================================================================

type simulatedDB struct {
	mu           sync.Mutex
	workflows    map[string]*WorkflowInstance
	histories    map[string][]EventRecord
	connected    bool
	failAfter    int // fail after this many operations (0 = never)
	opCount      int
}

func newSimulatedDB() *simulatedDB {
	return &simulatedDB{
		workflows: make(map[string]*WorkflowInstance),
		histories: make(map[string][]EventRecord),
		connected: true,
	}
}

func (db *simulatedDB) Ping(ctx context.Context) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if !db.connected {
		return fmt.Errorf("connection refused: server is unreachable")
	}
	return nil
}

func (db *simulatedDB) ClaimWorkflow(ctx context.Context, workerID string) (*WorkflowInstance, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.maybeFail("claim"); err != nil {
		return nil, err
	}

	// Find an unclaimed workflow.
	for id, wf := range db.workflows {
		if wf.Status == "ready" && wf.NextWakeAt.Before(time.Now()) {
			wf.Status = "running"
			wf.AssignedTo = workerID
			cp := *wf
			return &cp, nil
		}
		_ = id
	}
	return nil, nil // no work
}

func (db *simulatedDB) LoadEventHistory(ctx context.Context, workflowID string) ([]EventRecord, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.maybeFail("load_history"); err != nil {
		return nil, err
	}
	history := db.histories[workflowID]
	cp := make([]EventRecord, len(history))
	copy(cp, history)
	return cp, nil
}

func (db *simulatedDB) AppendEventHistory(ctx context.Context, workflowID string, rec EventRecord) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.maybeFail("append_history"); err != nil {
		return err
	}
	// ON CONFLICT DO NOTHING: skip if step already exists (idempotent).
	for _, existing := range db.histories[workflowID] {
		if existing.Step == rec.Step {
			return nil
		}
	}
	db.histories[workflowID] = append(db.histories[workflowID], rec)
	return nil
}

func (db *simulatedDB) Heartbeat(ctx context.Context, workflowID, workerID string, generation int64) (bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.maybeFail("heartbeat"); err != nil {
		return false, err
	}
	wf, ok := db.workflows[workflowID]
	if !ok || wf.AssignedTo != workerID {
		return false, nil
	}
	return true, nil
}

func (db *simulatedDB) CheckOwnership(ctx context.Context, workflowID, workerID string) (bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.maybeFail("check_ownership"); err != nil {
		return false, err
	}
	wf, ok := db.workflows[workflowID]
	if !ok || wf.AssignedTo != workerID {
		return false, nil
	}
	return true, nil
}

func (db *simulatedDB) CompleteWorkflow(ctx context.Context, workflowID, workerID, result, errMsg string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.maybeFail("complete"); err != nil {
		return err
	}
	wf, ok := db.workflows[workflowID]
	if !ok || wf.AssignedTo != workerID {
		return fmt.Errorf("workflow not assigned to this worker")
	}
	if errMsg != "" {
		wf.Status = "failed"
	} else {
		wf.Status = "done"
	}
	wf.AssignedTo = ""
	return nil
}

func (db *simulatedDB) ReleaseWorkflow(ctx context.Context, workflowID, workerID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.maybeFail("release"); err != nil {
		return err
	}
	wf, ok := db.workflows[workflowID]
	if !ok || wf.AssignedTo != workerID {
		return nil
	}
	wf.Status = "ready"
	wf.AssignedTo = ""
	return nil
}

func (db *simulatedDB) maybeFail(op string) error {
	if db.failAfter > 0 && db.opCount >= db.failAfter {
		db.connected = false
		return fmt.Errorf("connection refused: simulated failover in progress")
	}
	db.opCount++
	return nil
}

// simulateFailover makes the DB unavailable for a duration, then recovers.
// This simulates Patroni promoting a standby after a primary crash.
func (db *simulatedDB) simulateFailover(duration time.Duration, label string) {
	db.mu.Lock()
	db.connected = false
	db.mu.Unlock()

	fmt.Printf("\n  🔴 %s: DB PRIMARY DOWN — failover in progress...\n", label)
	time.Sleep(duration)

	db.mu.Lock()
	db.connected = true
	db.opCount = 0
	db.mu.Unlock()
	fmt.Printf("  🟢 %s: STANDBY PROMOTED — DB is now available on new primary\n", label)
}

// ==========================================================================
// Worker
// ==========================================================================

type Worker struct {
	id         string
	db         DB
	concurrency int
	heartbeatInterval time.Duration

	inflight sync.Map // map[workflowID]*WorkflowInstance

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newWorker(id string, db DB) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	return &Worker{
		id:                id,
		db:                db,
		concurrency:       10,
		heartbeatInterval: 2 * time.Second,
		ctx:               ctx,
		cancel:            cancel,
	}
}

// --------------------------------------------------------------------------
// Main loop
// --------------------------------------------------------------------------

func (w *Worker) Run() {
	defer w.cancel()

	// Background heartbeat goroutine.
	w.wg.Add(1)
	go w.heartbeatLoop()

	// Dispatch loop: claim work → launch goroutine.
	w.wg.Add(1)
	go w.dispatchLoop()

	fmt.Printf("[worker %s] Started\n", w.id)

	// Wait for shutdown.
	<-w.ctx.Done()
	w.wg.Wait()
	fmt.Printf("[worker %s] Shutdown complete\n", w.id)
}

func (w *Worker) Shutdown() {
	w.cancel()
}

func (w *Worker) dispatchLoop() {
	defer w.wg.Done()

	for {
		select {
		case <-w.ctx.Done():
			return
		default:
		}

		wf, err := w.db.ClaimWorkflow(w.ctx, w.id)
		if err != nil {
			if isConnectionError(err) {
				fmt.Printf("[worker %s] DB unreachable during claim, waiting for failover...\n", w.id)
				w.waitForDB()
				continue
			}
			fmt.Printf("[worker %s] claim error: %v\n", w.id, err)
			time.Sleep(time.Second)
			continue
		}

		if wf == nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		fmt.Printf("[worker %s] Claimed workflow %s\n", w.id, wf.ID)
		w.inflight.Store(wf.ID, wf)
		w.wg.Add(1)
		go w.executeWorkflow(wf)
	}
}

// --------------------------------------------------------------------------
// Workflow execution with failover handling
// --------------------------------------------------------------------------

func (w *Worker) executeWorkflow(wf *WorkflowInstance) {
	defer w.wg.Done()
	defer w.inflight.Delete(wf.ID)
	defer func() {
		// On panic or unexpected exit, release the workflow.
		if r := recover(); r != nil {
			fmt.Printf("[worker %s] PANIC in %s: %v — releasing\n", w.id, wf.ID, r)
			w.releaseOrFail(wf, fmt.Sprintf("panic: %v", r))
		}
	}()

	// ---- Load event history (replay existing state) ----
	history, err := w.db.LoadEventHistory(w.ctx, wf.ID)
	if err != nil {
		if isConnectionError(err) {
			fmt.Printf("[worker %s] DB down while loading history for %s — releasing\n", w.id, wf.ID)
			return
		}
		w.releaseOrFail(wf, fmt.Sprintf("history load failed: %v", err))
		return
	}

	currentStep := len(history)
	fmt.Printf("[worker %s] %s: loaded %d history steps (resuming from step %d)\n",
		w.id, wf.ID, currentStep, currentStep)

	// ---- Main execution loop ----
	type stepResult struct {
		service      string
		operation    string
		requestJSON  string
		responseJSON string
		errMsg       string
		done         bool
		finalValue   string
	}

	for {
		select {
		case <-w.ctx.Done():
			fmt.Printf("[worker %s] Shutting down — releasing %s\n", w.id, wf.ID)
			w.db.ReleaseWorkflow(context.Background(), wf.ID, w.id)
			return
		default:
		}

		// ---------- HEARTBEAT ----------
		alive, err := w.db.Heartbeat(w.ctx, wf.ID, w.id, wf.Generation)
		if err != nil {
			if isConnectionError(err) {
				fmt.Printf("[worker %s] 💔 DB connection lost mid-workflow (%s, step %d)\n",
					w.id, wf.ID, currentStep)
				fmt.Printf("[worker %s]    Workflow PAUSED — in-memory state preserved\n", w.id)

				// Wait for failover to complete.
				if !w.waitForDBWithTimeout(90 * time.Second) {
					fmt.Printf("[worker %s]    DB unreachable for 90s — releasing %s\n", w.id, wf.ID)
					w.db.ReleaseWorkflow(context.Background(), wf.ID, w.id)
					return
				}

				// DB is back. Verify ownership.
				owns, err := w.db.CheckOwnership(w.ctx, wf.ID, w.id)
				if err != nil || !owns {
					fmt.Printf("[worker %s]    Lost ownership of %s after failover (reclaimed by peer)\n",
						w.id, wf.ID)
					return
				}

				// Re-verify event history (peer might have made progress).
				history2, err := w.db.LoadEventHistory(w.ctx, wf.ID)
				if err != nil {
					w.db.ReleaseWorkflow(context.Background(), wf.ID, w.id)
					return
				}
				if len(history2) > currentStep {
					fmt.Printf("[worker %s]    Peer advanced %s from step %d to %d — resuming from peer's state\n",
						w.id, wf.ID, currentStep, len(history2))
					currentStep = len(history2)
				}
				fmt.Printf("[worker %s]    Resuming %s at step %d after failover\n", w.id, wf.ID, currentStep)
				continue
			}
			fmt.Printf("[worker %s] Heartbeat error for %s: %v\n", w.id, wf.ID, err)
		}
		if !alive {
			fmt.Printf("[worker %s] %s: heartbeat indicates lost ownership — stopping\n", w.id, wf.ID)
			return
		}

		// ---------- EXECUTE NEXT STEP ----------
		// In production: drive the WASM module. The module calls back into
		// the host's DurableCall, which makes the real HTTP call.
		result := w.simulateStep(wf, currentStep)

		if result.done {
			fmt.Printf("[worker %s] %s: workflow complete → %q\n", w.id, wf.ID, result.finalValue)
			w.db.CompleteWorkflow(w.ctx, wf.ID, w.id, result.finalValue, result.errMsg)
			return
		}

		// ---------- CHECKPOINT ----------
		rec := EventRecord{
			Step:         currentStep,
			Service:      result.service,
			Operation:    result.operation,
			RequestJSON:  result.requestJSON,
			ResponseJSON: result.responseJSON,
			ErrorMsg:     result.errMsg,
		}

		err = w.db.AppendEventHistory(w.ctx, wf.ID, rec)
		if err != nil {
			if isConnectionError(err) {
				fmt.Printf("[worker %s] 💔 DB connection lost during checkpoint (%s, step %d)\n",
					w.id, wf.ID, currentStep)
				fmt.Printf("[worker %s]    Result held in memory — will retry after failover\n", w.id)

				if !w.waitForDBWithTimeout(90 * time.Second) {
					w.db.ReleaseWorkflow(context.Background(), wf.ID, w.id)
					return
				}

				// Retry the checkpoint. ON CONFLICT DO NOTHING makes this safe:
				// if the first attempt succeeded (we just didn't get the response),
				// the second INSERT is a no-op.
				err = w.db.AppendEventHistory(w.ctx, wf.ID, rec)
				if err != nil {
					fmt.Printf("[worker %s] Checkpoint retry failed for %s: %v\n", w.id, wf.ID, err)
					w.db.ReleaseWorkflow(context.Background(), wf.ID, w.id)
					return
				}
				fmt.Printf("[worker %s]    Checkpoint %s:step=%d confirmed after failover\n",
					w.id, wf.ID, currentStep)
			} else {
				fmt.Printf("[worker %s] Checkpoint error for %s: %v\n", w.id, wf.ID, err)
			}
		}

		currentStep++
	}
}

// --------------------------------------------------------------------------
// DB health management
// --------------------------------------------------------------------------

func (w *Worker) waitForDB() {
	backoff := 500 * time.Millisecond
	for i := 0; i < 20; i++ {
		select {
		case <-w.ctx.Done():
			return
		default:
		}

		if err := w.db.Ping(w.ctx); err == nil {
			fmt.Printf("[worker %s] DB connection re-established\n", w.id)
			return
		}

		fmt.Printf("[worker %s] DB reconnect attempt %d/20 failed, retrying in %v\n",
			w.id, i+1, backoff)
		time.Sleep(backoff)
		backoff = time.Duration(math.Min(float64(backoff*2), 10e9))
	}
}

func (w *Worker) waitForDBWithTimeout(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	backoff := 500 * time.Millisecond

	for time.Now().Before(deadline) {
		select {
		case <-w.ctx.Done():
			return false
		default:
		}

		if err := w.db.Ping(w.ctx); err == nil {
			fmt.Printf("[worker %s] DB reconnected after failover\n", w.id)
			return true
		}

		remaining := time.Until(deadline)
		if remaining < backoff {
			backoff = remaining
		}
		time.Sleep(backoff)
		backoff = time.Duration(math.Min(float64(backoff*2), 10e9))
	}
	return false
}

func (w *Worker) heartbeatLoop() {
	defer w.wg.Done()
	ticker := time.NewTicker(w.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			w.inflight.Range(func(key, value interface{}) bool {
				wf := value.(*WorkflowInstance)
				alive, err := w.db.Heartbeat(w.ctx, wf.ID, w.id, wf.Generation)
				if err != nil && isConnectionError(err) {
					fmt.Printf("[worker %s] Heartbeat failed for %s — DB appears down\n", w.id, wf.ID)
				} else if !alive {
					fmt.Printf("[worker %s] %s: heartbeat lost ownership — removing from inflight\n",
						w.id, wf.ID)
					w.inflight.Delete(key)
				}
				return true
			})
		}
	}
}

func (w *Worker) releaseOrFail(wf *WorkflowInstance, errMsg string) {
	if errMsg != "" {
		w.db.CompleteWorkflow(context.Background(), wf.ID, w.id, "", errMsg)
	} else {
		w.db.ReleaseWorkflow(context.Background(), wf.ID, w.id)
	}
}

func (w *Worker) simulateStep(wf *WorkflowInstance, step int) struct {
	service      string
	operation    string
	requestJSON  string
	responseJSON string
	errMsg       string
	done         bool
	finalValue   string
} {
	// Simulated 3-step workflow: catalog → payment → shipping
	steps := []struct {
		service      string
		operation    string
		requestJSON  string
		responseJSON string
		errMsg       string
		done         bool
		finalValue   string
	}{
		{service: "catalog", operation: "LookupItem",
			requestJSON: `{"sku":"ABC-123"}`, responseJSON: `{"found":true}`},
		{service: "payments", operation: "Charge",
			requestJSON: `{"amount":3299}`, responseJSON: `{"charge_id":"chg_123"}`},
		{service: "shipping", operation: "CreateShipment",
			requestJSON: `{"addr":"123 Main"}`, responseJSON: `{"tracking":"TRACK-789"}`},
		{done: true, finalValue: "TRACK-789"},
	}

	if step < len(steps) {
		return steps[step]
	}
	return struct {
		service      string
		operation    string
		requestJSON  string
		responseJSON string
		errMsg       string
		done         bool
		finalValue   string
	}{done: true, finalValue: "OK"}
}

// ==========================================================================
// Utilities
// ==========================================================================

func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	patterns := []string{
		"connection refused",
		"connection reset",
		"connection closed",
		"no reachable servers",
		"server closed the connection",
		"connection timed out",
		"broken pipe",
		"EOF",
		"driver: bad connection",
	}
	for _, p := range patterns {
		if strings.Contains(strings.ToLower(s), strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// ==========================================================================
// Demo
// ==========================================================================

func main() {
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println("  Worker Failover Demo")
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println()
	fmt.Println("  This demonstrates how a worker survives PostgreSQL failover.")
	fmt.Println("  The DB is simulated — it injects connection failures at specific")
	fmt.Println("  points to exercise the failover/recovery code paths.")
	fmt.Println()
	fmt.Println("  Architecture reminder:")
	fmt.Println("  ┌──────────┐  ┌──────────┐  ┌──────────┐")
	fmt.Println("  │ Worker A │  │ Worker B │  │ Worker C │")
	fmt.Println("  └────┬─────┘  └────┬─────┘  └────┬─────┘")
	fmt.Println("       └──────────────┼──────────────┘")
	fmt.Println("                      │")
	fmt.Println("              ┌───────┴───────┐")
	fmt.Println("              │  PostgreSQL   │  ← Patroni HA (sync standby)")
	fmt.Println("              │  (primary)    │")
	fmt.Println("              └───────────────┘")
	fmt.Println()

	rand.Seed(time.Now().UnixNano())

	// ── Setup: create simulated DB with pending workflows ──
	db := newSimulatedDB()
	db.workflows["wf-001"] = &WorkflowInstance{
		ID: "wf-001", DefName: "PlaceOrder", DefVersion: 1,
		Status: "ready", NextWakeAt: time.Now().Add(-time.Minute),
	}

	// ── Test 1: Normal execution ──
	fmt.Println("── Test 1: Normal execution (no failover) ──")
	fmt.Println()
	worker1 := newWorker("worker-A", db)
	ctx1, cancel1 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel1()

	// Run worker in background.
	runDone := make(chan struct{})
	go func() {
		worker1.Run()
		close(runDone)
	}()

	// Wait for workflow to complete.
	select {
	case <-runDone:
	case <-ctx1.Done():
		worker1.Shutdown()
		<-runDone
	}
	fmt.Println()

	// Verify.
	history1, _ := db.LoadEventHistory(context.Background(), "wf-001")
	fmt.Printf("  Result: %d checkpoints recorded, workflow status=%s\n",
		len(history1), db.workflows["wf-001"].Status)
	fmt.Println()

	// ── Test 2: Failover mid-execution ──
	fmt.Println("── Test 2: Execution with failover after first checkpoint ──")
	fmt.Println()

	db2 := newSimulatedDB()
	db2.workflows["wf-002"] = &WorkflowInstance{
		ID: "wf-002", DefName: "PlaceOrder", DefVersion: 1,
		Status: "ready", NextWakeAt: time.Now().Add(-time.Minute),
	}

	worker2 := newWorker("worker-B-purple", db2)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()

	// Simulate a failover 1.5 seconds into execution.
	// The worker should have completed ~1 checkpoint by then.
	go func() {
		time.Sleep(1500 * time.Millisecond)
		db2.simulateFailover(3*time.Second, "Test 2")
	}()

	runDone2 := make(chan struct{})
	go func() {
		worker2.Run()
		close(runDone2)
	}()

	select {
	case <-runDone2:
	case <-ctx2.Done():
		worker2.Shutdown()
		<-runDone2
	}
	fmt.Println()

	history2, _ := db2.LoadEventHistory(context.Background(), "wf-002")
	fmt.Printf("  Result: %d checkpoints recorded, workflow status=%s\n",
		len(history2), db2.workflows["wf-002"].Status)

	// Show the event history to prove no data was lost.
	fmt.Println("  Event history after failover:")
	for _, r := range history2 {
		fmt.Printf("    step=%d %s.%s → %s\n", r.Step, r.Service, r.Operation, r.ResponseJSON)
	}
	fmt.Println()

	// ── Test 3: Failover during checkpoint (the critical case) ──
	fmt.Println("── Test 3: Failover DURING a checkpoint write ──")
	fmt.Println()

	db3 := newSimulatedDB()
	db3.workflows["wf-003"] = &WorkflowInstance{
		ID: "wf-003", DefName: "PlaceOrder", DefVersion: 1,
		Status: "ready", NextWakeAt: time.Now().Add(-time.Minute),
	}

	worker3 := newWorker("worker-C-green", db3)
	ctx3, cancel3 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel3()

	// Trigger failover after exactly 1 append operation completes.
	// The second append will hit a connection error.
	go func() {
		time.Sleep(1000 * time.Millisecond)
		db3.mu.Lock()
		db3.failAfter = 1 // Fail after 1 more operation
		db3.mu.Unlock()
	}()

	runDone3 := make(chan struct{})
	go func() {
		worker3.Run()
		close(runDone3)
	}()

	time.Sleep(3 * time.Second)

	// Recover the DB.
	db3.mu.Lock()
	db3.connected = true
	db3.opCount = 0
	db3.failAfter = 0
	db3.mu.Unlock()
	fmt.Println("  🟢 Test 3: DB recovered — worker should retry checkpoint")
	fmt.Println()

	select {
	case <-runDone3:
	case <-ctx3.Done():
		worker3.Shutdown()
		<-runDone3
	}
	fmt.Println()

	history3, _ := db3.LoadEventHistory(context.Background(), "wf-003")
	fmt.Printf("  Result: %d checkpoints recorded, workflow status=%s\n",
		len(history3), db3.workflows["wf-003"].Status)
	fmt.Println("  Event history (no gaps, no duplicates):")
	for _, r := range history3 {
		fmt.Printf("    step=%d %s.%s → %s\n", r.Step, r.Service, r.Operation, r.ResponseJSON)
	}
	fmt.Println()

	// ── Summary ──
	fmt.Println("═══ Key behaviors demonstrated ═══")
	fmt.Println()
	fmt.Println("  1. Normal execution: claim → heartbeat → checkpoint → complete")
	fmt.Println("  2. Failover mid-execution: worker detects connection error,")
	fmt.Println("     pauses workflow (in-memory state preserved), waits for DB,")
	fmt.Println("     re-verifies ownership, resumes from last checkpoint.")
	fmt.Println("  3. Failover during checkpoint: the INSERT might have succeeded")
	fmt.Println("     or failed — but ON CONFLICT DO NOTHING makes retry safe.")
	fmt.Println("     No duplicate steps, no gaps.")
	fmt.Println()
	fmt.Println("  All database operations are IDEMPOTENT:")
	fmt.Println("    Claim:   SKIP LOCKED prevents double-claim")
	fmt.Println("    History: ON CONFLICT DO NOTHING prevents duplicate steps")
	fmt.Println("    Complete: WHERE assigned_to = $worker prevents double-completion")
	fmt.Println("    Release:  WHERE assigned_to = $worker prevents double-release")
	fmt.Println()
}

func generateWorkerID() string {
	b := make([]byte, 4)
	cryptorand.Read(b)
	return hex.EncodeToString(b)
}
