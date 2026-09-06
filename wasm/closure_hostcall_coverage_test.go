package wasm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// unwiredClosureMethods are HostCallsImpl methods known to invoke a closure
// field that nothing wires, with the issue that will make each reachable.
//
// This is a list that may only SHRINK, and the test below fails in both
// directions: a method missing from it is an unreported defect, and an entry
// that no longer describes a violation is a standing exemption covering
// nothing -- which is how an allowlist rots into a hole for whatever function
// next happens to match it.
//
// That bidirectional property is not theoretical. Three guards in this
// repository caught real mistakes by failing on the second direction alone:
// tenantPredicateAllowlist reported an exemption for an MSSQL statement that
// had become tenant-scoped, and the skip ledger reported two lines pointing at
// a test that had been renamed. Both named the remedy in the failure message.
// Two categories, deliberately kept in one map so the bidirectional check
// covers both: a method that SHOULD be wired and is not yet, and one whose
// closure is genuinely optional. Both are exemptions, and an exemption that
// stops describing anything is what the second half of this test is for.
var unwiredClosureMethods = map[string]string{
	// ReplyToSignal and SendSignalAndWait were exempted here until 2026-09-06
	// as "blocked on a design decision, not an oversight -- delete this entry
	// when 3.220 lands". 3.220 landed: both are now composites over
	// CreatePromise/SignalWorkflow/AwaitPromise/ResolvePromise and reach no
	// closure field of their own, so the exemptions stopped describing
	// anything and this test said so by name. That is the second direction
	// working, and it is the reason to keep writing the remedy into the
	// failure message.

	// Not a defect. The closure is an override, not the mechanism: HandleUpdate
	// uses it when present and otherwise dispatches to handlers registered
	// through RegisterUpdateHandler, returning a named error when there is no
	// handler for the update. A nil field is a designed fallback here rather
	// than the silent zero value this test exists to catch, which is the
	// distinction that matters -- NowMs (#820) looked identical from the
	// outside and returned epoch 0.
	"HandleUpdate": "the closure is an override; a nil field falls back to locally " +
		"registered handlers and errors by name if none is registered. Not workflow-callable " +
		"either -- it is absent from the public HostCalls interface, though that alone would " +
		"be a weak reason, since NowMs was absent from it too and was a real defect.",
}

