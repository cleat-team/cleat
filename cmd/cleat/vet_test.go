package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cleatBinary is set by TestMain after build.
var cleatBinary string

func TestMain(m *testing.M) {
	// Build the cleat binary for use in subprocess tests.
	// Skip the build in short mode so pure unit tests (e.g. TestDetectVetLang)
	// can still run; individual vet tests that need the binary have their
	// own Short() gates.
	flag.Parse()
	var tmpDir string
	if !testing.Short() {
		var err error
		tmpDir, err = os.MkdirTemp("", "cleat-vet-test-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create temp dir: %v\n", err)
			os.Exit(1)
		}

		binary := filepath.Join(tmpDir, "cleat")
		build := exec.Command("go", "build", "-o", binary, ".")
		build.Dir = "."
		if out, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to build cleat: %v\n%s", err, out)
			os.Exit(1)
		}

		cleatBinary = binary
	}
	exitCode := m.Run()
	if tmpDir != "" {
		os.RemoveAll(tmpDir)
	}
	os.Exit(exitCode)
}

// runVetCmd runs `cleat vet` with the given arguments and returns stdout and error.
// For vets that emit stderr progress messages (Rust, Java), the JSON portion
// is extracted from the combined output.
func runVetCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()

	if testing.Short() {
		t.Skip("Skipping vet test in short mode")
	}

	cmd := exec.Command(cleatBinary, args...)
	out, err := cmd.CombinedOutput()
	output := string(out)

	// Extract JSON from combined output (stderr progress messages + stdout JSON).
	// Find the first '{' which indicates the start of the JSON payload.
	if idx := strings.Index(output, "\n{"); idx >= 0 {
		output = output[idx+1:]
	} else if idx := strings.Index(output, "{"); idx >= 0 {
		output = output[idx:]
	}

	return output, err
}

// vetFixture is a helper for Go vet tests that expect specific error codes.
// It parses JSON output and checks for the expected error code.
func vetFixture(t *testing.T, fixtureDir, expectedCode string) {
	t.Helper()
	fixture := filepath.Join("..", "..", "testdata", "vet-checks", "go", fixtureDir)
	out, err := runVetCmd(t, "vet", "--lang", "go", "--json", fixture)

	// A non-zero exit is expected when errors are found.
	_ = err

	var result VetOutput
	if jsonErr := json.Unmarshal([]byte(out), &result); jsonErr != nil {
		t.Fatalf("failed to parse JSON output: %v\nstdout: %s", jsonErr, out)
	}

	if len(result.Errors) == 0 {
		t.Fatalf("expected at least one error with code %q, got none", expectedCode)
	}

	found := false
	for _, e := range result.Errors {
		if e.Code == expectedCode {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected error code %q in results, got: %+v", expectedCode, result.Errors)
	}

	// Verify error references the correct file.
	for _, e := range result.Errors {
		if e.Code == expectedCode {
			if e.File == "" {
				t.Errorf("error %q has empty file field", expectedCode)
			}
			if e.Line <= 0 {
				t.Errorf("error %q has invalid line number %d", expectedCode, e.Line)
			}
			break
		}
	}
}

// TestVetGo_E001_Goroutine verifies that a goroutine triggers E001.
func TestVetGo_E001_Goroutine(t *testing.T) {
	vetFixture(t, "e001_goroutine", "E001")
}

// TestVetGo_E003_TimeNow verifies that time.Now() triggers E003.
func TestVetGo_E003_TimeNow(t *testing.T) {
	vetFixture(t, "e003_time_now", "E003")
}

// TestVetGo_E007_MathRand verifies that math/rand usage triggers E007.
func TestVetGo_E007_MathRand(t *testing.T) {
	vetFixture(t, "e007_math_rand", "E007")
}

// TestVetGo_E013_SyncMutex verifies that sync.Mutex usage triggers E013.
func TestVetGo_E013_SyncMutex(t *testing.T) {
	vetFixture(t, "e013_sync_mutex", "E013")
}

// TestVetGo_E015_FmtPrintln verifies that fmt.Println triggers E015.
func TestVetGo_E015_FmtPrintln(t *testing.T) {
	vetFixture(t, "e015_fmt_println", "E015")
}

// TestVetGo_E005_NetHttp verifies that net/http usage triggers E005.
func TestVetGo_E005_NetHttp(t *testing.T) {
	vetFixture(t, "e005_net_http", "E005")
}

// TestVetGo_E021_MapIteration verifies that map iteration triggers E021.
func TestVetGo_E021_MapIteration(t *testing.T) {
	vetFixture(t, "e021_map_iter", "E021")
}

// TestVetGo_EntryPointMustReturnString pins the rule that an entry point's
// result must be a string.
//
// The string is deliberate: a WASM entry point hands back bytes, and string is
// the one shape every language SDK expresses identically, which is why the
// interfaces use it. GenerateExports declares `var __r string` and emits
// `return []byte(__r)`, so a non-string result produced
//
//	./gen_wasm_exports.go:340:28: cannot convert __r (variable of type
//	    *BookingResult) to type []byte
//
// -- a Go type error in GENERATED code, naming a variable the author never
// wrote. `cleat vet` said OK on the same package, so the rule existed only as
// a compile failure in a file nobody wrote. Three shipped examples were in that
// state; nothing in CI runs `cleat build` on a Go example
// (IMPROVEMENT-PLAN 3.228).
//
// Asserted on the message rather than a code because threading errors carry no
// Code in VetOutput -- see vetJSONOutput -- and this is one.
func TestVetGo_EntryPointMustReturnString(t *testing.T) {
	fixture := filepath.Join("..", "..", "testdata", "vet-checks", "go", "entrypoint_struct_result")
	out, _ := runVetCmd(t, "vet", "--lang", "go", "--json", fixture)

	var result VetOutput
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\nstdout: %s", err, out)
	}
	if len(result.Errors) == 0 {
		t.Fatalf("vet accepted an entry point returning *Result; it cannot be built.\nstdout: %s", out)
	}

	found := false
	for _, e := range result.Errors {
		if strings.Contains(e.Message, "an entry point's result must be a string") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected an error naming the string-result rule, got: %+v", result.Errors)
	}
}

