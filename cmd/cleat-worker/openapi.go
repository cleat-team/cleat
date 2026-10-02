package main

import (
	"net/http"
	"sort"
	"strings"

	"github.com/cleat-team/cleat/engine"
)

// handleOpenAPIDocument serves GET /api/openapi.json (cleat#2913).
//
// It is the READER for workflow_defs.entry_point_schemas. cleat#2892 shipped
// the column and the Go emitter, cleat#2933 the Python one, and until this
// handler existed the column had no reader at all -- which is the shape
// cleat#1980's own text names as the one this repo keeps finding ("The column
// must ship with its readers (the endpoint, and #1981), not ahead of them. A
// populated column nothing reads is the failure shape this repo keeps
// finding.")
//
// Tenancy is inherited, not re-implemented. scopedStore is the same call
// every other tenant-facing handler makes, and workflow_defs is under FORCE
// ROW LEVEL SECURITY with a tenant_isolation_defs policy keyed on
// cleat.assert_tenant_set(), so ListWorkflowDefs(ctx, "") can only return
// rows the caller's tenant owns. There is no name parameter a caller could
// use to address another tenant's definitions.
//
// A definition whose entry_point_schemas are empty is UNTYPED BY DESIGN and
// is omitted: that is Rust, Java, AssemblyScript, and every build from before
// cleat#1980. The column's NULL means "this language has no schema emitter",
// not "a schema is missing", so the endpoint must not treat it as a fault.
// (A blob that fails to parse never reaches here at all: decodeEntryPointSchemas
// logs it and returns "no schemas", deliberately, so there is no
// corrupt-schema branch below to keep in step with that decision.)
func (s *apiServer) handleOpenAPIDocument(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}

	defs, err := st.ListWorkflowDefs(r.Context(), "")
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, buildOpenAPIDocument(defs))
}

// buildOpenAPIDocument assembles the document from the caller's definitions.
//
// Three choices worth stating, because each could reasonably have gone the
// other way:
//
//  1. ONE PATH PER WORKFLOW, with the workflow's own name in it, rather than a
//     single templated "/api/workflows/{name}/start" carrying a oneOf. Both
//     are accurate -- the literal path is a real concrete instance of the
//     template, and POSTing to it is exactly what the client does -- but the
//     literal form is what makes a generated client expose one typed call per
//     workflow, which is the acceptance cleat#2913 carries ("a TypeScript
//     client generated from it type-checks a correct call and fails to
//     compile on a misspelled field"). With one templated path the request
//     body becomes a union the caller must narrow before anything checks a
//     field name.
//
//  2. The newest version per name wins. ListWorkflowDefs orders by name, then
//     version DESC, and a caller generating a client wants the shape they
//     will be calling, not every version ever deployed. Version selection per
//     call is what `cleatctl` and the start API's own version resolution are
//     for; the document is not the place to enumerate history.
//
//  3. Entry points are carried as an enum on "entry_point", with "input" a
//     oneOf when a definition declares more than one. A single entry point
//     (the common case) puts its parameter schema on "input" directly, so the
//     generated type is the object the caller actually writes.
//
// Result schemas are published under components.schemas rather than being
// typed onto a response: the start endpoint returns a run id, not the
// workflow's result, so attaching a result schema to it would describe a
// response that does not exist. cleat#1980 stores both halves, and its
// motivating case -- an MCP tool's inputSchema and outputSchema coming from
// the same artifact -- needs the result half published somewhere, which is
// here.
//
// Deterministic by construction: encoding/json sorts map keys, and every
// slice is sorted explicitly, so two calls on one tenant produce byte-equal
// documents.
func buildOpenAPIDocument(defs []engine.WorkflowDef) map[string]any {
	type chosen struct {
		version    int
		entryPoint map[string]engine.EntryPointSchema
	}

	newest := map[string]chosen{}
	for _, def := range defs {
		if len(def.EntryPointSchemas) == 0 {
			continue // untyped by design; see handleOpenAPIDocument
		}
		cur, seen := newest[def.Name]
		if !seen || def.Version > cur.version {
			newest[def.Name] = chosen{version: def.Version, entryPoint: def.EntryPointSchemas}
		}
	}

	names := make([]string, 0, len(newest))
	for name := range newest {
		names = append(names, name)
	}
	sort.Strings(names)

	paths := map[string]any{}
	components := map[string]any{}

	for _, name := range names {
		c := newest[name]

		entryPoints := make([]string, 0, len(c.entryPoint))
		for ep := range c.entryPoint {
			entryPoints = append(entryPoints, ep)
		}
		sort.Strings(entryPoints)

		// len(Params) > 0 is the SAME predicate cmd/cleat-worker/server.go's
		// start-path validation uses to decide whether a schema applies to an
		// entry point. Two readers of one column must agree about when it is
		// present, or the document would advertise a schema the server does
		// not enforce (or the reverse).
		typed := make([]string, 0, len(entryPoints))
		inputs := make([]any, 0, len(entryPoints))
		for _, ep := range entryPoints {
			sch := c.entryPoint[ep]
			if len(sch.Params) == 0 {
				continue
			}
			typed = append(typed, ep)
			inputs = append(inputs, sch.Params)
			if len(sch.Result) > 0 {
				components[resultSchemaName(name, ep)] = sch.Result
			}
		}
		if len(typed) == 0 {
			continue // schemas present but empty: same as untyped
		}

		var inputSchema any
		if len(inputs) == 1 {
			inputSchema = inputs[0]
		} else {
			inputSchema = map[string]any{"oneOf": inputs}
		}

		paths["/api/workflows/"+name+"/start"] = map[string]any{
			"post": map[string]any{
				"operationId": "start_" + sanitiseOperationID(name),
				"summary":     "Start a " + name + " run",
				"requestBody": map[string]any{
					"required": true,
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"input":       inputSchema,
									"entry_point": map[string]any{"type": "string", "enum": typed},
								},
								"required": []string{"input"},
								// The binding ignores keys it does not know
								// (cleat#1690's rule, and the reason #1980's
								// schema allows them), so the document must
								// too, or a generated client would refuse a
								// call the server accepts.
								"additionalProperties": true,
							},
						},
					},
				},
				"responses": map[string]any{
					"201": map[string]any{"description": "The run was created."},
					"400": map[string]any{
						"description": "The input did not match the entry point's schema (cleat#1981).",
					},
				},
			},
		}
	}

	doc := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "cleat tenant workflow API",
			"version": "1.0.0",
			// Deliberately not the server version: this document describes the
			// HTTP surface for the CALLING tenant, which changes when a
			// workflow is deployed, not when the worker is upgraded.
			"description": "Assembled per tenant from the deployed workflow definitions that " +
				"carry a build-time entry-point schema. Untyped definitions are omitted.",
		},
		"paths": paths,
	}
	if len(components) > 0 {
		doc["components"] = map[string]any{"schemas": components}
	}
	return doc
}

// resultSchemaName is the components.schemas key for an entry point's result
// schema. Namespaced by both the workflow and the entry point because two
// workflows may legitimately share an entry-point name.
func resultSchemaName(name, entryPoint string) string {
	return sanitiseOperationID(name) + "." + sanitiseOperationID(entryPoint) + ".result"
}

// sanitiseOperationID maps a workflow or entry-point name onto the character
// set an OpenAPI operationId and a components key both accept. Workflow names
// are hyphenated slugs, and a client generator derives an identifier from
// this, so the mapping has to be total rather than a validation that can fail.
func sanitiseOperationID(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}
