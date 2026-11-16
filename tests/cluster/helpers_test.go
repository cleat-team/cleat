package cluster

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/cleat-team/cleat/engine/testutil"
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

// WaitForReady polls each worker's /readyz endpoint (the database answered, not draining) until all respond or the
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
			resp, err := http.Get(fmt.Sprintf("http://localhost:%s/readyz", port))
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

// ClusterDSN resolves this suite's PostgreSQL DSN, tagged for the cleat#982
// gate. cleat#1501.
//
// ONE FUNCTION BECAUSE THERE WERE FIVE COPIES. GetDB here and requireDB in
// cluster_test.go, failover_test.go, replay_test.go and scale_test.go each read
// CLEAT_TEST_DB and fell back to the same literal. cleat#1501 cited GetBD alone,
// and that is not an oversight in the issue so much as what duplication does to
// a census: four of the five were invisible because nothing named them
// together.
//
// TagPostgresDSN, not PostgresTestDSN. The gate identifies our PostgreSQL
// sessions by application_name and nothing else, so an untagged connection is
// reported as a stranger and refuses somebody else's run. But PostgresTestDSN
// also supplies ITS default -- testutil's superuser role -- and this suite
// talks to the compose stack, which runs as `cleat`. The tag is what is wanted
// here; the fallback is not.
func ClusterDSN() string {
	dsn, _ := clusterDSNWithConfigured()
	return dsn
}

// clusterDSNWithConfigured is ClusterDSN plus the distinction this suite was
// missing (cleat#3952): whether ANYBODY ASKED for a database.
//
// tests/exhaustion has had this split since it was written -- configured and
// unreachable is a t.Fatalf, only "nobody configured one" is a skip -- and its
// budget is 0 for exactly that reason. This suite skipped on a ping failure
// whatever the environment said, so in CI (where CLEAT_TEST_DB is always set)
// a cluster that was down read as "nothing to test here" and the required job
// stayed green. The window is not hypothetical: this suite runs
// TestKillPostgresAndRestart itself, which stops and restarts the container,
// and go test runs the files in name order -- so five tests after it
// (TestFullClusterRestart, then replay_test.go and scale_test.go) open a
// database against a server that was just restarted.
func clusterDSNWithConfigured() (dsn string, configured bool) {
	if v := os.Getenv("CLEAT_TEST_DB"); v != "" {
		return testutil.TagPostgresDSN(v), true
	}
	// NO FALLBACK DSN (cleat#3952). There used to be a `localhost:5432`
	// default, and it made an unconfigured run CONNECT to whatever answered
	// there -- on a developer machine that can be an unrelated PostgreSQL, and
	// this suite's cleanup deletes `test-cluster-%` rows. "Nobody asked for a
	// database" and "a database we did not ask about" must not be the same
	// outcome, so the unconfigured arm now names the variable and skips.
	//
	// CI is unaffected: both test steps in the cluster job set CLEAT_TEST_DB
	// explicitly, and the skip budget below would catch it as one skip against
	// a budget of 0 if that ever stopped being true.
	return "", false
}

