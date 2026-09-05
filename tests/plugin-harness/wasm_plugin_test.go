package pluginharness

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/cleat/wasmtest"

	// Blank imports to trigger plugin init() registration so that
	// plugin.Discover() finds them in the test binary.
	_ "github.com/cleat-team/cleat/plugins/blobstore"
	_ "github.com/cleat-team/cleat/plugins/eventtriggers"
	_ "github.com/cleat-team/cleat/plugins/featureflags"
	_ "github.com/cleat-team/cleat/plugins/kafkaconnect"
	_ "github.com/cleat-team/cleat/plugins/llm"
	_ "github.com/cleat-team/cleat/plugins/notifications"
	_ "github.com/cleat-team/cleat/plugins/pagerdutyalert"
	_ "github.com/cleat-team/cleat/plugins/pgvector"
	_ "github.com/cleat-team/cleat/plugins/slacknotify"
	_ "github.com/cleat-team/cleat/plugins/webhookingest"
)

// What the five TestPluginCalls_Wasm_* tests assert about each plugin call.
//
// They used to assert only that the key was PRESENT. Measured 2026-09-04, that
// hid a great deal: 16 of the 17 calls FAIL in every guest language, and the
// tests passed. A result of
//
//	{"error":"plugin function pgvector/upsert not registered. ..."}
//
// under an expected key was indistinguishable from a working plugin. That is
// the same shape as the skips converted in IMPROVEMENT-PLAN 3.303 -- a check
// that reports success without checking -- and #455's own commit message
// confesses to the identical trap ("my own shape assertion missed it because it
// only checked the field was PRESENT").
//
// The failures are not a defect in the guests. The in-memory harness
// environment genuinely has no tenant context, does not register pgvector, and
// wires no plugin stream registry. So the fix is not to demand success; it is
// to require that every failure match a REASON WRITTEN DOWN HERE, and that the
// one call which does work keeps working. A new failure mode, or a new key
// failing, then has to be looked at rather than absorbed.
//
// pluginCallsThatMustSucceed is the list that should grow. knownPluginFailures
// is the list that should shrink.
var pluginCallsThatMustSucceed = map[string]bool{
	// The only one of the seventeen that works: the llm plugin's mock HTTP
	// server is wired up by NewTestPluginEnvInMemory and needs no tenant.
	"llm.list_models": true,
}

// knownPluginFailures are the reasons a plugin call is allowed to fail in the
// in-memory environment. Each entry says why, so that removing one is a
// decision rather than an edit.
var knownPluginFailures = []struct {
	substr string
	why    string
}{
	{"no tenant context", "NewTestPluginEnvInMemory installs no tenant, and most plugins scope their storage by tenant"},
	{"not registered. Check that the plugin is deployed", "pgvector is not among the 10 plugins the harness registers"},
	{"no plugin stream registry configured", "llm.chat_stream needs a stream registry the in-memory env does not wire"},

	// There is deliberately no entry here for Go's old "plugin_call: error 1"
	// legend. Until #730 (3.200, and 3.306 for the finding) the Go adapter
	// discarded the host's message and printed a constant errCode against
	// another field's legend, so Go needed two entries of its own. It now
	// carries the host's text and matches the three reasons above like every
	// other guest -- re-measured 2026-09-04 after #730, all 16 of its
	// failures. Those two entries were removed rather than left harmless:
	// a dead reason in a list whose whole purpose is to shrink is the rot
	// this list exists to prevent.
}

// pluginResultError reports the error text a plugin result carries, if any.
//
// A result that is not a JSON object cannot carry an {"error": ...} field and
// is treated as a success. That is deliberate rather than an oversight: the
// harness workflows emit an object per call, so a non-object here is a shape
// change the caller's own decode assertions are responsible for catching.
func pluginResultError(v interface{}) (string, bool) {
	m, isObject := v.(map[string]interface{})
	if !isObject {
		return "", false
	}
	e, hasErr := m["error"]
	if !hasErr {
		return "", false
	}
	return fmt.Sprint(e), true
}

