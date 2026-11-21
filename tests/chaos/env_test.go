//go:build chaos

// Package chaos is the fault-injection harness (cleat#4169). It runs a real cleat-worker subprocess against a real PostgreSQL, drives one
// scripted workflow (testdata/chaosscript) fault-free to get a GOLDEN record, then runs the same script again with a fault injected at each
// step boundary and compares what the recording service received against the golden record.
//
// It is BLACK BOX on purpose: it imports no engine internals, only the worker binary and the database, so it can be pointed at a build from
// before a fix (CLEAT_CHAOS_TREE) to prove it finds the defect the fix closed, and it does not move when the replay logic is refactored.
//
// Run (opt-in: every file here carries the `chaos` build tag, so no ordinary `go test ./...` or `go vet ./...` touches it):
//
//	CLEAT_CRASH_DB='postgres://cleat:cleat@localhost:55432/cleat?sslmode=disable' go test -tags chaos ./tests/chaos -count=1 -timeout 60m -v
//
// CLEAT_CRASH_DB is the same variable tests/crash reads: an owner DSN on a PostgreSQL the caller controls (the harness creates its own database
// in it). CLEAT_CHAOS_TREE names another checkout to build the worker, the guest and read the migrations from (copy testdata/chaosscript into
// it first). CLEAT_CHAOS_SCENARIO runs one scenario; CLEAT_CHAOS_DBHOLD sets how long the event-write fault lasts. See README.md.
//
// SIMULATED RECLAIM. After a SIGKILL the harness ages the dead worker's heartbeat in the database instead of waiting for the reclaim window.
// That window cannot go below ~37 s (cmd/cleat-worker refuses a --reclaim-timeout under minimumReclaimAfter, whose largest term is a fixed
// 30 s), which at 10 s per run would make the sweep unaffordable. The row ends up in the state the wait would have left it in; what is NOT
// exercised is the reaper's timing, which is not what this harness asks about.
package chaos

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/migration"
)

const (
	appPassword   = "cleat-app-test-pw" // must match tests/crash and engine/flush_rls_test.go: one role, one database
	defaultTenant = "00000000-0000-0000-0000-000000000000"
	chaosDatabase = "cleat_chaos"
	startBudget   = 60 * time.Second
)

