package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#3508. `cleat build --db` resolved the children the scan found and wrote
// cleat.lock from exactly those. The scan reads only the workflow's own
// package, so a child started from an IMPORTED package is in the lock only
// because it was added by hand, and the rewrite dropped it whenever the own
// package had at least one nameable child. Since cleat#3507 a frozen build pins
// everything it reads from the lock, so the rewrite silently un-pinned it.

func TestMergeLockEntries(t *testing.T) {
	existing := &wasm.LockFile{Version: wasm.LockFileVersion, Entries: map[string]wasm.LockEntry{
		"scanned":    {Version: 1},
		"hand-added": {Version: 5},
		"another":    {Version: 9},
	}}
	got, kept := mergeLockEntries(existing, map[string]int{"scanned": 4, "new": 2})

	want := map[string]wasm.LockEntry{
		"scanned": {Version: 4}, "new": {Version: 2}, "hand-added": {Version: 5}, "another": {Version: 9},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(kept, []string{"another", "hand-added"}) {
		t.Errorf("kept = %v, want [another hand-added]", kept)
	}

	// No lock yet: the result is the resolved set and nothing is kept.
	got, kept = mergeLockEntries(nil, map[string]int{"a": 1})
	if len(kept) != 0 || !reflect.DeepEqual(got, map[string]wasm.LockEntry{"a": {Version: 1}}) {
		t.Errorf("with no existing lock: entries=%v kept=%v", got, kept)
	}
}

func TestADBBuildKeepsAHandAddedLockEntry(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	db := testutil.TestDB(t, testutil.DialectPostgres)
	dsn := pluginTestDSN(t)

	// Names are per-test: the database is shared with the rest of the package.
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	own := "own-child-" + suffix
	handAdded := "imported-child-" + suffix
	seedRollbackWorkflowDefVersion(t, db, own, 1)
	seedRollbackWorkflowDefVersion(t, db, own, 2)

	src := fmt.Sprintf(`package main

import "github.com/cleat-team/cleat/cleat"

const childName = %q

// @cleatEntry(name="parent")
func Parent(h cleat.HostCalls, input string) (string, error) {
	return h.ChildWorkflow(childName, input)
}
`, own)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	root := repoRoot(t)
	mod := "module myworkflow\n\ngo 1.24\n\nrequire github.com/cleat-team/cleat v0.0.0\n\n" +
		"replace github.com/cleat-team/cleat => " + root + "\n" +
		"replace github.com/cleat-team/cleat/cleat => " + filepath.Join(root, "cleat") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if b, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, b)
	}

	// The lock a user has: the own-package child at an out-of-date version, and
	// a child an imported package starts, added by hand.
	before := &wasm.LockFile{Version: wasm.LockFileVersion, Policy: "frozen", Entries: map[string]wasm.LockEntry{
		own: {Version: 1}, handAdded: {Version: 5},
	}}
	if err := wasm.WriteLockFile(dir, before); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// The database comes from CLEAT_DATABASE_URL, which resolveBuildChildVersions
	// reads when --db is empty. The global --db flag is not used here because
	// `cleat --db <dsn> build ...` hands the subcommand os.Args[2:], which still
	// holds the flag and its value, so the DSN is read as the package path
	// (cleat#3519).
	cmd := exec.CommandContext(ctx, cleatBinary, "build", "--channel", "frozen", "-o", "./out", ".")
	cmd.Env = append(os.Environ(), "CLEAT_DATABASE_URL="+dsn)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat build --channel frozen with a database failed:\n%s", out)
	}

	// The precondition: --db really did resolve and rewrite the lock. Without
	// it a build that never touched the lock passes everything below.
	after, err := wasm.ReadLockFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Entries[own].Version; got != 2 {
		t.Fatalf("UNMEASURED: the own-package child is pinned at %d after the build, want 2 (re-resolved from the database), so the --db path did not run:\n%s", got, out)
	}

	if got, ok := after.Entries[handAdded]; !ok || got.Version != 5 {
		t.Errorf("the hand-added entry did not survive --db: lock entries are %v, want %s pinned at 5", after.Entries, handAdded)
	}
	if !strings.Contains(string(out), handAdded) {
		t.Errorf("the build did not say which lock entries it kept:\n%s", out)
	}

	// And the build itself pins it, not just the file.
	wasms, _ := filepath.Glob(filepath.Join(dir, "out", "*.wasm"))
	if len(wasms) != 1 {
		t.Fatalf("want one .wasm, got %v", wasms)
	}
	b, err := os.ReadFile(wasms[0])
	if err != nil {
		t.Fatal(err)
	}
	meta, err := wasm.ReadMetadata(b)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if meta.ChildVersions[handAdded] != 5 || meta.ChildVersions[own] != 2 {
		t.Errorf("ChildVersions = %v, want %s:5 and %s:2", meta.ChildVersions, handAdded, own)
	}
}
