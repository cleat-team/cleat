package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// The two schema fragments used throughout. They are the shape
// jsonschema.EntryPointParamSchema actually emits for a multi-field Go entry
// point: an object with the parameters as properties and additionalProperties
// true, because the binding ignores keys it does not know (cleat#1690).
const (
	amountParamSchema  = `{"type":"object","properties":{"amount":{"type":"integer"}},"required":["amount"],"additionalProperties":true}`
	refundParamSchema  = `{"type":"object","properties":{"ref":{"type":"string"}},"required":["ref"],"additionalProperties":true}`
	amountResultSchema = `{"type":"object","properties":{"settled":{"type":"boolean"}},"additionalProperties":true}`
	refundResultSchema = `{"type":"object","properties":{"reversed":{"type":"boolean"}},"additionalProperties":true}`
)

func typedDef(name string, version int, entryPoints map[string]engine.EntryPointSchema) engine.WorkflowDef {
	return engine.WorkflowDef{Name: name, Version: version, EntryPointSchemas: entryPoints}
}

func paramsOf(schema string) engine.EntryPointSchema {
	return engine.EntryPointSchema{Params: json.RawMessage(schema)}
}

func serveOpenAPI(t *testing.T, api *apiServer, method string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	api.handleOpenAPIDocument(w, httptest.NewRequest(method, "/api/openapi.json", nil))
	return w
}

// document drives the handler and returns the parsed document, failing on a
// non-200 so every caller below reads a real document rather than an error
// body that happens to parse.
func document(t *testing.T, defs []engine.WorkflowDef) map[string]any {
	t.Helper()
	st := &mockStore{listWorkflowDefsFn: func(_ context.Context, name string) ([]engine.WorkflowDef, error) {
		// The endpoint is a tenant-wide document, so it must ask for every
		// definition. A name-scoped call here would be indistinguishable from
		// a correct one on a one-workflow tenant, so it is asserted.
		if name != "" {
			t.Errorf("ListWorkflowDefs called with name=%q; the document is tenant-wide and must pass \"\"", name)
		}
		return defs, nil
	}}
	resp := serveOpenAPI(t, newTestAPIServer(st), http.MethodGet)
	if resp.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", resp.Code, resp.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &doc); err != nil {
		t.Fatalf("document did not parse as JSON: %v: %s", err, resp.Body.String())
	}
	return doc
}

// dig walks a parsed document, failing with the path it could not follow
// rather than returning a zero value a later assertion would blame on the
// handler.
func dig(t *testing.T, v any, keys ...string) any {
	t.Helper()
	for i, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("at %v: value is %T, not an object", keys[:i], v)
		}
		v, ok = m[k]
		if !ok {
			t.Fatalf("at %v: key %q is absent", keys[:i], k)
		}
	}
	return v
}

// TestTheDocumentPublishesADeployedEntryPointSchema is the core acceptance.
// Before this handler the column had no reader at all -- cleat#1980's own
// text names that as the failure shape this repo keeps finding.
func TestTheDocumentPublishesADeployedEntryPointSchema(t *testing.T) {
	doc := document(t, []engine.WorkflowDef{
		typedDef("billing", 1, map[string]engine.EntryPointSchema{"run": paramsOf(amountParamSchema)}),
	})

	for _, k := range []string{"openapi", "paths"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("document has no %q key", k)
		}
	}

	// The whole parameter schema lands on "input", not a paraphrase of it: a
	// generated client must compile against the binding's own shape.
	amount := dig(t, doc, "paths", "/api/workflows/billing/start", "post",
		"requestBody", "content", "application/json", "schema",
		"properties", "input", "properties", "amount", "type")
	if amount != "integer" {
		t.Errorf("input.properties.amount.type = %v, want \"integer\"", amount)
	}

	// additionalProperties must survive: the binding accepts extra keys, so a
	// document that forbade them would reject calls the server allows.
	extra := dig(t, doc, "paths", "/api/workflows/billing/start", "post",
		"requestBody", "content", "application/json", "schema", "additionalProperties")
	if extra != true {
		t.Errorf("schema.additionalProperties = %v, want true (the binding ignores unknown keys)", extra)
	}
}

