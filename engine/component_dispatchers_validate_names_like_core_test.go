//go:build cgo

package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/wasm"
)

// cleat#3436: the core-module host functions refuse an invalid name with
// wasmtimeReadServiceName / wasmtimeReadOptionalServiceName before any handler
// runs. The component dispatchers once called readStrArg for the same
// parameters and passed whatever the guest sent to the handler.
//
// TestPythonGuestInvalidNamesAreRefusedAsTheCoreModulePathRefusesThem shows the
// refusal for each result SHAPE on a real guest. This file is the other half: it
// asks, for EVERY import that has both a core host function and a dispatcher,
// whether the two validate the same NUMBER of parameters, so the next dispatcher
// (or the next name added to an existing call) cannot silently go back to
// readStrArg. A count of validated reads is the cheapest comparison that fails
// when one side forgets; it does not say the same parameter is checked, and the
// e2e test covers that for the shapes.

var (
	coreNameReadRE = regexp.MustCompile(`\bwasmtimeRead(?:Optional)?ServiceName\(`)
	compNameReadRE = regexp.MustCompile(`\bread(?:Optional)?NameArg\(`)
	hostFuncNameRE = regexp.MustCompile(`b\.hostFunc\(linker, "env", "([a-z_]+)"`)
	// caseDispatchRE reads `case cbTypeX:\n\t\treturn entry.backend.dispatchX(` out
	// of goComponentCallback, the same arms the stop-correspondence guard parses.
	caseDispatchRE = regexp.MustCompile(`case (cbType\w+):\s*\n\s*return entry\.backend\.(dispatch\w+)\(`)
)

// withoutLineComments drops `//` comments. A doc comment that MENTIONS
// readNameArg( must not count as a call -- a text search cannot tell a call from
// a sentence about one. Nothing in the files read here puts `//` inside a string
// literal (checked by TestNameCheckSourceHasNoSlashSlashInStrings below).
func withoutLineComments(src string) string {
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		if j := strings.Index(l, "//"); j >= 0 {
			lines[i] = l[:j]
		}
	}
	return strings.Join(lines, "\n")
}

// coreNameReads counts validated name reads per core host function, from the
// concatenated source of the wasmtime_hostfuncs*.go files. A function's body is
// the text from its `b.hostFunc(linker, "env", "<name>"` to the next top-level
// `func`.
func coreNameReads(src string) map[string]int {
	src = withoutLineComments(src)
	out := map[string]int{}
	for _, loc := range hostFuncNameRE.FindAllStringSubmatchIndex(src, -1) {
		name := src[loc[2]:loc[3]]
		body := src[loc[1]:]
		if end := strings.Index(body, "\nfunc "); end >= 0 {
			body = body[:end]
		}
		out[name] += len(coreNameReadRE.FindAllString(body, -1))
	}
	return out
}

// componentNameReads counts validated name reads per dispatcher.
func componentNameReads(src string) map[string]int {
	src = withoutLineComments(src)
	out := map[string]int{}
	for _, m := range regexp.MustCompile(`(?s)func \(b \*wasmtimeBackend\) (dispatch\w+)\(.*?\n}\n`).
		FindAllStringSubmatch(src, -1) {
		out[m[1]] = len(compNameReadRE.FindAllString(m[0], -1))
	}
	return out
}

// compareNameChecks returns one line per import whose core and component counts
// differ. coreOnly names core host functions that no component import reaches.
func compareNameChecks(
	witToEnv map[string]map[string]string, typeMap map[string]map[string]cbType,
	cbToDispatcher map[string]string, typeNames map[cbType]string,
	core, comp map[string]int,
) (mismatches []string, compared int) {
	for module, funcs := range witToEnv {
		for wit, env := range funcs {
			coreN, hasCore := core[env]
			if !hasCore || coreN == 0 {
				continue // the core side validates no name here; nothing to match
			}
			typ, ok := typeMap[module][wit]
			if !ok {
				continue // TestEveryRegisteredWitImportHasADispatcher's finding
			}
			disp, ok := cbToDispatcher[typeNames[typ]]
			if !ok {
				mismatches = append(mismatches, module+"/"+wit+": no dispatcher found for "+typeNames[typ])
				continue
			}
			compared++
			if compN := comp[disp]; compN != coreN {
				mismatches = append(mismatches, module+"/"+wit+": core "+env+" validates "+strconv.Itoa(coreN)+
					" name(s), component "+disp+" validates "+strconv.Itoa(compN))
			}
		}
	}
	sort.Strings(mismatches)
	return mismatches, compared
}

// cbTypeNames inverts witTypeMap into a cbType -> identifier table. The
// identifier is what the `case cbTypeX:` arm spells.
func cbTypeNames(t *testing.T) map[cbType]string {
	t.Helper()
	src, err := os.ReadFile("component_callbacks.go")
	if err != nil {
		t.Fatal(err)
	}
	// witTypeMap's values are written `"wit-name": cbTypeX,`; the pairing of
	// identifier and value comes from the compiler, not from parsing: look each
	// identifier up by evaluating the table's own entries.
	names := map[cbType]string{}
	for _, m := range regexp.MustCompile(`"([a-z0-9-]+)":\s+(cbType\w+),`).FindAllStringSubmatch(string(src), -1) {
		for _, funcs := range witTypeMap {
			if typ, ok := funcs[m[1]]; ok {
				names[typ] = m[2]
			}
		}
	}
	return names
}