// TestEveryClosureBackedMethodIsWiredOrTracked is the guard for
// IMPROVEMENT-PLAN 3.234, and it covers the blind spot next to
// TestEveryCompositeHostCallHasAnImportRow rather than duplicating it.
//
// That test looks for methods that call another PUBLIC method -- `h.Upper(` --
// and checks the wrapper ends up with the inner import. A method that reaches
// the host directly through its closure field is not a composite at all and is
// invisible to it. NowMs was in exactly that state: it calls h.now(), the
// lowercase field, exactly as Now() does. Now had a hostFunctions row; NowMs
// had nothing, so a workflow whose only host call was h.NowMs() compiled with
// zero imports and the method returned 0.
//
// A zero timestamp is worse than the zero UUID the composite guard found,
// because it does not look wrong. It passes through JSON, into a timestamp
// column, and out to a user as 1970.
//
// The rule here is one question away from that one: a HostCallsImpl method
// that invokes a closure field, makes no call to another public method, and is
// named by none of the three wiring tables. Everything it flags either has a
// row or has a reason on file.
func TestEveryClosureBackedMethodIsWiredOrTracked(t *testing.T) {
	sdk := sdkDirForTest(t)

	// The two tables that cause an import to be REQUESTED: a hostFunctions row,
	// or a compositeRequires entry naming the import a wrapper needs.
	//
	// adapterDefs is deliberately not consulted. An adapter entry emits the
	// closure that calls the import; it does not ask for the import to exist.
	// A method with an adapter field and no hostFunctions row still compiles to
	// nothing -- which is the whole four-table point of IMPROVEMENT-PLAN 3.224,
	// and counting adapterDefs here would have this guard report ReplyToSignal
	// and SendSignalAndWait as wired when they are precisely not.
	wired := map[string]bool{}
	for _, hf := range hostFunctions {
		wired[hf.FieldName] = true
	}
	for method := range compositeRequires {
		wired[method] = true
	}
	if len(wired) < 30 {
		// Input assertion. An extractor that finds nothing reports perfect
		// coverage, which is the failure this file exists to prevent.
		t.Fatalf("found only %d wired method names across hostFunctions and "+
			"compositeRequires; expected at least 30. The tables moved or changed shape, "+
			"and this test is comparing against a set it never found.", len(wired))
	}

	closureFields := optionBackedFields(t, sdk)
	if len(closureFields) < 20 {
		t.Fatalf("found only %d closure fields assigned from HostCallsOptions; expected at "+
			"least 20. The assignment site moved and this test can no longer see what a "+
			"method reaches the host through.", len(closureFields))
	}

	publicCall := regexp.MustCompile(`\bh\.([A-Z]\w*)\(`)
	closureCall := regexp.MustCompile(`\bh\.([a-z]\w*)\b`)

	var unwired []string
	seen := map[string]bool{}

	forEachHostCallsMethod(t, sdk, func(name, body string) {
		if seen[name] {
			return
		}
		// Delegates to another public method: that is the composite shape, and
		// TestEveryCompositeHostCallHasAnImportRow owns it.
		for _, m := range publicCall.FindAllStringSubmatch(body, -1) {
			if m[1] != name {
				return
			}
		}
		// Must actually reach the host through a closure field. A method that
		// only touches local value fields -- SetScope and its pair, which
		// record a prefix and never call out -- has nothing to wire.
		reaches := false
		for _, m := range closureCall.FindAllStringSubmatch(body, -1) {
			if closureFields[m[1]] {
				reaches = true
				break
			}
		}
		if !reaches || wired[name] {
			return
		}
		seen[name] = true
		unwired = append(unwired, name)
	})

	sort.Strings(unwired)

	var untracked []string
	for _, name := range unwired {
		if _, ok := unwiredClosureMethods[name]; !ok {
			untracked = append(untracked, name)
		}
	}
	if len(untracked) > 0 {
		t.Errorf("%d HostCallsImpl method(s) reach the host through a closure field that no "+
			"table wires: %s\n\n"+
			"Each compiles to no import at all, so the field stays nil and the method returns "+
			"its zero value in a real workflow -- no build error, no run-time error, no log. "+
			"Wire it in hostFunctions (which also emits an adapter field), or in "+
			"compositeRequires if it needs an inner import but no field of its own, or add it "+
			"to unwiredClosureMethods with the issue that will make it reachable.",
			len(untracked), strings.Join(untracked, ", "))
	}

	// The other direction. An entry describing a method that IS wired now is a
	// grant covering nothing, and the next method to take that name inherits it.
	stillUnwired := map[string]bool{}
	for _, name := range unwired {
		stillUnwired[name] = true
	}
	var stale []string
	for name := range unwiredClosureMethods {
		if !stillUnwired[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("unwiredClosureMethods has %d entry/entries that no longer describe an "+
			"unwired method: %s\n\n"+
			"Delete them. The method is wired now, or it no longer reaches a closure field, "+
			"and leaving the entry means the exemption is available to whatever next matches "+
			"the name. This list may only shrink.",
			len(stale), strings.Join(stale, ", "))
	}
}

// optionBackedFields returns the lowercase HostCallsImpl fields assigned from
// HostCallsOptions -- the ones that hold a host closure and are nil when the
// corresponding import was never generated.
func optionBackedFields(t *testing.T, sdk string) map[string]bool {
	t.Helper()
	assign := regexp.MustCompile(`(?m)^\s*(\w+):\s+opts\.\w+,`)
	out := map[string]bool{}
	for _, path := range goFilesIn(t, sdk) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range assign.FindAllStringSubmatch(string(src), -1) {
			out[m[1]] = true
		}
	}
	return out
}

// forEachHostCallsMethod calls fn with the name and body text of every
// exported method on *HostCallsImpl.
func forEachHostCallsMethod(t *testing.T, sdk string, fn func(name, body string)) {
	t.Helper()
	fset := token.NewFileSet()
	for _, path := range goFilesIn(t, sdk) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			continue // not our business to police SDK syntax
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || fd.Body == nil {
				continue
			}
			if !receiverIsHostCallsImpl(fd) || !fd.Name.IsExported() {
				continue
			}
			body := string(src[fset.Position(fd.Body.Pos()).Offset:fset.Position(fd.Body.End()).Offset])
			fn(fd.Name.Name, body)
		}
	}
}

func goFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("SDK sources not readable at %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	if len(out) == 0 {
		t.Fatalf("no SDK sources found in %s", dir)
	}
	return out
}
