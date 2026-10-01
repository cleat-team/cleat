package jsonschema

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"testing"
)

// typeOf type-checks src (a standalone Go file) and returns the resolved
// type of the named top-level declaration -- a type alias/definition's
// underlying type if name is a type, or a function's result-0 type if name
// is a function (used to get at a parameter type via a one-param wrapper
// function, which is less noisy than constructing *types.Var by hand).
func typeOf(t *testing.T, src string, name string) types.Type {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", "package p\n\nimport \"time\"\nimport \"encoding/json\"\n\nvar _ = time.Time{}\nvar _ json.RawMessage\n\n"+src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	conf := types.Config{Importer: importer.Default()}
	info := &types.Info{
		Defs: map[*ast.Ident]types.Object{},
	}
	pkg, err := conf.Check("p", fset, []*ast.File{f}, info)
	if err != nil {
		t.Fatalf("type-check: %v", err)
	}
	obj := pkg.Scope().Lookup(name)
	if obj == nil {
		t.Fatalf("no declaration named %q", name)
	}
	if fn, ok := obj.(*types.Func); ok {
		sig := fn.Type().(*types.Signature)
		if sig.Params().Len() != 1 {
			t.Fatalf("%s: want exactly one parameter, got %d", name, sig.Params().Len())
		}
		return sig.Params().At(0).Type()
	}
	return obj.Type()
}

func schemaOf(t *testing.T, src, name string) Schema {
	t.Helper()
	return FromGoType(typeOf(t, src, name))
}

func TestBasicTypes(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want Schema
	}{
		{"func F(x string) {}", Schema{"type": "string"}},
		{"func F(x bool) {}", Schema{"type": "boolean"}},
		{"func F(x int) {}", Schema{"type": "integer"}},
		{"func F(x int64) {}", Schema{"type": "integer"}},
		{"func F(x uint32) {}", Schema{"type": "integer"}},
		{"func F(x float64) {}", Schema{"type": "number"}},
		{"func F(x float32) {}", Schema{"type": "number"}},
	} {
		got := schemaOf(t, tc.src, "F")
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %#v, want %#v", tc.src, got, tc.want)
		}
	}
}

func TestPointerUnwrapsToPointeeSchema(t *testing.T) {
	got := schemaOf(t, "func F(x *int) {}", "F")
	want := Schema{"type": "integer"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v -- a pointer parameter's PRESENT value must still match its pointee's schema", got, want)
	}
}

func TestByteSliceIsBase64String(t *testing.T) {
	got := schemaOf(t, "func F(x []byte) {}", "F")
	want := Schema{"type": "string", "contentEncoding": "base64"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v -- encoding/json base64-encodes []byte, never emits a JSON array of numbers", got, want)
	}
}

func TestOrdinarySliceIsArray(t *testing.T) {
	got := schemaOf(t, "func F(x []string) {}", "F")
	want := Schema{"type": "array", "items": Schema{"type": "string"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestMapIsObjectWithAdditionalProperties(t *testing.T) {
	got := schemaOf(t, "func F(x map[string]int) {}", "F")
	want := Schema{"type": "object", "additionalProperties": Schema{"type": "integer"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestTimeTimeIsRFC3339String(t *testing.T) {
	got := schemaOf(t, "func F(x time.Time) {}", "F")
	want := Schema{"type": "string", "format": "date-time"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v -- time.Time has its own MarshalJSON, the struct go/types sees underneath it is not what's on the wire", got, want)
	}
}

func TestJSONRawMessageIsUnconstrained(t *testing.T) {
	got := schemaOf(t, "func F(x json.RawMessage) {}", "F")
	want := anySchema()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v -- json.RawMessage holds an arbitrary already-valid JSON value verbatim", got, want)
	}
}

func TestAnyIsUnconstrained(t *testing.T) {
	got := schemaOf(t, "func F(x any) {}", "F")
	want := anySchema()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestStructFieldsUseJSONTagNames(t *testing.T) {
	src := `
type Order struct {
	ID       string ` + "`json:\"id\"`" + `
	Quantity int    ` + "`json:\"quantity,omitempty\"`" + `
	Internal string ` + "`json:\"-\"`" + `
	NoTag    bool
}
func F(x Order) {}
`
	got := schemaOf(t, src, "F")
	want := Schema{
		"type": "object",
		"properties": Schema{
			"id":       Schema{"type": "string"},
			"quantity": Schema{"type": "integer"},
			"NoTag":    Schema{"type": "boolean"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestStructHasNoRequiredArray pins the rule that matters most for "mirror
// the binding": encoding/json.Unmarshal does not enforce a struct field's
// presence, so a NESTED struct's schema must never carry a "required" array
// -- only the entry-point-level schema (built elsewhere, on top of this
// package) enforces presence, because THAT enforcement is this codebase's
// own custom logic in generateExport/generateDispatch, not encoding/json's.
func TestStructHasNoRequiredArray(t *testing.T) {
	got := schemaOf(t, "type T struct{ X string }\nfunc F(x T) {}", "F")
	if _, has := got["required"]; has {
		t.Errorf("struct schema carries a \"required\" array: %#v -- encoding/json.Unmarshal never enforces a struct field's presence", got)
	}
}

func TestStructHasNoAdditionalPropertiesFalse(t *testing.T) {
	got := schemaOf(t, "type T struct{ X string }\nfunc F(x T) {}", "F")
	if v, has := got["additionalProperties"]; has && v == false {
		t.Errorf("struct schema forbids additional properties: %#v -- encoding/json.Unmarshal ignores unrecognized keys rather than refusing them", got)
	}
}

func TestEmbeddedStructWithoutTagIsFlattened(t *testing.T) {
	src := `
type Base struct {
	ID string ` + "`json:\"id\"`" + `
}
type Order struct {
	Base
	Quantity int ` + "`json:\"quantity\"`" + `
}
func F(x Order) {}
`
	got := schemaOf(t, src, "F")
	props, _ := got["properties"].(Schema)
	if _, ok := props["id"]; !ok {
		t.Errorf("embedded Base's \"id\" was not promoted into Order's own properties: %#v -- encoding/json flattens an anonymous field with no explicit tag name", got)
	}
	if _, ok := props["Base"]; ok {
		t.Errorf("embedded field was nested under its type name \"Base\" instead of flattened: %#v", got)
	}
}

func TestEmbeddedStructWithTagIsNotFlattened(t *testing.T) {
	src := `
type Base struct {
	ID string ` + "`json:\"id\"`" + `
}
type Order struct {
	Base ` + "`json:\"base\"`" + `
}
func F(x Order) {}
`
	got := schemaOf(t, src, "F")
	props, _ := got["properties"].(Schema)
	if _, ok := props["base"]; !ok {
		t.Errorf("an embedded field WITH an explicit json tag must be a normal named property, not flattened: %#v", got)
	}
}

func TestSelfReferentialTypeDoesNotInfinitelyRecurse(t *testing.T) {
	src := `
type Node struct {
	Value int    ` + "`json:\"value\"`" + `
	Next  *Node  ` + "`json:\"next\"`" + `
}
func F(x Node) {}
`
	// The assertion is simply that this returns at all, under the test's own
	// timeout -- an infinite recursion would hang rather than fail an
	// assertion, so getting here at all is the proof.
	got := schemaOf(t, src, "F")
	props, _ := got["properties"].(Schema)
	if _, ok := props["next"]; !ok {
		t.Errorf("expected a \"next\" property even if its own shape is cut short: %#v", got)
	}
}
