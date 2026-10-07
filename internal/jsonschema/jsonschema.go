// Package jsonschema converts a Go type, as seen by go/types, into the JSON
// Schema fragment that describes what encoding/json actually accepts when
// unmarshaling into a value of that type.
//
// THE GOVERNING RULE (cleat#1980): a schema here must mirror the binding,
// not improve on it. Where encoding/json's own behavior is permissive (an
// unrecognized struct is still decodable with extra JSON keys ignored, a
// missing struct field just keeps its zero value), the schema must be
// equally permissive, even where a hand-written schema would plausibly be
// stricter. Where this package does not know a type's shape precisely
// enough to describe it honestly, it emits the unconstrained schema
// (anySchema) rather than guess -- a wrong, narrower schema would reject
// payloads the binding actually accepts, which is worse than admitting
// "unknown".
package jsonschema

import (
	"go/types"
	"reflect"
	"strings"
)

// Schema is a JSON Schema document fragment, represented as a plain Go value
// ready for json.Marshal. Draft 2020-12 vocabulary, but only the subset
// needed to describe what encoding/json accepts: type, properties,
// required, items, additionalProperties, format, contentEncoding.
type Schema map[string]any

// anySchema is the unconstrained JSON Schema -- every instance is valid.
// Used wherever this package cannot describe a type's shape more precisely
// without risking being wrong rather than merely imprecise.
func anySchema() Schema { return Schema{} }

// nullable widens s's declared "type" into the two-element list [T, "null"]
// (JSON Schema draft 2020-12 section 6.1.1 permits "type" to be an array), for a Go
// kind whose zero/nil state genuinely cannot be told apart from "absent" by
// encoding/json: a pointer, slice or map reaching a JSON null is reset to
// nil, not refused (see this file's three call sites for the empirical
// confirmation each one carries). validate.go's checkTypes is the reader
// this is paired with -- without it, a bare string-type assertion on
// schema["type"] silently treats a list as "no declared type" and skips
// validation entirely, which is a worse bug than the one this fixes.
//
// A schema with no string "type" (anySchema, or one already widened by a
// recursive call -- a **int's outer nullable() sees its inner call's
// []any already) is returned unchanged: there is nothing to add to, and an
// unconditional second wrap would silently no-op anyway once checkTypes
// treats both forms as equivalent, so leaving it alone here is about
// clarity, not correctness.
func nullable(s Schema) Schema {
	t, ok := s["type"].(string)
	if !ok {
		return s
	}
	out := make(Schema, len(s))
	for k, v := range s {
		out[k] = v
	}
	out["type"] = []any{t, "null"}
	return out
}

// FromGoType converts t into the JSON Schema fragment describing what
// encoding/json accepts when unmarshaling into a value of that type.
//
// visiting guards against infinite recursion on a self-referential type
// (e.g. a linked-list node with a *Node field) -- pass nil for a top-level
// call; FromGoType manages it on recursive calls itself.
func FromGoType(t types.Type) Schema {
	return fromGoType(t, map[types.Type]bool{})
}

