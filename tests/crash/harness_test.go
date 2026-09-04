// Package crash is IMPROVEMENT-PLAN §2.4: SIGKILL a worker mid-DurableCall,
// restart it, and assert the documented crash semantics.
//
// It runs a real cleat-worker as a subprocess against a real PostgreSQL, with
// the external service standing up in the test process so that every invocation
// is counted. That combination is the point. §1.4's write side was 350 lines of
// tested-but-dead durability code, and the reason it survived review is that
// nothing had ever crashed a worker to see what the code was for. The plan says
// so directly: "Do not start the intent work before the 2.4 crash harness
// exists -- building the fix before the observation is how this happened."
//
// This suite is the observation. It asserts the contract docs/durable-calls.md
// actually states today -- at-least-once, with silent duplicates -- rather than
// the one the deleted code aspired to. If a future change makes duplicates
// impossible, these tests are what should fail.
package crash

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/cleat-team/cleat/migration"
)

const (
	// appPassword is set on cleat_app by the harness. 005_app_role.sql creates
	// the role NOLOGIN with no password on purpose -- a committed credential
	// would be a defect -- so the deployment supplies one. Here, the harness is
	// the deployment.
	//
	// Must match engine/flush_rls_test.go's appRolePassword exactly. Both suites
	// connect as cleat_app, and it is one role in one database: different values
	// mean whichever package runs second re-ALTERs the role and breaks the
	// other's live connections. `go test ./...` runs packages in parallel, so
	// this is the ordinary local command.
	appPassword = "cleat-app-test-pw"

	// defaultTenant is engine.DefaultTenantUUID, spelled out rather than
	// imported so this suite does not depend on the engine package. Getting it
	// wrong is silent: the worker connects as cleat_app, which is NOBYPASSRLS,
	// so a row under an unrecognised tenant is not rejected -- it is invisible.
	defaultTenant = "00000000-0000-0000-0000-000000000000"

	// startBudget is how long the worker gets to connect, migrate and report
	// ready before the test gives up on it.
	startBudget = 60 * time.Second

	// completeBudget is how long a single one-call workflow gets. Generous: the
	// point of this suite is what happened, not how fast.
	completeBudget = 90 * time.Second
)

// ownerDSN is the migration/owner connection. Deliberately not defaulted to
// port 5432: that instance belongs to another workstream's checkout, and
// WORKSTREAM.md's DSN table assigns this one 5433.
func ownerDSN() string {
	if dsn := os.Getenv("CLEAT_CRASH_DB"); dsn != "" {
		return dsn
	}
	return "postgres://cleat:cleat@localhost:5433/cleat?sslmode=disable"
}

// appDSN is the unprivileged connection the worker serves traffic on. Two DSNs
// is the shipped configuration (see "Standing constraints" in
// IMPROVEMENT-PLAN.md): --db is cleat_app, --migrate-db is the owner. Running
// this suite as the owner would work and would also disable RLS silently, which
// is the §1.10 defect. Not worth reintroducing to save a role.
func appDSN(t *testing.T) string {
	t.Helper()
	return appDSNFor(t, crashDatabase)
}

// appDSNFor is appDSN against a named database. Only
// TestWorkerMigratesTheDatabaseItServes passes anything but crashDatabase; it
// needs a database no other test has started a worker against, because the
// evidence it looks for is created once and then stays created.
func appDSNFor(t *testing.T, dbName string) string {
	t.Helper()
	owner := ownerDSN()
	at := strings.Index(owner, "@")
	scheme := strings.Index(owner, "://")
	if at < 0 || scheme < 0 {
		t.Fatalf("cannot derive the cleat_app DSN from %s: expected scheme://user:pass@host/db", redact(owner))
	}
	return swapDatabase(owner[:scheme+3]+"cleat_app:"+appPassword+owner[at:], dbName)
}

func redact(dsn string) string {
	if i := strings.Index(dsn, "@"); i >= 0 {
		if j := strings.Index(dsn, "://"); j >= 0 && j+3 < i {
			return dsn[:j+3] + "***" + dsn[i:]
		}
	}
	return dsn
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	return root
}

// ownerDB connects as the owner and applies every migration in order.
//
// t.Fatalf, never t.Skip, when the database is unreachable. This suite is only
// run by something that stood a database up, so an unreachable one means that
// setup failed. A skip here would be the §2.12 defect: indistinguishable from a
// pass.
func ownerDB(t *testing.T) *sql.DB {
	t.Helper()
	return ownerDBFor(t, crashDatabase)
}

