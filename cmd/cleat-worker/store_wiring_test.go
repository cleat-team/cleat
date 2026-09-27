package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestWorkerWiresEveryStore is the guard for a defect class that no test of
// either side can see: an engine capability that is fully implemented, fully
// migrated, and never handed to the engine that ships.
//
// The promise store was in that state. workflow_promises existed, its
// migrations existed, PostgresStore implemented all four PromiseStore methods,
// and every promise path in the engine is written as
//
//	if s.engine.promiseStore != nil { ... }
//
// so with nothing wired, CreatePromise recorded its event, skipped the insert
// and returned SUCCESS, and AwaitPromise then found no row and suspended
// forever. Zero rows had ever been written to workflow_promises in a real
// deployment.
//
// WithPromiseStore was called from cleat/wasmtest and from a unit test, and
// from nothing that ships. That is why the engine's own tests were green: every
// test had a promise store and the worker did not.
//
// This compares two lists neither of which is derived from the other -- the
// option surface the engine offers, and what cmd/cleat-worker actually passes
// -- so it cannot agree with itself the way a single hand-maintained list can.
func TestWorkerWiresEveryStore(t *testing.T) {
	root := repoRootForTest(t)

	engineSrc := readOrFatal(t, filepath.Join(root, "engine", "engine.go"))
	offered := map[string]bool{}
	for _, m := range regexp.MustCompile(`func (With\w*Store)\(`).FindAllStringSubmatch(engineSrc, -1) {
		offered[m[1]] = true
	}
	// Input assertion. An extractor that finds nothing reports perfect
	// agreement, which is the failure this whole file is about.
	if len(offered) < 4 {
		t.Fatalf("found only %d With*Store options in engine/engine.go; expected at least 4. "+
			"If they moved or changed shape, this test is comparing against a set it never found.",
			len(offered))
	}

	setupSrc := readOrFatal(t, filepath.Join(root, "cmd", "cleat-worker", "setup.go"))
	wired := map[string]bool{}
	for _, m := range regexp.MustCompile(`engine\.(With\w*Store)\(`).FindAllStringSubmatch(setupSrc, -1) {
		wired[m[1]] = true
	}

	var missing []string
	for opt := range offered {
		if !wired[opt] {
			missing = append(missing, opt)
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf("the engine offers %d store options and the worker wires %d; not wired: %s\n\n"+
			"Every engine path that uses one of these is guarded by a nil check and does "+
			"NOTHING when the store is absent -- usually returning success. The capability is "+
			"then silently inert in every real deployment while the engine's own tests, which "+
			"wire their own stores, stay green.\n\n"+
			"If a store is deliberately optional for the worker, say so here rather than "+
			"leaving the two lists to disagree.",
			len(offered), len(wired), strings.Join(missing, ", "))
	}
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil &&
				strings.Contains(string(data), "module github.com/cleat-team/cleat\n") {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find the root module from the test's working directory")
		}
		dir = parent
	}
}

func readOrFatal(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}
