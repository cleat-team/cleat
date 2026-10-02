package main

import (
	"crypto/sha256"
	"encoding/hex"
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

	// cleat#1986 slice 2b: an `internal` definition is not published here.
	//
	// This route is on the external surface (mounted WITHOUT adminAPIOnly) and
	// enumerates every definition the caller's tenant owns, so it discloses an
	// internal definition's existence and its entry-point schemas -- the same
	// disclosure the 404 on the per-definition routes exists to prevent, and a
	// document a client is generated from would carry calls for it. Same
	// class as the run list, filtered rather than refused because a document
	// cannot answer 404 for one of the definitions inside it.
	s.writeJSON(w, http.StatusOK, buildOpenAPIDocument(withoutInternalDefs(defs)))
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
//  3. Entry points are carried as an enum on "entry_point", with "input" an
//     anyOf when a definition declares more than one. A single entry point
//     (the common case) puts its parameter schema on "input" directly, so the
//     generated type is the object the caller actually writes. It is anyOf
//     and not oneOf because the branches are open objects and therefore
//     overlap -- see the comment at the assignment.
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

	// Derived identifiers must be a TOTAL mapping from workflow name, or two
	// definitions that sanitise alike share one operationId (OpenAPI 3.1
	// requires uniqueness) and one components key (the second silently
	// overwrites the first). Not exotic: cleat build takes the workflow name
	// from the DIRECTORY name, so "my-workflow" and "my_workflow" are a
	// realistic pair for one tenant.
	stems := stemsFor(names)

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
				components[resultSchemaName(stems[name], ep)] = sch.Result
			}
		}
		if len(typed) == 0 {
			continue // schemas present but empty: same as untyped
		}

		var inputSchema any
		if len(inputs) == 1 {
			inputSchema = inputs[0]
		} else {
			// anyOf, NOT oneOf. Every emitted schema carries
			// additionalProperties:true (the binding ignores keys it does not
			// know), so two entry points' schemas are not disjoint: an object
			// with both an "amount" and a "ref" matches both branches.
			// oneOf demands EXACTLY one match and would reject a call
			// handleStartWorkflow accepts -- the document disagreeing with the
			// server, which is the same defect the empty-Params predicate above
			// exists to avoid, arriving from the other direction.
			inputSchema = map[string]any{"anyOf": inputs}
		}

		paths["/api/workflows/"+name+"/start"] = map[string]any{
			"post": map[string]any{
				"operationId": "start_" + stems[name],
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

// stemsFor returns a TOTAL mapping from workflow name to the identifier stem
// used for its operationId and its components keys.
//
// A plain sanitise is not total: "a-b" and "a_b" both become "a_b", so one
// tenant holding both would get one operationId between them (OpenAPI 3.1
// requires uniqueness) and one components key, the second schema silently
// overwriting the first. When a group of names collapses onto one stem,
// EVERY member of the group is disambiguated -- not just the later ones --
// so the result does not depend on the order the names arrived in.
//
// Only workflow names need this. Entry-point names are guest function names
// (Go, Python), which are identifiers before they reach here, so
// sanitiseOperationID is the identity on them and distinct names stay
// distinct.
func stemsFor(names []string) map[string]string {
	group := make(map[string][]string, len(names))
	for _, n := range names {
		s := sanitiseOperationID(n)
		group[s] = append(group[s], n)
	}

	stems := make(map[string]string, len(names))
	for _, n := range names {
		s := sanitiseOperationID(n)
		if len(group[s]) == 1 {
			stems[n] = s
			continue
		}
		sum := sha256.Sum256([]byte(n))
		stems[n] = s + "_" + hex.EncodeToString(sum[:4])
	}
	return stems
}

// resultSchemaName is the components.schemas key for an entry point's result
// schema. Namespaced by both the workflow and the entry point because two
// workflows may legitimately share an entry-point name. The stem is passed in
// already disambiguated by stemsFor.
func resultSchemaName(stem, entryPoint string) string {
	return stem + "." + sanitiseOperationID(entryPoint) + ".result"
}

// sanitiseOperationID maps a workflow or entry-point name onto the character
// set an OpenAPI operationId and a components key both accept. It is NOT
// injective -- see stemsFor for what that costs and how it is paid for.
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