// TestVetGo_SingleStringEntryPointWarns covers W003.
//
// An entry point whose only parameter (after HostCalls) is a string receives
// the WHOLE input JSON rather than the field of that name. That is deliberate
// -- something has to carry an opaque payload -- so it is a warning, not an
// error, and the binding rule is unchanged.
//
// What it costs when unwanted is why the warning exists. The rule is invisible
// at the call site, at build time and at deploy; it surfaces as a semantic
// failure in whatever the parameter was eventually used for. A lock test in
// cleat-team/cleat-ports took the lock `lock-{"key":"lock-abc"}` and failed
// every acquire with `cleat_acquire_lock: error 1` -- a message that points at
// locks, not at argument binding -- while a two-parameter workflow acquired
// the same key correctly in the same run. cleat#824.
//
// The fixture carries four entry points and only ONE must warn. A check that
// fired on all four would say nothing about the difference between them, which
// is the whole content of the warning.
func TestVetGo_SingleStringEntryPointWarns(t *testing.T) {
	fixture := filepath.Join("..", "..", "testdata", "vet-checks", "go", "entrypoint_single_string_param")
	out, err := runVetCmd(t, "vet", "--lang", "go", "--json", fixture)
	if err != nil {
		t.Fatalf("cleat vet failed on the fixture: %v\n%s", err, out)
	}

	var result VetOutput
	if jsonErr := json.Unmarshal([]byte(out), &result); jsonErr != nil {
		t.Fatalf("failed to parse JSON output: %v\nstdout: %s", jsonErr, out)
	}

	// A warning, not an error: the shape is legal and sometimes wanted.
	if len(result.Errors) != 0 {
		t.Errorf("W003 must not be an error -- the single-string shape is legal and "+
			"some workflows want it. Got errors: %+v", result.Errors)
	}

	var w003 []VetResult
	for _, w := range result.Warnings {
		if w.Code == "W003" {
			w003 = append(w003, w)
		}
	}
	if len(w003) != 1 {
		t.Fatalf("expected exactly 1 W003 warning, got %d: %+v\n"+
			"The fixture has four entry points: one single string (must warn), one with "+
			"two strings, one taking a struct, and one taking a single int -- the last "+
			"three bind by name and must not.", len(w003), w003)
	}
	if !strings.Contains(w003[0].Message, "HandleOneString") {
		t.Errorf("W003 fired on the wrong function: %q", w003[0].Message)
	}
	if !strings.Contains(w003[0].Message, "ENTIRE input JSON") {
		t.Errorf("W003 does not say what actually happens to the parameter: %q", w003[0].Message)
	}
	if !strings.Contains(w003[0].Suggestion, "add a second parameter or take a struct") {
		t.Errorf("W003 does not name the remedy: %q", w003[0].Suggestion)
	}
}

