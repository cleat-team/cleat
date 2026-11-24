package engine

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

// A held signal wait is woken only by the Recheck the call supplies (parkRequest.Recheck). A signal wait that bypassed the one step, by building its own park request, calling the park
// helper from a new place, or reaching the await spec some other way, would either never be held (no Recheck, refused) or be held with no read to wake it, and neither is visible to a test
// that drives the step: it would simply never happen. So the population of signal-wait sites is pinned by reading the source, as an AST and not as text (a comment about a call is not a call).
// This is the SWEEP the 2b-i review asked for, made a test: it found these sites on 2026-11-21 by three independent handles (the construction of a signal park request, the callers of the
// park helper, the callers of the guest-facing await), and a new one fails here until someone has looked at it.

// signalWaitSites are the files that may contain each kind of site. They are a measurement of the tree, not a design: add a file only after reading the new site.
var (
	signalParkRequestFiles = map[string]bool{"signaller.go": true}
	parkHelperCallerFiles  = map[string]bool{"signaller.go": true, "durablecalls.go": true}
	// The engine's guest-facing await has one function, DurableAwaitSignals, and these are the callers of it: the wazero core host function, the wasmtime core host function, the component
	// dispatch, and wait_for_event's wrapper. (cleat/runtime*.go call a DIFFERENT DurableAwaitSignals, the guest SDK's own, which is the other side of the host import.)
	awaitEntryCallerFiles = map[string]bool{"imports.go": true, "wasmtime_hostfuncs_workflow.go": true, "component_callbacks.go": true, "wait_for_event.go": true}
)

// signalParkLiterals returns, per function, whether each parkRequest literal that sets Kind: wakeSignal also sets Recheck.
func signalParkLiterals(t *testing.T, file, src string) []bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var out []bool
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "parkRequest" {
			return true
		}
		isSignal, hasRecheck := false, false
		for _, e := range lit.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			switch key.Name {
			case "Kind":
				if v, ok := kv.Value.(*ast.Ident); ok && v.Name == "wakeSignal" {
					isSignal = true
				}
			case "Recheck":
				hasRecheck = true
			}
		}
		if isSignal {
			out = append(out, hasRecheck)
		}
		return true
	})
	return out
}

// countCallsOf counts the call expressions in src that call a method or function named name. A comment that mentions the call is not one: the parser drops comments.
func countCallsOf(t *testing.T, file, src, name string) int {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if fn.Sel.Name == name {
				n++
			}
		case *ast.Ident:
			if fn.Name == name {
				n++
			}
		}
		return true
	})
	return n
}

// referencesIdent reports whether src mentions the identifier name anywhere in code (not in a comment: the parser drops comments).
func referencesIdent(t *testing.T, file, src, name string) bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// signalWaitSiteProblems reports every way the pinned population of signal-wait sites is no longer true of srcs (file name to source).
func signalWaitSiteProblems(t *testing.T, srcs map[string]string) []string {
	t.Helper()
	var problems []string
	names := make([]string, 0, len(srcs))
	for n := range srcs {
		names = append(names, n)
	}
	sort.Strings(names)
	literals, entryCallers := 0, 0
	for _, file := range names {
		src := srcs[file]
		for _, hasRecheck := range signalParkLiterals(t, file, src) {
			literals++
			switch {
			case !signalParkRequestFiles[file]:
				problems = append(problems, file+": builds a signal park request, and only awaitSignalsStep (signaller.go) may: a second signal-wait site has to be reviewed against the one that supplies the Recheck")
			case !hasRecheck:
				problems = append(problems, file+": a signal park request with no Recheck: it would be refused at admission and never held, silently")
			}
		}
		if countCallsOf(t, file, src, "parkOrSuspend") > 0 && !parkHelperCallerFiles[file] {
			problems = append(problems, file+": calls the park helper, which only signaller.go and durablecalls.go do: a new wait has to say what wakes it")
		}
		if countCallsOf(t, file, src, "DurableAwaitSignals") > 0 {
			entryCallers++
			if !awaitEntryCallerFiles[file] {
				problems = append(problems, file+": calls the engine's guest-facing await, which is not one of its four known entry points: check it reaches awaitSignalsStep and nothing else")
			}
		}
		if file != "signaller.go" && referencesIdent(t, file, src, "awaitSignalsStep") {
			problems = append(problems, file+": refers to awaitSignalsStep, which only signaller.go may: the await spec is reached through DurableAwaitSignals, and nothing may run it another way")
		}
	}
	if literals == 0 {
		problems = append(problems, "no signal park request was found anywhere: the scan is reading the wrong files, or the signal wait no longer parks")
	}
	if entryCallers == 0 {
		problems = append(problems, "no caller of the guest-facing await was found: the scan is reading the wrong files")
	}
	return problems
}

