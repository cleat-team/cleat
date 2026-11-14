package typescript

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// The TypeScript guest on a REAL worker: the cleat-worker binary, a real PostgreSQL and the wasmtime backend, driven only
// over the worker's HTTP API. Nothing is in-process, which is deliberate. The worker tests that run a built guest do so
// from inside cmd/cleat-worker, a tier-1 package, and a TypeScript toolchain there would be a tier-1 test that skips
// when the toolchain is absent. These live in tests/typescript, so the worker is a subprocess.
//
// Two things are proved, both of which an in-process engine test cannot show:
//   - a TypeScript run goes through all three kinds of suspension (a sleep, a signal, a child workflow) on a worker and
//     finishes with the right answer;
//   - kill -9 on the worker mid-run does not lose the run: a second worker claims it, replays it from its history and
//     finishes it.
//
// The helpers below are adapted from cmd/cleat-mcp's real-workflow test, which is package main and cannot be imported.

const (
	postgresEnv     = "CLEAT_TEST_POSTGRES"
	postgresEnvFall = "CLEAT_TEST_DB"
	// The all-zero tenant is the default tenant every database starts with.
	defaultTenant = "00000000-0000-0000-0000-000000000000"
)

// needPostgres returns the admin DSN. Unset: a SKIP, unless CLEAT_REQUIRE_TOOLCHAINS names "postgres", in which case it is a
// failure. "postgres" there means the DSN, and the tier-2 job that provides a PostgreSQL service declares it, so a service
// that stopped starting cannot turn these tests into skips.
func needPostgres(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(postgresEnv)
	if dsn == "" {
		dsn = os.Getenv(postgresEnvFall)
	}
	if dsn == "" {
		if toolchainRequired("postgres") {
			t.Fatalf("%s names postgres, but neither %s nor %s is set: the job's PostgreSQL service is broken, not the test",
				requireToolchainEnv, postgresEnv, postgresEnvFall)
		}
		t.Skip(postgresEnv + " is not set; only the TypeScript tier-2 job provides a PostgreSQL for these tests")
	}
	return dsn
}

func goBuild(t *testing.T, root, out, pkg string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, b)
	}
}

// scratchDB creates an empty database on the admin server and returns its owner DSN, dropping it (after terminating its
// backends, which PostgreSQL requires) when the test ends.
func scratchDB(t *testing.T, admin string) string {
	t.Helper()
	adb, err := sql.Open("postgres", admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adb.Close() })
	if err := adb.Ping(); err != nil {
		t.Fatalf("%s is set but unreachable: %v", postgresEnv, err)
	}
	name := fmt.Sprintf("cleat_ts_worker_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := adb.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, err := sql.Open("postgres", admin)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = a.Exec(`DROP DATABASE IF EXISTS ` + name)
	})
	i := strings.LastIndex(admin, "/")
	j := strings.Index(admin, "?")
	switch {
	case i < 0:
		t.Fatalf("DSN has no path: %q", admin)
	case j < 0 || j < i:
		return admin[:i+1] + name
	}
	return admin[:i+1] + name + admin[j:]
}

