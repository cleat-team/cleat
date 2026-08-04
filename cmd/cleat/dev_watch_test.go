package main

import (
	"os"
	"path/filepath"
	"testing"
)

// `cleat dev --watch` used to rebuild itself forever. buildDevRun writes its
// generated runner into the module directory -- it has to, so "go run" can
// resolve the workflow package import through go.mod -- and when the workflow
// package IS the module root, that directory is inside the watched tree. The
// watch loop matched every "*.go", so each rebuild created a file that
// triggered the next one. Measured on a single-package module: 76 rebuilds in
// 25 seconds with nobody touching anything, and 34 abandoned cleat_dev_*.go
// files left in the user's source directory.
//
// The two tests below are a pair and only work as a pair. The first pins the
// coupling that actually broke; the second stops it being satisfied by a
// filter that rejects everything.

// TestDevWatch_SkipsTheFileItGenerates asserts the real invariant end to end:
// the file buildDevRun actually creates is a file shouldRebuild actually
// skips. Asserting against the devGenPrefix constant instead would only prove
// the constant equals itself -- this runs the generator and filters its real
// output, so changing the os.CreateTemp pattern fails here.
func TestDevWatch_SkipsTheFileItGenerates(t *testing.T) {
	pattern := filepath.Join(testdataDir(t), "basic")

	// testdata/basic has several entry points, so one must be named.
	cmd, tmpPath, err := buildDevRun(pattern, "PlaceOrder", "", "")
	if err != nil {
		t.Fatalf("buildDevRun: %v", err)
	}
	defer os.Remove(tmpPath)
	if cmd == nil {
		t.Fatal("buildDevRun returned a nil command")
	}

	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("generated runner %s is not on disk: %v", tmpPath, err)
	}

	// Guard the premise: if the generator stopped emitting a .go file, the
	// assertion below would pass for the wrong reason.
	if filepath.Ext(tmpPath) != ".go" {
		t.Fatalf("generated runner %s is not a .go file, so the watch loop "+
			"would never have matched it and this test proves nothing", tmpPath)
	}

	if shouldRebuild(tmpPath) {
		t.Errorf("watch loop would rebuild on its own generated file %s: "+
			"each rebuild writes one of these, so the rebuild is self-triggering "+
			"whenever the module directory is inside the watched tree", tmpPath)
	}
}

// TestDevWatch_RebuildsOnUserSources is the non-vacuity half. shouldRebuild
// returning false for everything would satisfy the test above while turning
// --watch into a no-op that silently never rebuilds.
func TestDevWatch_RebuildsOnUserSources(t *testing.T) {
	trigger := []string{
		"workflow.go",
		"/abs/path/to/workflow.go",
		filepath.Join("sub", "pkg", "helpers.go"),
		// A user file may legitimately start with "cleat" -- only the full
		// generated prefix should be excluded.
		"cleatworkflow.go",
	}
	for _, path := range trigger {
		if !shouldRebuild(path) {
			t.Errorf("shouldRebuild(%q) = false, want true: --watch would ignore "+
				"a real source edit", path)
		}
	}

	ignore := []string{
		"README.md",
		"go.mod",
		"workflow.go.bak",
		devGenPrefix + "123456.go",
		filepath.Join("some", "module", devGenPrefix+"987.go"),
	}
	for _, path := range ignore {
		if shouldRebuild(path) {
			t.Errorf("shouldRebuild(%q) = true, want false", path)
		}
	}
}
