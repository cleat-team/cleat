// Package exhaustion is IMPROVEMENT-PLAN §2.5: deploy a workflow that never
// returns, and assert the worker terminates it and stays useful.
//
// It runs against the cluster from docker-compose.cluster.yml, so it exercises
// the shipped container image rather than an in-process engine. That is the
// whole point. §2.28 was a defect that every in-process test missed by
// construction: the fence works on wasmtime, every unit test uses wasmtime, and
// the image shipped without it. Only something that runs the real image can
// tell those apart.
//
// Deliberately not placed in tests/cluster: that suite is run by nothing at all
// (see UNWIRED_SUITES in scripts/check-ci-package-coverage.sh), so a test added
// there would never execute.
package exhaustion

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// The compose file does not set --wasm-instance-timeout, so the worker uses the
// 30s default. Everything here is sized against that.
const (
	instanceTimeout = 30 * time.Second
	terminateBudget = 90 * time.Second
	completeBudget  = 60 * time.Second
	taskQueue       = "queue-1"
)

func clusterDB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("CLEAT_CLUSTER_DB")
	if dsn == "" {
		dsn = "postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable"
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("opening cluster database: %v", err)
	}
	// Fatal, not Skip. This test is only ever run by a job that brings the
	// cluster up, so an unreachable database means that job's setup failed --
	// which is a failure, not "nobody asked". See the skip-budget rationale in
	// ci.yml.
	if err := db.Ping(); err != nil {
		t.Fatalf("cluster database unreachable at %s: %v -- this test requires "+
			"docker-compose.cluster.yml to be up", redact(dsn), err)
	}
	return db
}

func redact(dsn string) string {
	if i := strings.Index(dsn, "@"); i >= 0 {
		if j := strings.Index(dsn, "://"); j >= 0 && j+3 < i {
			return dsn[:j+3] + "***" + dsn[i:]
		}
	}
	return dsn
}

// deploySpin compiles testdata/spin and inserts it as a workflow definition.
func deploySpin(t *testing.T, db *sql.DB) {
	t.Helper()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	outDir := t.TempDir()

	cmd := exec.Command("go", "run", filepath.Join(root, "cmd", "cleat"),
		"build", "--target", "go", "-o", outDir, filepath.Join(root, "testdata", "spin"))
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the spin fixture: %v\n%s", err, out)
	}

	wasmPath := filepath.Join(outDir, "spin.wasm")
	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("reading %s: %v", wasmPath, err)
	}

	_, err = db.Exec(`
		INSERT INTO workflow_defs
			(name, version, wasm_bytes, entry_points, min_version,
			 max_history_length, dag_spec, task_queue, abi_version, plugin_deps)
		VALUES ('spin', 1, $1, ARRAY['spin'], 1, 10000, '{}'::jsonb, $2, 1, '{}'::jsonb)
		ON CONFLICT (name, version) DO UPDATE SET wasm_bytes = EXCLUDED.wasm_bytes`,
		wasm, taskQueue)
	if err != nil {
		t.Fatalf("deploying the spin definition: %v", err)
	}
}

// start queues one instance. __entry_point is required in the input: the
// definition's entry_points list alone does not resolve it, and a workflow
// without it fails instantly with "cannot determine entry point" -- which looks
// enough like a fence firing to be worth naming here.
func start(t *testing.T, db *sql.DB, id string, iterations int64) {
	t.Helper()
	input := fmt.Sprintf(`{"__entry_point":"spin","iterations":%d}`, iterations)
	if _, err := db.Exec(`
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES ($1, 'spin', 1, 'ready', $2::jsonb, $3)`, id, input, taskQueue); err != nil {
		t.Fatalf("starting workflow %s: %v", id, err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id)
	})
}

// awaitTerminal polls until the workflow leaves ready/running.
func awaitTerminal(t *testing.T, db *sql.DB, id string, budget time.Duration) (status, errMsg string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		var msg sql.NullString
		if err := db.QueryRow(
			`SELECT status, error_msg FROM workflow_instances WHERE id = $1`, id,
		).Scan(&status, &msg); err != nil {
			t.Fatalf("polling %s: %v", id, err)
		}
		if status != "ready" && status != "running" {
			return status, msg.String
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("workflow %s was still %q after %v -- a runaway workflow was not "+
		"terminated, so it is holding a worker's concurrency slot indefinitely "+
		"(the worker's --wasm-instance-timeout is %v)", id, status, budget, instanceTimeout)
	return "", ""
}

// TestRunawayWorkflowIsTerminatedAndWorkerSurvives is §2.5, and the end-to-end
// regression test for §2.28.
func TestRunawayWorkflowIsTerminatedAndWorkerSurvives(t *testing.T) {
	db := clusterDB(t)
	defer db.Close()

	deploySpin(t, db)

	// ~3 minutes of arithmetic at the measured rate: nothing finishes this by
	// accident, so a terminal status can only mean the fence fired.
	runaway := fmt.Sprintf("spin-runaway-%d", time.Now().UnixNano())
	start(t, db, runaway, 100000000000)

	began := time.Now()
	status, errMsg := awaitTerminal(t, db, runaway, terminateBudget)
	elapsed := time.Since(began)

	if status != "failed" {
		t.Errorf("runaway workflow ended %q, want \"failed\"", status)
	}

	// Name the mechanism. Any terminal status would satisfy a looser assertion,
	// including the workflow erroring out for an unrelated reason before it ever
	// spun -- §2.10's failure mode exactly.
	if !strings.Contains(errMsg, "execution time limit exceeded") {
		t.Errorf("runaway workflow failed, but not on the execution fence: %s", errMsg)
	}
	if elapsed < instanceTimeout/2 {
		t.Errorf("runaway workflow ended after only %v, which is too early for a %v "+
			"budget -- something other than the fence stopped it: %s",
			elapsed, instanceTimeout, errMsg)
	}

	// The other half of §2.5, and the one a fence test usually forgets: the
	// worker has to still be useful. Terminating a runaway workflow by wedging
	// or crashing the worker would pass every assertion above.
	normal := fmt.Sprintf("spin-normal-%d", time.Now().UnixNano())
	start(t, db, normal, 1000)

	status, errMsg = awaitTerminal(t, db, normal, completeBudget)
	if status != "done" {
		t.Fatalf("after terminating a runaway workflow the cluster did not complete "+
			"an ordinary one: status %q, error %q -- the worker did not survive in "+
			"any useful sense", status, errMsg)
	}
}
