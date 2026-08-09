package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const probeSecret = "shhh-super-secret-value"

// TestSecret_NeverMarshalsTheRealValue is the property the type exists for.
//
// It is asserted through several shapes deliberately: the defect being fixed
// was a struct field marshaled by a handler, and a type that redacted only when
// marshaled directly would not have caught it.
func TestSecret_NeverMarshalsTheRealValue(t *testing.T) {
	type config struct {
		Name   string `json:"name"`
		Secret Secret `json:"secret"`
	}

	for _, tc := range []struct {
		name string
		v    any
	}{
		{"the value itself", Secret(probeSecret)},
		{"a struct field", config{Name: "n", Secret: Secret(probeSecret)}},
		{"a pointer to a struct", &config{Name: "n", Secret: Secret(probeSecret)}},
		{"inside a slice, which is what a list endpoint returns", []config{{Secret: Secret(probeSecret)}}},
		{"inside a map", map[string]config{"a": {Secret: Secret(probeSecret)}}},
		{"a bare map value, bypassing the struct entirely", map[string]any{"secret": Secret(probeSecret)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(b), probeSecret) {
				t.Fatalf("the real value reached the JSON: %s", b)
			}
			if !strings.Contains(string(b), RedactedPlaceholder) {
				t.Errorf("no placeholder in the output: %s", b)
			}
		})
	}
}

// TestSecret_DoesNotLeakThroughFormatting covers the other way a credential
// escapes: a log line or an error. The value most likely to end up in an error
// message is the one that just failed to authenticate.
func TestSecret_DoesNotLeakThroughFormatting(t *testing.T) {
	s := Secret(probeSecret)
	for _, got := range []string{
		fmt.Sprintf("%s", s),
		fmt.Sprintf("%v", s),
		fmt.Sprintf("%q", s),
		fmt.Sprintf("%#v", s), // ignores Stringer, which is why Format exists
		fmt.Sprint(s),
		fmt.Sprintf("%s", []Secret{s}),
		fmt.Sprintf("%v", struct{ S Secret }{s}),
		fmt.Errorf("auth failed with %v", s).Error(),
	} {
		if strings.Contains(got, probeSecret) {
			t.Errorf("the real value leaked through formatting: %s", got)
		}
	}
}

// TestSecret_RoundTripsThroughTheDatabaseAndRequests is the other half. A type
// that redacted everywhere would be safe and useless: the credential has to
// arrive from a request and reach the database, or the plugin cannot work.
func TestSecret_RoundTripsThroughTheDatabaseAndRequests(t *testing.T) {
	var s Secret
	if err := json.Unmarshal([]byte(`"`+probeSecret+`"`), &s); err != nil {
		t.Fatalf("unmarshal a real value: %v", err)
	}
	if s.Reveal() != probeSecret {
		t.Errorf("Reveal() = %q, want the real value", s.Reveal())
	}

	v, err := s.Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if v != probeSecret {
		t.Errorf("Value() = %v, want the real value -- the database stores the credential", v)
	}

	for _, src := range []any{probeSecret, []byte(probeSecret)} {
		var back Secret
		if err := back.Scan(src); err != nil {
			t.Fatalf("Scan(%T): %v", src, err)
		}
		if back.Reveal() != probeSecret {
			t.Errorf("Scan(%T) produced %q", src, back.Reveal())
		}
	}
	var null Secret
	if err := null.Scan(nil); err != nil || null != "" {
		t.Errorf("Scan(nil) = %q, %v; want empty and no error", null, err)
	}
}

// TestSecret_RefusesToStoreThePlaceholder covers the round-trip data loss a
// redacting read invites: a client reads a config, edits an unrelated field,
// and PUTs the whole object back. Without this, the stored credential is
// silently replaced by the literal "[redacted]" and the plugin stops working
// with no error anywhere.
func TestSecret_RefusesToStoreThePlaceholder(t *testing.T) {
	var s Secret
	err := json.Unmarshal([]byte(`"`+RedactedPlaceholder+`"`), &s)
	if err == nil {
		t.Fatal("unmarshalling the placeholder was accepted; a read-modify-write cycle would " +
			"overwrite the real credential with it")
	}
	if !strings.Contains(err.Error(), "Omit the field") {
		t.Errorf("error does not tell the caller what to do instead: %v", err)
	}
	if s != "" {
		t.Errorf("the value was written despite the error: %q", s.Reveal())
	}
}