// assertPluginOutcomes checks every expected key for presence AND outcome.
func assertPluginOutcomes(t *testing.T, results map[string]interface{}, expectedKeys []string) {
	t.Helper()
	for _, key := range expectedKeys {
		v, present := results[key]
		if !present {
			t.Errorf("missing result key: %s", key)
			continue
		}
		errText, failed := pluginResultError(v)
		mustSucceed := pluginCallsThatMustSucceed[key]

		switch {
		case mustSucceed && failed:
			t.Errorf("%s must succeed in this environment and did not: %s", key, errText)
		case !mustSucceed && !failed:
			t.Errorf("%s now succeeds. That is progress, and it has to be "+
				"locked in: add %q to pluginCallsThatMustSucceed so it "+
				"cannot regress silently.", key, key)
		case failed && !isKnownPluginFailure(errText):
			t.Errorf("%s failed for a reason not in knownPluginFailures: %q\n"+
				"Either the environment changed or this is a real break. Do "+
				"not widen the list without saying why.", key, errText)
		}
	}
}

func isKnownPluginFailure(errText string) bool {
	for _, k := range knownPluginFailures {
		if strings.Contains(errText, k.substr) {
			return true
		}
	}
	return false
}

// findProjectRoot walks up from the working directory to locate the repo root.
//
// It looks for the go.mod declaring the ROOT module, not merely the nearest
// go.mod. This directory is its own module (see go.mod here, and CLAUDE.md on
// the root<->cleat/ module cycle), so "nearest go.mod" stops right here and
// every path built on top of it -- testdata directories, migrations -- lands
// one repo-depth too shallow.
//
// That failure is silent, which is why the check is on the module line rather
// than on the file's existence: the callers below t.Skipf when their testdata
// directory is missing, so a wrong root reads as "test data not found" and the
// suite goes green having built and run nothing.
func findProjectRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	const rootModule = "module github.com/cleat-team/cleat\n"
	dir := cwd
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(b), rootModule) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find the repo root (a go.mod declaring %q) from %s",
				strings.TrimSuffix(rootModule, "\n"), cwd)
		}
		dir = parent
	}
}

// commandAt builds an exec.Cmd that runs in dir, with $PWD corrected to match.
//
// cmd.Dir on its own is not enough for the `go` command: it resolves the main
// module from $PWD when that is set and consistent, so a child process that
// inherits the test binary's PWD looks for a go.mod in *this* directory rather
// than in cmd.Dir. That was harmless while this directory was part of the root
// module. Since it became its own module (see go.mod, and CLAUDE.md on the
// root<->cleat/ module cycle), the inherited PWD makes
//
//	go run <repoRoot>/cmd/cleat build ...
//
// fail with "directory <repoRoot>/cmd/cleat outside main module or its selected
// dependencies" -- while cmd.Dir points at the repo root the entire time, which
// is what makes it slow to diagnose. Measured: identical command, PWD inherited
// fails, PWD set to dir succeeds.
//
// Go's exec keeps the last value for a duplicated key, so appending is enough.
func commandAt(dir, name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PWD="+dir)
	return cmd
}

// cleatBinary compiles cmd/cleat once per test binary and returns its path.
//
// `go run <repoRoot>/cmd/cleat <workflowDir>` cannot serve both halves any
// more. cmd/cleat is in the root module and the Go workflow under testdata/ is
// in this one (see go.mod), and a single `go run` resolves everything against
// one main module: run it from the repo root and the workflow is "main module
// (github.com/cleat-team/cleat) does not contain package .../testdata/
// goworkflow"; run it from here and cmd/cleat is "outside main module".
//
// Compiling the tool first splits the two resolutions apart. The CLI is built
// in the root module, and each `cleat build` below then runs with its working
// directory inside whichever module owns the workflow it is compiling.
//
// Building once also removes four redundant compiles of the CLI -- each of the
// five callers used to `go run` it separately.
var (
	cleatBinOnce sync.Once
	cleatBinPath string
	cleatBinErr  error
)