// TestAnUntypedDefinitionIsOmittedWithoutErroring is cleat#2913's third
// acceptance item: "A Rust definition deploys with NULL schemas and still
// starts untyped (verify the endpoint handles a NULL schema without erroring)."
//
// Untyped is a DESIGN state, not a fault: the column's NULL means "this
// language has no schema emitter", so the endpoint must omit the definition
// and still answer 200.
func TestAnUntypedDefinitionIsOmittedWithoutErroring(t *testing.T) {
	doc := document(t, []engine.WorkflowDef{
		typedDef("legacy-rust", 1, nil), // NULL schemas: Rust, Java, AS, pre-#1980
		typedDef("billing", 1, map[string]engine.EntryPointSchema{"run": paramsOf(amountParamSchema)}),
	})

	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatalf("paths is %T, not an object", doc["paths"])
	}
	if _, present := paths["/api/workflows/legacy-rust/start"]; present {
		t.Error("an untyped definition was published as a typed path; NULL schemas mean untyped, not empty")
	}
	if _, present := paths["/api/workflows/billing/start"]; !present {
		t.Error("the typed definition is missing -- the untyped one must not take the document down with it")
	}
}

// TestAnEmptySchemaIsTreatedAsNoSchema pins the predicate against the OTHER
// reader of this column. cmd/cleat-worker/server.go's start-path validation
// applies a schema only when len(schema.Params) > 0; a document that treated
// an empty Params as present would advertise a schema the server does not
// enforce.
func TestAnEmptySchemaIsTreatedAsNoSchema(t *testing.T) {
	doc := document(t, []engine.WorkflowDef{
		typedDef("billing", 1, map[string]engine.EntryPointSchema{
			"typed":    paramsOf(amountParamSchema),
			"unpinned": {Params: json.RawMessage(``)},
		}),
	})

	enum := dig(t, doc, "paths", "/api/workflows/billing/start", "post",
		"requestBody", "content", "application/json", "schema",
		"properties", "entry_point", "enum").([]any)
	if len(enum) != 1 || enum[0] != "typed" {
		t.Errorf("entry_point enum = %v, want [\"typed\"]: an entry point with an empty schema is untyped", enum)
	}
}

// The "several entry points" case lives in
// TestOverlappingEntryPointSchemasUseAnyOfNotOneOf below, which absorbed it:
// that test also asserts both entry points are offered, and it is the one that
// can fail, because it states why the brancher must be anyOf rather than only
// that a brancher exists.

// TestTheNewestVersionDecidesTheShape. ListWorkflowDefs returns every version
// ordered name, version DESC; a caller generating a client wants the shape
// they will be calling.
func TestTheNewestVersionDecidesTheShape(t *testing.T) {
	// Deliberately ASCENDING here, though the real ListWorkflowDefs orders
	// name, version DESC. With the store's own order, "take the first" and
	// "take the highest version" are the same answer, so a test written in
	// that order cannot fail against a handler that took whichever it saw
	// first -- it would agree with the defect. Ascending input is what makes
	// this assertion able to disagree.
	doc := document(t, []engine.WorkflowDef{
		typedDef("billing", 1, map[string]engine.EntryPointSchema{"run": paramsOf(amountParamSchema)}),
		typedDef("billing", 2, map[string]engine.EntryPointSchema{"run": paramsOf(refundParamSchema)}),
	})

	amountProp := dig(t, doc, "paths", "/api/workflows/billing/start", "post",
		"requestBody", "content", "application/json", "schema",
		"properties", "input", "properties").(map[string]any)
	if _, old := amountProp["amount"]; old {
		t.Error("the document carries version 1's parameter name; the newest version must win")
	}
	if _, new := amountProp["ref"]; !new {
		t.Errorf("the document does not carry version 2's parameter: %v", amountProp)
	}
}

