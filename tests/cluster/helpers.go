package cluster

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// composeDir returns the project root directory containing the docker-compose files.
func composeDir() string {
	_, b, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(filepath.Dir(b)))
}

// SetupCluster starts the cluster via docker compose and waits for all workers
// to become healthy.
//
// `docker compose`, not `docker-compose`. The hyphenated v1 binary is gone from
// current GitHub runners and from Docker Desktop; ci.yml's cluster job has used
// the v2 plugin form throughout. This package still shelled out to v1, so the
// first thing it would have done on a runner is fail to find the command.
func SetupCluster(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping cluster test in short mode")
	}

	dir := composeDir()

	// Bring up postgres first, then workers.
	cmd := exec.Command("docker", "compose", //nolint:gosec // G204: fixed binary ("docker"), arguments as an array, no shell.
		"-f", filepath.Join(dir, "docker-compose.cluster.yml"),
		"up", "-d", "postgres",
	)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker-compose up postgres failed: %v\n%s", err, string(out))
	}

	// Wait for postgres to be healthy.
	waitForPostgres(t, 60*time.Second)

	// Bring up workers and dashboard.
	cmd = exec.Command("docker", "compose", //nolint:gosec // G204: fixed binary ("docker"), arguments as an array, no shell.
		"-f", filepath.Join(dir, "docker-compose.cluster.yml"),
		"up", "-d", "worker-1", "worker-2", "worker-3", "dashboard",
	)
	cmd.Dir = dir
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker-compose up workers failed: %v\n%s", err, string(out))
	}

	// Wait for all workers to be healthy.
	WaitForReady(t, 60*time.Second)
}

// TeardownCluster stops and removes all cluster containers and volumes.
func TeardownCluster(t *testing.T) {
	t.Helper()
	dir := composeDir()

	cmd := exec.Command("docker", "compose", //nolint:gosec // G204: fixed binary ("docker"), arguments as an array, no shell.
		"-f", filepath.Join(dir, "docker-compose.cluster.yml"),
		"down", "-v", "--remove-orphans",
	)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("docker-compose down failed (non-fatal): %v\n%s", err, string(out))
	}
}

// WaitForReady polls each worker's /healthz endpoint until all respond or the
// timeout elapses.
func WaitForReady(t *testing.T, timeout time.Duration) {
	t.Helper()

	workerPorts := []string{"8081", "8082", "8083", "8080"}
	deadline := time.Now().Add(timeout)
	for _, port := range workerPorts {
		for {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for worker on port %s", port)
			}
			resp, err := http.Get(fmt.Sprintf("http://localhost:%s/healthz", port))
			if err == nil && resp.StatusCode == http.StatusOK {
				resp.Body.Close()
				break
			}
			if resp != nil {
				resp.Body.Close()
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// waitForPostgres polls the postgres health endpoint until it responds or the
// timeout elapses.
func waitForPostgres(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for postgres")
		}
		cmd := exec.Command("docker", "exec", "cleat-postgres",
			"pg_isready", "-U", "cleat", "-d", "cleat",
		)
		out, err := cmd.CombinedOutput()
		if err == nil && len(out) > 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// The task queues this package uses.
//
// Deliberately *not* queue-1/2/3: those are what the compose cluster's three
// workers serve (docker-compose.cluster.yml), so a test that inserts a workflow
// there and then claims it races a live worker that is doing the same thing.
// TestFullClusterRestart failed exactly that way -- it released a workflow and
// asserted it could claim it back, and worker-1 got there first.
//
// These tests exercise the store's claim/release/replay bookkeeping against the
// cluster's database. The live workers' behaviour is tests/exhaustion's job.
const (
	TestQueue1 = "queue-cluster-tests-1"
	TestQueue2 = "queue-cluster-tests-2"
	TestQueue3 = "queue-cluster-tests-3"
)

// EnsureDef inserts the workflow_defs row a test's instances refer to.
//
// migrations/postgres/001_schema.sql puts
// `FOREIGN KEY (def_name, def_version) REFERENCES workflow_defs(name, version)`
// on workflow_instances, and every test in this package inserts instances with
// a raw INSERT naming a definition it never created. Against the real schema
// all eleven failed on that constraint -- the suite is in UNWIRED_SUITES
// (scripts/check-ci-package-coverage.sh) and nothing had ever run it.
//
// The bytes are a bare WASM header. Nothing here executes the module: these
// tests are about claiming, failover and replay bookkeeping, not about running
// guest code.
func EnsureDef(t *testing.T, db *sql.DB, name string, version int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points)
		VALUES ($1, $2, '\x0061736d01000000', '{}') ON CONFLICT DO NOTHING`, name, version); err != nil {
		t.Fatalf("EnsureDef(%s, %d): %v", name, version, err)
	}
}

// GetDB returns a *sql.DB connected to the cluster's PostgreSQL instance.
func GetDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("CLEAT_TEST_DB")
	if dsn == "" {
		dsn = "postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable" //nolint:gosec // G101: a localhost default DSN for the local compose stack, used only when CLEAT_TEST_DB is unset.
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("failed to ping database: %v", err)
	}
	return db
}
