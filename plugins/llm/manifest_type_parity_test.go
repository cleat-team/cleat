package llm

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/llm/providers"
)

// expectedHostFunctionType is what plugin.json should say about one host
// function this plugin registers: the manifest type NAME its input/output
// should be declared as, and the real Go type whose JSON shape that name
// should structurally match. cleat#2656.
//
// inputGoType/outputGoType are nil for a request or response with no fixed,
// named, package-level Go type to reflect on -- list_models's OUTPUT is
// assembled from map[string]any in two different shapes depending on
// whether a provider filter is given (see listModels), so there is nothing
// stable to compare its declared fields against. That is recorded here, not
// left as an absence: a nil entry still asserts the manifest type NAME is
// correct and still requires the function to be present in this table at
// all, it just skips the structural field-by-field comparison for that one
// side.
type expectedHostFunctionType struct {
	inputManifestType  string
	inputGoType        reflect.Type
	outputManifestType string
	outputGoType       reflect.Type
}

var expectedHostFunctions = map[string]expectedHostFunctionType{
	"chat": {
		"chat_input", reflect.TypeOf(chatRequest{}),
		"chat_output", reflect.TypeOf(providers.ChatOutput{}),
	},
	"chat_stream": {
		"chat_input", reflect.TypeOf(chatRequest{}),
		"chat_output", reflect.TypeOf(providers.ChatOutput{}),
	},
	"embed": {
		"embed_input", reflect.TypeOf(embedRequest{}),
		"embed_output", reflect.TypeOf(providers.EmbedOutput{}),
	},
	"list_models": {
		"list_models_input", reflect.TypeOf(listModelsRequest{}),
		"list_models_output", nil,
	},
}

// registeredNames captures the FuncOptions.Name strings a real
// RegisterHostFunctions call registers, by implementing the same
// FuncRegistry/StreamFuncRegistry interfaces the engine does. cleat#2656 R1
// (cleat-review): the manifest/Go-struct comparison below is only as
// complete as the set of functions it is told to check -- driving that set
// from RegisterHostFunctions's own real call, rather than from a literal
// list a person maintains by hand, means a function added to the plugin
// without a matching entry here fails this test instead of silently
// escaping every check in it.
type registeredNames struct {
	names map[string]bool
}

func (r *registeredNames) Register(opts plugin.FuncOptions, _ plugin.PluginFunc) error {
	r.names[opts.Name] = true
	return nil
}

func (r *registeredNames) RegisterStream(opts plugin.FuncOptions, _ plugin.PluginStreamFunc) error {
	r.names[opts.Name] = true
	return nil
}

func TestManifestTypesMatchGoStructs(t *testing.T) {
	m, err := plugin.LoadManifest("plugin.json")
	if err != nil {
		t.Fatalf("load plugin.json: %v", err)
	}

	reg := &registeredNames{names: map[string]bool{}}
	if err := (&Plugin{}).RegisterHostFunctions(reg); err != nil {
		t.Fatalf("RegisterHostFunctions: %v", err)
	}

	// Three sets that must all agree: what RegisterHostFunctions actually
	// registers, what plugin.json declares, and what this test knows how to
	// check. A mismatch in any direction is real: a function registered but
	// undeclared reaches workflows with no manifest at all; a function
	// declared but unregistered describes a call that would fail at runtime;
	// a function registered and declared but absent from
	// expectedHostFunctions is exactly the coverage gap cleat-review found --
	// this test silently checking fewer functions than the plugin has.
	for name := range reg.names {
		if _, ok := m.HostFunctions[name]; !ok {
			t.Errorf("RegisterHostFunctions registers %q, but plugin.json declares no such host function", name)
		}
		if _, ok := expectedHostFunctions[name]; !ok {
			t.Errorf("RegisterHostFunctions registers %q, but expectedHostFunctions has no entry for it -- add one so this test actually checks it", name)
		}
	}
	for name := range m.HostFunctions {
		if !reg.names[name] {
			t.Errorf("plugin.json declares host function %q, but RegisterHostFunctions never registers it", name)
		}
	}
	for name := range expectedHostFunctions {
		if !reg.names[name] {
			t.Errorf("expectedHostFunctions has an entry for %q, but RegisterHostFunctions never registers it -- stale entry", name)
		}
	}

	for name, exp := range expectedHostFunctions {
		t.Run(name, func(t *testing.T) {
			fn, ok := m.HostFunctions[name]
			if !ok {
				return // already reported above
			}

			if fn.Input.Type != exp.inputManifestType {
				t.Errorf("host function %q declares input type %q, want %q", name, fn.Input.Type, exp.inputManifestType)
			} else if exp.inputGoType != nil {
				checkManifestType(t, exp.inputManifestType, exp.inputGoType, m)
			}

			if fn.Output.Type != exp.outputManifestType {
				t.Errorf("host function %q declares output type %q, want %q", name, fn.Output.Type, exp.outputManifestType)
			} else if exp.outputGoType != nil {
				checkManifestType(t, exp.outputManifestType, exp.outputGoType, m)
			}
		})
	}
}

