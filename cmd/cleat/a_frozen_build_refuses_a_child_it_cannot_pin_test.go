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

	build := func(t *testing.T, src, channel string) (dir, out string, err error) {
		t.Helper()
		dir = t.TempDir()
		if werr := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); werr != nil {
			t.Fatal(werr)
		}
		root := repoRoot(t)
		mod := "module myworkflow\n\ngo 1.24\n\nrequire github.com/cleat-team/cleat v0.0.0\n\n" +
			"replace github.com/cleat-team/cleat => " + root + "\n" +
			"replace github.com/cleat-team/cleat/cleat => " + filepath.Join(root, "cleat") + "\n"
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
