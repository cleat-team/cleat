package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func testMaster(t *testing.T) []byte {
	t.Helper()
	k, err := MasterKeyFromEnv(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	if err != nil {
		t.Fatalf("master key: %v", err)
	}
	return k
}

func TestASecretRoundTripsUnderItsOwnTenantKey(t *testing.T) {
	s, err := NewSecretStore(nil, "postgres", testMaster(t))
	if err != nil {
		t.Fatalf("NewSecretStore: %v", err)
	}
	const tenant = "11111111-1111-1111-1111-111111111111"
	const value = `sk-live-abc123 with "quotes" and \a backslash`

	sealed, err := s.seal(tenant, value)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if strings.Contains(sealed, "sk-live") {
		t.Fatal("the sealed form contains the plaintext")
	}
	got, err := s.open(tenant, sealed, 1)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != value {
		t.Errorf("got %q, want %q", got, value)
	}
}

// A ciphertext moved into another tenant's row must not open. The tenant id is
// both the HKDF salt and the GCM additional data, so this fails twice over --
// which is the point: neither protection is load-bearing alone.
func TestASecretDoesNotOpenUnderAnotherTenant(t *testing.T) {
	s, _ := NewSecretStore(nil, "postgres", testMaster(t))
	const a = "11111111-1111-1111-1111-111111111111"
	const b = "22222222-2222-2222-2222-222222222222"

	sealed, err := s.seal(a, "tenant-a-key")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := s.open(b, sealed, 1); err == nil {
		t.Fatal("tenant B opened tenant A's ciphertext")
	}
}

func TestASecretDoesNotOpenUnderAnotherMasterKey(t *testing.T) {
	const tenant = "11111111-1111-1111-1111-111111111111"
	s1, _ := NewSecretStore(nil, "postgres", testMaster(t))
	other, _ := MasterKeyFromEnv(base64.StdEncoding.EncodeToString([]byte("fedcba9876543210fedcba9876543210")))
	s2, _ := NewSecretStore(nil, "postgres", other)

	sealed, err := s1.seal(tenant, "value")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := s2.open(tenant, sealed, 1); err == nil {
		t.Fatal("a different deployment key opened the ciphertext")
	}
}

// THE PROPERTY THIS FEATURE EXISTS FOR.
//
// A value spliced into a JSON string must not be able to terminate that string.
// Otherwise a secret is not merely leaked -- it becomes a way to reshape the
// document a plugin receives, which is worse, because the plugin then acts on
// fields the workflow author did not write.
func TestASecretValueCannotBreakOutOfItsJSONString(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"a quote", `a"b`},
		{"a backslash", `a\b`},
		{"a quote then structure", `","admin":true,"x":"`},
		{"a newline", "a\nb"},
		{"a control character", "a\x01b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			escaped := jsonEscape(tc.value)
			doc := `{"k":"` + escaped + `"}`
			var out map[string]string
			if err := jsonUnmarshalStrict(doc, &out); err != nil {
				t.Fatalf("value %q produced invalid JSON %q: %v", tc.value, doc, err)
			}
			if out["k"] != tc.value {
				t.Errorf("round trip changed the value: got %q, want %q", out["k"], tc.value)
			}
			if len(out) != 1 {
				t.Errorf("value %q introduced %d keys; it escaped its string", tc.value, len(out))
			}
		})
	}
}

func TestResolveSecretRefsLeavesInputAloneWhenThereIsNoReference(t *testing.T) {
	s, _ := NewSecretStore(nil, "postgres", testMaster(t))
	const in = `{"model":"claude","prompt":"cost is $50 {not a ref}"}`
	got, err := ResolveSecretRefs(context.Background(), s, "t", in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != in {
		t.Errorf("input was modified: %q", got)
	}
}

// A malformed reference is not a reference. It reaches the plugin as the
// literal text the workflow wrote, rather than being half-interpreted.
func TestAMalformedReferenceIsNotAReference(t *testing.T) {
	s, _ := NewSecretStore(nil, "postgres", testMaster(t))
	for _, in := range []string{
		`{"k":"${secret:}"}`,
		`{"k":"${secret:has space}"}`,
		`{"k":"${secret:has\"quote}"}`,
		`{"k":"${secret}"}`,
	} {
		got, err := ResolveSecretRefs(context.Background(), s, "t", in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", in, err)
			continue
		}
		if got != in {
			t.Errorf("%q was altered to %q", in, got)
		}
	}
}

func TestAReferenceWithNoMasterKeyIsRefusedRatherThanPassedThrough(t *testing.T) {
	s, _ := NewSecretStore(nil, "postgres", nil)
	_, err := ResolveSecretRefs(context.Background(), s, "t", `{"k":"${secret:api}"}`)
	if err == nil {
		t.Fatal("a reference resolved with no master key configured")
	}
	if !strings.Contains(err.Error(), "master key") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

func TestValidSecretName(t *testing.T) {
	for _, ok := range []string{"a", "openai", "OPEN_AI.key-1", strings.Repeat("a", 128)} {
		if !validSecretName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "has space", `has"quote`, "has/slash", strings.Repeat("a", 129)} {
		if validSecretName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

// jsonUnmarshalStrict is a local helper so the test does not depend on how the
// rest of the package happens to decode.
func jsonUnmarshalStrict(doc string, out *map[string]string) error {
	return json.Unmarshal([]byte(doc), out)
}

// TestResolutionEscapesTheValueItSubstitutes drives the substitution END TO END
// rather than testing jsonEscape in isolation.
//
// This exists because the isolated test did not catch the mutation it was
// supposed to: deleting the jsonEscape CALL from the resolver left every test
// green, since the test exercised the function and not its use. A secret value
// able to terminate its JSON string does not merely leak -- it lets whoever set
// it reshape the document the plugin receives.
func TestResolutionEscapesTheValueItSubstitutes(t *testing.T) {
	const hostile = `","injected":"yes`
	out, err := resolveRefsWith(`{"api_key":"${secret:k}","model":"claude"}`,
		func(string) (string, error) { return hostile, nil })
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	var doc map[string]string
	if err := jsonUnmarshalStrict(out, &doc); err != nil {
		t.Fatalf("substitution produced invalid JSON: %v\n  %s", err, out)
	}
	if _, bad := doc["injected"]; bad {
		t.Errorf("the value escaped its string and added a key: %s", out)
	}
	if doc["api_key"] != hostile {
		t.Errorf("api_key = %q, want the literal value %q", doc["api_key"], hostile)
	}
	if len(doc) != 2 {
		t.Errorf("expected exactly api_key and model, got %d keys: %s", len(doc), out)
	}
}

// Every reference in one argument is substituted, not just the first.
func TestResolutionReplacesEveryReference(t *testing.T) {
	out, err := resolveRefsWith(`{"a":"${secret:one}","b":"${secret:two}"}`,
		func(name string) (string, error) { return "V-" + name, nil })
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if strings.Contains(out, "${secret:") {
		t.Errorf("a reference survived: %s", out)
	}
	if !strings.Contains(out, "V-one") || !strings.Contains(out, "V-two") {
		t.Errorf("not every reference was replaced: %s", out)
	}
}