// TestTheResultSchemaIsPublishedSeparately. The start endpoint returns a run
// id, not the workflow's result, so the result schema cannot be typed onto
// that response -- but cleat#1980 stores it and its motivating case (an MCP
// tool's outputSchema) needs it published somewhere.
func TestTheResultSchemaIsPublishedSeparately(t *testing.T) {
	doc := document(t, []engine.WorkflowDef{
		typedDef("billing", 1, map[string]engine.EntryPointSchema{
			"run": {Params: json.RawMessage(amountParamSchema), Result: json.RawMessage(amountResultSchema)},
		}),
	})

	settled := dig(t, doc, "components", "schemas", "billing.run.result", "properties", "settled", "type")
	if settled != "boolean" {
		t.Errorf("components.schemas[billing.run.result].properties.settled.type = %v, want \"boolean\"", settled)
	}

	// And it must NOT be attached to the start response, which returns a run
	// id: a result schema there would describe a response that does not exist.
	post := dig(t, doc, "paths", "/api/workflows/billing/start", "post").(map[string]any)
	if _, ok := post["responses"].(map[string]any)["201"].(map[string]any)["content"]; ok {
		t.Error("the 201 response carries a body schema; POST .../start returns a run id, not the workflow result")
	}
}

// TestTheDocumentIsServedFromTheCallersTenantStore is the scoping test. It
// asserts the handler routes through scopedStore by giving three DISTINCT
// stores -- the caller's, another tenant's, and the process-wide default --
// and requiring the caller to see only their own.
//
// The default store is seeded with a definition that must not appear. Without
// that seed the test would pass against a handler that read s.store directly
// whenever the default store happened to be empty, which is the same way an
// unscoped read hides: it returns nothing, and nothing looks like "no leak".
func TestTheDocumentIsServedFromTheCallersTenantStore(t *testing.T) {
	api, storeA, storeB, _ := twoTenantServer(t, false)

	storeA.listWorkflowDefsFn = func(_ context.Context, _ string) ([]engine.WorkflowDef, error) {
		return []engine.WorkflowDef{
			typedDef("mine", 1, map[string]engine.EntryPointSchema{"run": paramsOf(amountParamSchema)}),
		}, nil
	}
	storeB.listWorkflowDefsFn = func(_ context.Context, _ string) ([]engine.WorkflowDef, error) {
		return []engine.WorkflowDef{
			typedDef("theirs", 1, map[string]engine.EntryPointSchema{"run": paramsOf(refundParamSchema)}),
		}, nil
	}
	if defaults, ok := api.store.(*mockStore); ok {
		defaults.listWorkflowDefsFn = func(_ context.Context, _ string) ([]engine.WorkflowDef, error) {
			return []engine.WorkflowDef{
				typedDef("default-tenant-only", 1, map[string]engine.EntryPointSchema{"run": paramsOf(amountParamSchema)}),
			}, nil
		}
	} else {
		t.Fatalf("api.store is %T, not *mockStore: the default-store control cannot be armed", api.store)
	}

	w := httptest.NewRecorder()
	api.handleOpenAPIDocument(w, asTenant(httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil), tenantA))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("document did not parse: %v: %s", err, w.Body.String())
	}
	paths := doc["paths"].(map[string]any)

	if _, ok := paths["/api/workflows/mine/start"]; !ok {
		t.Error("the caller's own typed definition is missing from their document")
	}
	for _, other := range []string{"theirs", "default-tenant-only"} {
		if _, ok := paths["/api/workflows/"+other+"/start"]; ok {
			t.Errorf("the document names %q, which belongs to another scope", other)
		}
	}
}

// TestTwoWorkflowsWhoseNamesSanitiseAlikeDoNotCollide.
//
// Found by cleat-review on cleat#2971. sanitiseOperationID maps every
// character outside [A-Za-z0-9_] to "_", so "billing-v2" and "billing_v2" --
// and, more realistically, "my-workflow" and "my_workflow", since cleat build
// takes the workflow name from the DIRECTORY name -- produce one operationId
// between them and one components key between them.
//
// Both consequences are silent in the response and both are wrong: OpenAPI 3.1
// requires operationId to be unique, and the second workflow's result schema
// overwrites the first's, so a caller reading components gets one schema where
// the tenant has two definitions.
func TestTwoWorkflowsWhoseNamesSanitiseAlikeDoNotCollide(t *testing.T) {
	doc := document(t, []engine.WorkflowDef{
		typedDef("billing-v2", 1, map[string]engine.EntryPointSchema{
			"run": {Params: json.RawMessage(amountParamSchema), Result: json.RawMessage(amountResultSchema)},
		}),
		typedDef("billing_v2", 1, map[string]engine.EntryPointSchema{
			"run": {Params: json.RawMessage(refundParamSchema), Result: json.RawMessage(refundResultSchema)},
		}),
	})

	paths := doc["paths"].(map[string]any)
	for _, want := range []string{"/api/workflows/billing-v2/start", "/api/workflows/billing_v2/start"} {
		if _, ok := paths[want]; !ok {
			t.Errorf("path %q is missing: both definitions must be served", want)
		}
	}

	seen := map[string]string{}
	for p, v := range paths {
		opID, _ := v.(map[string]any)["post"].(map[string]any)["operationId"].(string)
		if prev, dup := seen[opID]; dup {
			t.Errorf("operationId %q is used by both %q and %q; OpenAPI requires it to be unique", opID, prev, p)
		}
		seen[opID] = p
	}

	schemas, _ := doc["components"].(map[string]any)["schemas"].(map[string]any)
	if len(schemas) != 2 {
		t.Errorf("components.schemas holds %d entries, want 2 -- one workflow's result schema was overwritten: %v",
			len(schemas), schemas)
	}
}

