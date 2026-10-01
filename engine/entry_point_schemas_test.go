package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// TestEntryPointSchemasRoundTrip is the test that would have caught
// plugin_deps' SQL Server mangling bug (TestPluginDepsRoundTrip's own doc
// comment) for THIS column, had it shipped without one. MSSQLStore's
// DeployWorkflowDef binds entry_point_schemas as a Go string specifically to
// avoid go-mssqldb's []byte-into-NVARCHAR(MAX) VARBINARY reinterpretation --
// this is what proves that choice actually works, on a real connection,
// rather than resting on reading the fix and reasoning that it should.
//
// Reuses pluginDepsBackends()/setupPluginDepsDB() from plugin_deps_test.go:
// same three real databases, same per-dialect setup, a different column.
func TestEntryPointSchemasRoundTrip(t *testing.T) {
	for _, backend := range pluginDepsBackends() {
		t.Run(backend.name, func(t *testing.T) {
			_, store := setupPluginDepsDB(t, backend)
			ctx := context.Background()

			const defName = "entry-point-schemas-roundtrip"
			want := map[string]EntryPointSchema{
				"greet": {
					Params: json.RawMessage(`{"type":"string"}`),
					Result: json.RawMessage(`{"type":"string"}`),
				},
				"place_order": {
					Params: json.RawMessage(`{"type":"object","properties":{"userID":{"type":"string"}},"required":["userID"],"additionalProperties":true}`),
					Result: json.RawMessage(`{"type":"string"}`),
				},
			}
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1,
				WASMBytes:         []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion:        1,
				MinVersion:        1,
				EntryPointSchemas: want,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			got, err := store.GetWorkflowDef(ctx, defName, 1)
			if err != nil {
				t.Fatalf("GetWorkflowDef: %v", err)
			}
			if got == nil {
				t.Fatal("GetWorkflowDef returned (nil, nil) for a definition just deployed")
			}
			assertSchemasEqual(t, "GetWorkflowDef", want, got.EntryPointSchemas)

			defs, err := store.ListWorkflowDefs(ctx, defName)
			if err != nil {
				t.Fatalf("ListWorkflowDefs: %v", err)
			}
			if len(defs) != 1 {
				t.Fatalf("ListWorkflowDefs: got %d defs, want 1", len(defs))
			}
			assertSchemasEqual(t, "ListWorkflowDefs", want, defs[0].EntryPointSchemas)
		})
	}
}

func assertSchemasEqual(t *testing.T, caller string, want, got map[string]EntryPointSchema) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d entry point schemas, want %d: %+v", caller, len(got), len(want), got)
	}
	for name, wantSchema := range want {
		gotSchema, ok := got[name]
		if !ok {
			t.Errorf("%s: missing entry point %q", caller, name)
			continue
		}
		// Compared as decoded JSON, not raw bytes: json.RawMessage round-trips
		// through the database as whatever bytes the driver and column type
		// hand back, and this test's job is "is it still the same SCHEMA", not
		// "is it byte-identical" -- whitespace is not part of the contract.
		if !jsonEqual(t, gotSchema.Params, wantSchema.Params) {
			t.Errorf("%s: entry point %q params = %s, want %s", caller, name, gotSchema.Params, wantSchema.Params)
		}
		if !jsonEqual(t, gotSchema.Result, wantSchema.Result) {
			t.Errorf("%s: entry point %q result = %s, want %s", caller, name, gotSchema.Result, wantSchema.Result)
		}
	}
}

func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshal %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	ab, _ := json.Marshal(av)
	bb, _ := json.Marshal(bv)
	return string(ab) == string(bb)
}

// TestEntryPointSchemasNullWhenNotSupplied pins the nullable design (unlike
// plugin_deps' NOT NULL DEFAULT '{}'): a deploy that supplies no
// EntryPointSchemas must write SQL NULL, and reading it back must come back
// as an empty, non-nil map -- the same normalize-on-read contract
// decodePluginDeps already has, now checked against a real NULL column
// value rather than only against an empty JSON object.
func TestEntryPointSchemasNullWhenNotSupplied(t *testing.T) {
	for _, backend := range pluginDepsBackends() {
		t.Run(backend.name, func(t *testing.T) {
			_, store := setupPluginDepsDB(t, backend)
			ctx := context.Background()

			const defName = "entry-point-schemas-null"
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1,
				WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			got, err := store.GetWorkflowDef(ctx, defName, 1)
			if err != nil {
				t.Fatalf("GetWorkflowDef: %v", err)
			}
			if got == nil {
				t.Fatal("GetWorkflowDef returned (nil, nil) for a definition just deployed")
			}
			if got.EntryPointSchemas == nil {
				t.Error("EntryPointSchemas is nil, want an empty non-nil map (decodeEntryPointSchemas' normalize-on-read contract)")
			}
			if len(got.EntryPointSchemas) != 0 {
				t.Errorf("EntryPointSchemas = %+v, want empty", got.EntryPointSchemas)
			}
		})
	}
}