func cleatBinary(t *testing.T) string {
	t.Helper()
	cleatBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "cleat-cli")
		if err != nil {
			cleatBinErr = fmt.Errorf("temp dir for the cleat binary: %w", err)
			return
		}
		bin := filepath.Join(dir, "cleat")
		out, err := commandAt(findProjectRoot(t), "go", "build", "-o", bin, "./cmd/cleat").CombinedOutput()
		if err != nil {
			cleatBinErr = fmt.Errorf("building cmd/cleat:\n%s\n%w", out, err)
			return
		}
		cleatBinPath = bin
	})
	if cleatBinErr != nil {
		t.Fatalf("%v", cleatBinErr)
	}
	return cleatBinPath
}

// buildGoWorkflowWasm compiles the Go workflow in testdata/goworkflow to WASM
// using the cleat build pipeline (cleat build --target go).  The workflow
// is part of the main module (no separate go.mod), matching the pattern used
// by examples/subscription/ and other built-in workflow examples.
func buildGoWorkflowWasm(t *testing.T) []byte {
	t.Helper()

	projectRoot := findProjectRoot(t)
	workflowDir := filepath.Join(projectRoot, "tests", "plugin-harness", "testdata", "goworkflow")

	if _, err := os.Stat(workflowDir); os.IsNotExist(err) {
		t.Skipf("Go workflow test data not found at %s", workflowDir)
	}

	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("Go toolchain not available")
	}

	tmpDir := t.TempDir()
	cmd := commandAt(workflowDir, cleatBinary(t),
		"build", "--target", "go", "-o", tmpDir, workflowDir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat build (go) failed:\n%s\n%v", string(out), err)
	}

	// The cleat build pipeline outputs one .wasm file into tmpDir.
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("reading cleat build output: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			wasmBytes, err := os.ReadFile(filepath.Join(tmpDir, e.Name()))
			if err != nil {
				t.Fatalf("reading WASM file: %v", err)
			}
			t.Logf("built Go WASM (%d bytes) from %s using cleat+go", len(wasmBytes), workflowDir)
			return wasmBytes
		}
	}
	t.Fatalf("no .wasm file found in cleat build output: %s", tmpDir)
	return nil
}

// buildRustWorkflowWasm compiles the Rust workflow in testdata/rustworkflow to
// WASM using `cleat build --target rust`, which is the command users run.
//
// That path compiles for wasm32-unknown-unknown (cmd/cleat/build_rust.go:34),
// not wasm32-wasip1. This comment and the guard below both said wasip1, so the
// check was for a target the build does not use: a machine with wasip1 and not
// unknown-unknown passed the guard and then failed inside cargo, and one with
// unknown-unknown and not wasip1 skipped a build that would have worked.
func buildRustWorkflowWasm(t *testing.T) []byte {
	t.Helper()

	projectRoot := findProjectRoot(t)
	workflowDir := filepath.Join(projectRoot, "tests", "plugin-harness", "testdata", "rustworkflow")

	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("Rust toolchain not available — install from https://rustup.rs")
	}

	// Verify the target `cleat build --target rust` actually compiles for.
	checkCmd := exec.Command("rustup", "target", "list", "--installed")
	checkOut, _ := checkCmd.Output()
	if !strings.Contains(string(checkOut), "wasm32-unknown-unknown") {
		t.Skip("wasm32-unknown-unknown target not installed — run: rustup target add wasm32-unknown-unknown")
	}

	tmpDir := t.TempDir()
	cmd := commandAt(workflowDir, cleatBinary(t),
		"build", "--target", "rust", "-o", tmpDir, workflowDir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat build (rust) failed:\n%s\n%v", string(out), err)
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("reading cleat build output: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			wasmBytes, err := os.ReadFile(filepath.Join(tmpDir, e.Name()))
			if err != nil {
				t.Fatalf("reading WASM file: %v", err)
			}
			t.Logf("built Rust WASM (%d bytes)", len(wasmBytes))
			return wasmBytes
		}
	}
	t.Fatalf("no .wasm file found in cleat build output: %s", tmpDir)
	return nil
}

