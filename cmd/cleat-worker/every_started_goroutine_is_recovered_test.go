package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Every goroutine started under cmd/cleat-worker/ or plugins/ either recovers
// from a panic or is exempted here by name with a reason.
//
// # Why this exists
//
// cleat#1769. A CRITICAL panic-recovery finding was fixed precisely at its call
// site -- startPluginBackground wraps HasBackground.Run, with three passing
// tests -- and the class was never swept. Eight goroutines were still
// unrecovered when this was written, in an engine whose whole promise is that
// work survives failure. A panic in any of them killed the worker PROCESS,
// taking every in-flight workflow with it.
//
// # Why this matches `go`, and not `go func(`
//
// The obvious guard, and the one cleat#1769 was originally written to ask for,
// scans for `go func(`. It would have shipped blind. Four goroutines here are
// started as NAMED functions:
//
//	cmd/cleat-worker/main.go     go idempotencyCleanupLoop(...)   x2
//	cmd/cleat-worker/setup.go    go w.executeWorkflow(wf)
//	plugins/scheduledbackup/     go p.runBackupAsync(...)
//
// Two of those three targets were unrecovered, and one is in plugins/ -- the
// exact population the issue names. A guard reporting green while missing 3 of 8
// is the defect it exists to catch, so this walks *ast.GoStmt and resolves the
// call target, rather than matching text.
//
// w.executeWorkflow is the negative control: it IS recovered, and this guard
// must pass it. A check that flags everything discriminates nothing.
//
// # What it deliberately does not do
//
// Resolve a target into another package. Every `go` target in this tree today is
// declared in the same package as its call site, so same-package resolution is
// complete FOR THIS TREE and will stop being complete the day someone writes
// `go otherpkg.Loop()`. That case is an ERROR here, not a pass -- see
// errUnresolved below. A guard that silently skips what it cannot resolve is how
// the `go func(` version would have failed.

// recoveryHelpers are calls that establish panic recovery for their callee.
// A goroutine body that calls one of these is recovered even though the word
// `recover` does not appear in it.
var recoveryHelpers = map[string]bool{
	"withPanicRecovery":          true, // (w *Worker), cmd/cleat-worker/setup.go
	"recoverBackgroundGoroutine": true, // package-level, for pre-Worker goroutines
	"RecoverGoroutine":           true, // plugin.RecoverGoroutine, plugin/recovery.go
}

// exemptGoroutines are start sites that are deliberately unrecovered, keyed by
// "file:line". EVERY entry needs a reason. An empty map is the correct state.
//
// Keyed on the SITE, not on the target. The first version of this keyed on the
// target name, which is the string "func literal" for every func literal in the
// tree -- so a single exemption would have silenced all seventeen of them. That
// is the failure mode this guard exists to prevent, in the guard itself.
//
// file:line is brittle when code moves, deliberately. An exemption that stops
// matching is reported below rather than ignored, so a moved goroutine gets
// looked at again instead of inheriting a grant.
var exemptGoroutines = map[string]string{}

type goSite struct {
	file   string
	line   int
	target string // the function the goroutine runs, for reporting
}

// recovers reports whether a function body establishes panic recovery.
func recovers(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		switch v := n.(type) {
		case *ast.CallExpr:
			// A direct call to a helper that recovers on our behalf.
			switch fn := v.Fun.(type) {
			case *ast.Ident:
				if recoveryHelpers[fn.Name] {
					found = true
				}
			case *ast.SelectorExpr:
				if recoveryHelpers[fn.Sel.Name] {
					found = true
				}
			}
		case *ast.DeferStmt:
			// `defer func(){ ... recover() ... }()` or `defer helper(...)`.
			ast.Inspect(v, func(m ast.Node) bool {
				if id, ok := m.(*ast.Ident); ok && id.Name == "recover" {
					found = true
				}
				return !found
			})
		}
		return !found
	})
	return found
}

