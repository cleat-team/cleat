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

// SetupCluster starts the cluster via docker-compose and waits for all workers to become healthy.
func SetupCluster(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping cluster test in short mode")
	}

	dir := composeDir()

	// Bring up postgres first, then workers.
	cmd := exec.Command("docker-compose",
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
	cmd = exec.Command("docker-compose",
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

	cmd := exec.Command("docker-compose",
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

// GetDB returns a *sql.DB connected to the cluster's PostgreSQL instance.
func GetDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("CLEAT_TEST_DB")
	if dsn == "" {
		dsn = "postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable"
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