// buildASWorkflowWasm compiles the AssemblyScript workflow in testdata/asworkflow
// to WASM using cleat build --target assemblyscript.
func buildASWorkflowWasm(t *testing.T) []byte {
	t.Helper()

	projectRoot := findProjectRoot(t)
	workflowDir := filepath.Join(projectRoot, "tests", "plugin-harness", "testdata", "asworkflow")

	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not available — install Node.js")
	}

	tmpDir := t.TempDir()
	cmd := commandAt(workflowDir, cleatBinary(t),
		"build", "--target", "assemblyscript", "-o", tmpDir, workflowDir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat build (assemblyscript) failed:\n%s\n%v", string(out), err)
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("reading cleat build output: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			wasmBytes, err := os.ReadFile(filepath.Join(tmpDir, e.Name()))
			if err != nil {
				t.Fatalf("reading WASM file: %v", err)
			}
			t.Logf("built AS WASM (%d bytes)", len(wasmBytes))
			return wasmBytes
		}
	}
	t.Fatalf("no .wasm file found in cleat build output: %s", tmpDir)
	return nil
}

// buildPythonWorkflowWasm compiles the Python workflow in testdata/pythonworkflow
// to WASM using cleat build --target python (componentize-py).
func buildPythonWorkflowWasm(t *testing.T) []byte {
	t.Helper()

	projectRoot := findProjectRoot(t)
	pyFile := filepath.Join(projectRoot, "tests", "plugin-harness", "testdata", "pythonworkflow", "plugin_harness_workflow.py")

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	checkCmd := exec.Command("python3", "-c", "import componentize_py; import cleat_sdk")
	checkCmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(projectRoot, "python-sdk"))
	if err := checkCmd.Run(); err != nil {
		t.Skip("componentize-py or cleat_sdk not installed — run: pip install componentize-py && cd python-sdk && pip install -e .")
	}

	tmpDir := t.TempDir()
	cmd := commandAt(filepath.Dir(pyFile), cleatBinary(t),
		"build", "--target", "python", "--entry",
		pyFile+":call_all_plugins",
		"-o", tmpDir, pyFile,
	)
	cmd.Env = append(cmd.Env, "PYTHONPATH="+filepath.Join(projectRoot, "python-sdk"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Fatal, not Skip. Whether the toolchain is present was already decided
		// above, by importing componentize_py and cleat_sdk -- that check is
		// what legitimately skips. Reaching here means the toolchain IS
		// installed and the build broke, which is case (b) in
		// scripts/check-skips.sh's taxonomy and must fail.
		//
		// It was a Skipf, and it hid a real break: when tests/plugin-harness
		// became its own module, wasm.FindRepoRoot resolved the repo root to
		// this directory and the build could not find python-sdk/scripts/
		// build_wasm.py. The suite reported SKIP and the only thing that
		// noticed was this job's skip budget of 0.
		t.Fatalf("cleat build (python) failed although componentize-py and cleat_sdk "+
			"both import, so this is a real build failure rather than a missing "+
			"toolchain:\n%s", string(out))
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		// Fatal, not Skip, for the same reason as the build failure above:
		// toolchain presence was already decided, and the build reported
		// success. An output directory that will not read at this point is a
		// real failure.
		t.Fatalf("reading cleat build output %s: %v", tmpDir, err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			wasmBytes, err := os.ReadFile(filepath.Join(tmpDir, e.Name()))
			if err != nil {
				t.Fatalf("reading WASM file: %v", err)
			}
			t.Logf("built Python WASM (%d bytes)", len(wasmBytes))
			return wasmBytes
		}
	}
	t.Fatalf("Python componentize-py build reported success but produced no .wasm in %s", tmpDir)
	return nil
}

