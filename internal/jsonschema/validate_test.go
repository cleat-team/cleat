package jsonschema

import (
	"encoding/json"
	"testing"
)

func mustValidate(t *testing.T, schema, instance string) *ValidationError {
	t.Helper()
	v, err := Validate(json.RawMessage(schema), json.RawMessage(instance))
	if err != nil {
		t.Fatalf("Validate returned an error, not a violation: %v", err)
	}
	return v
}

func TestValidateAnySchemaAcceptsEverything(t *testing.T) {
	for _, instance := range []string{`{}`, `[1,2,3]`, `"hello"`, `42`, `null`, `not even json`} {
		if v := mustValidate(t, `{}`, instance); v != nil {
			t.Errorf("anySchema rejected %q: %v", instance, v)
		}
	}
	// An empty schemaJSON (nil/omitted column) behaves the same as the
	// literal {} anySchema -- the "no schema stored" case this validator
	// must treat as untyped, per #1981's owner decision.
	if v := mustValidate(t, ``, `{"anything":true}`); v != nil {
		t.Errorf("empty schemaJSON rejected a payload: %v", v)
	}
}

func TestValidateRequiredField(t *testing.T) {
	schema := `{"type":"object","properties":{"amount":{"type":"integer"}},"required":["amount"],"additionalProperties":true}`

	if v := mustValidate(t, schema, `{"amount":5}`); v != nil {
		t.Fatalf("a present required field was rejected: %v", v)
	}

	v := mustValidate(t, schema, `{}`)
	if v == nil {
		t.Fatal("missing required field was not reported")
	}
	if v.Rule != "required" || v.Field != "amount" {
		t.Errorf("got %+v, want Field=amount Rule=required", v)
	}
}

func TestValidateExtraPropertiesAreAlwaysAllowed(t *testing.T) {
	schema := `{"type":"object","properties":{"amount":{"type":"integer"}},"required":["amount"],"additionalProperties":true}`
	if v := mustValidate(t, schema, `{"amount":5,"surprise":"field"}`); v != nil {
		t.Errorf("an extra property was rejected: %v", v)
	}
}

func TestValidateTypeMismatch(t *testing.T) {
	cases := []struct {
		name     string
		schema   string
		instance string
		wantRule string
	}{
		{"string wanted, got number", `{"type":"string"}`, `5`, "type"},
		{"boolean wanted, got string", `{"type":"boolean"}`, `"true"`, "type"},
		{"integer wanted, got fraction", `{"type":"integer"}`, `5.5`, "type"},
		{"object wanted, got array", `{"type":"object","properties":{}}`, `[1]`, "type"},
		{"array wanted, got object", `{"type":"array","items":{"type":"string"}}`, `{}`, "type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := mustValidate(t, tc.schema, tc.instance)
			if v == nil {
				t.Fatal("expected a violation, got none")
			}
			if v.Rule != tc.wantRule {
				t.Errorf("got rule %q, want %q", v.Rule, tc.wantRule)
			}
		})
	}
}

func TestValidateIntegerAcceptsWholeFloats(t *testing.T) {
	// JSON has one numeric type; 5.0 on the wire is indistinguishable from 5
	// and must bind the same way encoding/json's own int field does.
	if v := mustValidate(t, `{"type":"integer"}`, `5.0`); v != nil {
		t.Errorf("a whole-valued float was rejected against integer: %v", v)
	}
}

// TestValidateIntegerCheckIsExactBeyondInt64Range is cleat#2927 A2: the
// original check converted the instance to int64 before comparing
// (`n != float64(int64(n))`), and a non-constant float64-to-int64
// conversion is implementation-defined, not merely truncating, once the
// value cannot be represented as an int64 (|n| >= 2^63) -- well within
// range for a JSON number. math.Trunc(n) == n stays in float64 throughout
// and is exact for every float64 value, including these.
//
// 2^64 is the measured regression case, not a guess: on this toolchain,
// int64(18446744073709551616.0) saturates to math.MaxInt64, so the OLD
// check (n != float64(int64(n))) reports a FALSE violation for a value
// that is, in fact, a whole number -- confirmed by running the old
// expression directly before writing this test. (No float64 at this
// magnitude can be non-integral in the first place -- the gap between
// adjacent representable doubles already exceeds 1 well before 2^63, so
// there is no corresponding "non-integral beyond int64 range" case to test
// against; every float64 this large is already whole.)
func TestValidateIntegerCheckIsExactBeyondInt64Range(t *testing.T) {
	if v := mustValidate(t, `{"type":"integer"}`, `18446744073709551616`); v != nil {
		t.Errorf("a whole number beyond int64 range (2^64) was rejected: %v", v)
	}
}

func TestValidateNestedObjectProperty(t *testing.T) {
	schema := `{
		"type":"object",
		"properties":{
			"order":{"type":"object","properties":{"id":{"type":"string"}}}
		},
		"required":["order"],
		"additionalProperties":true
	}`
	v := mustValidate(t, schema, `{"order":{"id":5}}`)
	if v == nil {
		t.Fatal("expected a violation for a nested type mismatch")
	}
	if v.Field != "order.id" || v.Rule != "type" {
		t.Errorf("got %+v, want Field=order.id Rule=type", v)
	}
}

