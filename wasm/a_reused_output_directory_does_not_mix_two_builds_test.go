package wasm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cleat#1823. `-o` is both the artifact destination and the staging directory,
// and nothing removed the previous build's staged sources -- so building two
// projects into one directory compiled both, and failed with a Go redeclaration
// error naming files from neither build as the user understands it.
//
// Every Go example's README documents the same `-o /tmp/out`, so following two
// of them in a row is the reported path to it. Measured on develop:
//
//	cleat build -o /tmp/out ./examples/datapipeline/   exit 0
//	cleat build -o /tmp/out ./examples/event-driven/   exit 1
//	    ./subscription_workflow.go:190:6: toJSON redeclared in this block
//
// These exercise clearStaleStagedSources directly. The end-to-end proof is in
// cmd/cleat, which can run the real binary; this is where the three cases are
// separable.
func TestClearStaleStagedSources(t *testing.T) {
	t.Run("a previous build's sources are removed", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "pipeline.go", "package main\n")
		write(t, dir, "gen_main_stub.go", "package main\n")
		write(t, dir, stagedManifestName, "pipeline.go\n")

		if err := clearStaleStagedSources(dir, map[string]bool{"subscription_workflow.go": true}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertAbsent(t, dir, "pipeline.go")
		// Generated files are rewritten every build and are not the manifest's
		// business; removing them here would be busywork with a window in it.
		assertPresent(t, dir, "gen_main_stub.go")
	})

	t.Run("a source this build stages again is kept", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "pipeline.go", "package main\n// the previous build's copy\n")
		write(t, dir, stagedManifestName, "pipeline.go\n")

		if err := clearStaleStagedSources(dir, map[string]bool{"pipeline.go": true}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Not because removing it would be wrong -- it is overwritten straight
		// after -- but because a delete-then-write leaves a window where the
		// file is absent, and the staging loop is the only thing that should
		// decide its contents.
		assertPresent(t, dir, "pipeline.go")
	})

	t.Run("an empty directory is left alone", func(t *testing.T) {
		dir := t.TempDir()
		if err := clearStaleStagedSources(dir, map[string]bool{"a.go": true}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	// THE CONTROL THAT MATTERS MOST. -o is a path the user chose, and
	// `cleat build -o ~/src/myproject` is a typo away. A glob-and-delete would
	// take their sources with it, so a directory cleat has never written to is
	// refused rather than cleaned.
	t.Run("foreign sources are refused, not deleted", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "my_real_code.go", "package mine\n")
		write(t, dir, "notes.txt", "not a go file\n")

		err := clearStaleStagedSources(dir, map[string]bool{"pipeline.go": true})
		if err == nil {
			t.Fatal("a directory with Go sources cleat did not write was accepted")
		}
		if !strings.Contains(err.Error(), "my_real_code.go") {
			t.Errorf("the error does not name the file in the way: %v", err)
		}
		// The file is still there. This is the assertion that separates
		// "refused" from "cleaned and then failed".
		assertPresent(t, dir, "my_real_code.go")
		assertPresent(t, dir, "notes.txt")
	})

	t.Run("a non-Go file never blocks a build", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "README.md", "hello\n")
		write(t, dir, "old.wasm", "\x00asm")

		if err := clearStaleStagedSources(dir, map[string]bool{"a.go": true}); err != nil {
			t.Fatalf("a directory holding only non-Go files was refused: %v", err)
		}
	})
}

// TestTheManifestRoundTrips pins the format, which is the only thing standing
// between "remove exactly what we staged" and "remove whatever matches a glob".
func TestTheManifestRoundTrips(t *testing.T) {
	dir := t.TempDir()
	want := []string{"b.go", "a.go"}
	if err := writeStagedManifest(dir, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, ok := readStagedManifest(dir)
	if !ok {
		t.Fatal("the manifest just written does not read back")
	}
	if strings.Join(got, ",") != "a.go,b.go" {
		t.Errorf("read back %v, want sorted [a.go b.go]", got)
	}

	// A directory with no manifest must be distinguishable from one with an
	// empty manifest: the first is "cleat has never written here" and refuses
	// foreign sources, the second is "cleat staged nothing" and does not.
	if _, ok := readStagedManifest(t.TempDir()); ok {
		t.Error("a directory with no manifest reported one")
	}
}

// TestPrepareBuildDirRemovesTheStaleSourceEndToEnd drives the real
// PrepareBuildDir twice into one directory, which is the shape the issue
// reports. The unit tests above pass whether or not PrepareBuildDir calls any
// of it.
func TestPrepareBuildDirRemovesTheStaleSourceEndToEnd(t *testing.T) {
	out := t.TempDir()

	first := &BuildConfig{
		OutDir:     out,
		PkgName:    "main",
		ModulePath: "example.com/first",
		GoVersion:  "1.26",
		Outputs:    &OutputFiles{},
		XfrmSource: map[string][]byte{"pipeline.go": []byte("package main\n\nfunc toJSON() {}\n")},
	}
	if err := PrepareBuildDir(first); err != nil {
		t.Fatalf("first build: %v", err)
	}
	assertPresent(t, out, "pipeline.go")

	second := &BuildConfig{
		OutDir:     out,
		PkgName:    "main",
		ModulePath: "example.com/second",
		GoVersion:  "1.26",
		Outputs:    &OutputFiles{},
		XfrmSource: map[string][]byte{"subscription_workflow.go": []byte("package main\n\nfunc toJSON() {}\n")},
	}
	if err := PrepareBuildDir(second); err != nil {
		t.Fatalf("second build into the same directory: %v", err)
	}
	assertPresent(t, out, "subscription_workflow.go")
	// The whole defect in one assertion: before this change both files were
	// here and `go build` reported toJSON redeclared.
	assertAbsent(t, out, "pipeline.go")
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func assertPresent(t *testing.T, dir, name string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Errorf("%s is missing: %v", name, err)
	}
}

func assertAbsent(t *testing.T, dir, name string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
		t.Errorf("%s is still there; a later build into this directory compiles it "+
			"alongside its own sources (cleat#1823)", name)
	}
}