// openClusterDB is the single place this suite opens and pings its database.
//
// ONE FUNCTION BECAUSE THERE WERE FOUR COPIES: testStore, requireDB,
// replayStoreDB and scaleStore each had the same open-then-ping-then-skip block.
// Consolidating them is what makes the configured/unconfigured split a single
// decision rather than four that can drift -- and the split is the substance of
// cleat#3952, not a tidy-up riding along with it.
func openClusterDB(t *testing.T) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping cluster test in short mode")
	}
	dsn, configured := clusterDSNWithConfigured()
	if !configured {
		// BEFORE sql.Open, so an unconfigured run opens no connection at all.
		t.Skipf("no cluster database configured: set CLEAT_TEST_DB to the " +
			"database docker-compose.cluster.yml serves. This suite has no " +
			"default DSN on purpose -- a localhost fallback made an unconfigured " +
			"run delete test-cluster-%% rows from whatever answered on 5432 " +
			"(cleat#3952)")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		// Unreachable while the skip above stands: configured is always true
		// here. Kept as a Fatalf rather than deleted so a future edit that
		// relaxes the skip cannot turn an open failure into a silent skip.
		t.Fatalf("configured cluster database at %s cannot be opened: %v -- "+
			"CLEAT_TEST_DB was set, so this is a failure of the configuration "+
			"rather than a missing prerequisite", redactDSN(dsn), err)
	}
	// A CONFIGURED DSN IS RETRIED BEFORE IT IS CALLED DEAD, because this suite
	// manufactures the window a retry exists for: TestKillPostgresAndRestart
	// stops and restarts Postgres, and five tests after it open a database
	// (TestFullClusterRestart, then all three of replay_test.go and both of
	// scale_test.go -- go test runs the files in name order). It waits for its
	// own ping to recover before returning, so the usual case never reaches
	// here, but "the next test opened the pool while the server was still
	// coming back" must be a pass 30s later rather than a red required job. A
	// dead DSN is still red; it is just red 30s later.
	//
	// UNCONFIGURED does not wait: nobody asked for a database, and making a
	// local `go test` sit for 30s before skipping would be a cost with no
	// failure to catch.
	if err := pingWithRetry(db); err != nil {
		t.Fatalf("configured cluster database at %s is still unreachable after "+
			"%s: %v -- this suite requires docker-compose.cluster.yml to be up, "+
			"and CLEAT_TEST_DB was set explicitly, so this is a failure of the "+
			"configuration and not an absent prerequisite",
			redactDSN(dsn), clusterPingTimeout, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// stopPostgresContainer stops the cluster's postgres container, and says which
// of three situations it is in (cleat#3952):
//
//   - the container does not exist: SKIP, naming the environment. tests/cluster
//     is in tier2.packages, and Tier 2 runs with docker but no compose stack,
//     so this is the ordinary case there rather than a broken run.
//   - it exists and stops: return, so the test can restart it.
//   - it exists and will not stop: FATAL. The test's subject is what happens
//     when Postgres goes away; if it cannot be stopped, the run measured
//     nothing. The cluster job always has this container, and its
//     `cluster/suite` budget is 0, so a skip there is still caught.
func stopPostgresContainer(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "inspect", "--format", "{{.State.Running}}",
		"cleat-postgres").Run(); err != nil {
		t.Skipf("no cleat-postgres container: this job does not run the cluster "+
			"stack (docker-compose.cluster.yml), so there is nothing to stop: %v", err)
	}
	if out, err := exec.Command("docker", "stop", "cleat-postgres").CombinedOutput(); err != nil {
		t.Fatalf("cannot stop the postgres container, so this test cannot test "+
			"what it is for: %v\n%s", err, string(out))
	}
}

// clusterPingTimeout bounds how long a CONFIGURED DSN may be unreachable
// before the test fails. It covers the restart window, not a dead database.
//
// FIVE SECONDS, not thirty, because ELEVEN TESTS OPEN A DATABASE and each pays
// the bound: 11 x 30s is 330s against the package's 300s `go test -timeout`,
// so a genuinely dead DSN would burn the whole timeout before reporting
// anything -- measured on #3963 at rc 1 after 302s. At 5s the worst case is
// ~55s, and the window this exists for is short: TestKillPostgresAndRestart
// waits for its own ping to recover (up to ~29s) before returning.
const clusterPingTimeout = 5 * time.Second

// pingWithRetry returns nil once the database answers, the last ping error if
// a CONFIGURED one never does within clusterPingTimeout, or the first error
// immediately when nobody configured a DSN.
func pingWithRetry(db *sql.DB) error {
	err := db.Ping()
	if err == nil {
		return nil
	}
	deadline := time.Now().Add(clusterPingTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if err = db.Ping(); err == nil {
			return nil
		}
	}
	return err
}

// redactDSN strips the password from a DSN so it can appear in test output.
// Mirrors tests/exhaustion's redact and engine/testutil's redactDSN; kept local
// rather than exported because consolidating the three is a change to a public
// package's surface and does not belong in a fix about skip budgets.
func redactDSN(dsn string) string {
	if i := strings.Index(dsn, "@"); i >= 0 {
		if j := strings.Index(dsn, "://"); j >= 0 && j+3 < i {
			return dsn[:j+3] + "***" + dsn[i:]
		}
	}
	return dsn
}

// GetDB returns a *sql.DB connected to the cluster's PostgreSQL instance.
//
// Routed through openClusterDB so it inherits the configured/unconfigured
// split and the no-fallback rule (cleat#3952): a second opener that Fatals on
// an empty DSN would be a way around both.
func GetDB(t *testing.T) *sql.DB {
	t.Helper()
	return openClusterDB(t)
}
