// cleat#2067. A newcomer who runs `cleat init --template fullstack` and follows the README gets a workflow
// that reaches `done`, and this test is that newcomer.
//
// The earlier version of this test could not see any of six breaks, because it stepped around each: it built
// the worker from source instead of using the compose file's image, started it with --require-auth=false
// instead of the compose file's worker command, migrated with --migrate-on-start instead of the compose file's
// one-shot --migrate-only step and app role, and deployed with CLEAT_DATABASE_URL instead of the Makefile's own
// --db. It proved the workflow code runs. It did not prove the documented commands do, which is how a template
// whose worker exited at boot (a superuser connection), whose `make deploy` deployed nothing and exited 0, and
// whose first run ended `failed` shipped with a green test.
//
// This one runs the scaffold's OWN files, unmodified, the way the README says:
//
//	make up, make logs, make deploy, make run       (docker compose, the compose file's flags and roles)
//
// with exactly two substitutions, both through variables the scaffold documents:
//
//   - the worker image. The compose file's default, ghcr.io/cleat-team/cleat-worker:latest, is published by a
//     release, so a PR cannot pull the one that reflects its own change. The test builds the repository's own
//     Dockerfile and points CLEAT_WORKER_IMAGE at it. The DEFAULT is asserted separately (below), so renaming
//     it in the compose file is still seen.
//   - the two host ports (CLEAT_PG_PORT, CLEAT_API_PORT), so a runner that already has something on 5432 or
//     8080 does not fail a test about the template.
//
// It asserts the state the README says the run reaches, `done` with `status` = `complete`, not merely that curl
// returned a run id: a route or entry-point bug can produce a 2xx with an id and fail a moment later
// (cleat#2066).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestFullstackTemplateRunStartsAWorkflow(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not available -- the scaffold's own targets need it")
	}

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	root := t.TempDir()
	if out, err := runCleatIn(t, root, "init", "--template", "fullstack", "my-fullstack-app"); err != nil {
		t.Fatalf("cleat init --template fullstack: %v\n%s", err, out)
	}
	proj := filepath.Join(root, "my-fullstack-app")

	// The default image is what a newcomer pulls; the test substitutes it, so pin the default here.
	compose, err := os.ReadFile(filepath.Join(proj, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(regexp.MustCompile(`\$\{CLEAT_WORKER_IMAGE:-ghcr\.io/cleat-team/cleat-worker:latest\}`).FindAll(compose, -1)); n != 2 {
		t.Fatalf("docker-compose.yml names the published worker image as its default %d times, want 2 (migrate and cleat-worker)", n)
	}

	// The routes the README and the page name must be routes the worker has: `/state` never existed and every
	// poll of it was a 404 (found re-measuring cleat#2067).
	for _, f := range []string{"README.md", "web/index.html", "main.go"} {
		data, err := os.ReadFile(filepath.Join(proj, f))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "/state") {
			t.Errorf("%s names a /state route; the worker's published-state route is /api/workflows/{id}/query", f)
		}
	}

	// The scaffold ships its own unit tests, and a project's first `go test` is the second thing a newcomer
	// runs. One of them once hung on a durable sleep it never advanced the clock for.
	unit := exec.Command("go", "test", "./...", "-count=1", "-timeout", "120s")
	unit.Dir = proj
	if out, err := unit.CombinedOutput(); err != nil {
		t.Fatalf("the scaffold's own `go test ./...` fails: %v\n%s", err, out)
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	image := "cleat-template-test-worker:" + suffix
	if out, err := exec.Command("docker", "build", "-t", image, repoRoot).CombinedOutput(); err != nil {
		t.Fatalf("docker build of the repository's Dockerfile: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rmi", "-f", image).Run() })

	pgPort, apiPort := freeTCPPort(t), freeTCPPort(t)
	env := append(os.Environ(),
		"PATH="+filepath.Dir(cleatBinary)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"COMPOSE_PROJECT_NAME=cleat-template-test-"+suffix,
		"CLEAT_WORKER_IMAGE="+image,
		fmt.Sprintf("CLEAT_PG_PORT=%d", pgPort),
		fmt.Sprintf("CLEAT_API_PORT=%d", apiPort),
	)
	runMake := func(target string) (string, error) {
		cmd := exec.Command("make", target)
		cmd.Dir = proj
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() { runMake("down") }) // `make down` is `docker compose down -v`

	if out, err := runMake("up"); err != nil {
		t.Fatalf("make up: %v\n%s", err, out)
	}
	base := fmt.Sprintf("http://localhost:%d", apiPort)
	dumpLogs := func() {
		if t.Failed() {
			out, _ := runMake("logs")
			t.Logf("make logs:\n%s", out)
		}
	}
	t.Cleanup(dumpLogs)
	waitForHealthz(t, base+"/healthz")

	// README: "make logs | grep rate-limiter ... You want `rate-limiter: initialized mode=db`", and the API key.
	logs, err := runMake("logs")
	if err != nil {
		t.Fatalf("make logs: %v\n%s", err, logs)
	}
	if !strings.Contains(logs, "rate-limiter: initialized mode=db") {
		t.Errorf("make logs does not show `rate-limiter: initialized mode=db`, which the README tells the reader to look for")
	}
	keyMatch := regexp.MustCompile(`Key:\s+(cleat_sk_[0-9a-f]+)`).FindStringSubmatch(logs)
	if keyMatch == nil {
		t.Fatalf("make logs shows no auto-generated API key, which the README says `make up` prints on first start:\n%s", logs)
	}
	env = append(env, "CLEAT_API_KEY="+keyMatch[1])

	if out, err := runMake("deploy"); err != nil {
		t.Fatalf("make deploy: %v\n%s", err, out)
	} else if !strings.Contains(out, `Deployed workflow "my-fullstack-app"`) {
		// `make deploy` used to exit 0 having deployed nothing.
		t.Fatalf("make deploy exited 0 but did not deploy:\n%s", out)
	}

	out, err := runMake("run")
	if err != nil {
		t.Fatalf("make run: %v\n%s", err, out)
	}
	// `make` echoes each recipe line, so the response is curl's last line.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var resp struct {
		ID string `json:"id"`
	}
	if jsonErr := json.Unmarshal([]byte(lines[len(lines)-1]), &resp); jsonErr != nil || resp.ID == "" {
		t.Fatalf("make run's last line is not a run id (%v):\n%s", jsonErr, out)
	}
	t.Logf("started run %s", resp.ID)

	status := pollWorkflowStatus(t, base+"/api/workflows/"+resp.ID, keyMatch[1])
	if status.Status != "done" {
		t.Fatalf("run %s ended %q (error: %q), want done: the README says the first run finishes", resp.ID, status.Status, status.Error)
	}

	// And what a poller reads (the README names this route): the published state, readable after the run has finished.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/api/workflows/"+resp.ID+"/query?key=status", nil)
	req.Header.Set("Authorization", "Bearer "+keyMatch[1])
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET state: %v", err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusOK || !strings.Contains(string(body), "complete") {
		t.Errorf("GET /query?key=status after the run finished = %d %s, want 200 containing \"complete\"", r.StatusCode, body)
	}
}

