package cluster

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/cleat-team/cleat/engine"
)

// requireDB is like testStore but skips if CLEAT_TEST_DB is unavailable.
func requireDB(t *testing.T) (*sql.DB, *engine.PostgresStore) {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping in short mode")
	}
	dsn := os.Getenv("CLEAT_TEST_DB")
	if dsn == "" {
		dsn = "postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable"
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("Skipping: no database available: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("Skipping: cannot ping database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, engine.NewPostgresStore(db, TestQueue1, TestQueue2, TestQueue3)
}

// TestKillWorkerMidExecution kills one worker and verifies that in-flight
// workflows complete on the remaining workers.
func TestKillWorkerMidExecution(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cluster test in short mode")
	}

	db, store := requireDB(t)
	ctx := context.Background()
	cleanTestWorkflows(t, db)

	// Create multiple workflows assigned to different queues.
	const numWorkflows = 6
	workflowIDs := make([]string, 0, numWorkflows)
	for i := 0; i < numWorkflows; i++ {
		id := fmt.Sprintf("test-cluster-failover-%d", i)
		q := []string{TestQueue1, TestQueue2, TestQueue3}[i%3]
		EnsureDef(t, db, "failover-test", 1)
		_, err := db.Exec(`
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
			VALUES ($1, 'failover-test', 1, 'ready', '{}', $2)
			ON CONFLICT (id) DO NOTHING
		`, id, q)
		if err != nil {
			t.Fatalf("Insert workflow %d: %v", i, err)
		}
		workflowIDs = append(workflowIDs, id)
	}
	defer func() {
		for _, id := range workflowIDs {
			db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, id)
			db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id)
		}
	}()

	// Claim workflows using multiple workers to simulate distribution.
	//
	// The claimed instance is kept, not just its ID: ReleaseWorkflow needs the
	// generation ClaimWorkflow handed back. This map used to be
	// map[id]workerID, so the release below passed a literal 0, lost the
	// fence, logged the refusal and carried on -- and the assertion at the end
	// was a t.Log too, so the test passed while releasing nothing and
	// reclaiming nothing.
	var mu sync.Mutex
	claimed := make(map[string]*engine.WorkflowInstance)
	claimedBy := make(map[string]string)
	var wg sync.WaitGroup
	for _, wid := range []string{"worker-1", "worker-2", "worker-3"} {
		wg.Add(1)
		go func(wID string) {
			defer wg.Done()
			for range 5 {
				wf, err := store.ClaimWorkflow(ctx, wID)
				if err != nil {
					return
				}
				if wf == nil {
					return
				}
				mu.Lock()
				claimed[wf.ID] = wf
				claimedBy[wf.ID] = wID
				mu.Unlock()
			}
		}(wid)
	}
	wg.Wait()

	// Crash whichever worker actually claimed work, rather than assuming it was
	// worker-1.
	//
	// Claiming is a race between three concurrent workers over six workflows,
	// so worker-1 getting none of them is an ordinary outcome and not a defect.
	// The guard below is right that the test must never pass vacuously -- it
	// was just asserting the wrong thing, and it failed a CI run saying
	// "worker-1 claimed none of the 6 workflows". What this test needs is *a*
	// worker holding claims, not a particular one.
	//
	// Picked deterministically (most claims, ties broken by the fixed name
	// order) so that a failure here is reproducible rather than depending on
	// map iteration order, which is randomised.
	workerIDs := []string{"worker-1", "worker-2", "worker-3"}
	claimCounts := make(map[string]int, len(workerIDs))
	for _, workerID := range claimedBy {
		claimCounts[workerID]++
	}
	victim := ""
	for _, wID := range workerIDs {
		if claimCounts[wID] > claimCounts[victim] {
			victim = wID
		}
	}
	if claimCounts[victim] == 0 {
		t.Fatalf("no worker claimed any of the %d workflows, so this test has nothing "+
			"to fail over and would pass without exercising failover at all", numWorkflows)
	}

	// Simulate the victim crashing: release its workflows back to ready.
	var released int
	for id, workerID := range claimedBy {
		if workerID == victim {
			if err := store.ReleaseWorkflow(ctx, id, victim, claimed[id].Generation, time.Now()); err != nil {
				t.Fatalf("ReleaseWorkflow for %s: %v", id, err)
			}
			released++
		}
	}

	// Verify that the *other* workers can claim the released workflows.
	survivors := make([]string, 0, len(workerIDs)-1)
	for _, wID := range workerIDs {
		if wID != victim {
			survivors = append(survivors, wID)
		}
	}
	reclaimed := make(map[string]string)
	for _, wid := range survivors {
		for range 10 {
			wf, err := store.ClaimWorkflow(ctx, wid)
			if err != nil {
				break
			}
			if wf == nil {
				break
			}
			reclaimed[wf.ID] = wid
		}
	}

	// Count how many of the victim's original workflows got reclaimed.
	var reclaimedFromVictim int
	for id, wID := range claimedBy {
		if wID == victim {
			if _, ok := reclaimed[id]; ok {
				reclaimedFromVictim++
			}
		}
	}

	// An assertion, not a note. "No workflows were reclaimed (may be timing)"
	// is the same output a completely broken failover path produces, and this
	// test is the only thing that would have said otherwise.
	if reclaimedFromVictim == 0 {
		t.Errorf("%s released %d workflows and the remaining workers (%v) reclaimed "+
			"none of them: work orphaned by a crashed worker is not being picked up",
			victim, released, survivors)
	}
	t.Logf("Reclaimed %d of %d workflows released by %s", reclaimedFromVictim, released, victim)
}

