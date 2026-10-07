package analyzer

import (
	"go/types"
	"strings"
)

// ParamField describes one non-HostCalls parameter of a workflow entry
// point, exactly as the generated WASM binding sees it: GoName and JSONTag
// are what wasm/exports.go's generateExport and generateDispatch use to
// declare the Go-side variable and to look the value up by key in the
// caller's "input" object. A reader of Type gets the resolved go/types.Type
// for anything that needs to reason about the parameter's shape (a JSON
// Schema emitter); GoType is the pre-rendered string form generateExport and
// generateDispatch emit directly into generated source.
type ParamField struct {
	GoName  string     // capitalized Go identifier used in generated code
	GoType  string     // types.TypeString rendering, for codegen
	Type    types.Type // the resolved type, for anything reasoning about shape
	JSONTag string     // the key the binding looks up in the input object
}

// EntryPointFields returns fd's parameters after the leading HostCalls
// handle (if any), plus whether its signature returns a value and/or an
// error.
//
// This is the SINGLE derivation of "what does this entry point bind, and by
// what name" in the tree. Before it existed, wasm/exports.go computed this
// shape independently in two places -- generateExport (the //go:wasmexport
// emitter) and generateDispatch (the cleatDispatch emitter, which is the
// path the wasmtime host actually calls, per that function's own comment)
// -- and they drifted once already: cleat#1057 fixed the pointer-optional
// binding rule in one arm and left the int arm reporting success in the
// other, because nothing forced the two to agree. A third caller (the JSON
// Schema emitter, cleat#1980) reading a THIRD independent computation would
// reintroduce the same hazard rather than avoid it -- so every caller that
// needs to know an entry point's bindable fields calls this function
// instead of re-deriving the shape from fd.Type.Params() itself.
func EntryPointFields(fd *FuncDecl, qual types.Qualifier) (fields []ParamField, hasResultValue, hasErrorReturn bool) {
	sig := fd.Type
	params := sig.Params()

	startIdx := 0
	if params != nil && params.Len() > 0 {
		if IsHostCallsType(params.At(0).Type()) {
			startIdx = 1
		}
		for i := startIdx; i < params.Len(); i++ {
			p := params.At(i)
			fields = append(fields, ParamField{
				GoName:  capitalizeIdent(p.Name()),
				GoType:  types.TypeString(p.Type(), qual),
				Type:    p.Type(),
				JSONTag: p.Name(),
			})
		}
	}

	results := sig.Results()
	if results != nil {
		for i := 0; i < results.Len(); i++ {
			if types.TypeString(results.At(i).Type(), qual) == "error" {
				hasErrorReturn = true
			} else {
				hasResultValue = true
			}
		}
	}
	return fields, hasResultValue, hasErrorReturn
}

// capitalizeIdent matches wasm package's unexported capitalize() byte for
// byte (strings.ToUpper on the first rune, not arithmetic -- a parameter
// already starting uppercase, e.g. "ID", must pass through unchanged rather
// than being corrupted by a 'a'-'A' offset that assumes a lowercase start).
// Duplicated rather than imported because wasm already imports analyzer, so
// the reverse import would cycle.
func capitalizeIdent(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
