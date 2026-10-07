package main

import (
	"encoding/json"
	"go/token"
	"go/types"
	"testing"

	"github.com/cleat-team/cleat/internal/analyzer"
	"github.com/cleat-team/cleat/internal/jsonschema"
)

// TestEntryPointSchemaAgreesWithTheBindingTableOnGo is cleat#1981's own
// "measure before merging" step, run against this repo's EXISTING ground
// truth rather than invented payloads: tests/conformance/entry_point_binding_cases.json,
// the same table engine/entry_point_binding_conformance_test.go drives
// against the real wasmtime binding (cleat#1065). For every case whose Go
// outcome is a "bound_*" tag -- the binding accepts the payload and the
// workflow runs -- the schema this package computes for the matching
// testdata/bindingconformance entry point via EntryPointParamSchema must
// ALSO accept it. A schema that rejects a payload the binding accepts is
// exactly the divergence #1981 asks to be found and fixed before validation
// ships default-on ("fix the schema to match the binding, don't loosen
// validation").
//
// SCOPE, STATED RATHER THAN LEFT IMPLICIT. This is Go-only, and it does not
// exercise an explicit JSON null in any payload -- this table predates
// cleat#2927 and has no case for it. The null-vs-binding divergence G1 found
// (a *int/[]string/map field sent null is accepted by encoding/json and was
// wrongly rejected by the schema) was measured and fixed separately, with
// its own unit tests in internal/jsonschema; running it here would only
// duplicate that, not extend it. What THIS test adds is the one check #1065's
// own table was built for and this package had never run: do the two
// readers of the binding contract -- the generated Go bind code and the
// schema this package emits for the SAME declaration -- agree on every case
// already proven to characterise the binding correctly?
//
// NOT COVERED, AND WHY: Python. #1981's acceptance criteria ask for a Python
// fixture too, but python-sdk/ has no JSON Schema emitter at all yet (no
// internal/jsonschema equivalent -- confirmed by searching python-sdk/ for
// "schema", which returns nothing). A Python entry point therefore carries
// no EntryPointSchemas today and starts untyped by this package's own design
// (cleat#1980/#1981's "definitions without one start untyped" rule), so
// there is no live Python code path for a null-vs-binding measurement to
// exercise yet. Building one is the still-open Python fixture-workflow
// acceptance criterion, tracked as follow-up rather than silently dropped.
func TestEntryPointSchemaAgreesWithTheBindingTableOnGo(t *testing.T) {
	cases := loadBindingTable(t)

	// THE THIRD OUTCOME, same discipline as engine/entry_point_binding_conformance_test.go
	// and cmd/cleat's own AS reader: EQUALITY, not a floor. A table that lost
	// its "go" rows would otherwise report zero cases checked and PASS.
	var goCases []bindingCase
	for _, c := range cases {
		if _, ok := c.Expect["go"]; ok {
			goCases = append(goCases, c)
		}
	}
	if len(goCases) != len(cases) {
		t.Fatalf("%d of %d cases carry a \"go\" expectation. Every case in this table "+
			"applies to Go, so a case without one is a case nothing checks.",
			len(goCases), len(cases))
	}

	entryFor := map[string]string{
		"string":      "BindString",
		"int":         "BindInt",
		"composite":   "BindComposite",
		"lone_string": "BindLoneString",
	}

	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/bindingconformance", fset)
	if err != nil {
		t.Fatalf("LoadPackages(bindingconformance): %v", err)
	}
	qual := types.RelativeTo(result.TargetPkg.Types)

	for _, c := range goCases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			if len(c.Declared) != 1 {
				t.Fatalf("this reader handles one declared parameter; case has %d", len(c.Declared))
			}
			d := c.Declared[0]
			funcName := entryFor[d.Kind]
			if d.OptionalDefault != "" {
				// Go's optional is a POINTER, not a declaration-site default
				// -- same mapping engine's reader uses.
				funcName = "BindOptional"
			}
			if funcName == "" {
				t.Fatalf("no fixture entry point for kind %q", d.Kind)
			}

			full := "github.com/cleat-team/cleat/testdata/bindingconformance." + funcName
			fd := result.Funcs[full]
			if fd == nil {
				t.Fatalf("no entry point %s in the loaded package", full)
			}
			schema := jsonschema.EntryPointParamSchema(fd, qual)

			schemaJSON, err := json.Marshal(schema)
			if err != nil {
				t.Fatalf("marshal schema: %v", err)
			}
			payloadJSON, err := json.Marshal(c.Payload)
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}

			v, verr := jsonschema.Validate(schemaJSON, payloadJSON)
			if verr != nil {
				t.Fatalf("schema itself did not parse: %v", verr)
			}

			bindingAccepted := c.Expect["go"] != "refused" && c.Expect["go"] != "refused_at_compile_time"
			schemaAccepted := v == nil

			if bindingAccepted && !schemaAccepted {
				t.Errorf("the Go binding %s this payload, but the schema REJECTS it: %v.\n"+
					"Per #1981's own rule: fix the schema to match the binding, don't loosen validation.\n"+
					"schema: %s\npayload: %s",
					c.Expect["go"], v, schemaJSON, payloadJSON)
			}
			if !bindingAccepted && schemaAccepted {
				// Not wrong on its own -- see this test's doc comment -- but
				// worth surfacing: a payload the binding refuses that the
				// schema lets through reaches execution's OWN refusal
				// instead of a clean pre-start 400.
				t.Logf("binding refuses (%s), schema accepts -- payload would still fail, just at execution rather than at start", c.Expect["go"])
			}
		})
	}
}