var validDatabaseName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,50}$`)

func ownerDSN() string {
	if dsn := os.Getenv("CLEAT_CRASH_DB"); dsn != "" {
		return dsn
	}
	return "postgres://cleat:cleat@localhost:5433/cleat?sslmode=disable"
}

func swapDatabase(dsn, name string) string {
	q := ""
	if i := strings.Index(dsn, "?"); i >= 0 {
		q = dsn[i:]
		dsn = dsn[:i]
	}
	if i := strings.LastIndex(dsn, "/"); i >= 0 {
		dsn = dsn[:i]
	}
	return dsn + "/" + name + q
}

func redact(dsn string) string {
	if i := strings.Index(dsn, "@"); i >= 0 {
		if j := strings.Index(dsn, "://"); j >= 0 && j+3 < i {
			return dsn[:j+3] + "***" + dsn[i:]
		}
	}
	return dsn
}

// tree is the checkout the worker, the guest and the migrations come from: this one unless CLEAT_CHAOS_TREE says otherwise.
func tree(t *testing.T) string {
	t.Helper()
	if d := os.Getenv("CLEAT_CHAOS_TREE"); d != "" {
		return d
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func appDSN(t *testing.T) string {
	t.Helper()
	owner := ownerDSN()
	at, scheme := strings.Index(owner, "@"), strings.Index(owner, "://")
	if at < 0 || scheme < 0 {
		t.Fatalf("cannot derive the cleat_app DSN from %s", redact(owner))
	}
	return swapDatabase(owner[:scheme+3]+"cleat_app:"+appPassword+owner[at:], chaosDatabase)
}

// ownerDB returns an owner connection to the harness database, creating and migrating it.
func ownerDB(t *testing.T) *sql.DB {
	t.Helper()
	base := ownerDSN()
	admin, err := sql.Open("postgres", base)
	if err != nil {
		t.Fatalf("opening %s: %v", redact(base), err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Fatalf("database unreachable at %s: %v (set CLEAT_CRASH_DB to a PostgreSQL you control)", redact(base), err)
	}
	if !validDatabaseName.MatchString(chaosDatabase) {
		t.Fatalf("bad database name %q", chaosDatabase)
	}
	var exists bool
	if err := admin.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, chaosDatabase).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		if _, err := admin.Exec(`CREATE DATABASE ` + chaosDatabase); err != nil {
			t.Fatalf("creating %s: %v", chaosDatabase, err)
		}
	}
	dsn := swapDatabase(base, chaosDatabase)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := migration.NewRunner(db, migration.DialectPostgres, filepath.Join(tree(t), "migrations")).Run(context.Background()); err != nil {
		t.Fatalf("applying %s's migrations: %v", tree(t), err)
	}
	if _, err := db.Exec(fmt.Sprintf(`ALTER ROLE cleat_app LOGIN PASSWORD '%s'`, appPassword)); err != nil {
		t.Fatalf("granting cleat_app a login: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// build compiles the worker and the script guest ONCE per test binary, from tree(t).
var (
	buildOnce sync.Once
	workerBin string
	guestWasm []byte
	buildErr  error
	// buildDir is where the worker and the guest were built, removed by TestMain when the run ends normally. A run that is killed or times out
	// removes nothing; the next run's OwnedTempDir sweeps what such a run left (cleat#4226).
	buildDir string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

func build(t *testing.T) (string, []byte) {
	t.Helper()
	buildOnce.Do(func() {
		root := tree(t)
		dir, err := testutil.OwnedTempDir("chaos-build-")
		if err != nil {
			buildErr = err
			return
		}
		buildDir = dir
		workerBin = filepath.Join(dir, "cleat-worker")
		// CGO at its default: CGO_ENABLED=0 removes the only WASM backend, and a result from that worker would not be about the engine that ships.
		cmd := exec.Command("go", "build", "-o", workerBin, "./cmd/cleat-worker")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("building cleat-worker in %s: %v\n%s", root, err, out)
			return
		}
		guestDir := filepath.Join(dir, "guest")
		_ = os.MkdirAll(guestDir, 0o755)
		cmd = exec.Command("go", "run", "./cmd/cleat", "build", "--target", "go", "-o", guestDir, "./testdata/chaosscript")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("building the chaosscript guest in %s: %v\n%s", root, err, out)
			return
		}
		if guestLanguage() == "python" {
			guestWasm, buildErr = buildPythonGuest(root, dir)
			return
		}
		guestWasm, buildErr = os.ReadFile(filepath.Join(guestDir, "workflow.wasm"))
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return workerBin, guestWasm
}

// guestLanguage is the language of the script guest under test: "go" (the default, a core module) or "python" (a component), chosen with
// CLEAT_CHAOS_GUEST. The scenarios and the oracles are the same for both: that is the point of a guest that interprets a list.
func guestLanguage() string {
	if g := os.Getenv("CLEAT_CHAOS_GUEST"); g != "" {
		return g
	}
	return "go"
}

// buildPythonGuest compiles testdata/chaosscript_py to a component with componentize-py (through cleat build --target python). The toolchain
// must be on PATH; when the Python guest is asked for and it is not, that is a failure, never a skip.
func buildPythonGuest(root, dir string) ([]byte, error) {
	src := filepath.Join(root, "testdata", "chaosscript_py", "chaosscript.py")
	out := filepath.Join(dir, "guest-py")
	_ = os.MkdirAll(out, 0o755)
	cmd := exec.Command("go", "run", "./cmd/cleat", "build", "--target", "python", "--entry", src+":chaos_script", "-o", out, src)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(root, "python-sdk"))
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("building the Python script guest (CLEAT_CHAOS_GUEST=python needs componentize-py on PATH): %v\n%s", err, b)
	}
	return os.ReadFile(filepath.Join(out, "chaos_script.wasm"))
}

func entryPoints() string {
	if guestLanguage() == "python" {
		return "chaos_script"
	}
	return "script"
}

func deploy(t *testing.T, db *sql.DB, wasm []byte, taskQueue string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO workflow_defs
			(name, version, wasm_bytes, entry_points, min_version,
			 max_history_length, dag_spec, task_queue, abi_version, plugin_deps, tenant_id)
		VALUES ('chaosscript', 1, $1, ARRAY[$4], 1, 10000, NULL, $2, 1, '{}', $3)
		ON CONFLICT (tenant_id, name, version) DO UPDATE SET
			wasm_bytes = EXCLUDED.wasm_bytes, entry_points = EXCLUDED.entry_points, min_version = EXCLUDED.min_version,
			max_history_length = EXCLUDED.max_history_length, dag_spec = EXCLUDED.dag_spec, task_queue = EXCLUDED.task_queue,
			abi_version = EXCLUDED.abi_version, plugin_deps = EXCLUDED.plugin_deps`,
		wasm, taskQueue, defaultTenant, entryPoints()); err != nil {
		t.Fatalf("deploying the chaosscript definition: %v", err)
	}
}

type worker struct {
	cmd *exec.Cmd
	log *strings.Builder
	mu  sync.Mutex
}

type syncWriter struct {
	w  *strings.Builder
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// startWorker launches a worker with a 1s heartbeat. The reclaim window cannot be shortened below ~37s (cmd/cleat-worker refuses a
// --reclaim-timeout under minimumReclaimAfter, whose largest term is a fixed 30s), so a kill is followed by ageLease, which ages the dead
// worker's heartbeat in the database: the lease EXPIRES the way it would after the wait, without spending the wait on every boundary.
func startWorker(t *testing.T, bin, taskQueue, svcURL string, extra ...string) *worker {
	t.Helper()
	ownerDB := ownerDSN()
	w := &worker{log: &strings.Builder{}}
	args := []string{
		"--db", appDSN(t),
		"--migrate-db", swapDatabase(ownerDB, chaosDatabase),
		"--task-queue", taskQueue,
		"--bench-svc-url", svcURL,
		"--poll", "100ms",
		"--concurrency", "1",
		"--heartbeat", "1s",
		"--migrate-on-start",
	}
	cmd := exec.Command(bin, append(args, extra...)...)
	cmd.Dir = tree(t)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = &syncWriter{w: w.log, mu: &w.mu}
	cmd.Stderr = &syncWriter{w: w.log, mu: &w.mu}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the worker: %v", err)
	}
	w.cmd = cmd
	t.Cleanup(func() { w.kill() })
	return w
}

