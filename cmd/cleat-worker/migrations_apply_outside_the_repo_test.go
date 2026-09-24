// cleat#1968. cleat-worker read migrations from a bare relative path,
// "migrations" (main.go, both the core and per-tenant call sites), which
// only resolved when the worker's own working directory happened to be a
// cleat source checkout. Started from a project scaffolded by `cleat init`
// -- the only layout a real user has -- it refused to boot:
//
//	read migrations: read directory migrations/postgres: open migrations/postgres: no such file or directory
//
// A test run from this package's own directory could not have caught that:
// the relative path resolves there by the same coincidence. This builds the
// worker binary and runs it with its working directory set to a TEMP
// DIRECTORY OUTSIDE the repo -- the issue's own stated requirement -- against
// a real, freshly created (empty) Postgres database, and asserts migrations
// were actually applied rather than merely that the process exited 0.
package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWorkerMigratesFromOutsideTheRepo(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a second cleat-worker binary and starts a container; skipped in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(repoRoot, "go.work")); statErr != nil {
		t.Fatalf("computed repo root %s has no go.work -- wrong directory: %v", repoRoot, statErr)
	}

	binDir := t.TempDir()
	workerBin := filepath.Join(binDir, "cleat-worker")
	build := exec.Command("go", "build", "-o", workerBin, "./cmd/cleat-worker")
	build.Dir = repoRoot
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("go build ./cmd/cleat-worker: %v\n%s", buildErr, out)
	}

	dsn, containerName := startWorkerTestPostgres(t)

	// The point of the test: this is NOT repoRoot, and is not anywhere
	// beneath it -- t.TempDir() is under the OS temp directory. A worker
	// that still needed to be started from the repo would fail here exactly
	// as cleat#1968 describes.
	outsideDir := t.TempDir()
	if strings.HasPrefix(outsideDir, repoRoot) {
		t.Fatalf("t.TempDir() %s is inside repoRoot %s -- this test needs a directory outside the "+
			"repo to mean anything", outsideDir, repoRoot)
	}

	const apiAddr = ":18299"
	waitForWorkerTestPortFree(t, 18299)

	// --migrate-on-start: this test is about WHERE the embedded migrations are read
	// from, and it starts the worker on an EMPTY database. A worker no longer
	// migrates unless asked (cleat#2117); without the flag it refuses to start, which
	// is asserted in a_migration_is_a_deploy_step_test.go.
	worker := exec.Command(workerBin,
		"--db="+dsn,
		"--api-addr="+apiAddr,
		"--require-auth=false",
		"--migrate-on-start",
	)
	worker.Dir = outsideDir
	worker.Env = os.Environ()
	var workerOut strings.Builder
	worker.Stdout = &workerOut
	worker.Stderr = &workerOut
	if startErr := worker.Start(); startErr != nil {
		t.Fatalf("start cleat-worker: %v", startErr)
	}
	t.Cleanup(func() {
		if worker.Process != nil {
			worker.Process.Kill()
			worker.Wait()
		}
		exec.Command("docker", "rm", "-f", containerName).Run()
		if t.Failed() {
			t.Logf("worker output:\n%s", workerOut.String())
		}
	})

	waitForWorkerTestHealthz(t, "http://localhost:18299/healthz")

	// NOT MERELY THAT IT BOOTED: query the database directly for the table
	// migration 001 creates. A worker that skipped migrations entirely (a
	// bug in the embedded-FS wiring, say) could still pass healthz if
	// healthz does not itself depend on the schema existing.
	out, psqlErr := exec.Command("docker", "exec", containerName,
		"psql", "-U", "cleat", "-d", "cleat", "-tAc",
		"SELECT count(*) FROM workflow_instances").CombinedOutput()
	if psqlErr != nil {
		t.Fatalf("querying workflow_instances after boot: %v\n%s\n\nworker output:\n%s",
			psqlErr, out, workerOut.String())
	}
	if strings.TrimSpace(string(out)) != "0" {
		t.Fatalf("expected workflow_instances to exist and be empty, got %q", out)
	}

	countOut, countErr := exec.Command("docker", "exec", containerName,
		"psql", "-U", "cleat", "-d", "cleat", "-tAc",
		"SELECT count(*) FROM schema_migrations").CombinedOutput()
	if countErr != nil {
		t.Fatalf("querying schema_migrations after boot: %v\n%s", countErr, countOut)
	}
	applied := strings.TrimSpace(string(countOut))
	if applied == "0" || applied == "" {
		t.Fatalf("schema_migrations has %q rows after boot -- migrations did not apply", applied)
	}
	t.Logf("applied %s migrations from outside the repo", applied)
}

func startWorkerTestPostgres(t *testing.T) (dsn, containerName string) {
	t.Helper()
	containerName = "cleat-test-1968-pg-" + t.Name()
	exec.Command("docker", "rm", "-f", containerName).Run()

	run := exec.Command("docker", "run", "-d", "--name", containerName,
		"-e", "POSTGRES_USER=cleat",
		"-e", "POSTGRES_PASSWORD=cleat",
		"-e", "POSTGRES_DB=cleat",
		"-p", "0:5432",
		"postgres:16",
	)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run postgres: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", containerName).Run() })

	portOut, err := exec.Command("docker", "port", containerName, "5432/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	hostPort := strings.TrimSpace(string(portOut))
	if i := strings.LastIndex(hostPort, ":"); i >= 0 {
		hostPort = hostPort[i+1:]
	}
	if hostPort == "" {
		t.Fatalf("could not determine published port from %q", string(portOut))
	}
	dsn = "postgres://cleat:cleat@localhost:" + hostPort + "/cleat?sslmode=disable"

	// The official postgres image starts twice (initdb, then the real
	// server); pg_isready can answer success during the first instance.
	// Confirmed directly in cleat#1969's fix -- wait for the log line twice.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		if strings.Count(string(logs), "database system is ready to accept connections") >= 2 {
			if err := exec.Command("docker", "exec", containerName, "pg_isready", "-U", "cleat").Run(); err == nil {
				return dsn, containerName
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("postgres did not become ready within 60s")
	return "", ""
}

func waitForWorkerTestPortFree(t *testing.T, port int) {
	t.Helper()
	addr := net.JoinHostPort("localhost", strconv.Itoa(port))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return
		}
		conn.Close()
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("port %d is still in use after 10s", port)
}

func waitForWorkerTestHealthz(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("worker at %s did not become healthy within 30s", url)
}
