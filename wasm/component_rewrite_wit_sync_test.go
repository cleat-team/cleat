package wasm

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// WitToEnvImport is the only thing connecting python-sdk/wit/cleat.wit to the
// host functions the engine exports, and nothing checked that the two agreed.
//
// The failure it allows is quiet and late. componentize-py happily builds a
// component from any WIT it can parse, so a new interface with no entry here
// produces a component that imports a module the rewriter does not know. The
// build passes, the SDK method looks implemented, and the workflow fails at
// instantiation on someone else's machine.
//
// That is not hypothetical in this repo. The three cron calls sat in
// host_calls.py for months with a message saying they only needed a WASM
// runtime; the actual reason they could never work was that cleat.wit had no
// interface for them, so no binding was generated and no mapping was needed or
// present. A test on this side would have said so.
//
// This is the mirror of engine.TestAssemblyScriptImportsAllExist, which catches
// an SDK naming a host function that does not exist. This catches a WIT
// interface the host cannot satisfy.
func TestEveryImportedWitFunctionHasAnEnvMapping(t *testing.T) {
	witPath := filepath.Join("..", "python-sdk", "wit", "cleat.wit")
	src, err := os.ReadFile(witPath)
	if err != nil {
		t.Fatalf("read %s: %v", witPath, err)
	}
	wit := string(src)

	pkg := regexp.MustCompile(`(?m)^package\s+([a-z0-9:\-]+)\s*;`).FindStringSubmatch(wit)
	if pkg == nil {
		t.Fatal("no package declaration found in cleat.wit; the parsing below is broken " +
			"and this test would silently check nothing")
	}
	pkgName := pkg[1]

	imported := importedInterfaces(t, wit)
	if len(imported) == 0 {
		t.Fatal("no `import <interface>;` lines found in the cleat-workflow world; parsing broken")
	}

	funcs := interfaceFunctions(wit)
	if len(funcs) == 0 {
		t.Fatal("no interfaces parsed out of cleat.wit; parsing broken")
	}
	// Anchor: an interface that has been mapped since long before this test.
	if _, ok := funcs["durable-messaging"]; !ok {
		t.Fatalf("durable-messaging not parsed; got %v", sortedKeys(funcs))
	}

	var missing []string
	for _, iface := range imported {
		module := pkgName + "/" + iface
		mapped, haveModule := WitToEnvImport[module]
		for _, fn := range funcs[iface] {
			if !haveModule {
				missing = append(missing, module+"."+fn+"  (no entry for the whole interface)")
				continue
			}
			if _, ok := mapped[fn]; !ok {
				missing = append(missing, module+"."+fn)
			}
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d WIT function(s) imported by the cleat-workflow world have no entry in "+
			"WitToEnvImport:\n  %s\n\n"+
			"A component built from this WIT will import a module the rewriter cannot "+
			"translate, so it fails to instantiate at runtime rather than at build time. "+
			"Add the mapping in wasm/component_rewrite.go, and make sure the engine really "+
			"exports the env function you map to.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// importedInterfaces returns the interface names the cleat-workflow world
// imports. Only imported ones matter: an interface defined but not imported is
// not in the component's import list.
func importedInterfaces(t *testing.T, wit string) []string {
	t.Helper()
	start := strings.Index(wit, "world cleat-workflow")
	if start < 0 {
		t.Fatal("world cleat-workflow not found in cleat.wit")
	}
	body := wit[start:]
	if end := strings.Index(body, "\n}"); end > 0 {
		body = body[:end]
	}
	var out []string
	for _, m := range regexp.MustCompile(`(?m)^\s*import\s+([a-z0-9\-]+)\s*;`).FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// interfaceFunctions maps each interface name to the function names it
// declares. WIT function declarations are `name: func(...)`, at the top level
// of an interface block.
func interfaceFunctions(wit string) map[string][]string {
	out := map[string][]string{}
	ifaceRe := regexp.MustCompile(`(?m)^interface\s+([a-z0-9\-]+)\s*\{`)
	funcRe := regexp.MustCompile(`(?m)^\s{4}([a-z0-9\-]+)\s*:\s*func\s*\(`)

	locs := ifaceRe.FindAllStringSubmatchIndex(wit, -1)
	for i, loc := range locs {
		name := wit[loc[2]:loc[3]]
		bodyStart := loc[1]
		bodyEnd := len(wit)
		if i+1 < len(locs) {
			bodyEnd = locs[i+1][0]
		}
		if closing := strings.Index(wit[bodyStart:bodyEnd], "\n}"); closing >= 0 {
			bodyEnd = bodyStart + closing
		}
		for _, m := range funcRe.FindAllStringSubmatch(wit[bodyStart:bodyEnd], -1) {
			out[name] = append(out[name], m[1])
		}
	}
	return out
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