// ownerDBFor is ownerDB against a named database. See appDSNFor.
func ownerDBFor(t *testing.T, dbName string) *sql.DB {
	t.Helper()
	dsn := ensureDatabaseNamed(t, dbName)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("opening %s: %v", redact(dsn), err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("database unreachable at %s: %v -- this suite needs a PostgreSQL "+
			"instance (WS-2's is port 5433; override with CLEAT_CRASH_DB)", redact(dsn), err)
	}

	// Applied through migration.Runner -- the same call cmd/cleat-worker makes
	// at boot -- rather than this suite's own ReadDir-and-Exec loop over
	// migrations/postgres/*.sql.
	//
	// That loop was a second implementation of "apply the shipped migrations",
	// which is the mechanism §1.9 and §2.60b are about: engine/testutil used to
	// carry its own copy of the schema, the copy drifted, and every test in the
	// repo ran against a schema production never uses. A loop that reads the
	// right files is a smaller version of the same thing, not a different
	// thing -- it has no schema_migrations bookkeeping, so it re-executes every
	// file on every run and can only ever work for statements that are
	// re-appliable, and it splits statements differently from the Runner.
	// engine/testutil has a guard test against reintroducing a hand-written
	// schema; this suite had no such guard and had quietly kept one.
	if err := migration.NewRunner(db, migration.DialectPostgres,
		filepath.Join(repoRoot(t), "migrations")).Run(context.Background()); err != nil {
		t.Fatalf("applying the shipped PostgreSQL migrations to %s: %v\n\n"+
			"This is the code path cmd/cleat-worker takes at boot, so a failure "+
			"here is a worker that cannot start, not a harness problem.",
			dbName, err)
	}

	// Give cleat_app a login. 005 creates it NOLOGIN and says the deployment
	// must do this; deploy/postgres/900-app-role.sh is the production analogue.
	if _, err := db.Exec(fmt.Sprintf(
		`ALTER ROLE cleat_app LOGIN PASSWORD '%s'`, appPassword)); err != nil {
		t.Fatalf("granting cleat_app a login: %v", err)
	}
	return db
}

// crashDatabase is the database this suite runs in, created on demand.
const crashDatabase = "cleat_crash"

// ensureCrashDatabase creates and returns a DSN for a database used only by
// this suite.
//
// It cannot share one with the engine package. engine/testutil's
// CleanupPostgresTestData issues an unqualified `DELETE FROM workflow_defs`
// (and workflow_instances, and event_history), so an engine test running
// concurrently deletes this suite's definition and queued workflow out from
// under a live worker. `go test ./...` runs packages in parallel, so that is
// the ordinary case, and the symptom is this suite timing out with "the
// external service was never called" -- which reads like a harness bug rather
// than a collision.
//
// §2.39 made the DB-backed suites concurrency-safe against each other with
// per-suite task queues and a schema fingerprint. Neither helps here: the
// deletes are not scoped to a task queue at all. A separate database is the
// smallest fix that does not change a helper every other suite depends on.
func ensureCrashDatabase(t *testing.T) string {
	t.Helper()
	return ensureDatabaseNamed(t, crashDatabase)
}

// ensureDatabaseNamed is ensureCrashDatabase against a named database. See
// appDSNFor for the one caller that passes anything else.
//
// dbName is interpolated into CREATE DATABASE, which cannot be parameterised,
// so it is restricted to characters that need no quoting. Both call sites pass
// a compile-time constant; the check is here so that stops being load-bearing.
func ensureDatabaseNamed(t *testing.T, dbName string) string {
	t.Helper()
	if !validDatabaseName.MatchString(dbName) {
		t.Fatalf("database name %q must match %s -- it is interpolated into "+
			"CREATE DATABASE, which cannot be parameterised", dbName, validDatabaseName)
	}
	base := ownerDSN()

	admin, err := sql.Open("postgres", base)
	if err != nil {
		t.Fatalf("opening %s: %v", redact(base), err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Fatalf("database unreachable at %s: %v -- this suite needs a PostgreSQL "+
			"instance (WS-2's is port 5433; override with CLEAT_CRASH_DB)", redact(base), err)
	}

	var exists bool
	if err := admin.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, dbName).Scan(&exists); err != nil {
		t.Fatalf("checking for the %s database: %v", dbName, err)
	}
	if !exists {
		// CREATE DATABASE cannot be parameterised; dbName is checked above.
		if _, err := admin.Exec(`CREATE DATABASE ` + dbName); err != nil {
			// A concurrent run may have won the race; re-check rather than fail.
			if err2 := admin.QueryRow(
				`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`,
				dbName).Scan(&exists); err2 != nil || !exists {
				t.Fatalf("creating the %s database: %v", dbName, err)
			}
		}
	}

	return swapDatabase(base, dbName)
}

