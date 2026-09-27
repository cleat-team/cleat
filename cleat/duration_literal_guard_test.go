package cleat

// A bare integer passed where a time.Duration is expected is nanoseconds, and
// that mistake is invisible: `h.AwaitSignals(names, 1000)` compiles, because
// 1000 is an untyped constant, and means ONE MICROSECOND.
//
// cleat#1331 is what that costs. A sub-millisecond signal wait truncates to
// 0ms, and a 0ms await used to livelock the workflow -- fourteen claims a
// second, forever, with nothing detecting it. The same mistake against
// AcquireLock gives a zero-TTL lock: `AcquireLock(key, 1000)` returns true for
// a lock that does not exist, and the mutual exclusion the caller believes they
// have is simply absent, surfacing later as two workflows in a critical
// section.
//
// THE GUARDS FOR THOSE TWO DEFECTS DO NOT COVER THIS, which is why this test
// exists as well. They make a sub-millisecond value safe at the boundary; they
// cannot tell the author that the value they wrote is a million times smaller
// than the one they meant. The repo's own canonical fixture,
// testdata/updatedispatch/main.go, carried `AwaitSignals([]string{"never"},
// 1000)` for as long as it existed, with a doc comment calling itself "the
// shape a real workflow takes". It is pinned as a COMPILE fixture, so it never
// ran and never failed.
//
// Scoped with `git ls-files` rather than a filesystem walk, deliberately: a
// walk descends into .claude/worktrees/ and other scratch checkouts, which is
// the scope mistake CLAUDE.md records under "no silent caps" -- it makes a
// guard MORE likely to pass as the working tree gets messier.
//
// WHAT A GREEN RUN OF THIS DOES NOT SAY, stated here because a clean scan will
// be quoted later as though it said the stronger thing.
//
// This is a SYNTAX scan. It proves "no bare-integer Duration LITERAL at these
// call sites". It does not prove "no units mistake at these call sites", and
// the gap between those is not academic -- it is exactly the indirect form:
//
//	timeoutMs := cfg.TimeoutMs                    // 1000, meaning milliseconds
//	h.AwaitSignals(names, time.Duration(timeoutMs))
//
// Same mistake, one variable away, and invisible to an AST walk that has no
// type flow. Worse, that is the form a CONFIGURABLE timeout takes, so it is
// likelier in real workflow code than in the two fixtures this scan caught.
// Closing it needs type-flow analysis, which is a different tool; the boundary
// guards in cleat/runtime_signals.go and engine/signaller.go are what make the
// indirect form survivable rather than fatal.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// durationArgs names, per method, which ARGUMENT POSITIONS are time.Duration.
// The ...Ms siblings are absent on purpose: AcquireLockMs and DurableSleepMs
// take an int64 count of milliseconds, so an integer literal is correct there.
var durationArgs = map[string][]int{
	"AwaitSignals":   {1},    // (signalNames, timeout)
	"AcquireLock":    {1},    // (key, ttl)
	"DurableSleep":   {0},    // (d)
	"AwaitCondition": {1, 2}, // (predicate, pollInterval, timeout)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// Fatal rather than Skip. The precondition is always satisfiable here --
	// this repo IS a git checkout and CI clones it with actions/checkout -- so
	// a skip would be case (c) in scripts/check-skips.sh: a guard that reports
	// success on the one tree it exists to scan.
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v -- this scan needs the repo's tracked "+
			"file list, and a filesystem walk is not a substitute (it descends into scratch "+
			"worktrees)", err)
	}
	return strings.TrimSpace(string(out))
}

func trackedGoFiles(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "*.go")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, filepath.Join(root, line))
		}
	}
	return files
}

func TestNoDurationArgumentIsWrittenAsABareInteger(t *testing.T) {
	root := repoRoot(t)
	files := trackedGoFiles(t, root)
	// A scan that found nothing to scan reports a clean tree. That is the
	// "checks never started" reading, and it belongs on the success path.
	if len(files) < 100 {
		t.Fatalf("git ls-files returned %d Go files; the scan did not see the repo", len(files))
	}

	fset := token.NewFileSet()
	var found int
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			continue // not parseable on its own; the compiler is the authority there
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			for _, pos := range durationArgs[sel.Sel.Name] {
				if pos >= len(call.Args) {
					continue
				}
				lit, ok := call.Args[pos].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					continue
				}
				// 0 is spelled the same either way and means the same thing in
				// both readings, so it carries no units mistake.
				if lit.Value == "0" {
					continue
				}
				found++
				t.Errorf("%s: %s argument %d is the bare integer %s, which is %s -- "+
					"not the milliseconds or seconds it reads as.\n\n"+
					"Write it with a unit (%s * time.Millisecond), or call the ...Ms "+
					"variant if you meant a millisecond count (cleat#1331).",
					fset.Position(lit.Pos()), sel.Sel.Name, pos, lit.Value,
					durationOf(lit.Value), lit.Value)
			}
			return true
		})
	}
	if found == 0 {
		t.Log("no bare-integer Duration argument in any tracked Go file")
	}
}

// durationOf renders what the literal actually means, because "1000" and
// "1µs" do not look like the same quantity and that is the entire point.
func durationOf(v string) string {
	var ns int64
	for _, c := range v {
		if c < '0' || c > '9' {
			return v + "ns"
		}
		ns = ns*10 + int64(c-'0')
		if ns > 1<<62 {
			return v + "ns"
		}
	}
	return (time.Duration(ns)).String()
}