// TestKillPostgresAndRestart kills the PostgreSQL container, restarts it,
// and verifies that workers can reconnect and claim workflows.
func TestKillPostgresAndRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cluster test in short mode")
	}

	// This test requires docker-compose to be running.
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Skipping: docker not available")
	}

	db, _ := requireDB(t)
	ctx := context.Background()
	cleanTestWorkflows(t, db)

	// Create a workflow before the kill.
	runID := "test-cluster-pg-restart"
	EnsureDef(t, db, "pg-restart", 1)
	_, err := db.Exec(`
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES ($1, 'pg-restart', 1, 'ready', '{}', $2)
		ON CONFLICT (id) DO NOTHING
	`, runID, TestQueue1)
	if err != nil {
		t.Fatalf("Insert workflow: %v", err)
	}
	defer db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)

	// Kill postgres container.
	killCmd := exec.Command("docker", "stop", "cleat-postgres")
	if out, err := killCmd.CombinedOutput(); err != nil {
		t.Skipf("Skipping: cannot stop postgres container: %v\n%s", err, string(out))
	}

	// Wait briefly, then restart.
	time.Sleep(2 * time.Second)

	startCmd := exec.Command("docker", "start", "cleat-postgres")
	if out, err := startCmd.CombinedOutput(); err != nil {
		t.Fatalf("Cannot start postgres container: %v\n%s", err, string(out))
	}

	// Wait for postgres to be healthy.
	for i := 0; i < 30; i++ {
		if err := db.Ping(); err == nil {
			break
		}
		time.Sleep(1 * time.Second)
		if i == 29 {
			t.Fatal("Timed out waiting for postgres to recover")
		}
	}

	// Verify we can claim a workflow after the restart.
	// We need a new store since the old one may have broken connections.
	newDB, newStore := requireDB(t)
	_ = newDB

	// An assertion, not a note. This used to log "No workflows to claim after
	// restart (may have been consumed by another worker)" and pass -- which is
	// also what a database that never recovered produces. Nothing else can
	// consume it now: the workflow is on a queue no compose worker serves.
	wf, err := newStore.ClaimWorkflow(ctx, "worker-1")
	if err != nil {
		t.Fatalf("Claim after restart failed: %v", err)
	}
	if wf == nil {
		t.Fatal("no workflow could be claimed after the PostgreSQL restart, " +
			"though one was inserted before it")
	}
	t.Logf("Successfully claimed workflow %s after postgres restart", wf.ID)
}

// TestFullClusterRestart stops all workers, restarts them, and verifies state
// is recovered from PostgreSQL.
func TestFullClusterRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cluster test in short mode")
	}

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Skipping: docker not available")
	}

	db, store := requireDB(t)
	ctx := context.Background()
	cleanTestWorkflows(t, db)

	// Create workflows and process some of them.
	runID := "test-cluster-restart"
	EnsureDef(t, db, "restart-test", 1)
	_, err := db.Exec(`
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES ($1, 'restart-test', 1, 'ready', '{}', $2)
		ON CONFLICT (id) DO NOTHING
	`, runID, TestQueue1)
	if err != nil {
		t.Fatalf("Insert workflow: %v", err)
	}
	defer db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)

	// Claim the workflow and append some events.
	wf, err := store.ClaimWorkflow(ctx, "worker-1")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}
	if wf == nil {
		t.Fatal("Expected to claim a workflow")
	}

	events := []engine.EventRecord{
		{Step: 1, EventType: "call", Service: "svc", Op: "op1", Request: `{}`, Response: `{"ok":true}`},
	}
	if err := store.AppendEventHistoryBatch(ctx, wf.ID, events); err != nil {
		t.Fatalf("AppendEventHistoryBatch: %v", err)
	}

	// Release the workflow (simulating worker restart before completion).
	if err := store.ReleaseWorkflow(ctx, wf.ID, "worker-1", wf.Generation, time.Now()); err != nil {
		t.Fatalf("ReleaseWorkflow: %v", err)
	}

	// Now claim it again on a "new" worker and verify events are intact.
	wf2, err := store.ClaimWorkflow(ctx, "worker-2")
	if err != nil {
		t.Fatalf("Second claim: %v", err)
	}
	if wf2 == nil {
		t.Fatal("Expected to claim workflow on second attempt")
	}

	history, err := store.LoadEventHistory(ctx, wf2.ID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(history) != 1 {
		t.Errorf("Expected 1 event after restart, got %d", len(history))
	}
	if history[0].Service != "svc" || history[0].Op != "op1" {
		t.Errorf("Event data mismatch: got Service=%q Op=%q", history[0].Service, history[0].Op)
	}

	t.Logf("State recovered successfully: workflow %s has %d events", wf2.ID, len(history))
}

// failoverTestHelper is a no-op helper to document the test pattern.
func failoverTestHelper(t *testing.T) {
	t.Helper()
	// This helper ensures consistent failover test setup.
	// Tests should use requireDB() for database access.
}

// dockerContainerRunning checks whether a docker container exists and is running.
func dockerContainerRunning(name string) bool {
	cmd := exec.Command("docker", "inspect", "--format", "{{.State.Running}}", name)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// healthCheck performs an HTTP GET to the given URL and returns true if the
// response status code is 200.
func healthCheck(url string) bool {
	resp, err := http.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}
