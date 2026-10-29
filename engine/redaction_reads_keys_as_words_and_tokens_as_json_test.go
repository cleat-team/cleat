package engine

import (
	"encoding/base64"
	"strings"
	"testing"
)

// cleat#3581: redaction rewrote the data a workflow consumes. These pin the two
// matchers that did it -- the key test (a substring: max_tokens, token_usage)
// and the JWT test (any three dotted segments: 1.2.3, report.final.pdf) -- from
// BOTH sides. A table that only said "ordinary data survives" would pass a
// matcher that redacted nothing, so every case that must survive has a sibling
// that must not.

func b64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// aRealJWT is a structurally real token: a JSON header carrying alg, JSON
// claims, a signature segment.
func aRealJWT() string {
	return b64url(`{"alg":"HS256","typ":"JWT"}`) + "." + b64url(`{"sub":"1234567890","name":"x"}`) + "." + b64url("signature-bytes")
}

func TestOrdinaryDataIsNotRedacted(t *testing.T) {
	// Every row is measured in cleat#3581 as rewritten at v0.5.0.
	for _, in := range []string{
		`{"version":"1.2.3"}`,
		`{"name":"a.b.c"}`,
		`{"note":"see v1.2.3 notes","file":"report.final.pdf","host":"api.example.com"}`,
		`{"max_tokens":5}`,
		`{"token_usage":{"input_tokens":3,"output_tokens":4}}`,
		`{"usage":{"input_tokens":3,"output_tokens":4}}`,
		`{"tokenizer":"cl100k","token_count":12,"tokens_per_minute":900}`,
		`{"password_policy":"strict","credential_type":"oauth","secret_name":"db","token_expires_at":"2026-10-29"}`,
		`"1.2.3"`,
	} {
		if got := Redact(in); got != in {
			t.Errorf("Redact rewrote ordinary data\n  in : %s\n  out: %s", in, got)
		}
		if got := RedactOnRead(in); got != in {
			t.Errorf("RedactOnRead rewrote ordinary data\n  in : %s\n  out: %s", in, got)
		}
	}
	// A bare string that is not JSON takes the other branch of Redact.
	for _, in := range []string{"1.2.3", "a.b.c", "report.final.pdf"} {
		if got := Redact(in); got != in {
			t.Errorf("Redact rewrote the bare string %q to %q", in, got)
		}
	}
}

func TestSecretsAreStillRedacted(t *testing.T) {
	jwt := aRealJWT()
	for _, tc := range []struct{ name, in string }{
		{"a JWT as a value", `{"note":"` + jwt + `"}`},
		{"a JWT nested in an array", `{"xs":["ok","` + jwt + `"]}`},
		{"a padded JWT", `{"v":"` + b64url(`{"alg":"none"}`) + "=." + b64url(`{"a":1}`) + "=.c2ln" + `"}`},
		{"token", `{"token":"abc"}`},
		{"access_token", `{"access_token":"abc"}`},
		{"idToken", `{"idToken":"abc"}`},
		{"X-Auth-Token", `{"X-Auth-Token":"abc"}`},
		{"accesstoken fused", `{"accesstoken":"abc"}`},
		{"refresh_tokens, a list of secrets", `{"refresh_tokens":["a","b"]}`},
		{"max_access_tokens, a list that carries a count word", `{"max_access_tokens":["a"]}`},
		{"tokens_per_user, not a time rate", `{"tokens_per_user":"abc"}`},
		{"accesstokens fused", `{"accesstokens":["a"]}`},
		{"tokenvalue fused", `{"tokenvalue":"abc"}`},
		{"TOKENVALUE fused", `{"TOKENVALUE":"abc"}`},
		{"tokenid fused", `{"tokenid":"abc"}`},
		{"api_key", `{"api_key":"abc"}`},
		{"apiKey", `{"apiKey":"abc"}`},
		{"APIKey", `{"APIKey":"abc"}`},
		{"x-api-key", `{"x-api-key":"abc"}`},
		{"client_secret", `{"client_secret":"abc"}`},
		{"password", `{"password":"abc"}`},
		{"passwordHash", `{"passwordHash":"abc"}`},
		{"DB_PASSWORD", `{"DB_PASSWORD":"abc"}`},
		{"credentials", `{"credentials":{"user":"u"}}`},
		{"Authorization", `{"Authorization":"Bearer abc"}`},
		{"a number under a sensitive key", `{"password":1234}`},
	} {
		if got := Redact(tc.in); got == tc.in || !strings.Contains(got, `[REDACTED]`) {
			t.Errorf("%s: not redacted\n  in : %s\n  out: %s", tc.name, tc.in, got)
		}
	}
	// A bare JWT that is not JSON takes the other branch of Redact.
	if got := Redact(jwt); got != redactedPlain {
		t.Errorf("a bare JWT was not redacted: %q", got)
	}
}

