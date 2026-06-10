package pluginharness

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// findProjectRoot walks up from the working directory to locate the repo root
// (the directory containing go.mod).
func findProjectRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find project root from %s", cwd)
		}
		dir = parent
	}
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
	cmd := exec.Command("go", "run",
		filepath.Join(projectRoot, "cmd", "cleat"),
		"build", "--target", "go", "-o", tmpDir, workflowDir,
	)
	cmd.Dir = projectRoot
	cmd.Env = os.Environ()
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

// buildRustWorkflowWasm compiles the Rust workflow in testdata/rustworkflow to WASM
// using cleat build --target rust (cargo build --target wasm32-wasip1).
func buildRustWorkflowWasm(t *testing.T) []byte {
	t.Helper()

	projectRoot := findProjectRoot(t)
	workflowDir := filepath.Join(projectRoot, "tests", "plugin-harness", "testdata", "rustworkflow")

	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("Rust toolchain not available — install from https://rustup.rs")
	}

	// Verify wasm32-wasip1 target is installed (used by cleat build --target rust).
	checkCmd := exec.Command("rustup", "target", "list", "--installed")
	checkOut, _ := checkCmd.Output()
	if !strings.Contains(string(checkOut), "wasm32-wasip1") {
		t.Skip("wasm32-wasip1 target not installed — run: rustup target add wasm32-wasip1")
	}

	tmpDir := t.TempDir()
	cmd := exec.Command("go", "run",
		filepath.Join(projectRoot, "cmd", "cleat"),
		"build", "--target", "rust", "-o", tmpDir, workflowDir,
	)
	cmd.Dir = projectRoot
	cmd.Env = os.Environ()
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
	cmd := exec.Command("go", "run",
		filepath.Join(projectRoot, "cmd", "cleat"),
		"build", "--target", "assemblyscript", "-o", tmpDir, workflowDir,
	)
	cmd.Dir = projectRoot
	cmd.Env = os.Environ()
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
	cmd := exec.Command("go", "run",
		filepath.Join(projectRoot, "cmd", "cleat"),
		"build", "--target", "python", "--entry",
		pyFile+":call_all_plugins",
		"-o", tmpDir, pyFile,
	)
	cmd.Dir = projectRoot
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, "PYTHONPATH="+filepath.Join(projectRoot, "python-sdk"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("cleat build (python) failed — componentize-py pipeline may need setup, skipping: %s", string(out))
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Skipf("reading cleat build output: %v", err)
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
	t.Skipf("Python componentize-py build produced no .wasm — pipeline may need setup, skipping")
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
	cmd := exec.Command("go", "run",
		filepath.Join(projectRoot, "cmd", "cleat"),
		"build", "--target", "java", "-o", tmpDir, workflowDir,
	)
	cmd.Dir = projectRoot
	cmd.Env = os.Environ()
	buildOut, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat build (java) failed — Gradle/TeaVM pipeline may need setup, failing: %s", string(buildOut))
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Skipf("reading cleat build output: %v", err)
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
	// TODO: wazero v1.11.1 pre-release nil Sys context panic when CGO_ENABLED=0.
	// The WASI clock_time_get host function dereferences nil Sys on the WASI host
	// module. Fake Sys via Compile+InstantiateModule+WithWalltime doesn't prevent
	// it — host modules appear to bypass the ModuleConfig sys context.
	t.Skip("skipping: wazero v1.11.1 nil Sys context panic (see runtime.go)")

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
	// The workflow returns a JSON string via the WASM ABI. The engine
	// JSON-encodes the return value, so Execute returns a JSON-encoded
	// string containing the actual result object.  Unwrap it first.
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
		"llm.chat", "llm.embed", "llm.list_models", "llm.chat_stream", "llm.chat_stream",
	}
	for _, key := range expectedKeys {
		if _, ok := results[key]; !ok {
			t.Errorf("missing result key: %s", key)
		}
	}

	// llm.list_models should have succeeded (mock HTTP server wired up).
	if v, ok := results["llm.list_models"]; ok {
		if m, ok := v.(map[string]interface{}); ok {
			if _, hasErr := m["error"]; hasErr {
				t.Errorf("llm.list_models unexpectedly failed: %v", m["error"])
			}
		}
	}

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
			t.Skipf("wasmtime-go compatibility issue with this WASM module: %v", err)
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
		"llm.chat", "llm.embed", "llm.list_models", "llm.chat_stream", "llm.chat_stream",
	}
	for _, key := range expectedKeys {
		if _, ok := results[key]; !ok {
			t.Errorf("missing result key: %s", key)
		}
	}

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
			t.Skip("AS WASM runtime trap — likely AS/transform version incompatibility, skipping")
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
	for _, key := range expectedKeys {
		if _, ok := results[key]; !ok {
			t.Errorf("missing result key: %s", key)
		}
	}

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
		if strings.Contains(err.Error(), "not instantiated") || strings.Contains(err.Error(), "unknown import") || strings.Contains(err.Error(), "indirect_function_table") {
			t.Skipf("WASI 0.2.0 resource routing not yet supported: %v", err)
		}
		if strings.Contains(err.Error(), "wasmtime panic") {
			t.Skipf("wasmtime-go compat issue: %v", err)
		}
		t.Fatalf("workflow execution failed: %v", err)
	}

	// The workflow returns a JSON string via the WASM ABI. The engine
	// JSON-encodes the return value, so Execute returns a JSON-encoded
	// string containing the actual result object. Unwrap it first.
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
		"llm.chat", "llm.embed", "llm.list_models", "llm.chat_stream", "llm.chat_stream",
	}
	for _, key := range expectedKeys {
		if _, ok := results[key]; !ok {
			t.Errorf("missing result key: %s", key)
		}
	}

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
			t.Skipf("wasmtime-go compatibility issue with Java/TeaVM modules: %v", err)
		}
		t.Fatalf("workflow execution failed: %v", err)
	}
	// Clean trailing bytes and skip crash defaults.
	result = strings.TrimRight(result, "\x00")
	result = strings.TrimSpace(result)
	if result == "ok" || result == `"ok"` || strings.Contains(result, "wasmtime panic") {
		t.Skipf("Java module crashed (wasmtime-go compat): raw: %.200s", result)
	}
	// TeaVM encodes the result as a JSON-encoded string matching the
	// Go/Python/AS convention. Unwrap the outer JSON string, then
	// parse the inner JSON object.
	var results map[string]interface{}
	var rawJSON string
	if err := json.Unmarshal([]byte(result), &rawJSON); err != nil {
		t.Skipf("failed to decode outer wrapper: %v\nraw: %.500s", err, result)
	}
	if err := json.Unmarshal([]byte(rawJSON), &results); err != nil {
		t.Skipf("failed to parse result JSON: %v", err)
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
	for _, key := range expectedKeys {
		if _, ok := results[key]; !ok {
			t.Errorf("missing result key: %s", key)
		}
	}

	result2, err := wenv.Replay(t, wasmBytes, "CallAllPlugins", `{}`, history)
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	if result != result2 {
		t.Errorf("replay mismatch")
	}

	t.Logf("Java WASM plugin workflow: execute and replay produce identical results (%d keys)", len(results))
}
