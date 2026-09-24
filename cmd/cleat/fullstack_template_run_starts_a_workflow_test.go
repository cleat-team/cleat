// cleat#1969. `make run` and web/index.html both POST to
// /api/workflows/my-fullstack-app -- one path segment, no /start -- and with
// the input unwrapped ({"item":...} instead of {"input":{"item":...}}).
// Neither reaches the start handler: the router has no case for POST at one
// path segment, so it falls through to 404, and the input shape was wrong
// regardless.
//
// #1949 (the_templates_own_commands_are_run_test.go) extracts and runs every
// `cleat ...` command a scaffold documents, and explicitly did not cover
// `run` or the web page -- the Makefile's `run` target is a curl command, not
// a `cleat` command, so documentedCleatCommands' extractor does not even see
// it (it filters on fields[0] == "cleat"). That gap is why this bug shipped
// past a test whose whole point was "only running the documented command
// does."
//
// "NOT MERELY A 2XX, AND NOT MERELY THAT CURL RAN" is the issue's own
// standard, because a route/body-shape bug can produce a misleading 2xx: a
// POST to the wrong route can still be answered by SOME handler (a 404 body
// is itself valid JSON-looking text under a loose check), and a string
// search over the Makefile/index.html source for "/start" cannot tell "the
// text is present" from "the text reaches a real request" -- the same "a
// text search cannot tell a thing from a sentence about the thing" trap this
// repo's CLAUDE.md names repeatedly. So this starts a REAL worker against a
// REAL database, deploys the REAL compiled workflow, runs the scaffold's OWN
// `make run` target unmodified, and asserts the response carries a run id.
//
// Built from source (cleat-worker, cleat) rather than docker-compose's
// published ghcr.io/cleat-team/cleat-worker:latest image the template
// documents for humans: this test is about a PR's own change reaching the
// route, which the published image cannot reflect, and a live network image
// pull is not something to make a PR gate depend on.
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
	"strings"
	"testing"
	"time"
)

