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

	// Same env var names and precedence as engine/testutil.TestDB(t,
	// DialectPostgres): CLEAT_TEST_POSTGRES, falling back to CLEAT_TEST_DB.
	// The default DSN differs from testutil.PostgresTestDSN's -- this suite
	// talks to the cluster brought up by docker-compose.cluster.yml, which
	// runs as the `cleat` role, not testutil's default superuser role -- so
	// it cannot just call that helper.
	dsn := os.Getenv("CLEAT_TEST_POSTGRES")
	if dsn == "" {
		dsn = os.Getenv("CLEAT_TEST_DB")
	}
	configured := dsn != ""
	if dsn == "" {
		dsn = "postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable"
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("opening cluster database: %v", err)
	}

	// Same distinction engine/testutil.TestDB makes, and for the same reason:
	// an unreachable database is only a legitimate skip when nobody asked for
	// one. This suite requires docker-compose.cluster.yml (or an equivalent)
	// to be up; if CLEAT_TEST_POSTGRES/CLEAT_TEST_DB was set explicitly and
	// the database at that DSN cannot be reached, that is a failure of the
	// configuration -- e.g. the job's cluster setup did not come up -- and
	// skipping it would hide exactly the kind of green-but-untested run
	// CLAUDE.md warns about. Only the true default (nobody configured
	// anything) is "nobody asked".
	if err := db.Ping(); err != nil {
		if !configured {
			// scripts/skip-baseline.txt records this site (tests/exhaustion,
			// clusterDB, 1). It is legitimate under scripts/check-skips.sh's
			// own test: nobody configured a DSN, which is the one thing a
			// skip is allowed to mean, and the sibling arm below is a
			// t.Fatalf rather than a skip. It is also inert in the one job
			// that runs this suite: ci.yml's "Cluster Integration Tests" sets
			// CLEAT_TEST_DB explicitly for this step, so `configured` is
			// always true there and this branch can never fire -- which is
			// why scripts/skip-budget.txt gives "cluster/exhaustion" a
			// budget of 0.
			t.Skipf("no cluster database configured (CLEAT_TEST_POSTGRES / CLEAT_TEST_DB "+
				"not set); default DSN %s is unreachable: %v -- this suite needs "+
				"docker-compose.cluster.yml up, so it skips rather than failing when "+
				"nobody asked for it", redact(dsn), err)
		}
		t.Fatalf("configured cluster database at %s is unreachable: %v -- this test "+
			"requires docker-compose.cluster.yml to be up", redact(dsn), err)
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
//
// It tracks whether the workflow was ever seen "running", because the two ways
// this can time out need opposite diagnoses and the difference is invisible in
// the final status alone:
//
//   - never running -- nothing ever claimed it. No worker is serving this task
//     queue, so the fence was never given the chance to fire and nothing here
//     is evidence about it.
//   - running -- a worker claimed it and did not stop it. That is the §2.5
//     defect this suite exists to catch.
//
// Until 2026-09-02 both produced "a runaway workflow was not terminated, so it
// is holding a worker's concurrency slot indefinitely". Against a database with
// no cluster attached that sentence is false in every clause: nothing was
// holding a slot, because nothing had claimed the workflow. The suite reads
// CLEAT_TEST_POSTGRES, which in a normal dev sandbox points at an ordinary test
// database rather than the cluster's, and clusterDB's precondition check pings
// the DSN -- proving it is *reachable*, not that a worker is behind it. So the
// misconfiguration was reported, confidently and specifically, as a broken
// execution fence.
func awaitTerminal(t *testing.T, db *sql.DB, id string, budget time.Duration) (status, errMsg string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	everRunning := false
	for time.Now().Before(deadline) {
		var msg sql.NullString
		if err := db.QueryRow(
			`SELECT status, error_msg FROM workflow_instances WHERE id = $1`, id,
		).Scan(&status, &msg); err != nil {
			t.Fatalf("polling %s: %v", id, err)
		}
		if status == "running" {
			everRunning = true
		}
		if status != "ready" && status != "running" {
			return status, msg.String
		}
		time.Sleep(2 * time.Second)
	}
	if !everRunning {
		t.Fatalf("workflow %s was still %q after %v and was never claimed by a "+
			"worker.\n\n"+
			"This is a configuration failure, not a fence failure: no worker is "+
			"serving task queue %q at the configured database, so the execution "+
			"fence was never given anything to stop and this run is evidence about "+
			"nothing.\n\n"+
			"This suite needs docker-compose.cluster.yml (or an equivalent) up, and "+
			"CLEAT_TEST_POSTGRES/CLEAT_TEST_DB pointing at THAT cluster's database. "+
			"A DSN that merely responds to a ping is not enough -- clusterDB checks "+
			"reachability, which an ordinary dev test database also satisfies.",
			id, status, budget, taskQueue)
	}
	t.Fatalf("workflow %s was still %q after %v -- a worker claimed it and did not "+
		"stop it, so a runaway workflow is holding a concurrency slot indefinitely "+
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
