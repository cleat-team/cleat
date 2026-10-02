package main

// cleat#1980 acceptance item 2, and the LAST of the four:
//
//	"/api/openapi.json lists both a Go and (once the Python emitter lands) a
//	 Python fixture workflow's schemas."
//
// It was deferred by cleat#2913 behind that conditional, and the conditional is
// now satisfied -- the Python emitter landed (#2914/#2985) and cleat#3000
// evidences a real Python build carrying a schema through to a stored
// definition. So this is the item that was left open, not one of a pair.
//
// # Why the existing document tests do not already cover it
//
// They build their definitions with `typedDef(name, version, entryPoints)` and
// `paramsOf(schema)`, where the schema is a GO STRING LITERAL. A
// `typedDef("go-thing", ...)` and a `typedDef("py-thing", ...)` differ by NAME
// and nothing else -- neither has been near an emitter. That is precisely the
// shape that made this item read as met while the named property was untouched:
// a synthetic fixture cannot express WHICH LANGUAGE a definition came from.
//
// So the definitions below carry schemas returned by the REAL builders, which
// run `cleat build` and hand back what the emitter produced:
//
//	buildTypedFixture        (a_real_fixture_validates_start_input_test.go:42)   --target go
//	buildTypedPythonFixture  (a_real_python_fixture_validates_start_input_test.go:77) --target python
//
// # The assertion is deliberately NARROW, and that is not a weakness
//
// cmd/cleat-worker/openapi.go builds the document from `ListWorkflowDefs(ctx,
// "")` with **no exposure filter** (grep -i exposure over that file: nothing).
// WS-3 is landing the internal/external split that will make some definitions
// invisible on this surface. An assertion that this document lists EVERY stored
// definition would encode today's pre-split behaviour and break when that
// lands, whichever order the two arrive in.
//
// So it names the two fixture workflows and asserts their entry points came
// through -- never the total. Naming two things is the property the item asks
// for; counting them is a different, more brittle claim.
//
// # It must be EVIDENCED where the property is claimed, not skipped there
//
// This test needs the Python toolchain, so TWO records say different things
// about it and they read as contradictory side by side:
//
//   - the skip-ledger fragment for `test-go/commands` declares the skip that
//     fires there, because that job does not install componentize-py. That is
//     the job where NOT running is legitimate.
//   - the tier-1 rest shard DOES provision it (`scripts/tier-gate.sh` refuses
//     to run at all without componentize-py and wasm-tools), so this test RUNS
//     there -- and a skip in tier 1 is a FAILURE, which is what makes the gate
//     the instrument that proves the property was exercised.
//
// The ledger entry is therefore NOT evidence that this test never runs; it is
// evidence about a different job. Said here because a reader who meets the
// fragment first will otherwise conclude the opposite.

import (
	"testing"

	"github.com/cleat-team/cleat/engine"
)

func TestTheDocumentListsAGoAndAPythonFixtureWorkflow(t *testing.T) {
	if reason := pythonWASMUnavailable(); reason != "" {
		if toolchainRequired("python") {
			t.Fatalf("cannot build a Python workflow, but %s declares python, so this job "+
				"installs componentize-py and treats Python as tier 1: %s",
				requireToolchainEnv, reason)
		}
		t.Skip("cannot build a Python workflow: " + reason)
	}
	if testing.Short() {
		t.Skip("compiles WASM modules with the real toolchain; skipped in short mode")
	}

	// Real emitter output, from real builds. A failure here is a build/emitter
	// defect and the helpers say so; this test's own subject is the listing.
	_, goSchemas := buildTypedFixture(t)
	_, pySchemas := buildTypedPythonFixture(t)

	const (
		goWorkflow = "bindingconformance"
		pyWorkflow = "pytyped"
	)

	doc := document(t, []engine.WorkflowDef{
		typedDef(goWorkflow, 1, goSchemas),
		typedDef(pyWorkflow, 1, pySchemas),
	})

	// Each assertion names its fixture AND the entry point the emitter produced
	// for it, so a document that listed both paths while carrying neither
	// schema would still fail. `dig` fatals on a missing path, so absence is
	// caught with the key path in the message.
	for _, want := range []struct {
		workflow string
		entry    string
	}{
		{goWorkflow, "bind_int"},       // testdata/bindingconformance
		{pyWorkflow, "count_workflow"}, // testdata/pytyped/count_workflow.py
	} {
		enum, ok := dig(t, doc, "paths", "/api/workflows/"+want.workflow+"/start", "post",
			"requestBody", "content", "application/json", "schema",
			"properties", "entry_point", "enum").([]any)
		if !ok {
			t.Fatalf("%s: entry_point enum is not an array", want.workflow)
		}
		// CONTAINS, not equality: testdata/bindingconformance declares ONE ENTRY
		// POINT PER CASE SHAPE -- its own comment says so -- so its enum is
		// [bind_composite bind_int bind_lone_string bind_optional bind_string],
		// not [bind_int]. Measured, not assumed: the first version of this
		// assertion required a single element and failed here on the real
		// emitter output. Equality would also be the wrong claim: this item is
		// about the schemas REACHING the document, and a fixture may
		// legitimately carry several.
		found := false
		for _, e := range enum {
			if s, ok := e.(string); ok && s == want.entry {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the %s fixture workflow's entry_point enum = %v, which does not contain "+
				"%q.\n\nA path that exists but advertises neither the emitted entry point means "+
				"the emitted schema did not reach the document, which is the half of this item "+
				"a name-only assertion would miss.", want.workflow, enum, want.entry)
		}
	}
}