// TestVetGo_HostCallsInAParameterStruct pins that a function reaching HostCalls
// through a field of a struct it is PASSED counts as threaded.
//
// The threading check credited a HostCalls field on a RECEIVER (phase 3) but
// not on a parameter, so `cleat vet` rejected cleat/dagrun's designed shape --
// TaskContext.H is handed to every user-written task body, and the package doc
// names examples/dag as a caller that does exactly this. examples/dag failed
// with four errors telling its author to add a parameter it already
// effectively had. IMPROVEMENT-PLAN 3.229.
//
// Both spellings are in the fixture, by pointer and by value, because
// structHasHostCallsField unwraps one level of pointer and a regression could
// plausibly break either.
func TestVetGo_HostCallsInAParameterStruct(t *testing.T) {
	fixture := filepath.Join("..", "..", "testdata", "vet-checks", "go", "hostcalls_in_param_struct")
	out, err := runVetCmd(t, "vet", "--lang", "go", "--json", fixture)
	if err != nil {
		t.Fatalf("cleat vet failed on a package that reaches HostCalls through a "+
			"parameter struct: %v\n%s", err, out)
	}

	var result VetOutput
	if jsonErr := json.Unmarshal([]byte(out), &result); jsonErr != nil {
		t.Fatalf("failed to parse JSON output: %v\nstdout: %s", jsonErr, out)
	}
	if len(result.Errors) != 0 {
		t.Errorf("expected no errors, got %d: %+v", len(result.Errors), result.Errors)
	}
}

// TestVetGo_NoErrors verifies that a clean package produces no errors.
func TestVetGo_NoErrors(t *testing.T) {
	fixture := filepath.Join("..", "..", "testdata", "vet-checks", "go", "no_errors")
	out, err := runVetCmd(t, "vet", "--lang", "go", "--json", fixture)
	if err != nil {
		t.Fatalf("cleat vet failed: %v\n%s", err, out)
	}

	var result VetOutput
	if jsonErr := json.Unmarshal([]byte(out), &result); jsonErr != nil {
		t.Fatalf("failed to parse JSON output: %v\n%s", jsonErr, out)
	}

	if len(result.Errors) > 0 {
		t.Fatalf("expected no errors for clean package, got: %+v", result.Errors)
	}
}

// TestVetRust_E001_FsAccess verifies that Rust FS access triggers R001.
func TestVetRust_E001_FsAccess(t *testing.T) {
	fixture := filepath.Join("..", "..", "testdata", "vet-checks", "rust", "e001_fs_access")
	out, err := runVetCmd(t, "vet", "--lang", "rust", "--json", fixture)
	if err == nil {
		t.Fatal("expected non-zero exit code for Rust fixture with errors")
	}

	var result VetOutput
	if jsonErr := json.Unmarshal([]byte(out), &result); jsonErr != nil {
		t.Fatalf("failed to parse JSON output: %v\n%s", jsonErr, out)
	}

	if len(result.Errors) == 0 {
		t.Fatal("expected at least one error, got none")
	}
}

// TestVetJava_E001_Timestamp verifies that Java timestamp triggers J001.
func TestVetJava_E001_Timestamp(t *testing.T) {
	fixture := filepath.Join("..", "..", "testdata", "vet-checks", "java", "e001_timestamp")
	out, err := runVetCmd(t, "vet", "--lang", "java", "--json", fixture)
	if err == nil {
		t.Fatal("expected non-zero exit code for Java fixture with errors")
	}

	var result VetOutput
	if jsonErr := json.Unmarshal([]byte(out), &result); jsonErr != nil {
		t.Fatalf("failed to parse JSON output: %v\n%s", jsonErr, out)
	}

	if len(result.Errors) == 0 {
		t.Fatal("expected at least one error, got none")
	}
}