func fromGoType(t types.Type, visiting map[types.Type]bool) Schema {
	if t == nil {
		return anySchema()
	}

	switch tt := t.(type) {
	case *types.Pointer:
		// A pointer's own nil-ness has no JSON Schema equivalent worth
		// encoding here (whether this field may be ABSENT is a property of
		// its ENCLOSING struct's field, handled by the struct case below and
		// by the top-level entry-point caller for parameters -- not by the
		// pointee's own schema). What a present value must look like is
		// exactly the pointee's schema -- EXCEPT that a present value may
		// also be the JSON literal null: encoding/json's Unmarshal sets a
		// pointer to nil on null with no error (cleat#2927's own probe,
		// re-verified empirically here: json.Unmarshal([]byte(`null`), &p)
		// for *int leaves p nil, err==nil), so a schema that only describes
		// the pointee rejects a payload the binding accepts. nullable() adds
		// the "null" alternative the pointee's own schema cannot know it
		// needs.
		return nullable(fromGoType(tt.Elem(), visiting))

	case *types.Basic:
		return basicSchema(tt)

	case *types.Slice:
		// Same reasoning as *types.Pointer above, for the same empirically-
		// confirmed reason: encoding/json resets a slice (including []byte,
		// handled inside sliceOrArraySchema) to nil on a JSON null, with no
		// error. A *types.Array (fixed-size, e.g. [3]int) is deliberately
		// NOT wrapped here -- Go arrays are value types with no nil state,
		// and encoding/json's null handling for one is a documented no-op
		// (the array keeps whatever it already held), the same as a plain
		// scalar, so widening its schema would claim a value the type can
		// never actually take.
		return nullable(sliceOrArraySchema(tt.Elem(), visiting))

	case *types.Array:
		return sliceOrArraySchema(tt.Elem(), visiting)

	case *types.Map:
		// encoding/json requires a map's key type to be a string, an integer,
		// or implement encoding.TextMarshaler/Unmarshaler -- in every case the
		// ENCODED key is a JSON string, so the key's own type contributes
		// nothing to the schema. The value type is what additionalProperties
		// describes.
		if visiting[t] {
			return anySchema()
		}
		visiting[t] = true
		defer delete(visiting, t)
		// nullable(): a map, like a pointer or slice, is reset to nil on a
		// JSON null with no error from encoding/json.
		return nullable(Schema{
			"type":                 "object",
			"additionalProperties": fromGoType(tt.Elem(), visiting),
		})

	case *types.Named:
		return namedSchema(tt, visiting)

	case *types.Struct:
		return structSchema(tt, visiting)

	case *types.Interface:
		// interface{}/any and json.RawMessage's underlying type (a named
		// []byte, handled in namedSchema before reaching here) carry no
		// shape at all -- encoding/json accepts any valid JSON value into an
		// any-typed field.
		return anySchema()

	default:
		// Channels, funcs, complex numbers: not JSON-serializable by
		// encoding/json at all (json.Marshal errors on them), so they
		// should never reach here from a real entry point parameter --
		// the Go compiler itself will have refused to emit a build that
		// calls one with such a type populated from JSON. anySchema is the
		// honest answer rather than a guess at a shape with no meaning.
		return anySchema()
	}
}

func basicSchema(b *types.Basic) Schema {
	switch b.Info() & (types.IsBoolean | types.IsInteger | types.IsFloat | types.IsString) {
	case types.IsBoolean:
		return Schema{"type": "boolean"}
	case types.IsInteger:
		return Schema{"type": "integer"}
	case types.IsFloat:
		return Schema{"type": "number"}
	case types.IsString:
		return Schema{"type": "string"}
	default:
		// types.Invalid, or an exotic basic kind (complex64/128, unsafe.Pointer,
		// uintptr) -- none are plain-JSON-serializable by encoding/json in a
		// way this package should claim a shape for.
		return anySchema()
	}
}

func sliceOrArraySchema(elem types.Type, visiting map[types.Type]bool) Schema {
	// []byte (and [N]byte) is base64-encoded by encoding/json, not emitted
	// as a JSON array of numbers -- RFC 4648 standard encoding, which is
	// what encoding/json's Marshal/Unmarshal both use unconditionally.
	if basic, ok := elem.Underlying().(*types.Basic); ok && basic.Kind() == types.Byte {
		return Schema{"type": "string", "contentEncoding": "base64"}
	}
	return Schema{
		"type":  "array",
		"items": fromGoType(elem, visiting),
	}
}