// buildJavaWorkflowWasm compiles the Java workflow in testdata/javaworkflow to
// WASM using cleat build --target java (Gradle + TeaVM).
func buildJavaWorkflowWasm(t *testing.T) []byte {
	t.Helper()

	projectRoot := findProjectRoot(t)
	workflowDir := filepath.Join(projectRoot, "tests", "plugin-harness", "testdata", "javaworkflow")

	if _, err := exec.LookPath("java"); err != nil {
		t.Skip("Java not available — install JDK 11+ from https://adoptium.net")
	}

	tmpDir := t.TempDir()
	cmd := commandAt(workflowDir, cleatBinary(t),
		"build", "--target", "java", "-o", tmpDir, workflowDir,
	)
	buildOut, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat build (java) failed — Gradle/TeaVM pipeline may need setup, failing: %s", string(buildOut))
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		// Fatal, not Skip, for the same reason as the build failure above:
		// toolchain presence was already decided, and the build reported
		// success. An output directory that will not read at this point is a
		// real failure.
		t.Fatalf("reading cleat build output %s: %v", tmpDir, err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			wasmBytes, err := os.ReadFile(filepath.Join(tmpDir, e.Name()))
			if err != nil {
				t.Fatalf("reading WASM file: %v", err)
			}
			t.Logf("built Java WASM (%d bytes)", len(wasmBytes))
			return wasmBytes
		}
	}
	t.Fatalf("Java TeaVM build produced no .wasm")
	return nil
}

// TestPluginCalls_Wasm_Go builds the Go workflow, executes it through the
// WASM engine with plugin support, and replays to verify determinism.
func TestPluginCalls_Wasm_Go(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping WASM compilation test in short mode")
	}
	// The unconditional skip that used to be here read "wazero v1.11.1 nil Sys
	// context panic" and was a workaround for the job running with
	// CGO_ENABLED=0. That forced wazero, and the panic is wazero's. With CGO on
	// -- the shipped configuration, and now this workflow's -- a Go workflow
	// runs on wasmtime and the test passes. Measured both ways before removing
	// it: CGO on passes, CGO off reproduces the exact nil-pointer dereference.

	// Create in-memory test env (discovers and initialises plugins).
	env := NewTestPluginEnvInMemory(t)
	defer env.Close()

	// Build the Go workflow to WASM.
	wasmBytes := buildGoWorkflowWasm(t)

	// Create wasmtest env with the plugin registry wired in.
	wenv := wasmtest.NewWasmTestEnv(t, wasmtest.WithPluginRegistry(env.Registry))
	defer wenv.Close()

	// Execute the workflow.
	result, history, err := wenv.Execute(t, wasmBytes, "call_all_plugins", `{}`)
	if err != nil {
		t.Fatalf("workflow execution failed: %v", err)
	}
	// One unmarshal, not two.
	//
	// This used to unwrap an outer JSON string first, because the generated
	// dispatch wrapper called encodeJSONString on a result the workflow had
	// already marshalled -- so {"a":1} arrived as "{\"a\":1}". The comment here
	// described that as "the engine JSON-encodes the return value", which read
	// like the design rather than the bug it was.
	//
	// The contract is a string containing a JSON-encoded object (ABI.md), the
	// wrapper now passes it through, and this test needed the extra unwrap only
	// for as long as the encoding was wrong.
	var results map[string]interface{}
	if err := json.Unmarshal([]byte(result), &results); err != nil {
		t.Fatalf("failed to parse result JSON: %v\nraw: %.2000s", err, result)
	}
	t.Logf("workflow completed with %d plugin results", len(results))

	// Verify all expected keys are present.
	expectedKeys := []string{
		"blobstore.put", "blobstore.get",
		"event-triggers.await_event",
		"feature-flags.evaluate_flag",
		"kafka-connect.produce",
		"notifications.send_webhook",
		"pagerduty-alert.trigger_incident", "pagerduty-alert.resolve_incident",
		"pgvector.upsert", "pgvector.search", "pgvector.delete",
		"slack-notify.send_message",
		"webhook-ingest.await_webhook",
		"llm.chat", "llm.embed", "llm.list_models", "llm.chat_stream",
	}
	assertPluginOutcomes(t, results, expectedKeys)

	// Replay verification: replaying from the recorded history must produce
	// the exact same result.
	result2, err := wenv.Replay(t, wasmBytes, "call_all_plugins", `{}`, history)
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	if result != result2 {
		// Show the first differing byte position.
		diffAt := -1
		minLen := len(result)
		if len(result2) < minLen {
			minLen = len(result2)
		}
		for i := 0; i < minLen; i++ {
			if result[i] != result2[i] {
				diffAt = i
				break
			}
		}
		ctx := 80
		execCtx := result
		replCtx := result2
		if diffAt >= 0 {
			start := diffAt - ctx
			if start < 0 {
				start = 0
			}
			end := diffAt + ctx
			if end > len(result) {
				end = len(result)
			}
			end2 := diffAt + ctx
			if end2 > len(result2) {
				end2 = len(result2)
			}
			execCtx = result[start:end]
			replCtx = result2[start:end2]
		}
		t.Errorf("replay mismatch (execute=%d bytes, replay=%d bytes, first diff at byte %d)\nexec: %s\nrepl: %s",
			len(result), len(result2), diffAt, execCtx, replCtx)
	}

	t.Logf("Go WASM plugin workflow: execute and replay produce identical results (%d keys)", len(results))
}

