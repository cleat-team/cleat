// cleat#2788 (split from cleat#2469). README.md's "## Quick Start" section is the newcomer path
// this repo advertises first, and until now nothing in CI ran it: `TestREADMEsTryItSnippetCompletes`
// (renamed from TestREADMEsFirstCommandCompletes, cleat#2469) covers only the three-line "try it"
// snippet above it, not this seven-step walkthrough. A PR that broke any of these steps passed CI.
//
// Building this test found that the walkthrough could not reach `done` as originally written:
// steps 3/4/7 built, deployed and triggered testdata/basic's PlaceOrder, which makes a DurableCall
// to a "catalog" service nothing in this repository provides -- the exact defect cleat#1967 found
// and fixed in the README's OTHER snippet (the "try it" block, by switching it to testdata/hello's
// dependency-free Greet). That fix never reached this section, so the same defect survived here,
// unguarded, for the five weeks between #2003 and this test. Fixed the same way, in the same
// commit as this test: this section now builds/deploys/triggers testdata/hello's Greet instead of
// testdata/basic's PlaceOrder. See README.md's own Quick Start comment block for the change note.
//
// This test runs the documented commands, in the documented order, against the documented flags --
// steps 1-7 of README.md's Quick Start block, reproduced literally except for three substitutions,
// each forced by running inside a test process rather than a person's shell, and each already
// established elsewhere in this package for the same reason:
//
//   - Postgres: `docker run postgres:16` (startSandboxPostgres, in
//     fullstack_template_run_starts_a_workflow_test.go) instead of
//     `docker compose -f docker-compose.partner.yml up -d postgres`. The compose file names a single,
//     unmodified `postgres:16` service; it is not itself under test, and avoiding docker-compose as a
//     second tool dependency is the same call that test already made, for the same reason.
//   - `ALTER ROLE cleat_app LOGIN PASSWORD 'cleat_app'` issued through database/sql (the "postgres"
//     driver is already linked into this binary transitively through package engine, per
//     cmd/cleat/db.go's doc comment) instead of shelling out to `psql` -- CI does not install psql by
//     default (ci.yml installs it explicitly for the one job that needs it), and a Go test asserting a
//     Go-documented SQL statement should not need a second binary on PATH to issue it.
//   - A free, kernel-assigned TCP port (freeTCPPort) instead of the README's literal :8080, so a
//     runner with something already bound to it does not fail a test about the walkthrough.
//
// Step 0 (`make setup`, toolchain verification) is not run: it has no effect on whether steps 1-7
// work, and this test already requires everything it would check (go, and docker via the
// substitution above).
//
// It asserts the run reaches `done` with a `complete` published state, not merely that the curl in
// step 7 returned 2xx with a run id -- the same distinction TestFullstackTemplateRunStartsAWorkflow's
// own doc comment gives for why: a route or entry-point bug can produce an id and fail a moment
// later.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestREADMEQuickStartReachesADoneWorkflow(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	// Step 1: build ./bin/cleat and ./bin/cleat-worker. Reuses the binaries TestMain and
	// buildWorkerBinaryOnce already build once for the whole package, rather than rebuilding into
	// ./bin -- the README's ./bin path exists so a reader has one place both binaries land, which
	// this process's own temp-dir binaries already satisfy.
	workerBin := buildWorkerBinaryOnce(t)

	// Step 2: start Postgres (substituted, see file doc comment) and apply the schema with
	// --migrate-only against the owner DSN.
	ownerDSN, _ := startSandboxPostgres(t)
	if out, err := exec.Command(workerBin, "--migrate-only", "--db", ownerDSN).CombinedOutput(); err != nil {
		t.Fatalf("cleat-worker --migrate-only --db <owner>: %v\n%s", err, out)
	}

	// Step 3: compile testdata/hello to WASM (see file doc comment for why hello, not basic), into a
	// directory outside this module (cleat#2473).
	buildOut := t.TempDir()
	if out, err := runCleatIn(t, repoRoot, "build", "-o", buildOut, "./testdata/hello/"); err != nil {
		t.Fatalf("cleat build -o %s ./testdata/hello/: %v\n%s", buildOut, err, out)
	}
	wasmPath := filepath.Join(buildOut, "greet.wasm")

	// Step 4: deploy to the owner DSN.
	if out, err := runCleatIn(t, repoRoot, "deploy", "--db", ownerDSN,
		"--name", "hello", wasmPath); err != nil {
		t.Fatalf("cleat deploy --db <owner> --name hello: %v\n%s", err, out)
	}

	// Step 5: give the worker a role it will accept -- cleat_app exists (schema-created, NOLOGIN by
	// default per cleat#2468) but cannot log in until this ALTER. Substituted via database/sql, see
	// file doc comment; identical statement to the README's psql invocation.
	odb, err := sql.Open("postgres", ownerDSN)
	if err != nil {
		t.Fatalf("open owner DSN: %v", err)
	}
	defer odb.Close()
	if _, err := odb.Exec(`ALTER ROLE cleat_app LOGIN PASSWORD 'cleat_app'`); err != nil {
		t.Fatalf("ALTER ROLE cleat_app LOGIN PASSWORD ...: %v", err)
	}
	appDSN := strings.Replace(ownerDSN, "cleat:cleat@", "cleat_app:cleat_app@", 1)
	if appDSN == ownerDSN {
		t.Fatalf("could not derive the app DSN from the owner DSN %q (expected a cleat:cleat@ userinfo)", ownerDSN)
	}

	// Step 6: start the worker daemon against the app DSN, with an explicit --api-addr (the README's
	// point: it has no default).
	apiPort := freeTCPPort(t)
	base := fmt.Sprintf("http://localhost:%d", apiPort)
	worker := exec.Command(workerBin, "--db", appDSN, "--api-addr", fmt.Sprintf(":%d", apiPort))
	if err := worker.Start(); err != nil {
		t.Fatalf("start cleat-worker: %v", err)
	}
	t.Cleanup(func() {
		if worker.Process != nil {
			worker.Process.Kill()
			worker.Wait()
		}
	})
	waitForHealthz(t, base+"/healthz")

	// Step 7: mint an API key for the default tenant and trigger the workflow.
	//
	// No "entry_point" in the body: testdata/hello declares exactly one (Greet), which
	// determineEntryPoint (cmd/cleat-worker/setup.go) resolves from cleat.metadata without the
	// caller naming it. That matters here because it CANNOT be named explicitly alongside a
	// bare-string input -- plugin.MergeEntryPoint requires "input" to be a JSON object so
	// "__entry_point" has a place to merge into, and Greet's only parameter is a single string,
	// which binds the WHOLE input value as text (the build's own W003 warning). Naming
	// entry_point here would need input to be an object it then is not.
	keyOut, err := exec.Command(workerBin, "--db", appDSN,
		"--generate-api-key", "00000000-0000-0000-0000-000000000000").CombinedOutput()
	if err != nil {
		t.Fatalf("cleat-worker --generate-api-key: %v\n%s", err, keyOut)
	}
	keyMatch := regexp.MustCompile(`Key:\s+(cleat_sk_[0-9a-f]+)`).FindStringSubmatch(string(keyOut))
	if keyMatch == nil {
		t.Fatalf("--generate-api-key printed no key:\n%s", keyOut)
	}
	apiKey := keyMatch[1]

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		base+"/api/workflows/hello/start",
		strings.NewReader(`{"input":"Ada"}`))
	if err != nil {
		t.Fatalf("build start request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST start: %v", err)
	}
	defer resp.Body.Close()
	var started struct {
		ID string `json:"id"`
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	if jsonErr := json.Unmarshal(bodyBytes, &started); resp.StatusCode/100 != 2 || jsonErr != nil || started.ID == "" {
		t.Fatalf("POST start = %d (decode err %v): %s", resp.StatusCode, jsonErr, bodyBytes)
	}

	status := pollWorkflowStatus(t, base+"/api/workflows/"+started.ID, apiKey)
	if status.Status != "done" {
		t.Fatalf("run %s ended %q (error: %q), want done -- the README's Quick Start says this run finishes",
			started.ID, status.Status, status.Error)
	}
}
