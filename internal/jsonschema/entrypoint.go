package jsonschema

import (
	"go/types"
	"strings"

	"github.com/cleat-team/cleat/internal/analyzer"
)

// EntryPointParamSchema builds the JSON Schema for a Go workflow entry
// point's "input" field, mirroring exactly what wasm/exports.go's
// generateExport and generateDispatch bind at runtime. It reads
// analyzer.EntryPointFields -- the single derivation of the binding shape
// both of those emitters use -- rather than re-deriving the field list
// independently, so this schema cannot drift from what the binding actually
// does.
//
// Four distinct wire shapes, matching the four arms the binding itself has:
//
//  1. No parameters beyond HostCalls: "input" is never read by the binding
//     at all. Schema: unconstrained -- the caller may send anything (it is
//     ignored) or omit "input" entirely.
//  2. Exactly one parameter, typed string: the WHOLE raw "input" payload is
//     assigned to it VERBATIM (wasm/exports.go: "argsJSON := readString(...)",
//     no json.Unmarshal, no key lookup). A caller posting a JSON object,
//     number, or any other value has that value's raw TEXT become the Go
//     string, unparsed. {"type":"string"} would be a LIE here: it would
//     reject a JSON object the binding genuinely accepts. The honest schema
//     is unconstrained.
//  3. Two or more parameters, or one non-string parameter: "input" must be a
//     JSON object, one property per parameter, keyed by the parameter's own
//     Go identifier (not a struct tag -- there is none at this level).
//     Every non-pointer parameter is REQUIRED: absence is refused by the
//     binding's own extraction logic (extractJSONRaw returning "" makes a
//     scalar's bind fail with an explicit "required" error, and makes a
//     composite's json.Unmarshal("") fail too) -- this is cleat#1690's own
//     rule, read directly from exports.go rather than assumed. A pointer
//     parameter is the one declared-optional case: absence binds nil.
//     additionalProperties is true: the binding only ever looks up the keys
//     it knows about and never rejects an input object for carrying extra
//     ones.
func EntryPointParamSchema(fd *analyzer.FuncDecl, qual types.Qualifier) Schema {
	fields, _, _ := analyzer.EntryPointFields(fd, qual)

	if len(fields) == 0 {
		return anySchema()
	}
	if len(fields) == 1 && fields[0].GoType == "string" {
		return anySchema()
	}

	properties := Schema{}
	var required []string
	for _, f := range fields {
		properties[f.JSONTag] = FromGoType(f.Type)
		// strings.HasPrefix(f.GoType, "*"), exactly matching the binding's
		// OWN check (wasm/exports.go) -- not types.Pointer via go/types.
		// The binding's optionality test is syntactic (does the RENDERED
		// type string start with "*"), not semantic: a named type whose
		// underlying is a pointer (`type IntPtr *int`) renders as "IntPtr",
		// fails that prefix check, and the binding therefore treats it as a
		// REQUIRED composite parameter, not an optional one -- a case a
		// semantic go/types.Pointer check would get backwards.
		if !strings.HasPrefix(f.GoType, "*") {
			required = append(required, f.JSONTag)
		}
	}

	schema := Schema{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": true,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// EntryPointResultSchema returns a Go entry point's result schema. Every Go
// entry point returns (string, error) or error alone -- verifyEntryPointResults
// (internal/closure/threading.go) enforces this as a build-time check, and
// wasm/exports.go's own codegen confirms it operationally:
// "__susResultJSON = []byte(__r)" then "resultStr := string(__susResultJSON)"
// puts the returned string on the wire VERBATIM. There is no richer
// structure anywhere in a Go entry point's signature to read a result shape
// from -- whatever the function's own code marshaled into that string is
// invisible to static analysis, and claiming to know its shape would be
// guessing at the user's own json.Marshal call, not reading the binding.
func EntryPointResultSchema(fd *analyzer.FuncDecl, qual types.Qualifier) Schema {
	_, hasResultValue, _ := analyzer.EntryPointFields(fd, qual)
	if !hasResultValue {
		// error-only entry point: nothing is ever written as a result value
		// on success (wasm/exports.go's generateExport has no
		// "__susResultJSON = ..." line in this arm at all).
		return anySchema()
	}
	return Schema{"type": "string"}
}