func readEngineSources(t *testing.T, glob string) string {
	t.Helper()
	files, err := filepath.Glob(glob)
	if err != nil || len(files) == 0 {
		t.Fatalf("no files match %s (err %v); this check is reading the wrong thing", glob, err)
	}
	var sb strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(b)
		sb.WriteString("\n")
	}
	return sb.String()
}

func dispatcherForCase(t *testing.T) map[string]string {
	t.Helper()
	src, err := os.ReadFile("component_cgo.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, m := range caseDispatchRE.FindAllStringSubmatch(withoutLineComments(string(src)), -1) {
		out[m[1]] = m[2]
	}
	return out
}

func TestEveryComponentDispatcherValidatesTheNamesTheCorePathValidates(t *testing.T) {
	core := coreNameReads(readEngineSources(t, "wasmtime_hostfuncs*.go"))
	comp := componentNameReads(readEngineSources(t, "component_callbacks.go"))
	cases := dispatcherForCase(t)
	names := cbTypeNames(t)

	// Vacuity, per thing: a regex that stopped matching reads as "all agree".
	// Each pair is one this check cannot legitimately lose.
	for env, want := range map[string]int{"cleat_call": 2, "cleat_signal_workflow": 2, "cleat_create_promise": 1, "cleat_set_scope": 2} {
		if core[env] != want {
			t.Fatalf("core %s validates %d name(s), want %d: this check is reading the wrong thing (core=%v)", env, core[env], want, core)
		}
	}
	// Component side: that the dispatchers were FOUND, not what they count. A
	// dispatcher that validates nothing is the defect this test reports, so
	// requiring a non-zero count here would let this check fail on its own
	// precondition and hide whether the comparison below works.
	if _, ok := comp["dispatchDurableCallString"]; !ok || len(comp) < 40 {
		t.Fatalf("found %d dispatchers (dispatchDurableCallString present: %v), want at least 40: this check is reading the wrong thing",
			len(comp), ok)
	}
	if len(cases) < 40 {
		t.Fatalf("parsed %d `case cbTypeX: return ...dispatchX(` arms, want at least 40: this check is reading the wrong thing", len(cases))
	}

	mismatches, compared := compareNameChecks(wasm.WitToEnvImport, witTypeMap, cases, names, core, comp)
	if compared < 25 {
		t.Fatalf("compared %d imports, want at least 25: the join between the core table and the dispatchers is broken", compared)
	}
	if len(mismatches) > 0 {
		t.Errorf("%d import(s) validate a different number of names on the component path than on the core-module path:\n  %s\n\n"+
			"Use readNameArg/readOptionalNameArg for every parameter the core host function reads with "+
			"wasmtimeReadServiceName/wasmtimeReadOptionalServiceName, and refuse with the helper for the import's result "+
			"type (cleat#3436).", len(mismatches), strings.Join(mismatches, "\n  "))
	}
}

// The known-positive: the same comparison on a dispatcher that dropped one of
// its checks must report it, and on a core function nobody mirrored must too.
func TestTheNameCheckComparisonReportsADispatcherThatDropsACheck(t *testing.T) {
	core := coreNameReads(readEngineSources(t, "wasmtime_hostfuncs*.go"))
	compSrc := readEngineSources(t, "component_callbacks.go")
	cases := dispatcherForCase(t)
	names := cbTypeNames(t)

	// Mutate durable-call's dispatcher: its second check back to readStrArg.
	mutated := strings.Replace(compSrc, `op, ok := readNameArg(args, 1, nargs)`, `op, ok := readStrArg(args, 1, nargs), true`, 1)
	if mutated == compSrc {
		t.Fatal("the mutation did not apply: dispatchDurableCallString no longer has `op, ok := readNameArg(args, 1, nargs)`")
	}
	got, _ := compareNameChecks(wasm.WitToEnvImport, witTypeMap, cases, names, core, componentNameReads(mutated))
	// Three imports share dispatchDurableCallString's shape of check; only the one
	// whose dispatcher was mutated may be reported.
	if len(got) != 1 || !strings.Contains(got[0], "durable-call: core cleat_call validates 2 name(s), component dispatchDurableCallString validates 1") {
		t.Fatalf("mutated dispatcher reported as %q, want exactly the durable-call mismatch", got)
	}

	// A comment that mentions the call must not be counted as one.
	commented := strings.Replace(compSrc, `op, ok := readNameArg(args, 1, nargs)`, "op, ok := readStrArg(args, 1, nargs), true // was readNameArg(args, 1, nargs)", 1)
	got, _ = compareNameChecks(wasm.WitToEnvImport, witTypeMap, cases, names, core, componentNameReads(commented))
	if len(got) != 1 {
		t.Fatalf("a comment naming readNameArg was counted as a check: %q", got)
	}
}

func TestNameCheckSourceHasNoSlashSlashInStrings(t *testing.T) {
	for _, glob := range []string{"wasmtime_hostfuncs*.go", "component_callbacks.go"} {
		for _, line := range strings.Split(readEngineSources(t, glob), "\n") {
			code := strings.TrimSpace(line)
			if strings.HasPrefix(code, "//") {
				continue
			}
			if i := strings.Index(code, "//"); i >= 0 && strings.Count(code[:i], `"`)%2 == 1 {
				t.Errorf("a `//` inside a string literal would make withoutLineComments drop code: %s", line)
			}
		}
	}
}
