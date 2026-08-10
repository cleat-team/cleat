package engine

import (
	"context"
	"os"
	"regexp"
	"sort"
	"testing"

	"github.com/tetratelabs/wazero"
)

// asExternalRe matches the AssemblyScript SDK's host-call declarations:
//
//	@external("env", "cleat_fetch")
var asExternalRe = regexp.MustCompile(`@external\(\s*"env"\s*,\s*"([^"]+)"\s*\)`)

const asHostCallsPath = "../packages/cleat-as/assembly/host-calls.ts"

// TestAssemblyScriptImportsAllExist is the check that was missing.
//
// The AssemblyScript SDK names its host calls in string literals, and nothing
// compared them against what the engine actually registers. Three of them --
// schedule_cron, delete_cron and list_crons -- named functions that did not
// exist, and the failure mode is the worst kind: an AS guest that referenced
// one compiled fine and then failed at INSTANTIATION with "unknown import",
// before a line of workflow code ran. No test anywhere went red.
//
// A guest cannot tell which backend it landed on, so this asserts against the
// wazero registration; the wasmtime side is held to the same list by the
// per-function closure tests.
func TestAssemblyScriptImportsAllExist(t *testing.T) {
	src, err := os.ReadFile(asHostCallsPath)
	if err != nil {
		// Committed to the repo, so absence is a broken checkout, not an
		// environment without a toolchain.
		t.Fatalf("reading the AssemblyScript SDK: %v", err)
	}

	registered := registeredEnvExports(t)

	seen := make(map[string]bool)
	var missing []string
	for _, m := range asExternalRe.FindAllSubmatch(src, -1) {
		name := string(m[1])
		if seen[name] {
			continue
		}
		seen[name] = true
		if !registered[name] {
			missing = append(missing, name)
		}
	}
	if len(seen) == 0 {
		t.Fatalf("no @external declarations found in %s -- the regexp has stopped matching "+
			"the SDK's syntax, so this test is no longer checking anything", asHostCallsPath)
	}

	sort.Strings(missing)
	for _, name := range missing {
		t.Errorf("the AssemblyScript SDK imports env.%s, which the engine does not register; "+
			"a guest that calls it fails at instantiation", name)
	}
}

// registeredEnvExports returns the names the engine exports on the "env"
// module, taken from the registration itself rather than from a list kept
// alongside it -- a second list is a second thing to forget to update.
func registeredEnvExports(t *testing.T) map[string]bool {
	t.Helper()
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	t.Cleanup(func() { rt.Close(ctx) })

	builder := rt.NewHostModuleBuilder("env")
	registerHostFunctions(builder, nil)
	mod, err := builder.Instantiate(ctx)
	if err != nil {
		t.Fatalf("instantiate host module: %v", err)
	}
	t.Cleanup(func() { mod.Close(ctx) })

	names := make(map[string]bool)
	for name := range mod.ExportedFunctionDefinitions() {
		names[name] = true
	}
	return names
}
