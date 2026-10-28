package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// cleat#3501. `cleat build` compiles inside the -o directory, so without
// -trimpath the Go toolchain recorded that directory's path in the WASM
// (DWARF, panic file names, the main module's path). One source tree built to
// two different -o paths gave two different binaries, and any guard that
// compares deployed bytes (cleat#3497's proposed refusal under open runs)
// would refuse a redeploy of unchanged source.
func TestABuildDoesNotEmbedTheOutputPath(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if cleatBinary == "" {
		t.Skip("cleatBinary not built (short mode)")
	}

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
	modContent := "module myworkflow\n\ngo 1.24\n\nrequire github.com/cleat-team/cleat v0.0.0\n\n" +
		"replace github.com/cleat-team/cleat => " + root + "\n" +
		"replace github.com/cleat-team/cleat/cleat => " + filepath.Join(root, "cleat") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(modContent), 0o644); err != nil {
		t.Fatal(err)
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}

	// Two output directories whose paths differ in content AND length, so a
	// path embedded anywhere in the binary changes its bytes.
	outs := []string{
		filepath.Join(t.TempDir(), "a"),
		filepath.Join(t.TempDir(), "a-considerably-longer-output-directory-name"),
	}
	sums := make([]string, len(outs))
	for i, out := range outs {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		// Run from the package directory, as the sibling build tests do: the
		// test process itself sits inside the repo's go.work.
		cmd := exec.CommandContext(ctx, cleatBinary, "build", "-o", out, ".")
		cmd.Dir = dir
		if b, err := cmd.CombinedOutput(); err != nil {
			cancel()
			t.Fatalf("cleat build -o %s failed:\n%s", out, b)
		}
		cancel()
		wasms, _ := filepath.Glob(filepath.Join(out, "*.wasm"))
		if len(wasms) != 1 {
			t.Fatalf("want exactly one .wasm under %s, got %v", out, wasms)
		}
		b, err := os.ReadFile(wasms[0])
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(b)
		sums[i] = hex.EncodeToString(h[:])
	}
	if sums[0] != sums[1] {
		t.Errorf("one source tree built to two output paths gave different bytes:\n  %s  %s\n  %s  %s",
			sums[0], outs[0], sums[1], outs[1])
	}
}
