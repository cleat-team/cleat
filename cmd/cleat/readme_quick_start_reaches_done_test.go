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
// EXTRACTED, NOT RETYPED (cleat-review, R2 on the first version of this test). The first version
// hardcoded "./testdata/hello/", "greet.wasm", "hello" and the curl body as Go string literals --
// which meant a PR that reverted README's step 3 back to testdata/basic would still pass, because
// nothing here ever read README.md. That is #2788's own gap, reproduced by the guard meant to
// close it. This version instead parses the actual Quick Start fenced block out of README.md --
// same technique TestREADMEsTryItSnippetCompletes already uses for the "try it" snippet, extended
// to a multi-step block -- and runs ITS build target, ITS deploy name and wasm, and ITS curl body.
// A revert to testdata/basic changes what this test builds and deploys, not just what it compares
// against, so it fails for the real reason (the catalog DurableCall) rather than a string mismatch.
// Falsified accordingly: see the file-level note in cmd/cleat/quickstart_extraction_test.go.
//
// This test runs the documented commands, in the documented order, against the documented flags --
// steps 1-7 of README.md's Quick Start block, with three substitutions, each forced by running
// inside a test process rather than a person's shell, and each already established elsewhere in
// this package for the same reason:
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
//   - The kernel-assigned API port (127.0.0.1:0) instead of the README's literal :8080, so a runner
//     with something already bound to it does not fail a test about the walkthrough. The port is not
//     pre-chosen: the worker binds ":0" and the test reads the address back from the worker's own log
//     line (startWorkerWithDiscoveredPort), so the allocation and the bind are one act and there is no
//     window for another process to take the number (cleat#3131, cleat#3136/#3137).
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

	qs := parseQuickStart(t, filepath.Join(repoRoot, "README.md"))

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

	// Step 3: compile README's documented build target (cleat#2473: the -o directory must sit
	// outside this module).
	buildOut := t.TempDir()
	if out, err := runCleatIn(t, repoRoot, "build", "-o", buildOut, qs.buildTarget); err != nil {
		t.Fatalf("cleat build -o %s %s: %v\n%s", buildOut, qs.buildTarget, err, out)
	}
	wasmPath := filepath.Join(buildOut, qs.wasmBasename)

	// Step 4: deploy to the owner DSN, under README's documented workflow name.
	if out, err := runCleatIn(t, repoRoot, "deploy", "--db", ownerDSN,
		"--name", qs.deployName, wasmPath); err != nil {
		t.Fatalf("cleat deploy --db <owner> --name %s: %v\n%s", qs.deployName, err, out)
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
	// point: it has no default). The address is 127.0.0.1:0 -- the kernel picks the port and the
	// worker binds it as one act -- and the test reads the bound port back from the worker's own log
	// line rather than pre-choosing one with holdTCPPort. That closes the cross-allocator gap the hold
	// cannot: there is no interval between the allocation and the bind for anything to take the number
	// in. Before cleat#3137 the worker logged its CONFIGURED --api-addr, so ":0" reported ":0"; it now
	// logs the socket's actual address. See startWorkerWithDiscoveredPort.
	base, worker := startWorkerWithDiscoveredPort(t, workerBin, appDSN)
	t.Cleanup(func() {
		if worker.Process != nil {
			worker.Process.Kill()
			worker.Wait()
		}
	})
	waitForHealthz(t, base+"/healthz")

	// Step 7: mint an API key for the default tenant and trigger the workflow, with README's own
	// documented start path and body. No "entry_point" in the body: the deployed WASM declares
	// exactly one entry point, which determineEntryPoint (cmd/cleat-worker/setup.go) resolves from
	// cleat.metadata without the caller naming it.
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
		base+"/api/workflows/"+qs.startName+"/start",
		strings.NewReader(qs.startBody))
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
