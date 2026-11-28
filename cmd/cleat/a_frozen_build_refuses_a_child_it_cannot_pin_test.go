package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/wasm"
)

// cleat#3500. `cleat build --channel frozen` pinned only children whose name
// was a string literal at the call site. A child named through a const, or
// computed at run time, got no pin; it resolved to MAX(version) at runtime
// while the metadata said "frozen" and the build exited 0.
//
// Two things now hold, and this file pins both with the same fixture shape:
//   - a CONST-named child is resolved by the scan and so IS pinned (positive
//     control: this is the clew case, and it must keep working);
//   - a child whose name is only known at run time makes a frozen build FAIL,
//     naming file:line, and writes no artifact.
func TestFrozenBuildRefusesAChildItCannotPin(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if cleatBinary == "" {
		t.Skip("cleatBinary not built (short mode)")
	}

	const header = `package main

import "github.com/cleat-team/cleat/cleat"

const childName = "pinned-child"

`
	constNamed := header + `// @cleatEntry(name="parent")
func Parent(h cleat.HostCalls, input string) (string, error) {
	return h.ChildWorkflow(childName, input)
}
`
	computed := header + `// @cleatEntry(name="parent")
func Parent(h cleat.HostCalls, input string) (string, error) {
	name := "child-" + input
	return h.ChildWorkflow(name, input)
}
`

	// modExtra is appended to the workflow's go.mod; "{{dir}}" is replaced by the
	// workflow directory, so a replace can point at a module nested under it.
	var modExtra string
	build := func(t *testing.T, src, channel string, extra ...map[string]string) (dir, out string, err error) {
		t.Helper()
		dir = t.TempDir()
		if werr := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); werr != nil {
			t.Fatal(werr)
		}
		for _, files := range extra {
			for rel, body := range files {
				path := filepath.Join(dir, rel)
				if werr := os.MkdirAll(filepath.Dir(path), 0o755); werr != nil {
					t.Fatal(werr)
				}
				if werr := os.WriteFile(path, []byte(body), 0o644); werr != nil {
					t.Fatal(werr)
				}
			}
		}
		root := repoRoot(t)
		mod := "module myworkflow\n\ngo 1.24\n\nrequire github.com/cleat-team/cleat v0.0.0\n\n" +
			"replace github.com/cleat-team/cleat => " + root + "\n" +
			"replace github.com/cleat-team/cleat/cleat => " + filepath.Join(root, "cleat") + "\n" +
			strings.ReplaceAll(modExtra, "{{dir}}", dir)
		if werr := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); werr != nil {
			t.Fatal(werr)
		}
		tidy := exec.Command("go", "mod", "tidy")
		tidy.Dir = dir
		if b, terr := tidy.CombinedOutput(); terr != nil {
			t.Fatalf("go mod tidy: %v\n%s", terr, b)
		}
		// The lock is where a frozen build without a database reads its pins.
		lf := &wasm.LockFile{
			Version: wasm.LockFileVersion,
			Policy:  "frozen",
			Entries: map[string]wasm.LockEntry{"pinned-child": {Version: 7}},
		}
		if werr := wasm.WriteLockFile(dir, lf); werr != nil {
			t.Fatal(werr)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, cleatBinary, "build", "--channel", channel, "-o", "./out", ".")
		cmd.Dir = dir
		b, cerr := cmd.CombinedOutput()
		return dir, string(b), cerr
	}

	t.Run("a const-named child is pinned", func(t *testing.T) {
		dir, out, err := build(t, constNamed, "frozen")
		if err != nil {
			t.Fatalf("a const-named child must build under --channel frozen:\n%s", out)
		}
		wasms, _ := filepath.Glob(filepath.Join(dir, "out", "*.wasm"))
		if len(wasms) != 1 {
			t.Fatalf("want one .wasm, got %v", wasms)
		}
		b, rerr := os.ReadFile(wasms[0])
		if rerr != nil {
			t.Fatal(rerr)
		}
		meta, merr := wasm.ReadMetadata(b)
		if merr != nil {
			t.Fatalf("ReadMetadata: %v", merr)
		}
		if got := meta.ChildVersions["pinned-child"]; got != 7 {
			t.Errorf("ChildVersions = %v, want pinned-child pinned at 7 -- the const name was not resolved, "+
				"which is cleat#3500 exactly", meta.ChildVersions)
		}
	})

	t.Run("a runtime-computed child fails a frozen build", func(t *testing.T) {
		dir, out, err := build(t, computed, "frozen")
		if err == nil {
			t.Fatalf("a frozen build with an unpinnable child must fail; it exited 0:\n%s", out)
		}
		if !strings.Contains(out, "main.go:") || !strings.Contains(out, "ChildWorkflow(name)") {
			t.Errorf("the refusal must name the call site as file:line and show the expression, got:\n%s", out)
		}
		if entries, _ := filepath.Glob(filepath.Join(dir, "out", "*.wasm")); len(entries) != 0 {
			t.Errorf("a refused build left an artifact behind: %v", entries)
		}
	})

	// The scan reads the workflow's own package, so a child started from an
	// IMPORTED package is invisible to it. The pin used to be built only when
	// the scan found a child, so such a workflow was pinned exactly when its own
	// package also happened to start a nameable child (cleat#3500 review).
	t.Run("a child started from an imported package is pinned from the lock", func(t *testing.T) {
		helperSrc := `package helper

import "github.com/cleat-team/cleat/cleat"

func Start(h cleat.HostCalls, input string) (string, error) {
	return h.ChildWorkflow("pinned-child", input)
}
`
		helperMod := "module example.com/helper\n\ngo 1.24\n\nrequire github.com/cleat-team/cleat v0.0.0\n"
		importer := `package main

import (
	"github.com/cleat-team/cleat/cleat"

	"example.com/helper"
)

// @cleatEntry(name="parent")
func Parent(h cleat.HostCalls, input string) (string, error) {
	return helper.Start(h, input)
}
`
		modExtra = "require example.com/helper v0.0.0\nreplace example.com/helper => {{dir}}/helpermod\n"
		defer func() { modExtra = "" }()
		dir, out, err := build(t, importer, "frozen", map[string]string{"helpermod/helper.go": helperSrc, "helpermod/go.mod": helperMod})
		if err != nil {
			t.Fatalf("build failed:\n%s", out)
		}
		wasms, _ := filepath.Glob(filepath.Join(dir, "out", "*.wasm"))
		if len(wasms) != 1 {
			t.Fatalf("want one .wasm, got %v", wasms)
		}
		b, _ := os.ReadFile(wasms[0])
		meta, merr := wasm.ReadMetadata(b)
		if merr != nil {
			t.Fatalf("ReadMetadata: %v", merr)
		}
		if meta.ChildVersions["pinned-child"] != 7 {
			t.Errorf("ChildVersions = %v, want pinned-child pinned at 7 from the lock. A frozen build whose "+
				"own package starts no child must still carry the lock's pins.", meta.ChildVersions)
		}
	})

	t.Run("a stable build is unchanged by a computed child", func(t *testing.T) {
		_, out, err := build(t, computed, "stable")
		if err != nil {
			t.Fatalf("only --channel frozen refuses; stable resolves at the point of use:\n%s", out)
		}
	})
}

