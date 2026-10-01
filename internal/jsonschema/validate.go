package jsonschema

import (
	"encoding/json"
	"fmt"
)

// ValidationError names one way an instance failed to satisfy a schema.
// Returned to a caller as a 400 body: Field and Rule let a client branch
// without parsing Message's prose, which is also safe to show verbatim
// since it never echoes anything from the server's own internals.
type ValidationError struct {
	// Field is a dotted breadcrumb to the failing value -- "amount" or
	// "items.2.name" -- or "" for a failure at the input's own root (the
	// input itself is not an object, for instance).
	Field string
	// Rule names the schema keyword that rejected it: "type", "required".
	// additionalProperties is never a rule a VIOLATION names here, because
	// every schema this package validates against sets it permissively (see
	// EntryPointParamSchema's own doc comment) -- there is no case where an
	// extra property is refused.
	Rule string
	// Message is human-readable and safe to put in a 400 body.
	Message string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("%s: %s", e.Rule, e.Message)
	}
	return fmt.Sprintf("%s (%s): %s", e.Field, e.Rule, e.Message)
}

// Validate checks instanceJSON against schemaJSON, both already-encoded JSON
// (schemaJSON is exactly what EntryPointParamSchema/EntryPointResultSchema
// produced and a plugin.Store round-tripped through a database column, not
// a Schema value -- this package's only runtime consumer, cmd/cleat-worker,
// has no go/types to build one from). It returns:
//
//   - (nil, nil) if instanceJSON conforms;
//   - (violation, nil) for a semantic mismatch -- a caller error, suitable
//     for a 400 naming Field and Rule;
//   - (nil, err) if schemaJSON itself does not parse as a JSON object. This
//     is a SEPARATE failure mode from a violation: the schema is the
//     server's own stored artifact, not the caller's input, so a corrupt
//     schema is not the caller's fault and must not be reported as if it
//     mismatched. #1980's own schema is always well-formed JSON written by
//     this package's own Schema type, so this is the "fail closed on
//     corruption" case, not a path either a Go or Python build normally
//     takes.
//
// A nil or empty schemaJSON ({} -- anySchema) matches everything, including
// instanceJSON that is not valid JSON at all: those correspond to "no
// parameters beyond HostCalls" and "the one string-typed parameter is bound
// to input verbatim" in EntryPointParamSchema's doc comment, neither of
// which constrains the wire payload.
func Validate(schemaJSON, instanceJSON json.RawMessage) (*ValidationError, error) {
	if len(schemaJSON) == 0 {
		return nil, nil
	}
	var schema map[string]any
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		return nil, fmt.Errorf("parsing stored schema: %w", err)
	}
	if len(schema) == 0 {
		return nil, nil
	}

	var instance any
	if err := json.Unmarshal(instanceJSON, &instance); err != nil {
		return &ValidationError{Rule: "type", Message: "input is not valid JSON: " + err.Error()}, nil
	}
	return validate(schema, instance, "")
}

func validate(schema map[string]any, instance any, field string) (*ValidationError, error) {
	if len(schema) == 0 {
		return nil, nil
	}

	wantType, _ := schema["type"].(string)
	if wantType != "" {
		if v := checkType(wantType, instance, field); v != nil {
			return v, nil
		}
	}

	switch wantType {
	case "object":
		obj, ok := instance.(map[string]any)
		if !ok {
			// checkType above already reported this when a type was
			// declared; an object schema with no "type" (should not occur
			// in a schema this package emits, but validate defensively
			// rather than panic on the type assertion below) skips the
			// object-only checks.
			return nil, nil
		}
		if reqRaw, ok := schema["required"]; ok {
			req, _ := reqRaw.([]any)
			for _, rAny := range req {
				r, _ := rAny.(string)
				if r == "" {
					continue
				}
				if _, present := obj[r]; !present {
					return &ValidationError{
						Field:   joinField(field, r),
						Rule:    "required",
						Message: fmt.Sprintf("missing required field %q", r),
					}, nil
				}
			}
		}
		if propsRaw, ok := schema["properties"]; ok {
			props, _ := propsRaw.(map[string]any)
			for name, propSchemaRaw := range props {
				val, present := obj[name]
				if !present {
					// Absence of a non-required property is never a
					// violation -- encoding/json's own behaviour (the
					// binding this schema mirrors) leaves an absent field
					// at its zero value rather than refusing the request.
					continue
				}
				propSchema, _ := propSchemaRaw.(map[string]any)
				if v, err := validate(propSchema, val, joinField(field, name)); v != nil || err != nil {
					return v, err
				}
			}
		}
		// additionalProperties is deliberately not enforced here even when
		// present and false: EntryPointParamSchema always sets it to true
		// (every binding this schema describes ignores unrecognised keys),
		// and structSchema never sets it at all. A hand-edited stored
		// schema setting it to false is a case this validator does not need
		// to support -- see this file's own package doc.
	case "array":
		arr, ok := instance.([]any)
		if !ok {
			return nil, nil
		}
		itemsRaw, ok := schema["items"]
		if !ok {
			return nil, nil
		}
		itemSchema, _ := itemsRaw.(map[string]any)
		for i, item := range arr {
			if v, err := validate(itemSchema, item, fmt.Sprintf("%s.%d", field, i)); v != nil || err != nil {
				return v, err
			}
		}
	}
	return nil, nil
}

// checkType reports a "type" violation, honouring the one place this
// package's own emitter asks a string to carry MORE than JSON's own type
// system distinguishes: contentEncoding:"base64" for a []byte field
// (sliceOrArraySchema) and format:"date-time" for a time.Time
// (namedSchema). JSON itself has one numeric type; "integer" vs "number" is
// this package's own distinction (FromGoType's basicSchema), checked here by
// value rather than by the schema's declared type alone, so a JSON number
// with a fractional part is rejected against an integer-typed field the same
// way encoding/json's own Decoder.DisallowUnknownFields sibling,
// UseNumber-based strict decoding, would reject it -- a float sent where a
// Go int is bound does not round-trip, and reporting the mismatch here is
// the whole point of validating before a run is created rather than after
// its first replay.
func checkType(wantType string, instance any, field string) *ValidationError {
	fail := func(got string) *ValidationError {
		return &ValidationError{
			Field:   field,
			Rule:    "type",
			Message: fmt.Sprintf("want %s, got %s", wantType, got),
		}
	}
	switch wantType {
	case "boolean":
		if _, ok := instance.(bool); !ok {
			return fail(jsonTypeName(instance))
		}
	case "string":
		if _, ok := instance.(string); !ok {
			return fail(jsonTypeName(instance))
		}
	case "integer":
		n, ok := instance.(float64)
		if !ok {
			return fail(jsonTypeName(instance))
		}
		if n != float64(int64(n)) {
			return &ValidationError{
				Field:   field,
				Rule:    "type",
				Message: fmt.Sprintf("want integer, got a non-integral number (%v)", n),
			}
		}
	case "number":
		if _, ok := instance.(float64); !ok {
			return fail(jsonTypeName(instance))
		}
	case "array":
		if _, ok := instance.([]any); !ok {
			return fail(jsonTypeName(instance))
		}
	case "object":
		if _, ok := instance.(map[string]any); !ok {
			return fail(jsonTypeName(instance))
		}
	}
	return nil
}

func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}

func joinField(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}