func TestEverySignalWaitIsBuiltByTheOneStepThatSuppliesItsRecheck(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	srcs := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		srcs[f] = string(raw)
	}
	if len(srcs) < 20 {
		t.Fatalf("PRECONDITION FAILED: read only %d non-test files, so a clean result would measure nothing", len(srcs))
	}
	for _, p := range signalWaitSiteProblems(t, srcs) {
		t.Error(p)
	}
	// The two requests in awaitSignalsStep (the replay frontier and the fresh wait) are what the sweep found; a third would be allowed here if it has a Recheck, but a drop to one means a
	// path stopped waiting or the scan stopped seeing it, and the second is the failure this test exists to catch.
	if n := len(signalParkLiterals(t, "signaller.go", srcs["signaller.go"])); n != 2 {
		t.Errorf("signaller.go builds %d signal park request(s), the sweep found 2 (the replay frontier and the fresh wait): re-read awaitSignalsStep", n)
	}
}

// KNOWN-POSITIVES: each way the pinned population can become false must be reported, and the good shape must not be. The fixtures follow the real layout: signaller.go builds the request and
// calls the park helper, and the entry points (imports.go here) call the await.
func TestTheSignalWaitSweepReportsEveryWayItCanBecomeFalse(t *testing.T) {
	const wait = "package engine\nfunc (s *S) f() { s.parkOrSuspend(nil, parkRequest{Kind: wakeSignal, Recheck: r}) }\n"
	const entry = "package engine\nfunc g() { h.DurableAwaitSignals() }\n"
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{"signaller.go": wait, "imports.go": entry}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for _, tc := range []struct {
		name string
		srcs map[string]string
		want string // "" means no problem expected; otherwise a substring of exactly one problem
	}{
		{"the good shape", with(nil), ""},
		{"a signal park request with no Recheck", with(map[string]string{"signaller.go": "package engine\nfunc f() { _ = parkRequest{Kind: wakeSignal} }\n"}), "no Recheck"},
		{"a signal park request in another file", with(map[string]string{"other.go": "package engine\nfunc f() { _ = parkRequest{Kind: wakeSignal, Recheck: r} }\n"}), "builds a signal park request"},
		{"a new caller of the park helper", with(map[string]string{"other.go": "package engine\nfunc f() { s.parkOrSuspend(nil, req) }\n"}), "calls the park helper"},
		{"a new entry to the await", with(map[string]string{"other.go": "package engine\nfunc f() { h.DurableAwaitSignals() }\n"}), "not one of its four known entry points"},
		{"the await spec reached another way", with(map[string]string{"other.go": "package engine\nfunc f() { s.step(&awaitSignalsStep) }\n"}), "refers to awaitSignalsStep"},
		{"a comment about a call is not a call", with(map[string]string{"other.go": "package engine\n// calls s.parkOrSuspend( and DurableAwaitSignals( and awaitSignalsStep\nfunc f() {}\n"}), ""},
		{"no signal park request anywhere", with(map[string]string{"signaller.go": "package engine\nfunc f() {}\n"}), "no signal park request was found"},
		{"no entry caller anywhere: the scan reads the wrong files", map[string]string{"signaller.go": wait}, "no caller of the guest-facing await was found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := signalWaitSiteProblems(t, tc.srcs)
			switch {
			case tc.want == "" && len(got) != 0:
				t.Fatalf("expected no problem, got %v", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
				t.Fatalf("want exactly one problem containing %q, got %v", tc.want, got)
			}
		})
	}
}