func TestFrozenChildBindingError(t *testing.T) {
	u := &wasm.UsageInfo{UnresolvedChildren: []wasm.UnresolvedChild{
		{Pos: "/w/main.go:10:5", Method: "ChildWorkflow", Expr: "name"},
		{Pos: "/w/main.go:14:9", Method: "ChildWorkflowTyped", Expr: "pick(x)"},
	}}
	msg := frozenChildBindingError(u, "frozen")
	for _, want := range []string{"/w/main.go:10:5", "ChildWorkflow(name)", "/w/main.go:14:9", "ChildWorkflowTyped(pick(x))", "2 child"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	for _, ch := range []string{"", "latest", "stable", "tag:canary"} {
		if got := frozenChildBindingError(u, ch); got != "" {
			t.Errorf("channel %q must not refuse, got %q", ch, got)
		}
	}
	if got := frozenChildBindingError(&wasm.UsageInfo{}, "frozen"); got != "" {
		t.Errorf("nothing unresolved must not refuse, got %q", got)
	}
	if got := frozenChildBindingError(nil, "frozen"); got != "" {
		t.Errorf("nil usage must not refuse, got %q", got)
	}
}

// With nothing for the scan to resolve, a --db build must not take the database
// branch: it would write an EMPTY cleat.lock over the one an imported package's
// children are pinned by. No database is needed to see this -- the DSN below
// points nowhere, so taking the branch fails the test loudly (the branch exits
// the process on a connection error) instead of passing quietly.
func TestAFrozenBuildWithNothingToResolveReadsTheLockNotTheDatabase(t *testing.T) {
	dir := t.TempDir()
	lf := &wasm.LockFile{
		Version: wasm.LockFileVersion,
		Policy:  "frozen",
		Entries: map[string]wasm.LockEntry{"from-an-imported-package": {Version: 4}},
	}
	if err := wasm.WriteLockFile(dir, lf); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	saved := dbConnStr
	dbConnStr = "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"
	defer func() { dbConnStr = saved }()

	got := resolveBuildChildVersions(map[string]bool{}, "frozen", true)
	if got["from-an-imported-package"] != 4 {
		t.Errorf("resolved %v, want the lock's pin (4) for from-an-imported-package", got)
	}
	after, err := wasm.ReadLockFile(dir)
	if err != nil || len(after.Entries) != 1 {
		t.Errorf("the lock was rewritten: entries=%v err=%v", after, err)
	}
}