var validDatabaseName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,50}$`)

// swapDatabase replaces the database component of a postgres URL.
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

// chargeService is the external service the fixture calls. It counts every
// invocation per order, and can hold a chosen invocation open so that the test
// can kill the worker with the call genuinely in flight.
type chargeService struct {
	srv *httptest.Server

	mu     sync.Mutex
	counts map[string]int // keyed by operation: work actually performed
	total  int            // every request that reached the handler, whatever its shape
	bodies []string       // raw request bodies, for diagnosing a mismatch

	// honourKeys makes the service deduplicate on Idempotency-Key, the way a
	// payment processor does. Off by default, so the crash test measures the
	// contract for services that cannot dedupe.
	honourKeys bool
	seenKeys   map[string]string // Idempotency-Key -> the response first returned

	// holdOp names the operation whose first invocation blocks. The crash
	// window is exactly that block: the service has processed the request and
	// the worker has not yet been told.
	holdOp string

	// gate is closed once holdOp has been recorded and is about to block. It is
	// how the test learns the side effect has happened.
	gateOnce sync.Once
	gate     chan struct{}

	hold chan struct{}
}

func newChargeService(t *testing.T) *chargeService {
	t.Helper()
	c := &chargeService{
		counts:   make(map[string]int),
		seenKeys: make(map[string]string),
		gate:     make(chan struct{}),
	}
	// The worker forwards unknown services to --bench-svc-url as
	// POST /call/{service}/{operation}; see dbServiceCaller.forwardToBenchSvc.
	c.srv = httptest.NewServer(http.HandlerFunc(c.handle))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *chargeService) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))

	// The operation comes from the path, not the body. The worker forwards to
	// /call/{service}/{operation} (dbServiceCaller.forwardToBenchSvc), which is
	// authoritative. The body is not: a fixture that builds its request by
	// concatenation produces invalid JSON the moment a value contains a quote,
	// and a silently-failing Unmarshal here reads as "the call never happened".
	op := path.Base(r.URL.Path)

	key := r.Header.Get("Idempotency-Key")

	// Record the side effect BEFORE blocking. This models a real service that
	// has committed the operation and is about to reply -- the case a durable
	// call cannot distinguish from one that never ran.
	c.mu.Lock()
	c.total++
	c.bodies = append(c.bodies, r.URL.Path+" "+string(raw))

	// A key-honouring service returns the original outcome rather than doing
	// the work again. The counter tracks work performed, not requests received,
	// which is the distinction the whole mechanism turns on.
	if c.honourKeys && key != "" {
		if prior, seen := c.seenKeys[key]; seen {
			c.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(prior))
			return
		}
	}

	c.counts[op]++
	n := c.counts[op]
	shouldHold := c.holdOp != "" && op == c.holdOp && n == 1
	hold := c.hold
	c.mu.Unlock()

	if shouldHold && hold != nil {
		c.gateOnce.Do(func() { close(c.gate) })
		<-hold
	}

	body := fmt.Sprintf(`{"charge_id":"chg-%s-%d","status":"ok"}`, op, n)
	if c.honourKeys && key != "" {
		c.mu.Lock()
		c.seenKeys[key] = body
		c.mu.Unlock()
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// keyCount reports how many distinct idempotency keys the service was sent.
func (c *chargeService) keyCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seenKeys)
}

// holdOperation makes the first invocation of op block until the returned
// release func is called.
func (c *chargeService) holdOperation(op string) (release func()) {
	c.mu.Lock()
	c.holdOp = op
	c.hold = make(chan struct{})
	h := c.hold
	c.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(h) }) }
}

// awaitHeldCall blocks until the held operation has been recorded.
//
// On timeout it prints the worker's log. A crash-diagnosis suite that reports
// only "it did not happen" makes every failure a fresh investigation.
func (c *chargeService) awaitHeldCall(t *testing.T, w *worker, budget time.Duration) {
	t.Helper()
	select {
	case <-c.gate:
	case <-time.After(budget):
		t.Fatalf("the external service was never called within %v; the worker "+
			"never reached the durable call, so there is no crash window to test"+
			"\n--- worker log ---\n%s", budget, w.output())
	}
}

func (c *chargeService) count(op string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[op]
}

// diagnose reports what the service actually received. A count of 0 has two
// very different causes -- the worker never called out, or it called with a
// shape this handler did not recognise -- and they need different fixes.
func (c *chargeService) diagnose() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.total == 0 {
		return "the service received no HTTP requests at all"
	}
	return fmt.Sprintf("the service received %d requests: %s", c.total, strings.Join(c.bodies, " | "))
}

// allCounts returns the invocation count for each operation, in call order.
//
// Keyed by operation alone, not by order ID. The fixture's single string
// parameter receives the *entire* input JSON rather than the named field:
// wasm/exports.go special-cases a lone string parameter and passes argsJSON
// through verbatim, so orderID arrives as
// `{"orderID": "...", "__entry_point": "three_charges"}`. Each test gets its
// own chargeService, so the operation name is enough to tell calls apart.
func (c *chargeService) allCounts() (reserve, charge, ship int) {
	return c.count("Reserve"), c.count("Charge"), c.count("Ship")
}

// buildWorker compiles cmd/cleat-worker once and returns the binary path.
//
// Built with CGO at its default. CGO_ENABLED=0 would remove NewWasmtimeBackend
// from the binary entirely and silently run the fallback, so a crash-recovery
// result obtained that way would not be about the engine that ships.
func buildWorker(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "cleat-worker")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/cleat-worker")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building cleat-worker: %v\n%s", err, out)
	}
	return bin
}

// deployFixture compiles testdata/crashcall and registers it as a definition.
func deployFixture(t *testing.T, db *sql.DB, taskQueue string) {
	t.Helper()
	root := repoRoot(t)
	outDir := t.TempDir()

	cmd := exec.Command("go", "run", filepath.Join(root, "cmd", "cleat"),
		"build", "--target", "go", "-o", outDir, filepath.Join(root, "testdata", "crashcall"))
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the crashcall fixture: %v\n%s", err, out)
	}

	var wasmPath string
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("reading build output: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			wasmPath = filepath.Join(outDir, e.Name())
		}
	}
	if wasmPath == "" {
		t.Fatalf("no .wasm produced in %s", outDir)
	}
	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("reading %s: %v", wasmPath, err)
	}

	// tenant_id must be set explicitly. workflow_defs' RLS policy is
	// `tenant_id = assert_tenant_set()`, and NULL does not satisfy it, so a
	// definition inserted without one is invisible to the worker's cleat_app
	// connection. The symptom is not a permission error -- it is "wasm not
	// found: crashcall v1", which reads like a build problem.
	//
	// The policy used to carry `OR tenant_id = <default>` as well; D7 removed
	// that clause along with the adoption window it served (IMPROVEMENT-PLAN
	// 3.77), so the tenant here matters more than it did, not less.
	//
	// ON CONFLICT names the full primary key, which is (tenant_id, name,
	// version) since D7. This fixture writes the INSERT by hand rather than
	// calling DeployWorkflowDef, so it does not get the store's version of
	// this statement for free -- and a hand-written copy of a production
	// statement is the same shape as the ReadDir-and-Exec migration loop this
	// file's header describes replacing. It is left hand-written because the
	// fixture deliberately sets columns DeployWorkflowDef does not expose
	// (entry_points, task_queue, dag_spec), but it is worth knowing that this
	// is the second copy and it broke when the first one changed.
	if _, err := db.Exec(`
		INSERT INTO workflow_defs
			(name, version, wasm_bytes, entry_points, min_version,
			 max_history_length, dag_spec, task_queue, abi_version, plugin_deps, tenant_id)
		VALUES ('crashcall', 1, $1, ARRAY['three_charges'], 1, 10000, '{}'::jsonb, $2, 1, '{}'::jsonb, $3)
		ON CONFLICT (tenant_id, name, version) DO UPDATE SET wasm_bytes = EXCLUDED.wasm_bytes,
			task_queue = EXCLUDED.task_queue, tenant_id = EXCLUDED.tenant_id`,
		wasm, taskQueue, defaultTenant); err != nil {
		t.Fatalf("deploying the crashcall definition: %v", err)
	}
}

// worker is a running cleat-worker subprocess.
type worker struct {
	cmd *exec.Cmd
	log *strings.Builder
	mu  sync.Mutex
}

// startWorker launches the worker and waits until it claims work or the start
// budget expires.
// startWorker starts a worker against the given task queue. extraFlags are
// appended verbatim, so a scenario can turn on a worker feature -- the point of
// a harness that runs the real binary is that a flag the shipped artifact does
// not honour cannot pass here.
func startWorker(t *testing.T, bin, taskQueue, svcURL string, extraFlags ...string) *worker {
	t.Helper()
	return startWorkerOn(t, crashDatabase, bin, taskQueue, svcURL, extraFlags...)
}

// startWorkerOn is startWorker against a named database. See appDSNFor.
func startWorkerOn(t *testing.T, dbName, bin, taskQueue, svcURL string, extraFlags ...string) *worker {
	t.Helper()

	w := &worker{log: &strings.Builder{}}
	// --migrate-db is the owner connection to the database this worker serves
	// from, NOT ownerDSN(). ownerDSN() names the *base* database -- `cleat` --
	// which this suite deliberately does not use: it took cleat_crash of its own
	// precisely so engine/testutil's unqualified DELETEs could not reach it (see
	// ensureCrashDatabase). Passing it here meant every worker start ran
	// migration.Runner and plugin.RunMigrations against the shared database and
	// migrated nothing in the one it actually reads and writes.
	//
	// Measured 2026-08-31 before the fix, after one worker start: `cleat` had
	// schema_migrations (14 rows) and plugin_migrations; cleat_crash had
	// neither, and its 14 tables came from ownerDB's loop instead. The tables
	// happened to agree, so nothing failed -- but the worker's own migration
	// step, the one cmd/cleat-worker runs at boot, was being exercised against
	// a database no assertion in this suite ever looks at.
	args := []string{
		"--db", appDSNFor(t, dbName),
		"--migrate-db", ensureDatabaseNamed(t, dbName),
		"--task-queue", taskQueue,
		"--bench-svc-url", svcURL,
		"--poll", "200ms",
		"--concurrency", "1",
	}
	args = append(args, extraFlags...)
	//nolint:gosec // bin is built by this test from this repo.
	cmd := exec.Command(bin, args...)
	// The worker resolves migrations/postgres relative to its working
	// directory, so it must run from the repo root. Getting this wrong is not
	// loud: the worker logs one ERROR line, keeps running, never claims
	// anything, and the test just times out with the workflow still "ready".
	cmd.Dir = repoRoot(t)
	// Own process group, so SIGKILL reaches the worker and nothing else.
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

// kill SIGKILLs the worker's process group. SIGKILL, not SIGTERM: a graceful
// shutdown is the case that already works. §2.4 is about the ungraceful one.
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

type syncWriter struct {
	w  *strings.Builder
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// startWorkflow queues one crashcall instance.
func startWorkflow(t *testing.T, db *sql.DB, id, orderID, taskQueue string) {
	t.Helper()
	input := fmt.Sprintf(`{"__entry_point":"three_charges","orderID":%q}`, orderID)
	if _, err := db.Exec(`
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id)
		VALUES ($1, 'crashcall', 1, 'ready', $2::jsonb, $3, $4)`,
		id, input, taskQueue, defaultTenant); err != nil {
		t.Fatalf("queueing workflow %s: %v", id, err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id)
	})
}

// awaitTerminal polls until the workflow leaves ready/running.
func awaitTerminal(t *testing.T, db *sql.DB, id string, budget time.Duration) (status, errMsg string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		var msg sql.NullString
		err := db.QueryRow(`SELECT status, error_msg FROM workflow_instances WHERE id = $1`, id).
			Scan(&status, &msg)
		if err != nil {
			t.Fatalf("polling %s: %v", id, err)
		}
		if status != "ready" && status != "running" {
			return status, msg.String
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("workflow %s was still %q after %v", id, status, budget)
	return "", ""
}

// eventCount returns how many events the workflow has recorded.
func eventCount(t *testing.T, db *sql.DB, id string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM event_history WHERE workflow_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("counting events for %s: %v", id, err)
	}
	return n
}

func uniqueSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

var _ = context.Background