func TestIsSensitiveFieldReadsWords(t *testing.T) {
	for name, want := range map[string]bool{
		// the words of a key
		"token": true, "Token": true, "access_token": true, "refresh-token": true, "idToken": true,
		"csrf.token": true, "AuthToken": true, "authtoken": true, "token2": true,
		"secret": true, "client_secret": true, "clientsecret": true, "secretKey": true,
		"password": true, "passwords": true, "dbpassword": true,
		"credential": true, "credentials": true,
		"authorization": true, "Authorization": true,
		"api_key": true, "api-key": true, "apiKey": true, "APIKey": true, "apikey": true, "api_keys": true,
		"accesstokens": true, "authtokens": true, "refreshtokens": true, "apitokens": true,
		"githubtokens": true, "bearertokens": true, "tokenvalue": true, "tokenstring": true,
		"tokendata": true, "tokenhash": true, "tokenid": true, "tokenkey": true, "tokenlist": true,
		"TOKENVALUE": true, "token_id": true, "max_access_tokens": true, "tokens_per_user": true,
		"tokens": true, "refresh_tokens": true, "access_tokens": true, "refreshTokens": true, "accessTokens": true,
		// a count of LLM tokens, and what merely describes a secret
		"max_tokens": false, "input_tokens": false, "output_tokens": false, "total_tokens": false,
		"maxTokens": false, "tokens_per_minute": false, "tokens_used": false,
		"cached_tokens": false, "used_tokens": false, "remaining_tokens": false, "tokens_remaining": false,
		"maxtokens": false, "inputtokens": false, "completion_tokens_details": false,
		"tokenization": false, "tokenizers": false, "tokenizer_name": false,
		"token_usage": false, "token_count": false, "token_limit": false, "token_type": false,
		"token_expires_at": false, "tokenizer": false,
		"password_policy": false, "password_min_length": false, "credential_type": false,
		"secret_name": false, "api_key_name": false,
		"passwordPolicy": false, "credentialType": false, "secretName": false, "tokenUsage": false,
		// unrelated
		"version": false, "name": false, "usage": false, "type": false, "key": false, "api": false,
		"": false,
	} {
		if got := isSensitiveField(name); got != want {
			t.Errorf("isSensitiveField(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestLooksLikeJWTWantsJSONSegments(t *testing.T) {
	hdr := b64url(`{"alg":"RS256"}`)
	claims := b64url(`{"iss":"x"}`)
	for in, want := range map[string]bool{
		aRealJWT():                      true,
		hdr + "." + claims + ".c2ln":    true,
		hdr + "=." + claims + "=.c2ln=": true, // padded
		"1.2.3":                         false,
		"a.b.c":                         false,
		"report.final.pdf":              false,
		"api.example.com":               false,
		"eyJ.eyJ.sig":                   false, // looks the part, does not decode
		b64url(`{"typ":"JWT"}`) + "." + claims + ".c2ln": false, // no alg
		b64url(`["alg"]`) + "." + claims + ".c2ln":       false, // not an object
		hdr + "." + b64url(`not json`) + ".c2ln":         false,
		hdr + "." + claims + ".":                         false, // empty signature
		hdr + "." + claims:                               false, // two segments
		hdr + "." + claims + ".c2ln.extra":               false, // four
		"":                                               false,
		hdr + "." + claims + ".sig nature":               false,
	} {
		if got := looksLikeJWT(in); got != want {
			t.Errorf("looksLikeJWT(%q) = %v, want %v", in, got, want)
		}
	}
}
