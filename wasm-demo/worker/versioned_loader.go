// Worker loading versioned WASM modules from the database.
//
// This demonstrates the core operational pattern: a SINGLE worker binary
// loads different WASM blobs for different workflow instances based on
// the (def_name, def_version) stored in each instance.
//
// Build & run:
//   GOTOOLCHAIN=local /home/rcownie/go/bin/go build -o /tmp/versioned_loader ./worker/versioned_loader.go
//   /tmp/versioned_loader

package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// ==========================================================================
// The database model (tables shown as Go structs)
// ==========================================================================

// WorkflowDef is a row in the workflow_defs table.
// This is what "deploying a new workflow version" means — an INSERT into
// this table. The worker never needs to restart.
type WorkflowDef struct {
	Name      string
	Version   int
	WASMBytes []byte // compiled WASM module (~50-200KB with tinygo)
	CreatedAt time.Time
}

// WorkflowInstance is a row in the workflow_instances table.
// The (def_name, def_version) columns are a FOREIGN KEY pointing to the
// specific code version this instance must use.
type WorkflowInstance struct {
	ID         string
	DefName    string
	DefVersion int    // ← THIS is what determines which WASM blob to load
	Status     string
	Input      string
	AssignedTo string
}

// ==========================================================================
// The worker's WASM loader
// ==========================================================================

// WorkflowLoader fetches WASM blobs from the database by (name, version).
// In production this queries workflow_defs. In this demo it uses a map.
type WorkflowLoader struct {
	mu   sync.RWMutex
	defs map[defKey]*WorkflowDef // keyed by (name, version)

	// Statistics for observability.
	LoadCounts map[defKey]int
}

type defKey struct {
	Name    string
	Version int
}

func NewWorkflowLoader() *WorkflowLoader {
	return &WorkflowLoader{
		defs:       make(map[defKey]*WorkflowDef),
		LoadCounts: make(map[defKey]int),
	}
}

// Deploy inserts a new workflow definition into the database.
// This is the "deploy" operation — no worker restart needed.
//
//	SQL: INSERT INTO workflow_defs (name, version, wasm_bytes)
//	     VALUES ($1, $2, $3)
//	     ON CONFLICT (name, version) DO UPDATE SET wasm_bytes = $3
func (l *WorkflowLoader) Deploy(name string, version int, wasmBytes []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.defs[defKey{name, version}] = &WorkflowDef{
		Name:      name,
		Version:   version,
		WASMBytes: wasmBytes,
		CreatedAt: time.Now(),
	}
	fmt.Printf("[loader] Deployed %s v%d (%d bytes)\n", name, version, len(wasmBytes))
}

// Load returns the WASM bytes for a specific workflow definition.
// Called by the worker at the start of every workflow execution.
//
//	SQL: SELECT wasm_bytes FROM workflow_defs
//	     WHERE name = $1 AND version = $2
//
// In production, this result is cached in the worker's in-memory LRU cache
// so repeated loads of the same version don't hit the database.
func (l *WorkflowLoader) Load(name string, version int) (*WorkflowDef, error) {
	l.mu.RLock()
	def, ok := l.defs[defKey{name, version}]
	l.mu.RUnlock()

	if ok {
		// Cache hit — in production this is an in-memory LRU cache.
		l.mu.Lock()
		l.LoadCounts[defKey{name, version}]++
		l.mu.Unlock()
		return def, nil
	}

	// Cache miss — query the database.
	// In production:
	//   row := db.QueryRow("SELECT name, version, wasm_bytes, created_at FROM workflow_defs WHERE name = $1 AND version = $2", name, version)
	//   err := row.Scan(&def.Name, &def.Version, &def.WASMBytes, &def.CreatedAt)

	return nil, fmt.Errorf("workflow definition not found: %s v%d", name, version)
}

