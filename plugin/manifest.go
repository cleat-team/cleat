package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/mod/semver"
)

// Manifest is the parsed form of a plugin.json or plugin.yaml file.
type Manifest struct {
	Name            string                 `json:"name" yaml:"name"`
	Version         string                 `json:"version" yaml:"version"`
	Description     string                 `json:"description" yaml:"description"`
	Author          string                 `json:"author" yaml:"author"`
	Repository      string                 `json:"repository,omitempty" yaml:"repository,omitempty"`
	MinCleatVersion string                 `json:"min_cleat_version,omitempty" yaml:"min_cleat_version,omitempty"`
	Capabilities    Capabilities           `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	HostFunctions   map[string]HostFuncDef `json:"host_functions,omitempty" yaml:"host_functions,omitempty"`
	Types           map[string]TypeDef     `json:"types,omitempty" yaml:"types,omitempty"`
	// Signature is an optional Ed25519 signature of the canonical JSON
	// representation of the manifest (excluding the signature field itself).
	// When present, the runtime verifies the signature against the plugin
	// author's public key before deploying. Initially this is required only
	// for official plugins (cleat/ prefix or no slash in name); community
	// plugins may opt in.
	Signature string `json:"signature,omitempty" yaml:"signature,omitempty"`

	// SigningKeyID identifies the public key used to produce Signature.
	// This is an opaque identifier (e.g., "cleat-official-2026") that the
	// runtime uses to look up the corresponding Ed25519 public key.
	SigningKeyID string `json:"signing_key_id,omitempty" yaml:"signing_key_id,omitempty"`
}

// Capabilities declares what infrastructure access a plugin needs.
type Capabilities struct {
	Database         bool     `json:"database" yaml:"database"`
	StartWorkflow    bool     `json:"start_workflow" yaml:"start_workflow"`
	SignalWorkflow   bool     `json:"signal_workflow" yaml:"signal_workflow"`
	HTTPRoutes       bool     `json:"http_routes" yaml:"http_routes"`
	HTTPMiddleware   bool     `json:"http_middleware" yaml:"http_middleware"`
	BackgroundWorker bool     `json:"background_worker" yaml:"background_worker"`
	CallPlugin       []string `json:"call_plugin" yaml:"call_plugin"`
}

// HostFuncDef describes a host function in a plugin manifest.
type HostFuncDef struct {
	Description string  `json:"description" yaml:"description"`
	Input       TypeDef `json:"input" yaml:"input"`
	Output      TypeDef `json:"output" yaml:"output"`
	// Idempotent here is DECLARATIVE ONLY and does not affect replay.
	//
	// It reaches internal/plugingen's IR and stops: no generator emits it into
	// a FuncOptions literal, so a manifest saying `idempotent: true` produces a
	// registration with the field unset. Verified by the field being written at
	// from_manifest.go and read by nothing outside tests.
	//
	// Stated because the opposite is the natural assumption, and because the
	// engine-side meaning changed in cleat#1318: replay re-invokes only when a
	// registration sets BOTH Idempotent and SameValueOnReplay. A manifest
	// author cannot ask for that here; a hand-written Register call is the only
	// way to set either.
	Idempotent bool `json:"idempotent,omitempty" yaml:"idempotent,omitempty"`
	Streaming  bool `json:"streaming,omitempty" yaml:"streaming,omitempty"`
}

// TypeDef describes a type in a plugin manifest.
type TypeDef struct {
	Type     string              `json:"type" yaml:"type"`
	Fields   map[string]FieldDef `json:"fields,omitempty" yaml:"fields,omitempty"`
	Optional bool                `json:"optional,omitempty" yaml:"optional,omitempty"`
}

// FieldDef describes a field in a type definition.
type FieldDef struct {
	Type        string              `json:"type" yaml:"type"`
	Description string              `json:"description,omitempty" yaml:"description,omitempty"`
	Optional    bool                `json:"optional,omitempty" yaml:"optional,omitempty"`
	Format      string              `json:"format,omitempty" yaml:"format,omitempty"`
	Items       *FieldDef           `json:"items,omitempty" yaml:"items,omitempty"`
	Values      []string            `json:"values,omitempty" yaml:"values,omitempty"`
	Fields      map[string]FieldDef `json:"fields,omitempty" yaml:"fields,omitempty"`
	KeyType     *FieldDef           `json:"key_type,omitempty" yaml:"key_type,omitempty"`
	ValueType   *FieldDef           `json:"value_type,omitempty" yaml:"value_type,omitempty"`
}

var pluginNameRegexp = regexp.MustCompile(`^[a-z][a-z0-9_-]*(/[a-z][a-z0-9_-]*)?$`)

// LoadManifest reads and parses a plugin manifest from a file path.
// Supports .json and .yaml/.yml extensions. Support for YAML requires
// gopkg.in/yaml.v3 or equivalent; currently only JSON is supported natively.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("plugin: read manifest %s: %w", path, err)
	}

	ext := strings.ToLower(filepath.Ext(path))
	var m Manifest

	switch ext {
	case ".json":
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("plugin: parse manifest JSON: %w", err)
		}
	case ".yaml", ".yml":
		return nil, fmt.Errorf("plugin: YAML manifest support not yet available; use .json format")
	default:
		return nil, fmt.Errorf("plugin: unsupported manifest extension %q (use .json)", ext)
	}

	return &m, nil
}

// ValidateManifest validates a manifest against programmatic rules.
// Returns nil if valid, or an error describing all validation failures.
func ValidateManifest(m *Manifest) error {
	var errs []string

	// Validate name.
	if m.Name == "" {
		errs = append(errs, "name is required")
	} else if !pluginNameRegexp.MatchString(m.Name) {
		errs = append(errs, fmt.Sprintf("name %q must match pattern %s", m.Name, pluginNameRegexp.String()))
	}

	// Validate version.
	if m.Version == "" {
		errs = append(errs, "version is required")
	} else {
		// semver.IsValid requires a "v" prefix.
		v := m.Version
		if !strings.HasPrefix(v, "v") {
			v = "v" + v
		}
		if !semver.IsValid(v) {
			errs = append(errs, fmt.Sprintf("version %q is not a valid semantic version", m.Version))
		}
	}

	// Validate description.
	if m.Description == "" {
		errs = append(errs, "description is required")
	}

	// Validate author.
	if m.Author == "" {
		errs = append(errs, "author is required")
	}

	// Validate host function names.
	for name := range m.HostFunctions {
		if strings.Contains(name, "/") {
			errs = append(errs, fmt.Sprintf("host function name %q must not contain '/'", name))
		}
		if strings.ContainsRune(name, '\x00') {
			errs = append(errs, fmt.Sprintf("host function name %q must not contain null bytes", name))
		}
	}

	// Validate type references in host function input/output.
	if len(m.Types) > 0 {
		for funcName, fn := range m.HostFunctions {
			if err := validateTypeRef(fn.Input, m.Types, fmt.Sprintf("host function %q input", funcName)); err != nil {
				errs = append(errs, err.Error())
			}
			if err := validateTypeRef(fn.Output, m.Types, fmt.Sprintf("host function %q output", funcName)); err != nil {
				errs = append(errs, err.Error())
			}
		}
		// Validate named type definitions themselves.
		for typeName, td := range m.Types {
			if err := validateTypeDef(td, m.Types, fmt.Sprintf("type %q", typeName)); err != nil {
				errs = append(errs, err.Error())
			}
		}
		// cleat#2806, R1: a field naming a defined type is only safe to
		// generate as a Go struct member BY VALUE when the reference graph
		// is acyclic. Measured live: Envelope{next: Envelope} validated
		// under the field-type-reference fix above and then generated Go
		// that fails with "invalid recursive type: Envelope refers to
		// itself" -- the field-type relaxation is correct for the acyclic
		// case (traced in the commit that added it) and wrong for this one,
		// which validation alone cannot see without walking the graph. An
		// indirect cycle (A -> B -> A) has the identical shape.
		if err := validateNoTypeCycles(m.Types); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("plugin manifest validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// validateTypeRef checks that a type reference either is an inline object type
// or resolves to a defined type name.
func validateTypeRef(td TypeDef, types map[string]TypeDef, context string) error {
	simpleTypes := map[string]bool{
		"string": true, "int64": true, "float64": true,
		"bool": true, "bytes": true, "timestamp": true, "uuid": true,
	}
	if td.Type == "" {
		return fmt.Errorf("%s: type is empty", context)
	}
	if simpleTypes[td.Type] {
		return nil
	}
	if td.Type == "object" {
		return nil // inline object type is always valid as a type ref
	}
	// Check if it's a named type reference.
	if _, ok := types[td.Type]; ok {
		return nil
	}
	return fmt.Errorf("%s: type %q is not a simple type, inline object, or defined type", context, td.Type)
}

// validateTypeDef validates a type definition, including its fields.
//
// A field's type may be a builtin (below) or the name of another entry in
// types -- the same "simple type, inline object, or defined type" allowance
// validateTypeRef already gives a host function's top-level input/output
// (cleat#2776: the two validators disagreed about whether defined types are
// a thing, and a field had no way to say "this is a Message" without
// inlining the whole shape at every use site). internal/plugingen already
// resolves a field's type name against the generated types when it is not
// one of the builtins below (from_manifest.go's goType/pyType/rustType each
// have a `default: return t // assume it's a named type` fallback, and
// fieldDefToFieldIR's array/map cases already carry a named ItemsType/
// ValueType through unresolved) -- this was a validation-only gap, not a
// codegen one, so accepting it here has no downstream migration to do.
func validateTypeDef(td TypeDef, types map[string]TypeDef, context string) error {
	validFieldTypes := map[string]bool{
		"string": true, "int64": true, "float64": true,
		"bool": true, "bytes": true, "timestamp": true, "uuid": true,
		"object": true, "enum": true, "array": true, "optional": true, "map": true,
	}
	for fieldName, fd := range td.Fields {
		if fd.Type == "" {
			return fmt.Errorf("%s field %q: type is required", context, fieldName)
		}
		if validFieldTypes[fd.Type] {
			continue
		}
		if _, ok := types[fd.Type]; ok {
			continue
		}
		return fmt.Errorf("%s field %q: unsupported type %q (must be a simple type, object, enum, array, optional, map, or a defined type)", context, fieldName, fd.Type)
	}
	return nil
}

// fieldTypeRefs returns the names a type's fields directly reference into
// types -- via a field's OWN type only -- restricted to names that
// actually resolve, since an unresolvable name is already reported by
// validateTypeDef.
//
// Deliberately does NOT walk into an array's item type or a map's value
// type (cleat#2806 R2, a real regression measured and fixed, not a
// preventive restriction): Node{children: array of Node} and
// Node{kids: map[string]Node} both generate as []Node / map[string]Node,
// neither of which needs the complete type up front the way a struct
// field held by value does, and both compile today on develop. An
// earlier version of this function added Items.Type and ValueType.Type
// as edges, which rejected exactly that legal, common (a tree) shape --
// contradicting validateNoTypeCycles' own doc comment, which already said
// array/map wrapping was "not caught here on purpose" while the code did
// the opposite. Verified through the real pipeline (ValidateManifest ->
// FromManifest -> GenerateGo) that both the array-tree and map-tree
// shapes validate and the generated Go compiles, and that a direct
// self-reference (Node{next: Node}) is still rejected.
func fieldTypeRefs(td TypeDef, types map[string]TypeDef) []string {
	var refs []string
	for _, fd := range td.Fields {
		if _, ok := types[fd.Type]; ok {
			refs = append(refs, fd.Type)
		}
	}
	return refs
}

// validateNoTypeCycles rejects a reference cycle anywhere in the named-type
// graph -- a direct self-reference (Envelope.next: Envelope) or an
// indirect one (A -> B -> A) has the identical consequence downstream:
// internal/plugingen generates a Go struct field of the named type BY
// VALUE, and go/types rejects a struct that contains itself, directly or
// through another struct, with "invalid recursive type" (cleat#2806, R1,
// measured against the real generator). A cycle broken by "array"/"map"/
// "optional" wrapping is not caught here on purpose: those generate a
// slice, a map, or an omitted field respectively, none of which need the
// complete type up front, so they are not the shape this function exists
// to reject -- only a field naming another type DIRECTLY (or through
// another such direct reference) is.
func validateNoTypeCycles(types map[string]TypeDef) error {
	names := make([]string, 0, len(types))
	for name := range types {
		names = append(names, name)
	}
	sort.Strings(names)

	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := make(map[string]int, len(types))
	var path []string
	var visit func(name string) error
	visit = func(name string) error {
		switch state[name] {
		case done:
			return nil
		case visiting:
			// path may hold ancestors OUTSIDE the cycle (an unrelated root
			// C that happens to lead into a separate A -> B -> A loop
			// without itself being part of it) -- trim to where name first
			// appears, so the message names only the cycle, not the path
			// that reached it.
			start := 0
			for i, p := range path {
				if p == name {
					start = i
					break
				}
			}
			cycle := append(append([]string{}, path[start:]...), name)
			return fmt.Errorf("type reference cycle: %s", strings.Join(cycle, " -> "))
		}
		state[name] = visiting
		path = append(path, name)
		for _, ref := range fieldTypeRefs(types[name], types) {
			if err := visit(ref); err != nil {
				return err
			}
		}
		path = path[:len(path)-1]
		state[name] = done
		return nil
	}
	for _, name := range names {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

// DefaultCapabilities returns a Capabilities struct with all fields false.
func DefaultCapabilities() Capabilities {
	return Capabilities{}
}

// JSONBytes returns the manifest as pretty-printed JSON bytes.
func (m *Manifest) JSONBytes() ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}
