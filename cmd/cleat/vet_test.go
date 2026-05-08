package main

import (
	"encoding/json"
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
	tmpDir, err := os.MkdirTemp("", "cleat-vet-test-*")
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
	exitCode := m.Run()
	os.RemoveAll(tmpDir)
	os.Exit(exitCode)
}

// runVetCmd runs `cleat vet` with the given arguments and returns stdout and error.
// For vets that emit stderr progress messages (Rust, Java), the JSON portion
// is extracted from the combined output.
func runVetCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
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

	var result map[string]interface{}
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
	summary, ok := result["summary"].(map[string]interface{})
	if !ok {
		t.Fatal("summary is not a JSON object")
	}
	for _, key := range []string{"functions", "durable_leaves", "durable_closure", "pure"} {
		if _, ok := summary[key]; !ok {
			t.Errorf("missing summary key %q", key)
		}
	}

	// Verify error structure.
	errors, ok := result["errors"].([]interface{})
	if !ok {
		t.Fatal("errors is not a JSON array")
	}
	if len(errors) > 0 {
		errMap, ok := errors[0].(map[string]interface{})
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
		dir      string
		wantLang string
		wantErr  bool
	}{
		{repoRoot, "go", false},
		{filepath.Join("..", "..", "testdata", "vet-checks", "rust", "e001_fs_access"), "rust", false},
		{filepath.Join("..", "..", "testdata", "vet-checks", "python", "py002_open"), "python", false},
		{"nonexistent-directory-12345", "", true},
	}

	for _, tt := range tests {
		t.Run(filepath.Base(tt.dir), func(t *testing.T) {
			got, err := detectVetLang(tt.dir)
			if tt.wantErr {
				if err == nil {
					t.Errorf("detectVetLang(%q) = %q, want error", tt.dir, got)
				}
				return
			}
			if err != nil {
				t.Errorf("detectVetLang(%q) = error: %v", tt.dir, err)
				return
			}
			if got != tt.wantLang {
				t.Errorf("detectVetLang(%q) = %q, want %q", tt.dir, got, tt.wantLang)
			}
		})
	}
}

// TestVetExitCode verifies exit code is 1 when errors are found, 0 otherwise.
func TestVetExitCode(t *testing.T) {
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

// TestVetPython verifies Python fixture detection.
func TestVetPython(t *testing.T) {
	fixture := filepath.Join("..", "..", "testdata", "vet-checks", "python", "py002_open")
	// Run the Python vet. It may fail if cleat_sdk is not importable.
	out, err := runVetCmd(t, "vet", "--lang", "python", "--json", fixture)
	if err != nil {
		// Python vet may not be available in all test environments.
		t.Skipf("Python vet not available: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Logf("Python vet output: %s", out)
	}
}
