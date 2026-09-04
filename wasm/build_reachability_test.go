package wasm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The long-running binaries must not be able to reach the build path.
//
// # Why this exists
//
// gosec's G703 (path traversal via taint analysis) reports nine HIGH-severity,
// HIGH-confidence findings, and every one of them is on the build path:
//
//	wasm/build.go:121, :555          (PrepareBuildDir, patchAdapterImports)
//	cmd/wit-rewrite/main.go:16, :29
//	cmd/cleat/main.go:466
//	cmd/cleat/build_{rust,python,java,as}.go
//
// They are all correct as reported and none is a vulnerability, for ONE reason:
// these are CLI build commands writing to a path the operator named, on the
// operator's own machine. `cleat build -o /some/dir` is supposed to write to
// /some/dir. There is no untrusted taint source, so there is nothing to
// traverse out of.
//
// # The justification is a reachability claim, so it is guarded rather than
// written down
//
// That reasoning holds only while the build path stays out of `cmd/cleat-worker`
// and `engine` -- the two components that run unattended and handle
// tenant-supplied input. A worker that compiled a workflow from a
// tenant-supplied definition would put untrusted data into exactly these
// functions, and all nine findings would become real at once.
//
// Nothing about the current code says that must not happen. Writing "not
// reachable from the worker" as a comment beside nine call sites records a fact
// that some later change silently falsifies -- and the comment would still be
// sitting there reading as a review. So this asserts it instead.
//
// Measured 2026-09-04: in non-test code, `cmd/cleat-worker` and `engine`
// reference seven `wasm.*` identifiers across 11 sites, none of them on the
// build path. Re-derive (this counts comments too, so it reads 14 and 8 --
// `wasm.RewriteWitImports` occurs only inside a comment at
// engine/wasmtime_hostfuncs_plugins.go:103, which the AST walk correctly does
// not count as a use):
//
//	find cmd/cleat-worker engine -name '*.go' ! -name '*_test.go' -print0 \
//	  | xargs -0 grep -hoE 'wasm\.[A-Z][A-Za-z0-9_]*' | sort | uniq -c
//
// The `find`, rather than `grep -r --include`, is load-bearing. The first
// version of this comment said nine identifiers across 36 sites, from
//
//	grep -rhoE 'wasm\.[A-Z]...' --include="*.go" cmd/... engine/ | grep -v _test
//
// where `-h` suppresses filenames, so `grep -v _test` had no filename to match
// and silently filtered nothing. 36 is the count WITH tests; 14 is without. The
// floor below was set from 36 and the test failed on its own anti-vacuity arm
// the first time it ran -- which is the arm working, but the number came from a
// filter that could not see what it claimed to filter.
//
// # What this deliberately does NOT claim
//
// That the build path is safe against a hostile OutDir. It is not audited for
// that and does not need to be: the caller is a developer at a terminal. If
// `wasm.PrepareBuildDir` ever does become reachable from a server, this test is
// the thing that fails, and the correct response is to audit it then -- not to
// add the new caller to the allowlist below.

// buildPathFuncs are the exported entry points into the path-taking build code.
// Derived from the package rather than typed out, so a new one is covered the
// day it is added.
var buildPathPrefixes = []string{"Build", "Prepare"}

// wasmAPIAllowedInLongRunning is what the worker and engine may use from this
// package: metadata readers and import rewriters, none of which takes a
// filesystem path from its caller.
//
// This is an allowlist and not a denylist on purpose. A denylist of build
// functions passes by default for anything added later, which is the failure
// mode the whole test is about.
var wasmAPIAllowedInLongRunning = map[string]string{
	"DetectLanguage":         "reads the guest's own cleat.metadata section; no filesystem access",
	"NeededEnvImports":       "inspects a module's import section in memory",
	"ReadMetadata":           "parses a custom section out of a []byte",
	"ReadMemoryInitialPages": "reads the memory section out of a []byte",
	"Metadata":               "the metadata struct type",
	"HasWasiImports":         "inspects a module's import section in memory",
	"WitToEnvImport":         "string mapping, no I/O",
}