func TestPluginCalls_Wasm_Rust(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping WASM compilation test in short mode")
	}

	env := NewTestPluginEnvInMemory(t)
	defer env.Close()

	wasmBytes := buildRustWorkflowWasm(t)

	wenv := wasmtest.NewWasmTestEnv(t, wasmtest.WithPluginRegistry(env.Registry))
	defer wenv.Close()

	result, history, err := wenv.Execute(t, wasmBytes, "call_all_plugins", `{}`)
	if err != nil {
		if strings.Contains(err.Error(), "wasmtime panic") {
			t.Fatalf("wasmtime-go crashed on this WASM module: %v", err)
		}
		t.Fatalf("workflow execution failed: %v", err)
	}
	var rawJSON string
	if err := json.Unmarshal([]byte(result), &rawJSON); err != nil {
		t.Fatalf("failed to decode outer wrapper: %v\nraw: %.2000s", err, result)
	}
	var results map[string]interface{}
	if err := json.Unmarshal([]byte(rawJSON), &results); err != nil {
		t.Fatalf("failed to parse result JSON: %v\nraw: %.2000s", err, rawJSON)
	}
	t.Logf("workflow completed with %d plugin results", len(results))

	expectedKeys := []string{
		"blobstore.put", "blobstore.get",
		"event-triggers.await_event",
		"feature-flags.evaluate_flag",
		"kafka-connect.produce",
		"notifications.send_webhook",
		"pagerduty-alert.trigger_incident", "pagerduty-alert.resolve_incident",
		"pgvector.upsert", "pgvector.search", "pgvector.delete",
		"slack-notify.send_message",
		"webhook-ingest.await_webhook",
		"llm.chat", "llm.embed", "llm.list_models", "llm.chat_stream",
	}
	assertPluginOutcomes(t, results, expectedKeys)

	result2, err := wenv.Replay(t, wasmBytes, "call_all_plugins", `{}`, history)
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	if result != result2 {
		diffAt := -1
		minLen := len(result)
		if len(result2) < minLen {
			minLen = len(result2)
		}
		for i := 0; i < minLen; i++ {
			if result[i] != result2[i] {
				diffAt = i
				break
			}
		}
		ctx := 60
		start := diffAt - ctx
		if start < 0 {
			start = 0
		}
		end := diffAt + ctx
		if end > len(result) {
			end = len(result)
		}
		end2 := diffAt + ctx
		if end2 > len(result2) {
			end2 = len(result2)
		}
		t.Errorf("replay mismatch at byte %d\n  exec[%d:%d]: %s\n  repl[%d:%d]: %s",
			diffAt, start, end, result[start:end], start, end2, result2[start:end2])
	}

	t.Logf("Rust WASM plugin workflow: execute and replay produce identical results (%d keys)", len(results))
}