// pollWorkflowStatus polls a workflow's status endpoint until it reaches a
// terminal status (done, failed, terminated, dead_lettered) or 45s pass.
func pollWorkflowStatus(t *testing.T, url string, bearer ...string) struct {
	Status string `json:"status"`
	Error  string `json:"error"`
} {
	t.Helper()
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	terminal := map[string]bool{"done": true, "failed": true, "terminated": true, "dead_lettered": true}
	deadline := time.Now().Add(45 * time.Second)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if len(bearer) > 0 {
			req.Header.Set("Authorization", "Bearer "+bearer[0])
		}
		r, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(r.Body)
			r.Body.Close()
			if r.StatusCode == http.StatusOK {
				if jsonErr := json.Unmarshal(body, &resp); jsonErr == nil && terminal[resp.Status] {
					return resp
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("workflow at %s did not reach a terminal status within 45s (last status: %q)", url, resp.Status)
	return resp
}

// buildWorkerBinaryOnce builds cmd/cleat-worker once per test process and
// returns its path. Not folded into TestMain (vet_test.go): that TestMain is
// shared by every test in this package, and building a second binary there
// would cost every one of them the build time even though only this test
// needs it.
//
// workerBinaryDir is deliberately built with os.MkdirTemp, not t.TempDir().
// This used to use t.TempDir(), on the reasoning (stated here until
// cleat#2109) that per-package test serialization made that "safe" -- it
// does the opposite: t.TempDir()'s cleanup runs via t.Cleanup on the FIRST
// test that calls this, which fires when THAT test returns, and sequential
// execution guarantees every LATER caller runs after that cleanup has
// already deleted the directory. Invisible as long as there was only one
// caller (this file, cleat#1969/#2066); the second one, cleat#2109's live
// Rust test, hit it immediately: "fork/exec .../cleat-worker: no such file
// or directory". Cleaned up in TestMain (vet_test.go), which is what
// actually spans the whole process, rather than left to leak -- matching
// how that same TestMain already handles the `cleat` CLI binary's tmpDir.
var (
	workerBinaryPath  string
	workerBinaryDir   string
	workerBinaryErr   error
	workerBinaryBuilt bool
)

func buildWorkerBinaryOnce(t *testing.T) string {
	t.Helper()
	if workerBinaryBuilt {
		if workerBinaryErr != nil {
			t.Fatalf("cached cleat-worker build failure: %v", workerBinaryErr)
		}
		return workerBinaryPath
	}
	workerBinaryBuilt = true

	dir, err := os.MkdirTemp("", "cleat-worker-build-*")
	if err != nil {
		workerBinaryErr = err
		t.Fatalf("create temp dir for cleat-worker build: %v", err)
	}
	workerBinaryDir = dir
	bin := filepath.Join(dir, "cleat-worker")
	build := exec.Command("go", "build", "-o", bin, "../cleat-worker")
	build.Dir = "."
	out, buildErr := build.CombinedOutput()
	if buildErr != nil {
		workerBinaryErr = buildErr
		t.Fatalf("build cleat-worker: %v\n%s", buildErr, out)
	}
	workerBinaryPath = bin
	return bin
}

// startSandboxPostgres starts a dedicated, disposable PostgreSQL container
// and returns a DSN for it plus the container's name for cleanup. Not
// docker-compose: this test needs exactly one dependency (a database) and
// starting it directly avoids a second tool requirement and the compose
// file's use of a published image this test is not meant to validate.
func startSandboxPostgres(t *testing.T) (dsn, containerName string) {
	t.Helper()
	containerName = "cleat-test-1969-pg-" + t.Name()
	exec.Command("docker", "rm", "-f", containerName).Run() // best effort, in case a prior run leaked one

	run := exec.Command("docker", "run", "-d", "--name", containerName,
		"-e", "POSTGRES_USER=cleat",
		"-e", "POSTGRES_PASSWORD=cleat",
		"-e", "POSTGRES_DB=cleat",
		"-p", "0:5432", // let docker pick a free host port
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

	// The official postgres image starts twice: once to run initdb, then it
	// shuts down and restarts for real. pg_isready can answer success during
	// the first instance -- confirmed by running both checks side by side
	// against a fresh container, where pg_isready already reports success
	// with only ONE "ready to accept connections" log line present. A client
	// that connects in that window gets the second startup's shutdown, which
	// reads back as "connection reset by peer" -- exactly what the worker
	// logged when this test failed in CI (fast enough locally not to lose the
	// race, slow enough on a loaded runner to lose it). Wait for the log line
	// twice, not just once, before trusting pg_isready at all.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		logs, _ := exec.Command("docker", "logs", containerName).CombinedOutput()
		readyCount := strings.Count(string(logs), "database system is ready to accept connections")
		if readyCount >= 2 {
			if err := exec.Command("docker", "exec", containerName, "pg_isready", "-U", "cleat").Run(); err == nil {
				return dsn, containerName
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("postgres did not become ready within 60s")
	return "", ""
}

// waitForPortFree waits until nothing is listening on localhost:port, up to
// 10s, before this test's own worker tries to bind it. Guards specifically
// against back-to-back runs of this test in one session: the prior worker
// process is a subprocess, and t.Cleanup's Kill+Wait ensures the PROCESS is
// gone, but the OS reclaiming the TCP port it held is a separate, not
// perfectly synchronous event. Measured once directly: a run immediately
// following another took 41s instead of the usual 13-17s, entirely inside
// waitForHealthz's 30s budget -- consistent with the new worker's bind
// racing the old one's port release. A real CI job runs this test once, so
// this mainly protects local back-to-back runs, but the cost of checking is
// a handful of failed dials, not worth skipping for that.
func waitForPortFree(t *testing.T, port int) {
	t.Helper()
	addr := fmt.Sprintf("localhost:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return // nothing answered -- the port is free
		}
		conn.Close()
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("port %d is still in use after 10s -- something other than this test's own "+
		"worker (already cleaned up by this point) is bound to it", port)
}

// waitForHealthz polls url until it answers 200 or the deadline passes.
func waitForHealthz(t *testing.T, url string) {
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