// ActiveVersions returns all versions of a workflow that have active instances.
// Used to know which versions can be garbage-collected.
//
//	SQL: SELECT DISTINCT def_name, def_version
//	     FROM workflow_instances
//	     WHERE status IN ('ready', 'running')
func (l *WorkflowLoader) ActiveVersions(db *SimulatedDB) map[string][]int {
	active := make(map[string][]int)
	for _, wf := range db.Instances {
		if wf.Status == "ready" || wf.Status == "running" {
			versions := active[wf.DefName]
			found := false
			for _, v := range versions {
				if v == wf.DefVersion {
					found = true
					break
				}
			}
			if !found {
				active[wf.DefName] = append(versions, wf.DefVersion)
			}
		}
	}
	return active
}

// Stats returns load statistics for observability.
func (l *WorkflowLoader) Stats() map[string]int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	stats := make(map[string]int)
	for k, count := range l.LoadCounts {
		stats[fmt.Sprintf("%s_v%d", k.Name, k.Version)] = count
	}
	return stats
}

// ==========================================================================
// Simulated database of workflow instances
// ==========================================================================

type SimulatedDB struct {
	Instances map[string]*WorkflowInstance
}

func NewSimulatedDB() *SimulatedDB {
	return &SimulatedDB{
		Instances: make(map[string]*WorkflowInstance),
	}
}

func (db *SimulatedDB) Enqueue(wf *WorkflowInstance) {
	db.Instances[wf.ID] = wf
}

// Claim atomically dequeues a workflow. In production this is:
//
//	UPDATE workflow_instances
//	SET status = 'running', assigned_to = $1, heartbeat_at = now()
//	WHERE id = (
//	    SELECT id FROM workflow_instances
//	    WHERE status = 'ready' AND next_wake_at <= now()
//	    ORDER BY created_at LIMIT 1
//	    FOR UPDATE SKIP LOCKED
//	)
//	RETURNING id, def_name, def_version, input
func (db *SimulatedDB) Claim(workerID string) *WorkflowInstance {
	for id, wf := range db.Instances {
		if wf.Status == "ready" {
			wf.Status = "running"
			wf.AssignedTo = workerID
			return wf
		}
		_ = id
	}
	return nil
}

// ==========================================================================
// Worker
// ==========================================================================

type Worker struct {
	ID     string
	DB     *SimulatedDB
	Loader *WorkflowLoader
}

func NewWorker(id string, db *SimulatedDB, loader *WorkflowLoader) *Worker {
	return &Worker{ID: id, DB: db, Loader: loader}
}

// ExecuteLoop claims workflows and executes them.
func (w *Worker) ExecuteLoop() {
	for {
		time.Sleep(300 * time.Millisecond)

		wf := w.DB.Claim(w.ID)
		if wf == nil {
			continue
		}

		w.executeWorkflow(wf)
	}
}

func (w *Worker) executeWorkflow(wf *WorkflowInstance) {
	fmt.Printf("[worker %s] Claimed %s (%s v%d)\n", w.ID, wf.ID, wf.DefName, wf.DefVersion)

	// ---- THE KEY OPERATION: load the right WASM version ----
	def, err := w.Loader.Load(wf.DefName, wf.DefVersion)
	if err != nil {
		fmt.Printf("[worker %s] ❌ %s: %v\n", w.ID, wf.ID, err)
		wf.Status = "failed"
		return
	}

	fmt.Printf("[worker %s]   Loaded %s v%d (%d bytes)\n",
		w.ID, def.Name, def.Version, len(def.WASMBytes))

	// ---- Execute the WASM module ----
	// In production: instantiate wazero runtime, load WASM module, call
	// the exported workflow function. The module calls back into host
	// functions for durable_call, durable_log, etc.
	result := w.simulateWASMExecution(wf, def)

	fmt.Printf("[worker %s]   Completed %s → %s\n", w.ID, wf.ID, result)
	wf.Status = "done"
}

