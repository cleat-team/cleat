package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// cleat#1889. `cleat build -o <relative-path> <pkg>` silently wrote its WASM
// binary one directory too deep and reported the (actually successful) build
// as a hard failure.
//
// CAUSE. wasmPath was computed once, `filepath.Join(outDir, wasmFile)`,
// relative to THIS process's cwd. The compile subprocess then had its OWN
// cwd changed to outDir (`buildCmd.Dir = outDir`) before running `go build -o
// <that same relative wasmPath>` -- and a relative `-o` resolves against the
// SUBPROCESS's cwd, not the parent's, so the binary landed at
// outDir/outDir/<wasmFile>. The immediately following os.Stat(wasmPath) read
// the original, single-level path and found nothing there.
//
// Fixed by making outDir absolute once, before anything derives a path from
// it -- os.MkdirTemp already returns an absolute path, so this only changes
// behaviour for a user-supplied relative -o, which is the common, documented
// form (README.md's own Quick Start uses `-o ./out`).
func TestARelativeOutputDirectoryIsNotMisplaced(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if cleatBinary == "" {
		t.Skip("cleatBinary not built (short mode)")
	}

	// A minimal workflow package, entirely self-contained -- no go.mod
	// resolution needed, so this cannot be confused with cleat#1888's
	// unrelated module-resolution failure mode.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

import "github.com/cleat-team/cleat/cleat"

// @cleatEntry(name="hello")
func Hello(h cleat.HostCalls, input string) (string, error) {
	return `+"`"+`{"greeting":"hello"}`+"`"+`, nil
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	root := repoRoot(t)
	sdkDir := filepath.Join(root, "cleat")
	modContent := "module myworkflow\n\ngo 1.24\n\nrequire github.com/cleat-team/cleat v0.0.0\n\n" +
		"replace github.com/cleat-team/cleat => " + root + "\n" +
		"replace github.com/cleat-team/cleat/cleat => " + sdkDir + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(modContent), 0o644); err != nil {
		t.Fatal(err)
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}

	t.Chdir(dir)

	// The documented form: a RELATIVE -o, from the package's own directory --
	// exactly `README.md`'s Quick Start and cleat#1888's reproduction.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, cleatBinary, "build", "-o", "./out", ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat build -o ./out . failed:\n%s", out)
	}

	// The assertion: the binary landed at ONE level, out/<name>.wasm, not
	// out/out/<name>.wasm.
	entries, err := os.ReadDir(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatalf("reading out/: %v", err)
	}
	var found bool
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".wasm" {
			found = true
		}
		if e.IsDir() && e.Name() == "out" {
			t.Errorf("found out/out/ -- exactly cleat#1889's double-nesting, still present")
		}
	}
	if !found {
		t.Errorf("no .wasm file directly under out/; got: %v", entries)
	}
}
