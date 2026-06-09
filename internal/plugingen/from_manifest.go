package plugingen

import (
	"strings"
	"unicode"

	"github.com/cleat-team/cleat/plugin"
)

// FromManifest converts a plugin.Manifest to the code-generation IR.
// It normalizes all types and host function signatures into the IR format.
func FromManifest(m *plugin.Manifest) (*IR, error) {
	ir := &IR{
		PluginName:    m.Name,
		PluginVersion: m.Version,
		Description:   m.Description,
	}

	// Convert named types first.
	for name, typ := range m.Types {
		ir.Types = append(ir.Types, convertTypedef(name, typ))
	}

	// Convert host functions.
	for name, fn := range m.HostFunctions {
		hf := HostFuncIR{
			Name:        name,
			Description: fn.Description,
			Idempotent:  fn.Idempotent,
			Streaming:   fn.Streaming,
		}

		// Resolve input type.
		hf.InputType = resolveTypeRef(fn.Input, m.Types, ir, name+"Input")
		// Resolve output type.
		hf.OutputType = resolveTypeRef(fn.Output, m.Types, ir, name+"Output")

		ir.HostFunctions = append(ir.HostFunctions, hf)
	}

	return ir, nil
}

// resolveTypeRef resolves a TypeDef reference: if it is a named type it
// returns the name; if it is a simple type it returns the type name; if it
// is an inline object it creates a synthetic TypeIR, appends it to ir.Types,
// and returns the synthetic name.
func resolveTypeRef(td plugin.TypeDef, namedTypes map[string]plugin.TypeDef, ir *IR, syntheticName string) string {
	// If it is a simple type, use it directly.
	if isSimpleType(td.Type) {
		return td.Type
	}

	// If it is a reference to a named type, use the name.
	if _, ok := namedTypes[td.Type]; ok {
		return td.Type
	}

	// If it is an inline object, generate a synthetic type.
	if td.Type == "object" || len(td.Fields) > 0 {
		typ := convertTypedef(syntheticName, td)
		ir.Types = append(ir.Types, typ)
		return syntheticName
	}

	// Fallback: return the type field as-is (possibly array/map as top-level).
	return td.Type
}

// convertTypedef converts a named TypeDef into a TypeIR.
func convertTypedef(name string, td plugin.TypeDef) TypeIR {
	tir := TypeIR{Name: name}
	for fname, fd := range td.Fields {
		tir.Fields = append(tir.Fields, fieldDefToFieldIR(fname, fd))
	}
	return tir
}

// fieldDefToFieldIR converts a single manifest FieldDef into a FieldIR.
func fieldDefToFieldIR(name string, fd plugin.FieldDef) FieldIR {
	fir := FieldIR{
		Name:        name,
		Type:        fd.Type,
		Optional:    fd.Optional,
		Description: fd.Description,
	}

	switch fd.Type {
	case "object":
		// Inline object: wrap in a synthetic TypeIR nested inside this field.
		nested := &TypeIR{}
		for fn, ffd := range fd.Fields {
			nested.Fields = append(nested.Fields, fieldDefToFieldIR(fn, ffd))
		}
		fir.Nested = nested
		fir.Type = "object"

	case "array":
		if fd.Items != nil {
			if isSimpleType(fd.Items.Type) {
				fir.ItemsType = fd.Items.Type
			} else if fd.Items.Type == "object" || len(fd.Items.Fields) > 0 {
				nested := &TypeIR{}
				for fn, ffd := range fd.Items.Fields {
					nested.Fields = append(nested.Fields, fieldDefToFieldIR(fn, ffd))
				}
				fir.Nested = nested
			} else {
				// Reference to a named type.
				fir.ItemsType = fd.Items.Type
			}
		}

	case "map":
		if fd.KeyType != nil {
			fir.KeyType = fd.KeyType.Type
		}
		if fd.ValueType != nil {
			if isSimpleType(fd.ValueType.Type) {
				fir.ValueType = fd.ValueType.Type
			} else if fd.ValueType.Type == "object" || len(fd.ValueType.Fields) > 0 {
				nested := &TypeIR{}
				for fn, ffd := range fd.ValueType.Fields {
					nested.Fields = append(nested.Fields, fieldDefToFieldIR(fn, ffd))
				}
				fir.Nested = nested
			} else {
				fir.ValueType = fd.ValueType.Type
			}
		}

	case "optional":
		fir.Optional = true
		// An optional type might embed fields directly, or wrap another type.
		if len(fd.Fields) > 0 {
			nested := &TypeIR{}
			for fn, ffd := range fd.Fields {
				nested.Fields = append(nested.Fields, fieldDefToFieldIR(fn, ffd))
			}
			fir.Nested = nested
		}
	}

	return fir
}

// toPascalCase converts a snake_case or kebab-case string to PascalCase.
func toPascalCase(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-' || r == '.'
	})
	for i, p := range parts {
		if len(p) > 0 {
			r := []rune(p)
			r[0] = unicode.ToUpper(r[0])
			parts[i] = string(r)
		}
	}
	return strings.Join(parts, "")
}