func namedSchema(n *types.Named, visiting map[types.Type]bool) Schema {
	obj := n.Obj()
	if obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == "time" && obj.Name() == "Time" {
		// time.Time marshals via its own MarshalJSON as an RFC 3339 string,
		// not as the struct literal go/types sees underneath it.
		return Schema{"type": "string", "format": "date-time"}
	}
	if obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == "encoding/json" && obj.Name() == "RawMessage" {
		// json.RawMessage's whole purpose is to hold an arbitrary, already-
		// valid JSON value verbatim -- there is no narrower shape to claim.
		return anySchema()
	}

	t := types.Type(n)
	if visiting[t] {
		// A self-referential type (a linked list, a tree node). Breaking the
		// cycle with anySchema is honest: encoding/json itself has no depth
		// limit, so any attempt at a fixed-depth schema would be an
		// arbitrary guess at how deep a real payload goes.
		return anySchema()
	}
	visiting[t] = true
	defer delete(visiting, t)

	switch under := n.Underlying().(type) {
	case *types.Struct:
		return structSchema(under, visiting)
	case *types.Basic:
		// A named []byte-ELEMENT type reaching here directly (rather than
		// through sliceOrArraySchema) doesn't occur in practice. Falling
		// through to the ordinary basic handling either way is correct for
		// a named scalar (e.g. `type Status int`): encoding/json marshals
		// it exactly as its underlying basic kind.
		return basicSchema(under)
	default:
		return fromGoType(n.Underlying(), visiting)
	}
}

func structSchema(s *types.Struct, visiting map[types.Type]bool) Schema {
	properties := Schema{}
	for i := 0; i < s.NumFields(); i++ {
		f := s.Field(i)
		if !f.Exported() {
			// encoding/json never reads or writes an unexported field.
			continue
		}
		name, omit, inline := jsonFieldName(f, s.Tag(i))
		if omit {
			continue
		}
		if inline {
			// An anonymous embedded field with no explicit JSON name.
			// encoding/json promotes a STRUCT embed's own fields into this
			// struct's set of keys rather than nesting them under the
			// field's type name -- fromGoType already unwraps one pointer
			// level, so this also covers an embedded *Base.
			embedded := fromGoType(f.Type(), visiting)
			if nested, ok := embedded["properties"].(Schema); ok {
				for k, v := range nested {
					properties[k] = v
				}
				continue
			}
			// A NON-struct anonymous embed (`type Money int; struct{
			// Money }`) is not flattened -- there is nothing to promote a
			// scalar/slice/map value's fields FROM. encoding/json instead
			// treats it as an ordinary field keyed by its own unqualified
			// type name, which is exactly what f.Name() already returns
			// for an anonymous field (confirmed against a live
			// json.Marshal: `{"Money":5,...}`, not an absent key). The
			// first version of this function had no such branch and
			// dropped the field from the schema entirely.
			properties[f.Name()] = embedded
			continue
		}
		properties[name] = fromGoType(f.Type(), visiting)
	}

	schema := Schema{
		"type":       "object",
		"properties": properties,
	}
	// DELIBERATELY NO "required" HERE. encoding/json's Unmarshal does not
	// enforce a struct field's presence -- an absent key simply leaves the
	// field at its zero value, for every struct this package will ever see.
	// "required" at the top level of an entry point's whole input is a
	// SEPARATE rule, enforced by generateExport/generateDispatch's own
	// extraction logic (cleat#1980's entry-point-level schema adds it there,
	// not here).
	//
	// DELIBERATELY NO "additionalProperties": false, for the same reason the
	// design note on cleat#1980 gives for the whole input object: the
	// binding (encoding/json, same as the top level) ignores keys it does
	// not recognize rather than refusing them.
	return schema
}

// jsonFieldName applies encoding/json's own struct-tag rules to a single
// field: the key encoding/json looks for, whether it is skipped entirely,
// and whether it should be flattened (an anonymous field with no explicit
// name in its tag).
//
// go/types.Struct.Tag(i) returns the field's raw tag text in exactly the
// format reflect.StructTag already parses, so this reuses that rather than
// re-implementing backtick-tag scanning by hand.
func jsonFieldName(f *types.Var, tag string) (name string, omit, inline bool) {
	jsonTag := reflect.StructTag(tag).Get("json")
	tagName, _, _ := strings.Cut(jsonTag, ",")

	if jsonTag == "-" {
		return "", true, false
	}
	if tagName != "" {
		return tagName, false, false
	}
	if f.Anonymous() {
		return "", false, true
	}
	return f.Name(), false, false
}