// appRoleDSN gives cleat_app a login password and returns its DSN against the database ownerDSN names. The worker serves
// as that role, which is subject to row-level security, and migrates as the owner.
func appRoleDSN(t *testing.T, ownerDSN string) string {
	t.Helper()
	owner, err := sql.Open("postgres", ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	const pw = "cleat_ts_worker_pw"
	if _, err := owner.Exec(`ALTER ROLE cleat_app LOGIN PASSWORD '` + pw + `'`); err != nil {
		t.Fatalf("giving cleat_app a login (is the schema applied?): %v", err)
	}
	const scheme = "postgres://"
	if !strings.HasPrefix(ownerDSN, scheme) {
		t.Fatalf("DSN does not start with %q: %q", scheme, ownerDSN)
	}
	rest := strings.TrimPrefix(ownerDSN, scheme)
	at := strings.Index(rest, "@")
	if at < 0 {
		t.Fatalf("DSN has no user@host: %q", ownerDSN)
	}
	return scheme + "cleat_app:" + pw + "@" + rest[at+1:]
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// worker is a running cleat-worker process.
type worker struct {
	cmd    *exec.Cmd
	out    *syncBuffer
	exited chan struct{}
	addr   string
}

// startWorker boots the binary and waits for the line that names the address its socket actually bound.
//
// --reclaim-timeout is NOT set: left at 0 the worker derives the shortest window it considers safe from the heartbeat, and
// it refuses anything shorter (a 3s window was tried and refused with "below the minimum safe window ... (41s)"). That
// floor is dominated by a fixed 30s tolerable-stall allowance, so no heartbeat makes a dead worker's run reclaimable in
// seconds; the kill test below therefore waits about 41s for the takeover, and that wait is the worker's own invariant, not
// slack in the test.
func startWorker(t *testing.T, bin, appDSN, ownerDSN string) *worker {
	t.Helper()
	cmd := exec.Command(bin, "--driver=postgres", "--db="+appDSN, "--migrate-db="+ownerDSN, "--api-addr=127.0.0.1:0",
		"--poll=100ms", "--heartbeat=500ms")
	w := &worker{cmd: cmd, out: &syncBuffer{}, exited: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = w.out, w.out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait(); close(w.exited) }()
	t.Cleanup(w.kill)

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-w.exited:
			t.Fatalf("cleat-worker exited before reporting a bound API address:\n%s", w.out.String())
		default:
		}
		for _, line := range strings.Split(w.out.String(), "\n") {
			var rec struct {
				Msg  string `json:"msg"`
				Addr string `json:"addr"`
			}
			if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "HTTP API listening" {
				w.addr = rec.Addr
				return w
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	w.kill()
	t.Fatalf("no \"HTTP API listening\" line within 90s:\n%s", w.out.String())
	return nil
}

// kill is SIGKILL: no shutdown, no flush, no lease release. Idempotent.
func (w *worker) kill() {
	_ = w.cmd.Process.Kill()
	<-w.exited
}

func generateAPIKey(t *testing.T, bin, appDSN string) string {
	t.Helper()
	out, err := exec.Command(bin, "--driver=postgres", "--db="+appDSN, "--generate-api-key="+defaultTenant).CombinedOutput()
	if err != nil {
		t.Fatalf("--generate-api-key: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Key:") {
			if f := strings.Fields(line); len(f) == 2 {
				return f[1]
			}
		}
	}
	t.Fatalf("no Key: line in --generate-api-key output:\n%s", out)
	return ""
}

func deployWorkflow(t *testing.T, ctl, ownerDSN, name, wasmPath string) {
	t.Helper()
	out, err := exec.Command(ctl, "-db", ownerDSN, "deploy", "workflow", name, wasmPath).CombinedOutput()
	if err != nil {
		t.Fatalf("cleatctl deploy workflow %s: %v\n%s", name, err, out)
	}
}

// api calls the worker's HTTP API as the default tenant and decodes a JSON object reply.
func api(t *testing.T, w *worker, key, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://"+w.addr+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m
}

func startRun(t *testing.T, w *worker, key, name string, input any) string {
	t.Helper()
	code, r := api(t, w, key, "POST", "/api/workflows/"+name+"/start", map[string]any{"input": input})
	id, _ := r["id"].(string)
	if (code != 200 && code != 201) || id == "" {
		t.Fatalf("start %s: HTTP %d %v", name, code, r)
	}
	return id
}

func isTerminal(status string) bool {
	switch status {
	case "done", "failed", "dead_lettered", "terminated", "cancelled":
		return true
	}
	return false
}

// waitTerminal polls until the run is terminal and returns its reply. The worker is the one it is asked of, so after a
// kill the caller passes the replacement.
func waitTerminal(t *testing.T, w *worker, key, id string, within time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(within)
	var last map[string]any
	for time.Now().Before(deadline) {
		code, r := api(t, w, key, "GET", "/api/workflows/"+id, nil)
		if code == 200 {
			last = r
			if s, _ := r["status"].(string); isTerminal(s) {
				return r
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("run %s was not terminal within %s; last reply %v\nworker log tail:\n%s", id, within, last, tail(w.out.String(), 3000))
	return nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// resultObject decodes a run's stored result, which the API returns as a JSON string holding the workflow's return value.
func resultObject(t *testing.T, r map[string]any) map[string]any {
	t.Helper()
	var obj map[string]any
	switch v := r["result"].(type) {
	case string:
		if err := json.Unmarshal([]byte(v), &obj); err != nil {
			t.Fatalf("result %q is not a JSON object: %v", v, err)
		}
	case map[string]any:
		obj = v
	default:
		t.Fatalf("run has no result: %v", r)
	}
	return obj
}

// workerEnv is a database, the two binaries, and the three modules built and deployed: everything a test needs but a
// running worker.
type workerEnv struct {
	root, workerBin, ownerDSN, appDSN, key string
}

func newWorkerEnv(t *testing.T, modules map[string]string) *workerEnv {
	t.Helper()
	root := needToolchain(t)
	admin := needPostgres(t)

	bin := t.TempDir()
	e := &workerEnv{root: root, workerBin: filepath.Join(bin, "cleat-worker")}
	goBuild(t, root, e.workerBin, "./cmd/cleat-worker")
	ctl := filepath.Join(bin, "cleatctl")
	goBuild(t, root, ctl, "./cmd/cleatctl")

	e.ownerDSN = scratchDB(t, admin)
	if out, err := exec.Command(e.workerBin, "--driver=postgres", "--db="+e.ownerDSN, "--migrate-only").CombinedOutput(); err != nil {
		t.Fatalf("--migrate-only: %v\n%s", err, out)
	}
	e.appDSN = appRoleDSN(t, e.ownerDSN)
	e.key = generateAPIKey(t, e.workerBin, e.appDSN)

	for name, src := range modules {
		res := build(t, root, src, name)
		if res.exit != 0 {
			t.Fatalf("building %s: build.sh exited %d:\n%s", src, res.exit, res.output)
		}
		deployWorkflow(t, ctl, e.ownerDSN, name, res.module)
	}
	return e
}

// TestTypeScriptRunsAllThreeSuspensionsOnARealWorker runs the journey workflow: a durable sleep, then a wait for a signal,
// then a child workflow that itself sleeps. Every resume replays the whole run from its history, so the answer it returns
// is also a check that replay agreed with the first execution.
func TestTypeScriptRunsAllThreeSuspensionsOnARealWorker(t *testing.T) {
	e := newWorkerEnv(t, map[string]string{
		"journey": "tests/typescript/testdata/workflows/journey.ts",
		"sleeper": "tests/typescript/testdata/workflows/sleeper.ts",
	})
	w := startWorker(t, e.workerBin, e.appDSN, e.ownerDSN)

	id := startRun(t, w, e.key, "journey", map[string]any{"sleepMs": 500, "childMs": 500})
	// A signal is stored until the run asks for it, so it may be sent before the run reaches its wait.
	code, r := api(t, w, e.key, "POST", "/api/workflows/"+id+"/signal", map[string]any{"signal_name": "go", "payload": `"hello"`})
	if code != 200 && code != 201 && code != 202 {
		t.Fatalf("signal: HTTP %d %v", code, r)
	}

	done := waitTerminal(t, w, e.key, id, 90*time.Second)
	if s, _ := done["status"].(string); s != "done" {
		t.Fatalf("status = %q, want done; reply %v\nworker log tail:\n%s", s, done, tail(w.out.String(), 3000))
	}
	got := resultObject(t, done)
	if got["id"] != id {
		t.Errorf("result.id = %v, want the run's own id %s", got["id"], id)
	}
	if fmt.Sprint(got["signal"]) != "hello" {
		t.Errorf("result.signal = %v, want the payload that was sent", got["signal"])
	}
	child, _ := got["child"].(map[string]any)
	if child == nil || child["slept"] != float64(500) {
		t.Errorf("result.child = %v, want the sleeper child's own result {slept: 500}", got["child"])
	}
	if got["now"] != true {
		t.Errorf("result.now = %v, want true", got["now"])
	}
}

// TestTypeScriptRunSurvivesAKillInTheMiddleOfExecution kills the worker with SIGKILL while the guest is EXECUTING, not while
// it is suspended, then starts a different worker and requires the run to finish there with the answer an uninterrupted run
// of the same input gives.
//
// "In the middle of executing" is a precondition and not an assumption: the test reads the run's status as `running` before
// the kill and again after a pause, and fails as unmeasured if the burn had already finished. A run waiting out a sleep is
// not held by any worker (it reads `ready`), so killing the worker then would show durability and nothing about reclaim.
// What separates the two afterwards is reclaim_count: a run that merely woke on a new worker has 0, a run taken over from a
// dead one has at least 1.
func TestTypeScriptRunSurvivesAKillInTheMiddleOfExecution(t *testing.T) {
	e := newWorkerEnv(t, map[string]string{"busy": "tests/typescript/testdata/workflows/busy.ts"})
	first := startWorker(t, e.workerBin, e.appDSN, e.ownerDSN)

	// Long enough to be killed inside, short of the worker's per-instance limit on a slow shared runner.
	const iters = 30_000_000
	input := map[string]any{"iters": iters}

	base := waitTerminal(t, first, e.key, startRun(t, first, e.key, "busy", input), 120*time.Second)
	if s, _ := base["status"].(string); s != "done" {
		t.Fatalf("the uninterrupted baseline run ended %q, want done: %v", s, base)
	}
	wantSum := resultObject(t, base)["sum"]

	id := startRun(t, first, e.key, "busy", input)
	status := func() string {
		code, r := api(t, first, e.key, "GET", "/api/workflows/"+id, nil)
		if code != 200 {
			t.Fatalf("GET run: HTTP %d %v", code, r)
		}
		s, _ := r["status"].(string)
		return s
	}
	deadline := time.Now().Add(60 * time.Second)
	for status() != "running" {
		if time.Now().After(deadline) {
			t.Fatalf("UNMEASURED: the run never reported running; worker log tail:\n%s", tail(first.out.String(), 3000))
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(1 * time.Second)
	if s := status(); s != "running" {
		t.Fatalf("UNMEASURED: the run was %q one second after it started running, so the burn is shorter than the kill delay on this "+
			"machine and a kill now would land outside execution; raise iters", s)
	}

	owner, err := sql.Open("postgres", e.ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	var rows int
	if err := owner.QueryRow(`SELECT count(*) FROM event_history WHERE workflow_id = $1`, id).Scan(&rows); err != nil {
		t.Fatalf("reading the run's history: %v", err)
	}
	if rows == 0 {
		t.Fatalf("UNMEASURED: the run has no persisted history row before the burn, so there is nothing for a replay to resume from")
	}

	first.kill()
	// A closed port is what makes "the first worker is gone" a fact and not an assumption.
	if c, err := net.DialTimeout("tcp", first.addr, 500*time.Millisecond); err == nil {
		c.Close()
		t.Fatalf("the killed worker's API address %s still accepts connections", first.addr)
	}

	second := startWorker(t, e.workerBin, e.appDSN, e.ownerDSN)
	done := waitTerminal(t, second, e.key, id, 150*time.Second)
	if s, _ := done["status"].(string); s != "done" {
		t.Fatalf("status = %q after the kill, want done; reply %v\nsecond worker log tail:\n%s", s, done, tail(second.out.String(), 3000))
	}
	got := resultObject(t, done)
	if got["sum"] != wantSum {
		t.Errorf("result.sum = %v after the kill, want %v (the uninterrupted run's)", got["sum"], wantSum)
	}
	if got["workflow"] != id {
		t.Errorf("result.workflow = %v, want the run's own id %s: a different run finished", got["workflow"], id)
	}
	if n, _ := done["reclaim_count"].(float64); n < 1 {
		t.Errorf("reclaim_count = %v, want at least 1: the run was not taken over from the dead worker", done["reclaim_count"])
	}
	t.Logf("history rows before the kill: %d; reclaim_count after: %v", rows, done["reclaim_count"])
}
