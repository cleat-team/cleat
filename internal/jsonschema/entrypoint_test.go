package jsonschema

import (
	"go/token"
	"go/types"
	"reflect"
	"testing"

	"github.com/cleat-team/cleat/internal/analyzer"
)

// loadEntryPoint loads a real fixture package under testdata/ (the same
// fixtures wasm/ and internal/analyzer's own tests exercise against the
// real cleat.HostCalls type, rather than a synthetic one built by hand) and
// returns the named entry point's *analyzer.FuncDecl plus a qualifier for
// its own package.
func loadEntryPoint(t *testing.T, pkgPath, funcName string) (*analyzer.FuncDecl, types.Qualifier) {
	t.Helper()
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/"+pkgPath, fset)
	if err != nil {
		t.Fatalf("LoadPackages(%s): %v", pkgPath, err)
	}
	full := "github.com/cleat-team/cleat/testdata/" + pkgPath + "." + funcName
	fd := result.Funcs[full]
	if fd == nil {
		t.Fatalf("could not find %s in loaded package", full)
	}
	return fd, types.RelativeTo(result.TargetPkg.Types)
}

// TestZeroParameterEntryPointParamSchemaIsUnconstrained is the explicitly
// flagged historical gap: wasm/exports.go's own comment records that "every
// fixture and example in the repo happens to take an input parameter, so no
// test ever generated the zero-field case" for the binding itself. This is
// that fixture's first use for the schema side of the same shape.
func TestZeroParameterEntryPointParamSchemaIsUnconstrained(t *testing.T) {
	fd, qual := loadEntryPoint(t, "noargs", "Cleanup")
	got := EntryPointParamSchema(fd, qual)
	want := anySchema()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v -- a zero-parameter entry point never reads \"input\" at all", got, want)
	}
}

// TestLoneStringParamSchemaIsUnconstrainedNotTypeString pins the one case
// this whole package exists to get right rather than guess at: a single
// string-typed parameter receives the RAW "input" payload verbatim
// (wasm/exports.go: "argsJSON := readString(...)", no json.Unmarshal). A
// caller posting a JSON object is accepted by the binding and would be
// wrongly rejected by {"type":"string"}.
func TestLoneStringParamSchemaIsUnconstrainedNotTypeString(t *testing.T) {
	fd, qual := loadEntryPoint(t, "hello", "Greet")
	got := EntryPointParamSchema(fd, qual)
	want := anySchema()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v -- {\"type\":\"string\"} would reject payloads the binding genuinely accepts verbatim", got, want)
	}
}

// TestPointerParamIsOptionalStringParamIsRequired uses cleat#1065's own
// fixture -- built specifically to carry both arms of the optional/required
// distinction (see the fixture's own doc comment) -- so this test reads the
// exact case the binding mechanism itself was built and tested against.
func TestPointerParamIsOptionalStringParamIsRequired(t *testing.T) {
	fd, qual := loadEntryPoint(t, "optionalparam", "ApplyCoupon")
	got := EntryPointParamSchema(fd, qual)

	props, ok := got["properties"].(Schema)
	if !ok {
		t.Fatalf("no properties object in %#v", got)
	}
	if _, ok := props["userID"]; !ok {
		t.Errorf("missing \"userID\" property: %#v", got)
	}
	if _, ok := props["promo"]; !ok {
		t.Errorf("missing \"promo\" property: %#v", got)
	}

	required, _ := got["required"].([]string)
	if len(required) != 1 || required[0] != "userID" {
		t.Errorf("required = %#v, want exactly [\"userID\"] -- promo is a pointer (optional), userID is not", required)
	}

	if ap, has := got["additionalProperties"]; !has || ap != true {
		t.Errorf("additionalProperties = %#v, want true -- the binding only looks up known keys and never rejects extras", got["additionalProperties"])
	}
}

// TestMultiParamEntryPointRequiresEveryNonPointerField uses the repo's own
// README/testdata/basic fixture: two required parameters (a string and a
// slice of structs), neither a pointer, so both must be required.
func TestMultiParamEntryPointRequiresEveryNonPointerField(t *testing.T) {
	fd, qual := loadEntryPoint(t, "basic", "PlaceOrder")
	got := EntryPointParamSchema(fd, qual)

	required, _ := got["required"].([]string)
	want := map[string]bool{"userID": true, "cart": true}
	if len(required) != len(want) {
		t.Fatalf("required = %#v, want exactly %v", required, want)
	}
	for _, r := range required {
		if !want[r] {
			t.Errorf("unexpected required field %q", r)
		}
	}

	props, _ := got["properties"].(Schema)
	cartSchema, _ := props["cart"].(Schema)
	if cartSchema["type"] != "array" {
		t.Errorf("cart schema = %#v, want type array (cart is []CartItem)", cartSchema)
	}
}

// TestResultSchemaIsAlwaysStringForAValueReturningEntryPoint pins the
// cross-cutting result rule: every Go entry point that returns a value
// returns it as a bare string (verifyEntryPointResults enforces this at
// build time), so the result schema is always exactly {"type":"string"} --
// never anything richer, regardless of what the fixture's own return
// actually contains.
func TestResultSchemaIsAlwaysStringForAValueReturningEntryPoint(t *testing.T) {
	for _, tc := range []struct{ pkg, fn string }{
		{"basic", "PlaceOrder"},
		{"hello", "Greet"},
		{"optionalparam", "ApplyCoupon"},
	} {
		fd, qual := loadEntryPoint(t, tc.pkg, tc.fn)
		got := EntryPointResultSchema(fd, qual)
		want := Schema{"type": "string"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s.%s: got %#v, want %#v", tc.pkg, tc.fn, got, want)
		}
	}
}

// TestErrorOnlyEntryPointResultSchemaIsUnconstrained covers the
// error-alone arm verifyEntryPointResults also allows: nothing is ever
// written as a result value on success, so there is no "string" to claim
// either. testdata/basic.CancelOrder is a real, already-exported entry
// point of exactly this shape (error alone).
func TestErrorOnlyEntryPointResultSchemaIsUnconstrained(t *testing.T) {
	fd, qual := loadEntryPoint(t, "basic", "CancelOrder")
	got := EntryPointResultSchema(fd, qual)
	want := anySchema()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}
