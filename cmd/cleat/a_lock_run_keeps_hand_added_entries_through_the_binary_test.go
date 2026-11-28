package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#3521, through the real binary and a real database.
//
// TestLockCommandKeepsHandAddedEntries drives lockFileFor with a stub store and
// so cannot see runLock itself: the existing-lock read, the branch that makes
// `--update <package>` mean something, the write, and the "Changed" and "Kept"
// lines the docs promise. Those are what this runs. It was added after review
// pointed out that the stub-store tests were the only ones, so the claim that CI
// would measure past the seam was not true of them.
//
// The database comes from CLEAT_DATABASE_URL for the same reason as the
// cleat#3508 test.
func TestCleatLockKeepsAHandAddedEntryThroughTheBinary(t *testing.T) {
	needBinary(t)
	db := testutil.TestDB(t, testutil.DialectPostgres)
	dsn := pluginTestDSN(t)

	// Names are per-test: the database is shared with the rest of the package.
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	own := "lock-own-child-" + suffix
	handAdded := "lock-imported-child-" + suffix
	seedRollbackWorkflowDefVersion(t, db, own, 1)
	seedRollbackWorkflowDefVersion(t, db, own, 2)
	// The imported child is not in the package, so only a run that resolves
	// every name in the lock reaches it. 9 is not the version the lock holds.
	seedRollbackWorkflowDefVersion(t, db, handAdded, 9)

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

	// run puts the lock a user has in place (the own-package child out of date,
	// and a child an imported package starts, listed by hand at a version they
	// chose), runs `cleat lock <args>`, and returns the lock afterwards and the
	// output.
	run := func(t *testing.T, args ...string) (*wasm.LockFile, string) {
		t.Helper()
		before := &wasm.LockFile{Version: wasm.LockFileVersion, Policy: "frozen", Entries: map[string]wasm.LockEntry{
			own: {Version: 1}, handAdded: {Version: 5},
		}}
		if err := wasm.WriteLockFile(dir, before); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, cleatBinary, append([]string{"lock"}, args...)...)
		cmd.Env = append(os.Environ(), "CLEAT_DATABASE_URL="+dsn)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("cleat lock %s failed:\n%s", strings.Join(args, " "), out)
		}
		after, rerr := wasm.ReadLockFile(dir)
		if rerr != nil {
			t.Fatal(rerr)
		}
		// The precondition: the run really resolved the own-package child from
		// the database and wrote the lock. Without it a run that never touched the
		// lock would pass the assertions about the other entry.
		if got := after.Entries[own].Version; got != 2 {
			t.Fatalf("UNMEASURED: the own-package child is pinned at %d after `cleat lock %s`, want 2 (re-resolved from the database), so the run did not reach the lock:\n%s",
				got, strings.Join(args, " "), out)
		}
		return after, string(out)
	}

	for _, c := range []struct {
		name string
		args []string
	}{
		{"lock <package>", []string{"."}},
		{"lock --update <package> behaves the same", []string{"--update", "."}},
	} {
		t.Run(c.name+" keeps the hand-added entry and says so", func(t *testing.T) {
			after, out := run(t, c.args...)
			if got, ok := after.Entries[handAdded]; !ok || got.Version != 5 {
				t.Errorf("the hand-added entry did not survive: lock entries are %v, want %s pinned at 5:\n%s", after.Entries, handAdded, out)
			}
			if !strings.Contains(out, "Kept 1 entry") || !strings.Contains(out, handAdded) {
				t.Errorf("the run did not say which entry it kept:\n%s", out)
			}
			if want := fmt.Sprintf("Changed %s: v1 -> v2", own); !strings.Contains(out, want) {
				t.Errorf("the run did not report the version it moved (want %q):\n%s", want, out)
			}
		})
	}

	t.Run("a bare lock --update re-resolves every name and says which versions moved", func(t *testing.T) {
		after, out := run(t, "--update")
		if got := after.Entries[handAdded].Version; got != 9 {
			t.Errorf("a bare --update left the hand-added entry at %d, want it re-resolved to 9:\n%s", got, out)
		}
		if want := fmt.Sprintf("Changed %s: v5 -> v9", handAdded); !strings.Contains(out, want) {
			t.Errorf("the run replaced a hand-chosen version without saying so (want %q):\n%s", want, out)
		}
		if strings.Contains(out, "Kept") {
			t.Errorf("a bare --update resolves every name, so nothing is kept:\n%s", out)
		}
	})
}