func TestPluginCalls_Wasm_AS(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping WASM compilation test in short mode")
	}

	env := NewTestPluginEnvInMemory(t)
	defer env.Close()

	wasmBytes := buildASWorkflowWasm(t)

	wenv := wasmtest.NewWasmTestEnv(t, wasmtest.WithPluginRegistry(env.Registry))
	defer wenv.Close()

	result, history, err := wenv.Execute(t, wasmBytes, "call_all_plugins", `{}`)
	if err != nil {
		if strings.Contains(err.Error(), "wasm trap") {
			t.Fatalf("AS WASM module trapped at runtime: %v", err)
		}
		t.Fatalf("workflow execution failed: %v", err)
	}

	var results map[string]interface{}
	// Clean trailing bytes that AS/Java may write past the JSON content.
	result = strings.TrimRight(result, "\x00")
	result = strings.TrimSpace(result)
	// Use json.Decoder which is more lenient than Unmarshal for trailing data.
	dec := json.NewDecoder(strings.NewReader(result))
	if err := dec.Decode(&results); err != nil {
		t.Fatalf("AS result JSON parse failed: %v\nraw: %.2000s", err, result)
	}
	t.Logf("workflow completed with %d plugin results", len(results))

	expectedKeys := []string{
		"blobstore.put", "blobstore.get",
		"event-triggers.await_event",
		"feature-flags.evaluate_flag",
		"kafka-connect.produce",
		"notifications.send_webhook",
		"pagerduty-alert.trigger_incident", "pagerduty-alert.resolve_incident",
		"pgvector.upsert", "pgvector.search", "pgvector.delete",
		"slack-notify.send_message",
		"webhook-ingest.await_webhook",
		"llm.chat", "llm.embed", "llm.list_models", "llm.chat_stream",
	}
	assertPluginOutcomes(t, results, expectedKeys)

	result2, err := wenv.Replay(t, wasmBytes, "call_all_plugins", `{}`, history)
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	if result != result2 {
		t.Errorf("replay mismatch")
	}

	t.Logf("AS WASM plugin workflow: execute and replay produce identical results (%d keys)", len(results))
}

// TestPluginCalls_Wasm_Python builds the Python workflow, executes it through
// the WASM engine with plugin support, and replays to verify determinism.
//
// The Python plugin workflow is at testdata/pythonworkflow/ and exercises every
// plugin host function including llm.chat_stream (streaming).
//
// Note: Python WASM components generated by componentize-py always export a
// single "run" function per the cleat WIT world definition (cleat.wit). The
// @cleat_entry("CallAllPlugins") decorator registers the function with a
// dispatcher inside the WASM module, which is invoked via the "run" export.
func TestPluginCalls_Wasm_Python(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping WASM compilation test in short mode")
	}

	// Create in-memory test env (discovers and initialises plugins).
	env := NewTestPluginEnvInMemory(t)
	defer env.Close()

	// Build the Python workflow to WASM.
	wasmBytes := buildPythonWorkflowWasm(t)

	// Create wasmtest env with the plugin registry wired in.
	wenv := wasmtest.NewWasmTestEnv(t, wasmtest.WithPluginRegistry(env.Registry))
	defer wenv.Close()

	// Execute the workflow. Python WASM components always export "run" as
	// the sole entry point per the cleat WIT world definition.
	entryPoint := "run"
	result, history, err := wenv.Execute(t, wasmBytes, entryPoint, `{}`)
	if err != nil {
		// No escape hatches. This used to skip on "not instantiated",
		// "unknown import", "indirect_function_table" and "wasmtime panic" --
		// which are, precisely and exclusively, the errors the decomposition
		// path emits when it cannot assemble a component.
		//
		// That was defensible while Python ran there and failed. It is not now:
		// Python executes on wasmtime's native Component Model runtime
		// (IMPROVEMENT-PLAN 2.72), and if it ever regresses to decomposition
		// those four strings are exactly what would come back. The skips would
		// have turned the regression they were named after into a green run.
		t.Fatalf("workflow execution failed: %v", err)
	}

	// Two unmarshals here, deliberately, unlike the Go case above.
	//
	// Go's generated wrapper now passes the result through (ABI.md: an entry
	// point returns a string containing a JSON-encoded object), so its test
	// unmarshals once. This guest still arrives double-encoded for its own
	// reason -- componentize-py hands back a JSON string, and the Rust fixture
	// returns a String that serde then serialises -- so the outer unwrap is
	// still correct HERE. Do not remove it for symmetry with the Go case.
	var rawJSON string
	if err := json.Unmarshal([]byte(result), &rawJSON); err != nil {
		t.Fatalf("failed to decode outer wrapper: %v\nraw: %.2000s", err, result)
	}
	var results map[string]interface{}
	if err := json.Unmarshal([]byte(rawJSON), &results); err != nil {
		t.Fatalf("failed to parse result JSON: %v\nraw: %.2000s", err, rawJSON)
	}
	t.Logf("workflow completed with %d plugin results", len(results))

	// Verify all expected keys are present.
	expectedKeys := []string{
		"blobstore.put", "blobstore.get",
		"event-triggers.await_event",
		"feature-flags.evaluate_flag",
		"kafka-connect.produce",
		"notifications.send_webhook",
		"pagerduty-alert.trigger_incident", "pagerduty-alert.resolve_incident",
		"pgvector.upsert", "pgvector.search", "pgvector.delete",
		"slack-notify.send_message",
		"webhook-ingest.await_webhook",
		"llm.chat", "llm.embed", "llm.list_models", "llm.chat_stream",
	}
	assertPluginOutcomes(t, results, expectedKeys)

	// Replay verification: replaying from the recorded history must produce
	// the exact same result.
	result2, err := wenv.Replay(t, wasmBytes, entryPoint, `{}`, history)
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	if result != result2 {
		t.Errorf("replay mismatch")
	}

	t.Logf("Python WASM plugin workflow: execute and replay produce identical results (%d keys)", len(results))
}