func TestFullstackTemplateRunStartsAWorkflow(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not available -- the scaffold's own run target needs it")
	}

	workerBinary := buildWorkerBinaryOnce(t)

	root := t.TempDir()
	if out, err := runCleatIn(t, root, "init", "--template", "fullstack", "my-fullstack-app"); err != nil {
		t.Fatalf("cleat init --template fullstack: %v\n%s", err, out)
	}
	proj := filepath.Join(root, "my-fullstack-app")

	if out, err := runCleatIn(t, proj, "build", "-o", "./out", "."); err != nil {
		t.Fatalf("cleat build: %v\n%s", out, err)
	}
	wasmPath := filepath.Join(proj, "out", "submit_order.wasm")
	if _, err := os.Stat(wasmPath); err != nil {
		t.Fatalf("build did not produce %s: %v", wasmPath, err)
	}

	dsn, containerName := startSandboxPostgres(t)

	// The scaffold's docker-compose.yml migrates the schema in a separate
	// one-shot `--migrate-only` step (cleat#2117), run as the postgres
	// superuser, before the serving worker starts as the unprivileged
	// cleat_app role and only VERIFIES the schema (cleat#2067 break 2: the
	// serving connection must not be a superuser, or PostgreSQL exempts it
	// from row-level security). This test starts one binary directly rather
	// than reproducing that two-step split, so it uses --migrate-on-start
	// instead -- equivalent for a single process. So it has to start, and
	// become healthy, BEFORE `cleat deploy` writes a workflow_defs row into
	// a schema that does not exist yet.
	//
	// The scaffold hardcodes localhost:8080 in both the Makefile and
	// web/index.html -- matching that rather than parameterizing the test
	// worker's port is what lets `make run` be run completely unmodified.
	worker := exec.Command(workerBinary,
		"--db="+dsn,
		"--api-addr=:8080",
		"--require-auth=false",
		"--migrate-on-start",
	)
	// migration.NewRunner is given the literal relative path "migrations"
	// (cmd/cleat-worker/main.go), resolved against the process's CWD -- not
	// the binary's location. Run it from the repo root, where migrations/
	// actually lives, rather than this test package's directory.
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(repoRoot, "migrations", "postgres")); statErr != nil {
		t.Fatalf("computed repo root %s has no migrations/postgres: %v", repoRoot, statErr)
	}
	worker.Dir = repoRoot
	waitForPortFree(t, 8080)
	worker.Env = os.Environ()
	var workerOut strings.Builder
	worker.Stdout = &workerOut
	worker.Stderr = &workerOut
	if err := worker.Start(); err != nil {
		t.Fatalf("start cleat-worker: %v", err)
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

	waitForHealthz(t, "http://localhost:8080/healthz")

	deployCmd := exec.Command(cleatBinary, "deploy", "--name", "my-fullstack-app", "./out/submit_order.wasm")
	deployCmd.Dir = proj
	deployCmd.Env = append(os.Environ(), "CLEAT_DATABASE_URL="+dsn)
	if out, err := deployCmd.CombinedOutput(); err != nil {
		t.Fatalf("cleat deploy: %v\n%s", err, out)
	}

	runCmd := exec.Command("make", "run")
	runCmd.Dir = proj
	runCmd.Env = os.Environ() // the target reads CLEAT_API_KEY; --require-auth=false means it is not checked
	out, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make run: %v\n%s\n\nworker output:\n%s", err, out, workerOut.String())
	}

	// `curl -fsS` in the Makefile already turns a non-2xx into a non-zero
	// exit, which the err check above would have caught. What that does NOT
	// rule out is a 2xx response carrying no id -- exactly what a request
	// answered by the wrong handler could still produce -- so the id is
	// parsed and checked directly rather than trusting the exit code alone.
	//
	// `make` echoes each recipe line before running it (the run: target has
	// no leading @), so out is the echoed curl command followed by curl's
	// own stdout -- not JSON alone. The response is curl's last line.
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	jsonLine := lines[len(lines)-1]

	var resp struct {
		ID string `json:"id"`
	}
	if jsonErr := json.Unmarshal([]byte(jsonLine), &resp); jsonErr != nil {
		t.Fatalf("make run's last output line did not parse as JSON: %v\nline: %s\nfull output: %s",
			jsonErr, jsonLine, out)
	}
	if resp.ID == "" {
		t.Fatalf("make run succeeded but returned no run id -- output: %s", out)
	}
	t.Logf("started run %s", resp.ID)

	// cleat#2066: a run id alone does not prove the workflow ran. Before that
	// fix, this exact sequence -- deploy via the CLI, start via the
	// Makefile's documented body with no __entry_point -- got a 2xx with an
	// id and then failed a moment later with "cannot determine entry point",
	// invisible to every assertion above. Poll to a terminal status and check
	// the failure, if any, is not that one. Not asserting success outright:
	// this template's first run is DOCUMENTED to fail for an unrelated,
	// already-tracked reason (cleat#2067 item 5, an egress-refused
	// http.fetch to a placeholder URL) -- conflating that into this test
	// would make it fail for a reason cleat#2066 doesn't own and didn't fix.
	status := pollWorkflowStatus(t, "http://localhost:8080/api/workflows/"+resp.ID)
	if status.Status == "failed" && strings.Contains(status.Error, "cannot determine entry point") {
		t.Fatalf("workflow %s failed on entry-point resolution -- cleat#2066 regressed: %s", resp.ID, status.Error)
	}
	t.Logf("run %s reached terminal status %q (error: %q)", resp.ID, status.Status, status.Error)
}

// pollWorkflowStatus polls a workflow's status endpoint until it reaches a
// terminal status (done, failed, terminated, dead_lettered) or 20s pass.
func pollWorkflowStatus(t *testing.T, url string) struct {
	Status string `json:"status"`
	Error  string `json:"error"`
} {
	t.Helper()
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	terminal := map[string]bool{"done": true, "failed": true, "terminated": true, "dead_lettered": true}
	deadline := time.Now().Add(20 * time.Second)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
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
	t.Fatalf("workflow at %s did not reach a terminal status within 20s (last status: %q)", url, resp.Status)
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