// TestOverlappingEntryPointSchemasUseAnyOfNotOneOf.
//
// Found by cleat-review on cleat#2971. Every emitted Params schema carries
// additionalProperties:true (the binding ignores unknown keys), so two entry
// points' schemas are NOT disjoint: an object satisfying "charge" also
// satisfies "refund" whenever it happens to carry a ref key too, and a
// superset instance matches both.
//
// oneOf requires EXACTLY one match, so it would reject a call that
// handleStartWorkflow accepts -- the same document-disagrees-with-the-server
// defect this file guards against for an empty Params, in the other
// direction. anyOf says what is true: at least one.
func TestOverlappingEntryPointSchemasUseAnyOfNotOneOf(t *testing.T) {
	doc := document(t, []engine.WorkflowDef{
		typedDef("billing", 1, map[string]engine.EntryPointSchema{
			"charge": paramsOf(amountParamSchema),
			"refund": paramsOf(refundParamSchema),
		}),
	})

	props := dig(t, doc, "paths", "/api/workflows/billing/start", "post",
		"requestBody", "content", "application/json", "schema", "properties").(map[string]any)

	// Both entry points must still be offered: a document that dropped one to
	// dodge the overlap would be a different way of disagreeing with the
	// server.
	enum, _ := props["entry_point"].(map[string]any)["enum"].([]any)
	if len(enum) != 2 {
		t.Fatalf("entry_point enum = %v, want both entry points", enum)
	}

	input, _ := props["input"].(map[string]any)
	if input == nil {
		t.Fatalf("input = %v, want an object", props["input"])
	}
	if _, bad := input["oneOf"]; bad {
		t.Error("input uses oneOf, which requires exactly one branch to match; the branches are open objects " +
			"(additionalProperties:true) so a valid call can match both and would be rejected")
	}
	branches, ok := input["anyOf"].([]any)
	if !ok || len(branches) != 2 {
		t.Fatalf("input = %v, want anyOf over both schemas", input)
	}

	// The overlap is not hypothetical, so it is shown rather than asserted in
	// prose: this instance satisfies BOTH branches.
	superset := map[string]any{"amount": 1, "ref": "r-1"}
	matched := 0
	for _, b := range branches {
		props, _ := b.(map[string]any)["properties"].(map[string]any)
		required, _ := b.(map[string]any)["required"].([]any)
		ok := true
		for _, r := range required {
			if _, present := superset[r.(string)]; !present {
				ok = false
			}
		}
		for k := range superset {
			if _, declared := props[k]; !declared {
				// additionalProperties:true, so an undeclared key still matches
			}
		}
		if ok {
			matched++
		}
	}
	if matched != 2 {
		t.Fatalf("the fixture instance matched %d branches, not 2 -- this test would not detect a oneOf "+
			"rejecting it, so it cannot disagree and proves nothing", matched)
	}
}

// TestANonGetIsRefused. The document is a read; a client that POSTs to it
// should be told so rather than served a document from a write-shaped request.
func TestANonGetIsRefused(t *testing.T) {
	st := &mockStore{}
	resp := serveOpenAPI(t, newTestAPIServer(st), http.MethodPost)
	if resp.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/openapi.json = %d, want 405", resp.Code)
	}
}