func TestPluginCalls_Wasm_Java(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping WASM compilation test in short mode")
	}

	env := NewTestPluginEnvInMemory(t)
	defer env.Close()

	wasmBytes := buildJavaWorkflowWasm(t)

	wenv := wasmtest.NewWasmTestEnv(t, wasmtest.WithPluginRegistry(env.Registry))
	defer wenv.Close()

	// Java TeaVM uses @Export(name = "CallAllPlugins") — CamelCase naming.
	result, history, err := wenv.Execute(t, wasmBytes, "CallAllPlugins", `{}`)
	if err != nil {
		if strings.Contains(err.Error(), "wasmtime panic") || strings.Contains(err.Error(), "wasm trap") {
			t.Fatalf("wasmtime-go crashed on this Java/TeaVM module: %v", err)
		}
		t.Fatalf("workflow execution failed: %v", err)
	}
	// Clean trailing bytes and detect crash defaults.
	result = strings.TrimRight(result, "\x00")
	result = strings.TrimSpace(result)
	if result == "ok" || result == `"ok"` || strings.Contains(result, "wasmtime panic") {
		t.Fatalf("Java/TeaVM module crashed: execution returned the crash-default placeholder result instead of plugin output, raw: %.200s", result)
	}
	// TeaVM encodes the result as a JSON-encoded string matching the
	// Go/Python/AS convention. Unwrap the outer JSON string, then
	// parse the inner JSON object.
	var results map[string]interface{}
	var rawJSON string
	if err := json.Unmarshal([]byte(result), &rawJSON); err != nil {
		t.Fatalf("Java/TeaVM result is not the JSON-encoded string the ABI "+
			"contract requires: %v\nraw: %.500s", err, result)
	}
	if err := json.Unmarshal([]byte(rawJSON), &results); err != nil {
		t.Fatalf("Java/TeaVM result unwrapped to text that is not a JSON "+
			"object: %v\nunwrapped: %.500s", err, rawJSON)
	}
	t.Logf("workflow completed with %d plugin results", len(results))

	expectedKeys := []string{
		"blobstore.put", "blobstore.get",
		"event-triggers.await_event",
		"feature-flags.evaluate_flag",
		"kafka-connect.produce",
		"notifications.send_webhook",
		"pagerduty-alert.trigger_incident", "pagerduty-alert.resolve_incident",
		"pgvector.upsert", "pgvector.search", "pgvector.delete",
		"slack-notify.send_message",
		"webhook-ingest.await_webhook",
		"llm.chat", "llm.embed", "llm.list_models", "llm.chat_stream",
	}
	assertPluginOutcomes(t, results, expectedKeys)

	result2, err := wenv.Replay(t, wasmBytes, "CallAllPlugins", `{}`, history)
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	if result != result2 {
		t.Errorf("replay mismatch")
	}

	t.Logf("Java WASM plugin workflow: execute and replay produce identical results (%d keys)", len(results))
}
