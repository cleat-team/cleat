package jsonschema

import (
	"encoding/json"
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

// TestPointerUnwrapsToPointeeSchemaWithNull is cleat#2927 G1: a PRESENT
// value must match the pointee's schema, but encoding/json also accepts the
// JSON literal null for a pointer field (sets it to nil, no error), so the
// schema must admit both -- verified empirically in internal/jsonschema's
// own commit message, not assumed from the Go spec's prose alone.
func TestPointerUnwrapsToPointeeSchemaWithNull(t *testing.T) {
	got := schemaOf(t, "func F(x *int) {}", "F")
	want := Schema{"type": []any{"integer", "null"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v -- a pointer parameter's PRESENT value must match its pointee's schema OR be null", got, want)
	}
}

// TestDoublePointerDoesNotDoubleWrapNull covers nullable()'s own
// idempotency guard: fromGoType recurses through **int's outer Pointer
// into its inner Pointer, which already returns {"type":["integer","null"]}
// -- the outer nullable() call must see that as "already has type", not
// panic or silently produce a nested/duplicated null.
func TestDoublePointerDoesNotDoubleWrapNull(t *testing.T) {
	got := schemaOf(t, "func F(x **int) {}", "F")
	want := Schema{"type": []any{"integer", "null"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestByteSliceIsBase64StringOrNull(t *testing.T) {
	got := schemaOf(t, "func F(x []byte) {}", "F")
	want := Schema{"type": []any{"string", "null"}, "contentEncoding": "base64"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v -- encoding/json base64-encodes []byte, never emits a JSON array of numbers, and resets it to nil on a JSON null (cleat#2927)", got, want)
	}
}

func TestOrdinarySliceIsArrayOrNull(t *testing.T) {
	got := schemaOf(t, "func F(x []string) {}", "F")
	want := Schema{"type": []any{"array", "null"}, "items": Schema{"type": "string"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestFixedArrayIsNotNullable is the negative control TestOrdinarySliceIsArrayOrNull
// needs: a Go array (unlike a slice) is a value type with no nil state, and
// encoding/json's own null handling for one is a documented no-op (the
// array keeps whatever it already held) rather than a reset -- confirmed
// empirically, not assumed. Marking it nullable would claim a value the
// type can never actually take.
func TestFixedArrayIsNotNullable(t *testing.T) {
	got := schemaOf(t, "func F(x [3]int) {}", "F")
	want := Schema{"type": "array", "items": Schema{"type": "integer"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v -- a fixed-size array must NOT be nullable", got, want)
	}
}

// TestScalarStructAndFixedArrayRejectNullByOwnerDecision pins the owner's
// ruling on cleat#2927: a plain int/string/bool/struct/fixed-array parameter
// stays STRICT -- null is a 400 -- even though encoding/json's own binding
// treats null as a no-op for these kinds (the field keeps its zero value, no
// error). That is a DELIBERATE divergence from "mirror the binding" for
// exactly these kinds, decided because null-for-a-scalar is the same
// "null looks like zero" ambiguity cleat#1065 already named for an ABSENT
// parameter -- closing it for a PRESENT-but-null one is judged worth being
// stricter than the binding, unlike pointer/slice/map (jsonschema.go's
// nullable()), where null is a real, distinct value the binding can
// genuinely produce.
//
// Without this test, nothing pins the ruling: TestValidateNullableTypeAcceptsNull
// only asserts the nullable SIDE, so a later "make every kind nullable"
// change (the option the owner declined) would fail no test here. Falsified
// by wrapping each kind's schema in nullable() before writing this test --
// every case went from a violation to nil, confirming the test would have
// caught the declined change.
func TestScalarStructAndFixedArrayRejectNullByOwnerDecision(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"int", "func F(x int) {}"},
		{"string", "func F(x string) {}"},
		{"bool", "func F(x bool) {}"},
		{"struct", "type S struct{ A int `json:\"a\"` }\nfunc F(x S) {}"},
		{"fixed array", "func F(x [3]int) {}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := schemaOf(t, tc.src, "F")

			// The schema itself must not be nullable -- "type" is a bare
			// string, not the [T,"null"] list nullable() emits.
			if _, isList := schema["type"].([]any); isList {
				t.Fatalf("schema %#v declares a nullable type for %s -- the owner ruling keeps this kind strict", schema, tc.name)
			}

			schemaJSON, err := json.Marshal(schema)
			if err != nil {
				t.Fatalf("marshal schema: %v", err)
			}
			v, err := Validate(schemaJSON, json.RawMessage(`null`))
			if err != nil {
				t.Fatalf("Validate returned an error, not a violation: %v", err)
			}
			if v == nil {
				t.Errorf("null was accepted against a %s schema %s -- the owner ruling says this must be a 400", tc.name, schemaJSON)
			}
		})
	}
}

func TestMapIsObjectWithAdditionalPropertiesOrNull(t *testing.T) {
	got := schemaOf(t, "func F(x map[string]int) {}", "F")
	want := Schema{"type": []any{"object", "null"}, "additionalProperties": Schema{"type": "integer"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestNestedPointerFieldIsNullable covers the case cleat#2927's review
// measured directly: nullable() is applied inside fromGoType's own Pointer/
// Slice/Map cases, so a STRUCT FIELD of one of these kinds inherits the
// same null tolerance with no separate change needed in structSchema.
func TestNestedPointerFieldIsNullable(t *testing.T) {
	got := schemaOf(t, "type S struct{ P *int `json:\"p\"` }\nfunc F(x S) {}", "F")
	want := Schema{"type": "object", "properties": Schema{
		"p": Schema{"type": []any{"integer", "null"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v -- a nested pointer field must be nullable too", got, want)
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

// TestAnonymousNonStructEmbedIsAnOrdinaryFieldNotDropped pins a real defect
// found in review (cleat#1980): an anonymous embed whose type is NOT a
// struct -- `type Money int; struct{ Money }` -- was silently vanishing
// from the generated schema instead of either flattening (which makes no
// sense for a scalar -- there is nothing to promote FROM) or appearing as
// an ordinary property. Confirmed against a live json.Marshal first:
// encoding/json keys it "Money", not absent and not flattened.
func TestAnonymousNonStructEmbedIsAnOrdinaryFieldNotDropped(t *testing.T) {
	src := `
type Money int
type Order struct {
	Money
	Name string ` + "`json:\"name\"`" + `
}
func F(x Order) {}
`
	got := schemaOf(t, src, "F")
	props, _ := got["properties"].(Schema)
	moneySchema, ok := props["Money"].(Schema)
	if !ok {
		t.Fatalf("got %#v, want a \"Money\" property -- encoding/json keys an anonymous non-struct embed by its own type name, it does not drop it", got)
	}
	if moneySchema["type"] != "integer" {
		t.Errorf("Money schema = %#v, want {\"type\":\"integer\"} (Money's underlying type)", moneySchema)
	}
	if _, ok := props["name"]; !ok {
		t.Errorf("got %#v, want the ordinary \"name\" field to still be present", got)
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