// collect walks one directory tree and returns every `go` statement in it,
// classified. errUnresolved names targets it could not resolve, which is a
// failure of the check rather than a finding about the tree.
func collect(t *testing.T, roots []string) (recovered, unrecovered []goSite, errUnresolved []string) {
	t.Helper()
	fset := token.NewFileSet()

	// funcs indexes every function declaration by name, per directory, so a
	// `go namedFunc()` can be resolved. Methods are indexed by their own name;
	// a collision between two methods of the same name in one package would be
	// ambiguous, and is reported rather than guessed.
	type declKey struct{ dir, name string }
	funcs := map[declKey][]*ast.FuncDecl{}
	files := map[string]*ast.File{}

	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if perr != nil {
				return perr
			}
			files[path] = f
			dir := filepath.Dir(path)
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok {
					funcs[declKey{dir, fd.Name.Name}] = append(funcs[declKey{dir, fd.Name.Name}], fd)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("UNMEASURED: walking %s: %v — this is a failure of the check, not a finding.", root, err)
		}
	}

	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, path := range paths {
		dir := filepath.Dir(path)
		ast.Inspect(files[path], func(n ast.Node) bool {
			gostmt, ok := n.(*ast.GoStmt)
			if !ok {
				return true
			}
			pos := fset.Position(gostmt.Pos())
			site := goSite{file: path, line: pos.Line}

			switch fn := gostmt.Call.Fun.(type) {
			case *ast.FuncLit: // go func() { ... }()
				site.target = "func literal"
				if recovers(fn.Body) {
					recovered = append(recovered, site)
				} else {
					unrecovered = append(unrecovered, site)
				}
			case *ast.Ident: // go namedFunc(...)
				site.target = fn.Name
				decls := funcs[declKey{dir, fn.Name}]
				switch len(decls) {
				case 0:
					errUnresolved = append(errUnresolved,
						pos.String()+": go "+fn.Name+"() — declared outside this package; this guard cannot resolve it")
				case 1:
					if recovers(decls[0].Body) {
						recovered = append(recovered, site)
					} else {
						unrecovered = append(unrecovered, site)
					}
				default:
					errUnresolved = append(errUnresolved,
						pos.String()+": go "+fn.Name+"() — "+fn.Name+" is declared more than once here; ambiguous")
				}
			case *ast.SelectorExpr: // go x.method(...)
				site.target = fn.Sel.Name
				decls := funcs[declKey{dir, fn.Sel.Name}]
				switch len(decls) {
				case 0:
					errUnresolved = append(errUnresolved,
						pos.String()+": go …."+fn.Sel.Name+"() — declared outside this package; this guard cannot resolve it")
				case 1:
					if recovers(decls[0].Body) {
						recovered = append(recovered, site)
					} else {
						unrecovered = append(unrecovered, site)
					}
				default:
					errUnresolved = append(errUnresolved,
						pos.String()+": go …."+fn.Sel.Name+"() — declared more than once here; ambiguous")
				}
			default:
				errUnresolved = append(errUnresolved,
					pos.String()+": a `go` statement whose call target this guard does not understand")
			}
			return true
		})
	}
	return recovered, unrecovered, errUnresolved
}

// goroutineFloor is the number of `go` statements this guard expects to find.
// It exists so the check DEPENDS ON ITS OWN SCANNER: if a refactor, a bad path
// or a parser change makes the walk find nothing, that must fail loudly rather
// than read as a clean tree. Measured 2026-09-17: 21 sites (17 func literals,
// 4 named). Lower it only together with the reason.
const goroutineFloor = 18