// TestVetJSONOutputSchema verifies that the JSON output schema matches the
// expected format with errors, warnings, and summary fields.
func TestVetJSONOutputSchema(t *testing.T) {
	fixture := filepath.Join("..", "..", "testdata", "vet-checks", "go", "e001_goroutine")
	out, err := runVetCmd(t, "vet", "--lang", "go", "--json", fixture)
	// Non-zero exit expected when errors are found.
	_ = err

	var result map[string]any
	if jsonErr := json.Unmarshal([]byte(out), &result); jsonErr != nil {
		t.Fatalf("failed to parse JSON: %v\n%s", jsonErr, out)
	}

	// Verify top-level keys.
	for _, key := range []string{"errors", "warnings", "summary"} {
		if _, ok := result[key]; !ok {
			t.Errorf("missing top-level key %q in JSON output", key)
		}
	}

	// Verify summary keys.
	summary, ok := result["summary"].(map[string]any)
	if !ok {
		t.Fatal("summary is not a JSON object")
	}
	for _, key := range []string{"functions", "durable_leaves", "durable_closure", "pure"} {
		if _, ok := summary[key]; !ok {
			t.Errorf("missing summary key %q", key)
		}
	}

	// Verify error structure.
	errors, ok := result["errors"].([]any)
	if !ok {
		t.Fatal("errors is not a JSON array")
	}
	if len(errors) > 0 {
		errMap, ok := errors[0].(map[string]any)
		if !ok {
			t.Fatal("first error is not a JSON object")
		}
		for _, key := range []string{"code", "file", "line", "column", "message"} {
			if _, ok := errMap[key]; !ok {
				t.Errorf("missing error key %q", key)
			}
		}
	}
}

// TestDetectVetLang verifies language auto-detection.
func TestDetectVetLang(t *testing.T) {
	// Use the repo root (has go.mod) for Go detection.
	repoRoot := filepath.Join("..", "..")
	tests := []struct {
		name     string
		setup    func(t *testing.T) string
		wantLang string
		wantErr  bool
	}{
		{
			name:     "go_mod",
			setup:    func(t *testing.T) string { return repoRoot },
			wantLang: "go",
		},
		{
			name: "rust_cargo_toml",
			setup: func(t *testing.T) string {
				return filepath.Join("..", "..", "testdata", "vet-checks", "rust", "e001_fs_access")
			},
			wantLang: "rust",
		},
		{
			name: "python_py_files",
			setup: func(t *testing.T) string {
				return filepath.Join("..", "..", "testdata", "vet-checks", "python", "py002_open")
			},
			wantLang: "python",
		},
		{
			name:    "nonexistent_directory",
			setup:   func(t *testing.T) string { return "nonexistent-directory-12345" },
			wantErr: true,
		},
		{
			name: "java_build_gradle_kts",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				os.WriteFile(filepath.Join(dir, "build.gradle.kts"), []byte("plugins {}"), 0644)
				return dir
			},
			wantLang: "java",
		},
		{
			name: "java_build_gradle",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				os.WriteFile(filepath.Join(dir, "build.gradle"), []byte("plugins {}"), 0644)
				return dir
			},
			wantLang: "java",
		},
		{
			name: "assemblyscript_package_json",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0644)
				return dir
			},
			wantLang: "as",
		},
		{
			name: "fallback_go_ext",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0644)
				return dir
			},
			wantLang: "go",
		},
		{
			name: "fallback_rust_ext",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				os.WriteFile(filepath.Join(dir, "lib.rs"), []byte("fn main() {}"), 0644)
				return dir
			},
			wantLang: "rust",
		},
		{
			name: "fallback_java_ext",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				os.WriteFile(filepath.Join(dir, "Main.java"), []byte("class Main {}"), 0644)
				return dir
			},
			wantLang: "java",
		},
		{
			name: "empty_dir_no_detection",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := tt.setup(t)
			got, err := detectVetLang(dir)
			if tt.wantErr {
				if err == nil {
					t.Errorf("detectVetLang(%q) = %q, want error", dir, got)
				}
				return
			}
			if err != nil {
				t.Errorf("detectVetLang(%q) = error: %v", dir, err)
				return
			}
			if got != tt.wantLang {
				t.Errorf("detectVetLang(%q) = %q, want %q", dir, got, tt.wantLang)
			}
		})
	}
}