func TestValidateArrayItems(t *testing.T) {
	schema := `{"type":"array","items":{"type":"integer"}}`
	if v := mustValidate(t, schema, `[1,2,3]`); v != nil {
		t.Errorf("a conforming array was rejected: %v", v)
	}
	v := mustValidate(t, schema, `[1,"two",3]`)
	if v == nil {
		t.Fatal("expected a violation for a wrong-typed array element")
	}
	if v.Field != ".1" || v.Rule != "type" {
		t.Errorf("got %+v, want Field=.1 Rule=type", v)
	}
}

func TestValidateAbsentOptionalPropertyIsFine(t *testing.T) {
	// A non-required property's own schema is never consulted when the
	// property is absent -- matching encoding/json leaving it at its zero
	// value, not refusing the request.
	schema := `{"type":"object","properties":{"nickname":{"type":"string"}},"additionalProperties":true}`
	if v := mustValidate(t, schema, `{}`); v != nil {
		t.Errorf("an absent optional property was rejected: %v", v)
	}
}

func TestValidateCorruptSchemaIsAnErrorNotAViolation(t *testing.T) {
	v, err := Validate(json.RawMessage(`not json`), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected an error for a schema that does not parse as JSON")
	}
	if v != nil {
		t.Errorf("a corrupt schema must not also report a violation: %+v", v)
	}
}

// TestValidateNullableTypeAcceptsNull is cleat#2927 G1, reproducing the
// review's own probe directly against Validate rather than against a real
// HTTP start: a nullable schema (jsonschema.go's nullable(), the form
// FromGoType now emits for *int/[]string/map) must accept the JSON literal
// null, matching encoding/json's own documented behaviour for a pointer,
// slice or map field (reset to nil, no error).
func TestValidateNullableTypeAcceptsNull(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema string
	}{
		{"optional *int", `{"type":["integer","null"]}`},
		{"required []string", `{"type":["array","null"],"items":{"type":"string"}}`},
		{"required map", `{"type":["object","null"],"additionalProperties":{"type":"integer"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if v := mustValidate(t, tc.schema, `null`); v != nil {
				t.Errorf("a nullable schema rejected null: %v", v)
			}
		})
	}
}

// TestValidateNullableTypeStillRejectsOtherMismatches is the negative
// control for the test above: widening a type to admit null must not widen
// it to admit anything else -- a nullable integer schema still has to
// refuse a string.
func TestValidateNullableTypeStillRejectsOtherMismatches(t *testing.T) {
	v := mustValidate(t, `{"type":["integer","null"]}`, `"not a number"`)
	if v == nil {
		t.Fatal("expected a violation for a string against a nullable-integer schema")
	}
	if v.Rule != "type" {
		t.Errorf("got rule %q, want \"type\"", v.Rule)
	}
}

// TestValidateRequiredFieldPresentAsNullIsNotMissing covers the exact shape
// of cleat#2927's review probe end to end: a REQUIRED property (present as
// a key, per EntryPointParamSchema's own required-list rule) whose VALUE is
// the JSON literal null is PRESENT, not absent -- the "required" check
// tests key presence, not non-nullness, and a nullable property schema must
// then accept the null value rather than refusing it as a type mismatch.
func TestValidateRequiredFieldPresentAsNullIsNotMissing(t *testing.T) {
	schema := `{"type":"object","properties":{"s":{"type":["array","null"],"items":{"type":"string"}}},"required":["s"],"additionalProperties":true}`
	if v := mustValidate(t, schema, `{"s":null}`); v != nil {
		t.Errorf("a required-but-nullable field sent as null was rejected: %v", v)
	}
}

// TestValidateListValuedTypeIsNotSilentlyUnvalidated is the regression this
// file exists to prevent from recurring: before cleat#2927's fix,
// validate() read schema["type"] with a single `.(string)` assertion, which
// fails silently for a list and skips ALL validation for that field --
// worse than the bug it was guarding against, because a mismatched type
// that ISN'T null then passes too. Falsified by reverting schemaTypes to a
// bare `.(string)` assertion: this test must fail when that regression is
// present.
func TestValidateListValuedTypeIsNotSilentlyUnvalidated(t *testing.T) {
	v := mustValidate(t, `{"type":["integer","null"]}`, `"definitely not an integer or null"`)
	if v == nil {
		t.Fatal("a list-valued type silently accepted a value matching neither alternative")
	}
}

func TestValidateBase64ContentEncodingIsStringTyped(t *testing.T) {
	// sliceOrArraySchema emits {"type":"string","contentEncoding":"base64"}
	// for a []byte field. contentEncoding itself is not a rule this
	// validator enforces (the binding, encoding/json, already refuses
	// non-base64 text at decode time before this ever runs) -- only the
	// "type":"string" half is checked here.
	schema := `{"type":"string","contentEncoding":"base64"}`
	if v := mustValidate(t, schema, `"aGVsbG8="`); v != nil {
		t.Errorf("a base64 string was rejected: %v", v)
	}
	v := mustValidate(t, schema, `5`)
	if v == nil || v.Rule != "type" {
		t.Errorf("got %+v, want a type violation", v)
	}
}
