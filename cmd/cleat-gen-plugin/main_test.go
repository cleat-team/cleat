package main

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHelperProcess is not a real test. It is invoked as a subprocess to
// test os.Exit behavior in main(). It reconstructs os.Args to only include
// the binary name and the extra args passed via RUNHELPERARGS env var.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_HELPER_PROCESS") != "1" {
		t.Skip("not a helper process")
	}
	extra := os.Getenv("RUNHELPERARGS")
	os.Args = []string{"cleat-gen-plugin.test"}
	if extra != "" {
		os.Args = append(os.Args, strings.Fields(extra)...)
	}
	// Re-register flags for the helper process.
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	manifestPath = flag.String("manifest", "", "Path to plugin.json")
	lang = flag.String("lang", "typescript", "Target language (typescript, python, rust, go)")
	output = flag.String("out", "", "Output file (default: stdout)")
	main()
}

// runHelper executes the test binary as a subprocess with the given extra
// args and returns combined stdout+stderr.
func runHelper(t *testing.T, extraArgs ...string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=TestHelperProcess$")
	cmd.Env = append(os.Environ(),
		"GO_HELPER_PROCESS=1",
		"RUNHELPERARGS="+strings.Join(extraArgs, " "),
	)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

func TestMain_MissingManifest(t *testing.T) {
	out := runHelper(t)
	if !strings.Contains(out, "--manifest is required") {
		t.Errorf("expected '--manifest is required' in output, got: %s", out)
	}
}

func TestMain_InvalidManifestPath(t *testing.T) {
	out := runHelper(t, "--manifest", "/nonexistent/manifest.json")
	if !strings.Contains(out, "error loading manifest") {
		t.Errorf("expected 'error loading manifest' in output, got: %s", out)
	}
}

func TestMain_UnknownLanguage(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "plugin.json")
	writePluginManifest(t, manifestPath)

	out := runHelper(t, "--manifest", manifestPath, "--lang", "brainfuck")
	if !strings.Contains(out, "unknown language") {
		t.Errorf("expected 'unknown language' in output, got: %s", out)
	}
}

func TestMain_TypeScriptOutput(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "plugin.json")
	writePluginManifest(t, manifestPath)
	outFile := filepath.Join(dir, "output.ts")

	out := runHelper(t, "--manifest", manifestPath, "--lang", "typescript", "--out", outFile)
	if out != "" {
		t.Logf("stderr: %s", out)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("expected output file: %v (stderr: %s)", err, out)
	}
	content := string(data)
	if !strings.Contains(content, "Auto-generated") {
		t.Errorf("expected TypeScript output to contain header, got: %s", content)
	}
}

func TestMain_PythonOutput(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "plugin.json")
	writePluginManifest(t, manifestPath)
	outFile := filepath.Join(dir, "output.py")

	out := runHelper(t, "--manifest", manifestPath, "--lang", "python", "--out", outFile)
	if out != "" {
		t.Logf("stderr: %s", out)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("expected output file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "Auto-generated") {
		t.Errorf("expected Python output to contain header, got: %s", content)
	}
}

func TestMain_RustOutput(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "plugin.json")
	writePluginManifest(t, manifestPath)
	outFile := filepath.Join(dir, "output.rs")

	out := runHelper(t, "--manifest", manifestPath, "--lang", "rust", "--out", outFile)
	if out != "" {
		t.Logf("stderr: %s", out)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("expected output file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "Auto-generated") {
		t.Errorf("expected Rust output to contain header, got: %s", content)
	}
}

func TestMain_GoOutput(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "plugin.json")
	writePluginManifest(t, manifestPath)
	outFile := filepath.Join(dir, "output.go")

	out := runHelper(t, "--manifest", manifestPath, "--lang", "go", "--out", outFile)
	if out != "" {
		t.Logf("stderr: %s", out)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("expected output file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "Auto-generated") {
		t.Errorf("expected Go output to contain header, got: %s", content)
	}
}

func TestMain_InvalidManifest(t *testing.T) {
	dir := t.TempDir()
	badPath := filepath.Join(dir, "bad_plugin.json")
	if err := os.WriteFile(badPath, []byte(`{"name": ""}`), 0644); err != nil {
		t.Fatal(err)
	}

	out := runHelper(t, "--manifest", badPath)
	if !strings.Contains(out, "error validating manifest") && !strings.Contains(out, "error building IR") {
		t.Errorf("expected validation error in output, got: %s", out)
	}
}

// writePluginManifest creates a minimal valid plugin manifest at the given path.
func writePluginManifest(t *testing.T, path string) {
	t.Helper()
	content := `{
  "name": "test-plugin",
  "author": "test-author",
  "version": "1.0.0",
  "description": "A test plugin",
  "functions": [
    {
      "name": "hello",
      "args": [
        {"name": "name", "type": "string"}
      ]
    }
  ]
}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// ---- Original flag tests ----

func TestFlags_Manifest(t *testing.T) {
	manifest := flag.Lookup("manifest")
	if manifest == nil {
		t.Fatal("expected -manifest flag to be defined")
	}
	if manifest.DefValue != "" {
		t.Errorf("expected default '' got %q", manifest.DefValue)
	}
}

func TestFlags_Lang(t *testing.T) {
	lang := flag.Lookup("lang")
	if lang == nil {
		t.Fatal("expected -lang flag to be defined")
	}
	if lang.DefValue != "typescript" {
		t.Errorf("expected default 'typescript' got %q", lang.DefValue)
	}
}

func TestFlags_Out(t *testing.T) {
	out := flag.Lookup("out")
	if out == nil {
		t.Fatal("expected -out flag to be defined")
	}
	if out.DefValue != "" {
		t.Errorf("expected default '' got %q", out.DefValue)
	}
}

func TestAllFlagsDefined(t *testing.T) {
	flags := []string{"manifest", "lang", "out"}
	for _, name := range flags {
		if flag.Lookup(name) == nil {
			t.Errorf("flag -%s is not defined", name)
		}
	}
}