// TestVetExitCode verifies exit code is 1 when errors are found, 0 otherwise.
func TestVetExitCode(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping vet test in short mode")
	}

	// Should exit 1 (errors found).
	fixture := filepath.Join("..", "..", "testdata", "vet-checks", "go", "e001_goroutine")
	cmd := exec.Command(cleatBinary, "vet", "--lang", "go", "--json", fixture)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("expected non-zero exit code for fixture with errors, got: %s", out)
	}

	// Should exit 0 (no errors).
	cleanFixture := filepath.Join("..", "..", "testdata", "vet-checks", "go", "no_errors")
	cmd = exec.Command(cleatBinary, "vet", "--lang", "go", "--json", cleanFixture)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("expected zero exit code for clean fixture, got: %v\n%s", err, out)
	}
}

// TestVetSummaryFields verifies the summary contains correct counts.
func TestVetSummaryFields(t *testing.T) {
	fixture := filepath.Join("..", "..", "testdata", "vet-checks", "go", "e001_goroutine")
	out, err := runVetCmd(t, "vet", "--lang", "go", "--json", fixture)
	// Non-zero exit expected when errors are found.
	_ = err

	var result VetOutput
	if jsonErr := json.Unmarshal([]byte(out), &result); jsonErr != nil {
		t.Fatalf("failed to parse JSON output: %v\n%s", jsonErr, out)
	}

	if result.Summary.Functions <= 0 {
		t.Errorf("expected positive Functions in summary, got %d", result.Summary.Functions)
	}
	if result.Summary.DurableLeaves < 0 {
		t.Errorf("expected non-negative DurableLeaves, got %d", result.Summary.DurableLeaves)
	}
	if result.Summary.DurableClosure < 0 {
		t.Errorf("expected non-negative DurableClosure, got %d", result.Summary.DurableClosure)
	}
	if result.Summary.Pure < 0 {
		t.Errorf("expected non-negative Pure, got %d", result.Summary.Pure)
	}
}

// TestVetPython verifies that `cleat vet --lang python` actually detects
// py002_open's violation, rather than merely not erroring.
//
// This used to be: run vet, and if it returned a non-nil error, call that
// "Python vet not available" and skip; the non-skip branch asserted nothing
// (t.Logf only). That made the test vacuous in every environment. py002_open
// is a fixture built specifically to contain a violation (file I/O in
// workflow code, PY002), so the *correct* outcome -- vet finding it -- and
// "the tooling is missing" need different, disjoint signals, and this test
// had only one (err). Reproduced live: runVetPython (main.go) treats a
// python3 exit status of 1 as "vet ran, found violations" and returns exit 0
// -- finding PY002 does not make `cleat vet` exit non-zero -- but treats
// anything that writes to stderr, e.g. `ModuleNotFoundError: No module named
// 'cleat_sdk'`, as a real failure and returns 1. So a non-nil err here has
// always meant "cleat_sdk was not importable," never "vet found the
// violation and that's fine."
//
// It has also, in this harness, always been non-nil for a second, unrelated
// reason: findPythonSDKDir (build_python.go) locates <repoRoot>/python-sdk
// relative to the cleat binary's own cwd/executable directory, neither of
// which is the repo root when the binary runs under `go test` from cmd/cleat
// (cwd) or a TestMain-built tmpdir (exec dir) -- so cleat_sdk was never on
// PYTHONPATH and every prior run of this test skipped for that reason, not
// because python3 itself was absent. Setting PYTHONPATH explicitly below
// (the same fix TestPythonRoundTripBuildAndExecute in cleat_pipeline_test.go
// already applies to its own python subprocess) removes that false skip and
// leaves only the real, worth-skipping condition: no python3 interpreter at
// all.
func TestVetPython(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	if testing.Short() {
		t.Skip("Skipping vet test in short mode")
	}

	fixture := filepath.Join("..", "..", "testdata", "vet-checks", "python", "py002_open")
	cmd := exec.Command(cleatBinary, "vet", "--lang", "python", "--json", fixture)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(repoRoot(t), "python-sdk"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat vet --lang python failed (cleat_sdk not importable, or another real "+
			"tooling failure -- not the fixture's violation, which does not set a non-zero exit): "+
			"%v\n%s", err, out)
	}

	var result VetOutput
	if jsonErr := json.Unmarshal(out, &result); jsonErr != nil {
		t.Fatalf("failed to parse JSON output: %v\n%s", jsonErr, out)
	}

	foundPY002 := false
	for _, e := range result.Errors {
		if e.Code == "PY002" {
			foundPY002 = true
			break
		}
	}
	if !foundPY002 {
		t.Errorf("expected PY002 (file I/O) violation for fixture %s, got: %s", fixture, out)
	}
}