// longRunningDirs are the components that run unattended and see
// tenant-supplied input. cmd/cleat, cmd/cleat-bench and cmd/wit-rewrite are
// deliberately absent: they are developer tools invoked from a terminal, and
// they are where the build path belongs.
var longRunningDirs = []string{
	filepath.Join("..", "cmd", "cleat-worker"),
	filepath.Join("..", "engine"),
}

func TestTheWorkerAndEngineCannotReachTheBuildPath(t *testing.T) {
	// The build-path set, read out of this package's own source so it cannot
	// go stale against it.
	buildPath := map[string]bool{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading wasm/: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", e.Name(), err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() {
				continue
			}
			for _, p := range buildPathPrefixes {
				if strings.HasPrefix(fn.Name.Name, p) {
					buildPath[fn.Name.Name] = true
				}
			}
		}
	}
	// Anti-vacuity: if the scan finds no build functions it would pass no
	// matter what the worker did. There were 4 on 2026-09-04
	// (PrepareBuildDir, BuildPythonWasm, BuildPythonWasmWithRuntime,
	// BuildOutputs).
	if len(buildPath) < 3 {
		t.Fatalf("found only %d exported build-path functions in wasm/ (%v); there were 4 "+
			"on 2026-09-04.\n\nWith an empty or near-empty set this test cannot fail, so "+
			"this is a failure: fix buildPathPrefixes rather than lowering the floor.",
			len(buildPath), sortedSetKeys(buildPath))
	}

	// What the long-running components actually reference.
	used := map[string][]string{} // wasm identifier -> where
	total := 0
	for _, dir := range longRunningDirs {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Fatalf("parsing %s: %v", path, perr)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "wasm" {
					return true
				}
				total++
				// Position().String() already carries the file name; prefixing
				// `path` as well produced "engine/x.go:../engine/x.go:6:27".
				used[sel.Sel.Name] = append(used[sel.Sel.Name],
					strings.TrimPrefix(filepath.ToSlash(fset.Position(sel.Pos()).String()), "../"))
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}

	// Anti-vacuity again, and the one that matters more: if the walk finds no
	// wasm references at all -- a moved directory, a renamed import -- every
	// assertion below is vacuous and the test reports success.
	if total < 8 {
		t.Fatalf("found only %d wasm.* references across %v; the AST walk saw 11 on "+
			"2026-09-04 (14 textual, three of them in comments, which this does not count).\n\n"+
			"A scan that sees almost nothing passes vacuously. Check that "+
			"longRunningDirs still point at real directories.", total, longRunningDirs)
	}

	for name, sites := range used {
		if buildPath[name] {
			t.Errorf("%s reaches wasm.%s, which is on the build path.\n\n"+
				"Nine gosec G703 path-traversal findings are justified ONLY by this "+
				"being unreachable from a component that handles tenant-supplied "+
				"input. If this call is intended, those nine need re-auditing "+
				"against a hostile path -- do not add %s to "+
				"wasmAPIAllowedInLongRunning to make this pass.\n\ncalled at: %v",
				strings.TrimPrefix(sites[0], "../"), name, name, sites)
			continue
		}
		if _, ok := wasmAPIAllowedInLongRunning[name]; !ok {
			t.Errorf("%s uses wasm.%s, which is not in wasmAPIAllowedInLongRunning.\n\n"+
				"That is not necessarily wrong -- but the allowlist is what records "+
				"that someone checked this identifier takes no filesystem path from "+
				"its caller. Add it with that reason, or use something already "+
				"listed.\n\ncalled at: %v", strings.TrimPrefix(sites[0], "../"), name, sites)
		}
	}

	// A stale allowlist entry is a grant covering something that is not there,
	// and it makes the list read as broader review than was actually done.
	for name, why := range wasmAPIAllowedInLongRunning {
		if _, ok := used[name]; !ok {
			t.Errorf("wasmAPIAllowedInLongRunning has %s (%q) and nothing uses it. "+
				"Remove the entry.", name, why)
		}
	}

	if !t.Failed() {
		t.Logf("%d wasm.* references across %d identifiers in %v; build path is %v and none "+
			"of it is reachable", total, len(used), longRunningDirs, sortedSetKeys(buildPath))
	}
}

func sortedSetKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