// simulateWASMExecution stands in for actual wazero invocation.
// Each version of the workflow has different behavior (different API
// call order, different service names) to show that the right code
// runs for each version instance.
func (w *Worker) simulateWASMExecution(wf *WorkflowInstance, def *WorkflowDef) string {
	// The WASM bytes would be loaded into wazero here:
	//   module, _ := runtime.Instantiate(ctx, def.WASMBytes)
	//   result, _ := module.ExportedFunction("place_order").Call(ctx, wf.Input)
	//
	// For the demo, the "behavior" is encoded in the version number.

	switch {
	case def.Name == "PlaceOrder" && def.Version == 1:
		// v1: catalog → inventory → payment → shipping
		_ = wf.Input
		return "v1-result-OK"

	case def.Name == "PlaceOrder" && def.Version == 2:
		// v2: payment FIRST → catalog → inventory → shipping (changed order!)
		_ = wf.Input
		return "v2-result-OK"

	case def.Name == "PlaceOrder" && def.Version == 3:
		// v3: adds fraud check step
		_ = wf.Input
		return "v3-result-OK"

	default:
		return fmt.Sprintf("v%d-result", def.Version)
	}
}

// ==========================================================================
// Demo scenario
// ==========================================================================

func main() {
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println("  Versioned WASM Loading — Worker Demo")
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println()
	fmt.Println("  Scenario:")
	fmt.Println("    • PlaceOrder v1 deployed on Monday (Alice's order starts)")
	fmt.Println("    • PlaceOrder v2 deployed on Tuesday (Bob's order starts)")
	fmt.Println("    • PlaceOrder v3 deployed on Wednesday (Carol's order starts)")
	fmt.Println("    • Alice's v1 workflow is STILL RUNNING (waiting for approval)")
	fmt.Println("    • A single worker pool handles all three versions")
	fmt.Println("    • Deploying v2 and v3 required NO worker restarts")
	fmt.Println()

	// ---- Setup ----
	loader := NewWorkflowLoader()
	db := NewSimulatedDB()

	// ---- Monday: deploy v1 ----
	fmt.Println("── Monday 09:00: Deploy PlaceOrder v1 ──")
	loader.Deploy("PlaceOrder", 1, []byte("(wasm-binary-v1-50KB)"))

	// Alice starts an order. Her workflow begins executing with v1 code.
	db.Enqueue(&WorkflowInstance{
		ID: "order-alice-001", DefName: "PlaceOrder", DefVersion: 1,
		Status: "ready", Input: `{"user":"alice","items":["widget"]}`,
	})

	// Start a worker.
	worker := NewWorker("worker-01", db, loader)
	fmt.Println()

	// ---- Tuesday: deploy v2 (Alice's v1 is still running) ----
	fmt.Println("── Tuesday 14:00: Deploy PlaceOrder v2 ──")
	loader.Deploy("PlaceOrder", 2, []byte("(wasm-binary-v2-52KB)"))

	// Bob starts an order. His workflow gets v2.
	db.Enqueue(&WorkflowInstance{
		ID: "order-bob-002", DefName: "PlaceOrder", DefVersion: 2,
		Status: "ready", Input: `{"user":"bob","items":["gadget"]}`,
	})

	// Alice's v1 workflow is still in the queue (simulated: it was
	// waiting for approval and got rescheduled).
	db.Enqueue(&WorkflowInstance{
		ID: "order-alice-001", DefName: "PlaceOrder", DefVersion: 1,
		Status: "ready", Input: `{"user":"alice","items":["widget"]}`,
	})

	fmt.Println()

	// ---- Wednesday: deploy v3 ----
	fmt.Println("── Wednesday 09:00: Deploy PlaceOrder v3 ──")
	loader.Deploy("PlaceOrder", 3, []byte("(wasm-binary-v3-55KB)"))

	// Carol starts an order with v3.
	db.Enqueue(&WorkflowInstance{
		ID: "order-carol-003", DefName: "PlaceOrder", DefVersion: 3,
		Status: "ready", Input: `{"user":"carol","items":["doohickey"]}`,
	})

	fmt.Println()

	// ---- Now execute all queued workflows ----
	fmt.Println("── Worker processing queue ──")
	fmt.Println()

	// Run the worker until all workflows are done.
	done := make(chan struct{})
	go func() {
		worker.ExecuteLoop()
	}()

	// Wait for all workflows to complete (poll for simplicity).
	time.Sleep(3 * time.Second)
	close(done)

	// ---- Show results ----
	fmt.Println()
	fmt.Println("── Results ──")
	fmt.Println()

	fmt.Println("  Workflow instances processed:")
	for id, wf := range db.Instances {
		fmt.Printf("    %s: %s v%d → %s\n", id, wf.DefName, wf.DefVersion, wf.Status)
	}
	fmt.Println()
	fmt.Println("  Workflow definitions in database:")
	for key := range loader.defs {
		fmt.Printf("    %s v%d\n", key.Name, key.Version)
	}
	fmt.Println()
	fmt.Println("  Load statistics (which versions were loaded by workers):")
	for name, count := range loader.Stats() {
		fmt.Printf("    %s: loaded %d times\n", name, count)
	}
	fmt.Println()

	// ---- Show active versions ----
	active := loader.ActiveVersions(db)
	fmt.Println("  Active versions (still have running instances):")
	for name, versions := range active {
		if len(versions) > 0 {
			fmt.Printf("    %s: v%v\n", name, versions)
		}
	}

	if len(active) == 0 {
		fmt.Println("    (none — all workflows completed)")
	}
	fmt.Println()

	// ---- Summary ----
	fmt.Println("═══ Key Properties ═══")
	fmt.Println()
	fmt.Println("  1. Deploying v2 and v3 were database INSERTs.")
	fmt.Println("     No worker restarts. No new worker pools.")
	fmt.Println()
	fmt.Println("  2. Alice's (v1), Bob's (v2), and Carol's (v3) workflows")
	fmt.Println("     all executed on the SAME worker process.")
	fmt.Println()
	fmt.Println("  3. Each workflow instance loaded its specific WASM blob")
	fmt.Println("     based on (def_name, def_version) in the instance row.")
	fmt.Println()
	fmt.Println("  4. When all v1 instances complete, v1 can be marked")
	fmt.Println("     deprecated. Query for active instances by version:")
	fmt.Println("     SELECT COUNT(*) FROM workflow_instances")
	fmt.Println("     WHERE def_name = $1 AND def_version = $2")
	fmt.Println("       AND status IN ('ready', 'running')")
	fmt.Println()
	fmt.Println("  5. The worker binary is a STABLE RUNTIME. It changes only")
	fmt.Println("     when the host interface changes (new durable_* function),")
	fmt.Println("     not when workflow business logic changes.")
	fmt.Println()
	fmt.Println("  ── The SQL that makes this work ──")
	fmt.Println()
	fmt.Println("  -- Deploying a new version:")
	fmt.Println("  INSERT INTO workflow_defs (name, version, wasm_bytes)")
	fmt.Println("  VALUES ('PlaceOrder', 2, decode('...', 'base64'));")
	fmt.Println()
	fmt.Println("  -- Worker loads the right version for each instance:")
	fmt.Println("  SELECT wd.wasm_bytes")
	fmt.Println("  FROM workflow_instances wi")
	fmt.Println("  JOIN workflow_defs wd ON wd.name = wi.def_name")
	fmt.Println("                        AND wd.version = wi.def_version")
	fmt.Println("  WHERE wi.id = $1;")
	fmt.Println()
	fmt.Println("  -- Check if a version can be garbage-collected:")
	fmt.Println("  SELECT version, COUNT(*) as active_count")
	fmt.Println("  FROM workflow_instances")
	fmt.Println("  WHERE def_name = 'PlaceOrder'")
	fmt.Println("    AND status IN ('ready', 'running')")
	fmt.Println("  GROUP BY version;")
	fmt.Println("  -- If active_count = 0 for a version, it's safe to deprecate.")
	fmt.Println()
}
