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
