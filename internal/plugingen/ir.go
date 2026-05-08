// Package plugingen provides intermediate representation and code generation
// from cleat plugin manifests. It normalizes a plugin.Manifest into an IR
// that is then fed to language-specific code generators (TypeScript, Python,
// Rust, Go).
package plugingen

// IR (Intermediate Representation) is the normalized form of a plugin's API
// surface, suitable for code generation in any target language.
type IR struct {
	PluginName    string
	PluginVersion string
	Description   string
	HostFunctions []HostFuncIR
	Types         []TypeIR
}

// HostFuncIR describes a single host function (workflow-callable plugin
// method) in the IR.
type HostFuncIR struct {
	Name        string
	Description string
	InputType   string // name of the type in Types, or a simple type ("string", etc.)
	OutputType  string // name of the type in Types, or a simple type ("string", etc.)
	Idempotent  bool
	Streaming   bool
}

// TypeIR describes a named structured type in the IR.
type TypeIR struct {
	Name   string
	Fields []FieldIR
}

// FieldIR describes a single field within a structured type.
//
// Simple fields set only Type (e.g. "string", "int64", "bool", "bytes",
// "timestamp", "uuid", or a reference to a named TypeIR).
//
// Array fields set ItemsType for simple element types, or Nested for complex
// element types (inline objects).
//
// Map fields set KeyType and ValueType for simple key/value types, or Nested
// for complex value types.
//
// Inline object fields set Nested to a *TypeIR with the embedded fields.
type FieldIR struct {
	Name        string
	Type        string  // "string", "int64", "float64", "bool", "bytes", "timestamp", "uuid", "array", "map", or a TypeIR name
	Optional    bool
	Description string
	Nested      *TypeIR // for array<T>, optional<T>, map<K,V>, inline objects
	ItemsType   string  // for array<T> with simple element type
	KeyType     string  // for map<K,V> with simple key type
	ValueType   string  // for map<K,V> with simple value type
}

// isSimpleType returns true for built-in primitive type names.
func isSimpleType(t string) bool {
	switch t {
	case "string", "int64", "float64", "bool", "bytes", "timestamp", "uuid":
		return true
	default:
		return false
	}
}

// isBuiltinType returns true for language-built-in types that should be
// referenced directly rather than rendered as a generated type.
func isBuiltinType(t string) bool {
	if t == "" || isSimpleType(t) {
		return true
	}
	return false
}