func checkManifestType(t *testing.T, manifestType string, goType reflect.Type, m *plugin.Manifest) {
	t.Helper()
	td, ok := m.Types[manifestType]
	if !ok {
		t.Errorf("plugin.json declares no type %q", manifestType)
		return
	}
	diffs := compareStructFields(manifestType, td.Fields, goType, m.Types, map[string]bool{manifestType: true})
	if len(diffs) > 0 {
		t.Errorf("plugin.json %q has drifted from %s (cleat#2656):\n  %s",
			manifestType, goType, strings.Join(diffs, "\n  "))
	}
}

// resolveFieldGoType strips pointer indirection, which is the only wrapper
// a JSON-tagged struct field carries that a manifest FieldDef has no
// opinion about.
func resolveFieldGoType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// jsonFieldName returns the JSON name a struct field is visible under, and
// skip=true for a field encoding/json would never emit: unexported, or
// tagged json:"-".
func jsonFieldName(f reflect.StructField) (name string, skip bool) {
	if f.PkgPath != "" {
		return "", true
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", true
	}
	name = strings.Split(tag, ",")[0]
	if name == "" {
		name = f.Name
	}
	return name, false
}

func structFieldsByJSONName(t reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	t = resolveFieldGoType(t)
	if t.Kind() != reflect.Struct {
		return out
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, skip := jsonFieldName(f)
		if skip {
			continue
		}
		out[name] = f
	}
	return out
}

// isDynamicGoType reports whether goType is a JSON shape with no fixed
// structure of its own -- interface{} (any) or a map -- which is the only
// case a manifest is entitled to describe as a bare, field-less "object" or
// an items-less "array". cleat#2656 R2 (cleat-review): without this check,
// a manifest could describe any concrete Go struct or slice as bare
// "object"/"array" and this test would silently stop checking it.
func isDynamicGoType(t reflect.Type) bool {
	t = resolveFieldGoType(t)
	return t.Kind() == reflect.Interface || t.Kind() == reflect.Map
}

// compareStructFields diffs an object-shaped manifest field set against a Go
// struct type's JSON-visible fields, in both directions, then recurses into
// each field present on both sides. label identifies the manifest path for
// error messages (e.g. "chat_output.usage"). visited guards named-type
// recursion against a cycle in the manifest's own Types map.
func compareStructFields(label string, manifestFields map[string]plugin.FieldDef, goType reflect.Type, types map[string]plugin.TypeDef, visited map[string]bool) []string {
	goType = resolveFieldGoType(goType)
	if goType.Kind() != reflect.Struct {
		return []string{fmt.Sprintf("%s: manifest declares an object but the Go type is %s", label, goType)}
	}
	goFields := structFieldsByJSONName(goType)

	var diffs []string
	for name := range manifestFields {
		if _, ok := goFields[name]; !ok {
			diffs = append(diffs, fmt.Sprintf("%s: manifest field %q has no matching field in %s", label, name, goType))
		}
	}
	for name, gf := range goFields {
		if _, ok := manifestFields[name]; !ok {
			diffs = append(diffs, fmt.Sprintf("%s: %s field %s (json %q) is not declared in the manifest", label, goType, gf.Name, name))
		}
	}
	for name, fd := range manifestFields {
		gf, ok := goFields[name]
		if !ok {
			continue // already reported above
		}
		diffs = append(diffs, diffFieldDef(label+"."+name, fd, gf.Type, types, visited)...)
	}

	sort.Strings(diffs)
	return diffs
}