func TestEveryStartedGoroutineIsRecovered(t *testing.T) {
	selfCheck(t)

	roots := []string{".", filepath.Join("..", "..", "plugins")}
	recovered, unrecovered, unresolved := collect(t, roots)

	total := len(recovered) + len(unrecovered)
	t.Logf("go statements: %d recovered, %d unrecovered, %d unresolved (total %d)",
		len(recovered), len(unrecovered), len(unresolved), total)

	if len(unresolved) > 0 {
		for _, u := range unresolved {
			t.Errorf("UNRESOLVED: %s", u)
		}
		t.Fatal("this guard could not classify every `go` statement, so its clean result would be " +
			"meaningless. Extend the resolver rather than exempting the site.")
	}

	if total < goroutineFloor {
		t.Fatalf("UNMEASURED: found %d `go` statements, expected at least %d. "+
			"The scan measured almost nothing, which reads identically to a clean tree. "+
			"This is a failure of the check, not a finding about the tree.", total, goroutineFloor)
	}

	for _, s := range unrecovered {
		key := fmt.Sprintf("%s:%d", filepath.Base(s.file), s.line)
		if reason, ok := exemptGoroutines[key]; ok {
			t.Logf("exempt: %s (%s) — %s", key, s.target, reason)
			continue
		}
		t.Errorf("%s:%d starts a goroutine (%s) with no panic recovery. A panic there kills the "+
			"worker PROCESS and every in-flight workflow with it. Wrap it with "+
			"w.withPanicRecovery, recoverBackgroundGoroutine, or plugin.RecoverGoroutine — "+
			"or exempt it in exemptGoroutines with a reason. cleat#1769.",
			s.file, s.line, s.target)
	}

	// An exemption that matches nothing is a grant covering code that no longer
	// exists. Same arm as plugins/every_plugin_routes_its_egress_through_the_guard_test.go.
	live := map[string]bool{}
	for _, s := range unrecovered {
		live[fmt.Sprintf("%s:%d", filepath.Base(s.file), s.line)] = true
	}
	for key := range exemptGoroutines {
		if !live[key] {
			t.Errorf("exemption for %q matches no unrecovered goroutine. Either it was fixed "+
				"(delete the exemption) or the code moved (re-check the site and update the line).", key)
		}
	}
}

// selfCheck runs the classifier over source with a known answer, on every
// invocation rather than behind a flag.
//
// Without this, "the tree is clean" and "the classifier stopped matching" are
// the same green run -- and the second is exactly how the `go func(` version of
// this guard would have failed. The cases are chosen to break the two mistakes
// that were actually available: matching only func literals, and treating an
// unresolvable target as fine.
func selfCheck(t *testing.T) {
	t.Helper()
	const src = `package p

func recoveredLiteral() {
	go func() {
		defer func() { recover() }()
		work()
	}()
}

func unrecoveredLiteral() {
	go func() {
		work()
	}()
}

func viaHelper() {
	go func() {
		plugin.RecoverGoroutine("x", nil, func() { work() })
	}()
}

func namedRecovered() {
	defer func() { recover() }()
	work()
}

func namedUnrecovered() {
	work()
}

func startsNamed() {
	go namedRecovered()
	go namedUnrecovered()
}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("UNMEASURED: writing the self-check fixture: %v", err)
	}
	rec, unrec, unresolved := collect(t, []string{dir})
	if len(unresolved) != 0 {
		t.Fatalf("UNMEASURED: the self-check fixture produced %d unresolved sites: %v", len(unresolved), unresolved)
	}
	// recoveredLiteral, viaHelper, namedRecovered  = 3
	// unrecoveredLiteral, namedUnrecovered         = 2
	if len(rec) != 3 || len(unrec) != 2 {
		t.Fatalf("UNMEASURED: the classifier failed its own known-positive: "+
			"got %d recovered / %d unrecovered, want 3 / 2. "+
			"This is a failure of the check, not a finding about the tree.", len(rec), len(unrec))
	}
	names := map[string]bool{}
	for _, s := range unrec {
		names[s.target] = true
	}
	if !names["namedUnrecovered"] {
		t.Fatal("UNMEASURED: the classifier did not flag `go namedUnrecovered()`. " +
			"That is the whole reason this guard does not scan for `go func(`.")
	}
}
