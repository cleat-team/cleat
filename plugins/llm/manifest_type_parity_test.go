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

// manifestGoTypeCases pairs each host function's manifest-declared input/
// output type name with the real Go type whose JSON shape a workflow
// actually exchanges with it -- the struct json.Unmarshal reads a request
// into, or json.Marshal writes a response out of. cleat#2656:
// plugin.ValidateManifest checks a manifest's internal consistency only, and
// nothing cross-references a HostFuncDef's declared TypeDef against the real
// Go types it claims to describe. This test is that cross-reference, for the
// one plugin in the tree that maintains a plugin.json at all.
//
// list_models is deliberately absent. Its response is assembled ad hoc from
// map[string]any and a function-local anonymous struct (see listModels in
// host_functions.go), not a named package-level Go type, so there is no
// stable reflect.Type to point this test at. That is not a gap this test is
// leaving open: list_models_output's own manifest fields ("models",
// "providers") are both untyped generic "object"s with no nested Fields, so
// the manifest itself declares nothing structural to check there either --
// see the "object with no Fields is opaque by the manifest's own choice"
// case in diffFieldDef below, which is the same rule applied consistently.
var manifestGoTypeCases = []struct {
	function     string
	manifestType string
	goType       reflect.Type
}{
	{"chat", "chat_input", reflect.TypeOf(chatRequest{})},
	{"chat", "chat_output", reflect.TypeOf(providers.ChatOutput{})},
	{"chat_stream", "chat_input", reflect.TypeOf(chatRequest{})},
	{"chat_stream", "chat_output", reflect.TypeOf(providers.ChatOutput{})},
	{"embed", "embed_input", reflect.TypeOf(embedRequest{})},
	{"embed", "embed_output", reflect.TypeOf(providers.EmbedOutput{})},
}

func TestManifestTypesMatchGoStructs(t *testing.T) {
	m, err := plugin.LoadManifest("plugin.json")
	if err != nil {
		t.Fatalf("load plugin.json: %v", err)
	}

	for _, c := range manifestGoTypeCases {
		t.Run(c.function+"/"+c.manifestType, func(t *testing.T) {
			td, ok := m.Types[c.manifestType]
			if !ok {
				t.Fatalf("plugin.json declares no type %q", c.manifestType)
			}
			diffs := compareStructFields(c.manifestType, td.Fields, c.goType, m.Types,
				map[string]bool{c.manifestType: true})
			if len(diffs) > 0 {
				t.Errorf("plugin.json %q has drifted from %s (cleat#2656):\n  %s",
					c.manifestType, c.goType, strings.Join(diffs, "\n  "))
			}
		})
	}
}

// resolveFieldGoType strips pointer indirection, which is the only wrapper
// a JSON-tagged struct field carries that a manifest FieldDef has no
// opinion about.
func resolveFieldGoType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Ptr {
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
			return nil
		}
		return diffFieldDef(label+"[]", *fd.Items, goType.Elem(), types, visited)
	case "map":
		if goType.Kind() != reflect.Map {
			return []string{fmt.Sprintf("%s: manifest declares map but Go type is %s", label, goType)}
		}
		if fd.ValueType == nil {
			return nil
		}
		return diffFieldDef(label+"{}", *fd.ValueType, goType.Elem(), types, visited)
	case "object":
		// A Go field typed any/map[string]any (dynamic JSON) has no fixed
		// shape to compare structurally, whatever the manifest says.
		if goType.Kind() == reflect.Interface || goType.Kind() == reflect.Map {
			return nil
		}
		if len(fd.Fields) == 0 {
			// The manifest itself declares this object opaque -- nothing to
			// check, by the manifest's own choice, not this test's.
			return nil
		}
		return compareStructFields(label, fd.Fields, goType, types, visited)
	case "enum":
		if goType.Kind() != reflect.String {
			return []string{fmt.Sprintf("%s: manifest declares enum but Go type is %s", label, goType)}
		}
		return nil
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