func (w *worker) kill() {
	if w.cmd == nil || w.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)
	_, _ = w.cmd.Process.Wait()
}

func (w *worker) output() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.log.String()
}

func startRun(t *testing.T, db *sql.DB, id, input, taskQueue string) {
	t.Helper()
	testutil.SeedRun(t, db, testutil.RunSeed{ID: id, DefName: "chaosscript", TenantID: defaultTenant, Input: input, TaskQueue: taskQueue})
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id)
	})
}

func eventCount(db *sql.DB, id string) int {
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM event_history WHERE workflow_id = $1`, id).Scan(&n)
	return n
}

// awaitStatus polls until done(status) holds. ok is false if the run was still in another state after the budget.
func awaitStatus(db *sql.DB, id string, budget time.Duration, done func(string) bool) (status, errMsg, result string, ok bool) {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		var msg, res sql.NullString
		err := db.QueryRow(`SELECT w.status, p.error_msg, p.result::text FROM workflow_leases w
			JOIN workflow_payloads p ON p.id = w.id WHERE w.id = $1`, id).Scan(&status, &msg, &res)
		if err == nil && done(status) {
			return status, msg.String, res.String, true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return status, "", "", false
}

// ageLease makes the run's lease look expired: the killed worker's last heartbeat is moved an hour into the past. This is what the passing of
// the reclaim window does to the row, so the next worker's reaper takes the run over at once instead of after ~40s per boundary.
func ageLease(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	res, err := db.Exec(`UPDATE workflow_leases SET heartbeat_at = now() - interval '1 hour' WHERE id = $1`, id)
	if err != nil {
		t.Fatalf("ageing the lease of %s: %v", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("ageing the lease of %s touched %d rows, want 1", id, n)
	}
}

// denyEventWrites makes every event write by the worker's role fail, the way a full disk, a revoked grant or a failing primary does to the
// worker: the database answers an INSERT with an error. grantEventWrites undoes it.
func denyEventWrites(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`REVOKE INSERT ON event_history FROM cleat_app, cleat_tenant_00000000_0000_0000_0000_000000000000`); err != nil {
		t.Fatalf("revoking INSERT on event_history: %v", err)
	}
}

func grantEventWrites(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`GRANT INSERT ON event_history TO cleat_app, cleat_tenant_00000000_0000_0000_0000_000000000000`); err != nil {
		t.Fatalf("restoring INSERT on event_history: %v", err)
	}
}

// deliverSignal sends a signal to the run the way the API does: a row in workflow_signals and the lease's signal sequence advanced.
func deliverSignal(t *testing.T, db *sql.DB, id, name string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO workflow_signals (workflow_id, signal_name, payload, tenant_id) VALUES ($1, $2, '{}'::jsonb, $3)`, id, name, defaultTenant); err != nil {
		t.Fatalf("delivering signal %q to %s: %v", name, id, err)
	}
	if _, err := db.Exec(`UPDATE workflow_leases SET signal_seq = signal_seq + 1 WHERE id = $1`, id); err != nil {
		t.Fatalf("advancing the signal sequence of %s: %v", id, err)
	}
}

// resolveWhenSuspended resolves the run's promise once the run has parked on it (a parked run reads status 'ready' with its wake time at the
// await's deadline, well in the future), the way the API does: the promise row, the lease's promise
// sequence advanced, and the wake time brought forward.
//
// It returns "" always, and that is deliberate and not an oversight: it acts whether or not it saw the run park (a timed-out wait falls through
// to the resolve), and the promise scenarios are judged by promiseViolations, which reads the rows themselves. Reporting a timeout here would turn
// scenarios that pass today into UNMEASURED ones, which is a change to those scenarios and not to this fix. The cron hooks report; see Scenario.Drive.
func resolveWhenSuspended(db *sql.DB, run string) string {
	deadline := time.Now().Add(runBudget)
	for time.Now().Before(deadline) {
		var n int
		_ = db.QueryRow(`SELECT count(*) FROM workflow_promises p JOIN workflow_leases l ON l.id = p.workflow_id
			WHERE p.workflow_id = $1 AND p.status = 'pending' AND l.status = 'ready' AND l.next_wake_at > now() + interval '5 seconds'`, run).Scan(&n)
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, _ = db.Exec(`UPDATE workflow_promises SET status = 'resolved', result = '"ok"'::jsonb, resolved_at = now() WHERE workflow_id = $1`, run)
	_, _ = db.Exec(`UPDATE workflow_leases SET promise_seq = promise_seq + 1,
		next_wake_at = CASE WHEN status IN ('ready', 'suspended') THEN now() ELSE next_wake_at END WHERE id = $1`, run)
	return ""
}

func ownerExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
}