// toSnakeCase converts a PascalCase or camelCase string to snake_case.
func toSnakeCase(s string) string {
	var result []rune
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				result = append(result, '_')
			}
			result = append(result, unicode.ToLower(r))
		} else {
			result = append(result, r)
		}
	}
	return string(result)
}

// sanitizeIdent sanitizes a string for use as an identifier.
func sanitizeIdent(s string) string {
	if s == "" {
		return "empty"
	}
	var clean strings.Builder
	for i, r := range s {
		if i == 0 && !unicode.IsLetter(r) && r != '_' {
			clean.WriteRune('_')
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			clean.WriteRune(r)
		} else {
			clean.WriteRune('_')
		}
	}
	result := clean.String()
	if result == "" {
		return "unnamed"
	}
	return result
}

// collectReferencedTypes returns the set of type names referenced by host
// functions — useful when deciding which interfaces/structs to emit.
func collectReferencedTypes(ir *IR) map[string]bool {
	refs := make(map[string]bool)
	for _, fn := range ir.HostFunctions {
		if fn.InputType != "" && !isSimpleType(fn.InputType) {
			refs[fn.InputType] = true
		}
		if fn.OutputType != "" && !isSimpleType(fn.OutputType) {
			refs[fn.OutputType] = true
		}
	}
	return refs
}

// tsType maps an IR type string to a TypeScript type.
func tsType(t string, ir *IR) string {
	switch t {
	case "string":
		return "string"
	case "int64", "float64":
		return "number"
	case "bool":
		return "boolean"
	case "bytes":
		return "Uint8Array"
	case "timestamp", "uuid":
		return "string"
	case "object":
		return "Record<string, any>"
	case "array":
		return "any[]"
	default:
		// Check if it is a named type in the IR.
		for _, typ := range ir.Types {
			if typ.Name == t {
				return t
			}
		}
		return "any"
	}
}

// pyType maps an IR type string to a Python type annotation.
func pyType(t string) string {
	switch t {
	case "string":
		return "str"
	case "int64":
		return "int"
	case "float64":
		return "float"
	case "bool":
		return "bool"
	case "bytes":
		return "bytes"
	case "timestamp", "uuid":
		return "str"
	case "object":
		return "dict"
	default:
		return t // Assume it's a named type
	}
}

// rustType maps an IR type string to a Rust type.
func rustType(t string) string {
	switch t {
	case "string":
		return "String"
	case "int64":
		return "i64"
	case "float64":
		return "f64"
	case "bool":
		return "bool"
	case "bytes":
		return "Vec<u8>"
	case "timestamp", "uuid":
		return "String"
	case "object":
		return "serde_json::Value"
	default:
		return t
	}
}

// goType maps an IR type string to a Go type.
func goType(t string) string {
	switch t {
	case "string":
		return "string"
	case "int64":
		return "int64"
	case "float64":
		return "float64"
	case "bool":
		return "bool"
	case "bytes":
		return "[]byte"
	case "timestamp", "uuid":
		return "string"
	case "object":
		return "map[string]interface{}"
	default:
		return t
	}
}

// isMapType returns true if the field descriptor represents a map type.
func isMapType(fir FieldIR) bool {
	return fir.Type == "map" || fir.KeyType != ""
}

// isArrayType returns true if the field descriptor represents an array type.
func isArrayType(fir FieldIR) bool {
	return fir.Type == "array" || fir.ItemsType != "" || (fir.Type != "object" && fir.Nested != nil && fir.Type == "array")
}

// pyFieldType returns the Python type string for a FieldIR.
func pyFieldType(fir FieldIR) string {
	if fir.Type == "array" {
		if fir.ItemsType != "" {
			return "list[" + pyType(fir.ItemsType) + "]"
		}
		return "list"
	}
	if isMapType(fir) {
		if fir.ValueType != "" {
			return "dict[str, " + pyType(fir.ValueType) + "]"
		}
		return "dict"
	}
	if fir.Nested != nil {
		return "dict"
	}
	return pyType(fir.Type)
}

// rustFieldType returns the Rust type string for a FieldIR.
func rustFieldType(fir FieldIR) string {
	if fir.Type == "array" {
		if fir.ItemsType != "" {
			return "Vec<" + rustType(fir.ItemsType) + ">"
		}
		return "Vec<serde_json::Value>"
	}
	if isMapType(fir) {
		if fir.ValueType != "" {
			return "std::collections::HashMap<String, " + rustType(fir.ValueType) + ">"
		}
		return "std::collections::HashMap<String, serde_json::Value>"
	}
	if fir.Nested != nil {
		return "serde_json::Value"
	}
	return rustType(fir.Type)
}

// goFieldType returns the Go type string for a FieldIR.
func goFieldType(fir FieldIR) string {
	if fir.Type == "array" {
		if fir.ItemsType != "" {
			return "[]" + goType(fir.ItemsType)
		}
		return "[]interface{}"
	}
	if isMapType(fir) {
		if fir.ValueType != "" {
			return "map[string]" + goType(fir.ValueType)
		}
		return "map[string]interface{}"
	}
	if fir.Nested != nil {
		return "interface{}"
	}
	return goType(fir.Type)
}

// unique returns the deduplicated elements of a string slice, preserving
// insertion order.
func unique(slice []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, s := range slice {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}