// diffFieldDef diffs one manifest FieldDef against the Go type of the
// struct field it claims to describe, recursing through array/map/object
// wrappers and named type references the same way plugin.ValidateManifest's
// own validateTypeRef resolves them.
func diffFieldDef(label string, fd plugin.FieldDef, goType reflect.Type, types map[string]plugin.TypeDef, visited map[string]bool) []string {
	goType = resolveFieldGoType(goType)

	switch fd.Type {
	case "array":
		if goType.Kind() != reflect.Slice && goType.Kind() != reflect.Array {
			return []string{fmt.Sprintf("%s: manifest declares array but Go type is %s", label, goType)}
		}
		if fd.Items == nil {
			if isDynamicGoType(goType.Elem()) {
				return nil
			}
			return []string{fmt.Sprintf("%s: manifest array has no items type, but Go element type %s has a fixed structure -- declare items or the drift underneath it is invisible", label, goType.Elem())}
		}
		return diffFieldDef(label+"[]", *fd.Items, goType.Elem(), types, visited)
	case "map":
		if goType.Kind() != reflect.Map {
			return []string{fmt.Sprintf("%s: manifest declares map but Go type is %s", label, goType)}
		}
		if fd.ValueType == nil {
			if isDynamicGoType(goType.Elem()) {
				return nil
			}
			return []string{fmt.Sprintf("%s: manifest map has no value type, but Go value type %s has a fixed structure -- declare value_type or the drift underneath it is invisible", label, goType.Elem())}
		}
		return diffFieldDef(label+"{}", *fd.ValueType, goType.Elem(), types, visited)
	case "object":
		if isDynamicGoType(goType) {
			return nil
		}
		if len(fd.Fields) == 0 {
			return []string{fmt.Sprintf("%s: manifest declares a bare object, but Go type %s has a fixed structure -- declare its fields or the drift underneath it is invisible", label, goType)}
		}
		return compareStructFields(label, fd.Fields, goType, types, visited)
	case "enum":
		if goType.Kind() != reflect.String {
			return []string{fmt.Sprintf("%s: manifest declares enum but Go type is %s", label, goType)}
		}
		return nil
	case "optional":
		// plugin.ValidateManifest's validateTypeDef accepts "optional" as a
		// field type, but no manifest in this tree uses it, and unlike
		// "array"/"map" it carries no wrapped-type information (Items,
		// ValueType) to recurse into -- a FieldDef already has a separate
		// Optional bool for marking optionality. There is nothing here to
		// structurally verify, so this reports rather than silently passing.
		// cleat#2776.
		return []string{fmt.Sprintf("%s: manifest declares field type \"optional\", which carries no further type information for this checker to verify against %s (see cleat#2776)", label, goType)}
	case "string", "int64", "float64", "bool", "bytes", "timestamp", "uuid":
		return diffScalar(label, fd.Type, goType)
	default:
		named, ok := types[fd.Type]
		if !ok {
			return []string{fmt.Sprintf("%s: manifest references undefined type %q", label, fd.Type)}
		}
		if visited[fd.Type] {
			return nil // already on this recursion path -- a manifest cycle, not a drift
		}
		next := make(map[string]bool, len(visited)+1)
		for k := range visited {
			next[k] = true
		}
		next[fd.Type] = true
		return compareStructFields(label, named.Fields, goType, types, next)
	}
}

func diffScalar(label, manifestType string, goType reflect.Type) []string {
	ok := false
	switch manifestType {
	case "string", "timestamp", "uuid":
		ok = goType.Kind() == reflect.String
	case "int64":
		switch goType.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			ok = true
		}
	case "float64":
		ok = goType.Kind() == reflect.Float32 || goType.Kind() == reflect.Float64
	case "bool":
		ok = goType.Kind() == reflect.Bool
	case "bytes":
		ok = goType.Kind() == reflect.Slice && goType.Elem().Kind() == reflect.Uint8
	}
	if ok {
		return nil
	}
	return []string{fmt.Sprintf("%s: manifest declares %s but Go type is %s", label, manifestType, goType)}
}
