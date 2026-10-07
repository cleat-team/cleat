// cleat#2802 (split from cleat#2469/#2788). README.md's "## Quick Start" is
// guarded by TestREADMEQuickStartReachesADoneWorkflow (cleat#2798);
// docs/tutorials/quick-start.md -- the longer, 11-step walkthrough a reader
// lands on from the docs site -- was not exercised by anything in CI.
//
// WHY THIS NEEDED ITS OWN GUARD RATHER THAN REUSING #2798's. The tutorial's
// own text (quoted in #2469's original report) says it "cannot be run from
// CI without a checkout-only docker-compose.partner.yml". Checked rather than
// assumed: docker-compose.partner.yml's only content is a plain, unmodified
// postgres:16 service (docker-compose.partner.yml, repo root) -- nothing about
// its CONTENT is checkout-only, and #2798's test already established the
// substitute (startSandboxPostgres) for the identical file. So the "checkout-
// only" framing was about the compose file's repo-relative PATH, which this
// guard (running inside the checkout, like every other test in this package)
// was never going to lack either.
//
// The genuine structural difference is the fixture: README's Quick Start
// builds a fixture directory already in the repo (testdata/hello); this
// tutorial's step 4 runs `cleat init <name>` to SCAFFOLD A FRESH PROJECT, the
// same command cmd/cleat/a_scaffold_builds_test.go and
// fullstack_template_run_starts_a_workflow_test.go already exercise -- reused
// here rather than re-derived. The scaffold resolves the SDK from the module
// proxy, as a reader's would. This paragraph has needed two corrections in one
// day and is worth keeping honest: it first said a scaffold "pins an
// unpublished pseudo-version of the SDK, which only resolves against THIS
// checkout" (the submodule has been tagged since 2026-09-27, so that stopped
// being true -- cleat#3078), and then said a helper pointed the scaffold back
// at the tree (that helper was removed once the tag made it unnecessary --
// cleat#3083).
//
// EXTRACTED, NOT RETYPED (same discipline as #2798's own R2 fix, applied from
// the start here rather than added after an initial hardcoded version).
// parseTutorialQuickStart (tutorial_quickstart_extraction_test.go) parses the
// actual scaffold name, build output directory, deploy name/artifact and
// trigger route/body out of the tutorial's fenced ```bash blocks. See that
// file's own falsification note.
//
// Same three substitutions as #2798's guard, for the same reasons (its own
// doc comment explains each): startSandboxPostgres instead of `docker compose
// -f docker-compose.partner.yml up -d postgres`; database/sql's ALTER ROLE
// instead of shelling out to psql (not installed on every CI runner); a free
// kernel-assigned port instead of the tutorial's literal :8080 -- bound as ":0" and
// read back from the worker's own log line, the same substitution #2788's guard now
// makes (cleat#3131, cleat#3136/#3137).
//
// A FOURTH substitution this tutorial needs that README's did not: step 7
// tells the reader to "open a new [terminal] for the next steps" -- the
// worker runs in the foreground and step 8 runs against it from elsewhere.
// This test starts the worker as a background *exec.Cmd (the same shape
// #2798's own step 6 already uses for its worker, which never asked a reader
// to background it manually) and waits on /healthz before sending step 8's
// request, rather than trying to emulate a second terminal literally.
//
// It asserts the run reaches `done`, not merely that step 8's curl returns a
// run id -- the same distinction #2798's guard and
// TestFullstackTemplateRunStartsAWorkflow both give: a route or entry-point
// bug can produce an id and fail a moment later.
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

func TestTutorialQuickStartReachesADoneWorkflow(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	root := repoRoot(t)
	qs := parseTutorialQuickStart(t, filepath.Join(root, "docs/tutorials/quick-start.md"))

	// Step "Prerequisites": build ./bin/cleat and ./bin/cleat-worker. Reuses
	// the binaries TestMain and buildWorkerBinaryOnce already build once for
	// the whole package.
	workerBin := buildWorkerBinaryOnce(t)

	// Step 2/3: start Postgres (substituted, see file doc comment) and apply
	// the schema with --migrate-only against the owner DSN. --migrate-only and
	// --db are fixed CLI flags, not a fixture identifier the tutorial could
	// drift on independently of its prose, so they are not extracted the way
	// the scaffold/build/deploy/trigger values are.
	ownerDSN, _ := startSandboxPostgres(t)
	if out, err := exec.Command(workerBin, "--migrate-only", "--db", ownerDSN).CombinedOutput(); err != nil {
		t.Fatalf("cleat-worker --migrate-only --db <owner>: %v\n%s", err, out)
	}

	// Step 4: scaffold the tutorial's documented project name, then resolve
	// it against this checkout rather than the module proxy (see file doc
	// comment).
	scaffoldParent := t.TempDir()
	if out, err := runCleatIn(t, scaffoldParent, "init", qs.scaffoldName); err != nil {
		t.Fatalf("cleat init %s: %v\n%s", qs.scaffoldName, err, out)
	}
	proj := filepath.Join(scaffoldParent, qs.scaffoldName)

	// Step 5: compile the scaffolded project under its own documented output
	// directory name, from inside the project (the tutorial's own step 4 "cd
	// my-workflow", carried here as cmd.Dir rather than a literal cd).
	buildOut := filepath.Join(proj, qs.buildOutDir)
	if out, err := runCleatIn(t, proj, "build", "-o", buildOut, "."); err != nil {
		t.Fatalf("cleat build -o %s . (in %s): %v\n%s", qs.buildOutDir, proj, err, out)
	}
	wasmPath := filepath.Join(buildOut, qs.wasmBasename)

	// Step 6: deploy to the owner DSN, under the tutorial's documented
	// workflow name.
	if out, err := runCleatIn(t, root, "deploy", "--db", ownerDSN,
		"--name", qs.deployName, wasmPath); err != nil {
		t.Fatalf("cleat deploy --db <owner> --name %s %s: %v\n%s", qs.deployName, wasmPath, err, out)
	}

	// Step 7: give the worker a role it will accept (substituted via
	// database/sql, see file doc comment), then start it in the background --
	// the tutorial's "open a new terminal", emulated as a background process
	// plus /healthz synchronization rather than literally.
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

	// The API port is 127.0.0.1:0 -- the kernel picks it and the worker binds it as one act -- and
	// the test reads the bound port back from the worker's own log line rather than pre-choosing one
	// with holdTCPPort. See startWorkerWithDiscoveredPort for why that closes the cross-allocator gap
	// the hold cannot, and the note in readme_quick_start_reaches_done_test.go for the same change.
	base, worker := startWorkerWithDiscoveredPort(t, workerBin, appDSN)
	t.Cleanup(func() {
		if worker.Process != nil {
			worker.Process.Kill()
			worker.Wait()
		}
	})
	waitForHealthz(t, base+"/healthz")

	// Step 8: mint an API key for the default tenant and trigger the
	// workflow, with the tutorial's own documented start path and body.
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

	// Step 9: "see the result" -- poll until terminal and assert done, the
	// same distinction the file doc comment gives.
	status := pollWorkflowStatus(t, base+"/api/workflows/"+started.ID, apiKey)
	if status.Status != "done" {
		t.Fatalf("run %s ended %q (error: %q), want done -- the tutorial says this run finishes",
			started.ID, status.Status, status.Error)
	}
}
