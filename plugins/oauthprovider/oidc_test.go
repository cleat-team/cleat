package oauthprovider

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// A fake OIDC provider: a discovery document, a JWKS, and the ability to mint
// tokens the way a real IdP would -- and the ways a hostile one would.
type fakeIDP struct {
	t          *testing.T
	srv        *httptest.Server
	key        *rsa.PrivateKey
	kid        string
	issuerOver string // when set, the discovery document lies about its issuer
	jwksCalls  int
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	f := &fakeIDP{t: t, key: key, kid: "test-key-1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		iss := f.srv.URL
		if f.issuerOver != "" {
			iss = f.issuerOver
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 iss,
			"authorization_endpoint": f.srv.URL + "/authorize",
			"token_endpoint":         f.srv.URL + "/token",
			"userinfo_endpoint":      f.srv.URL + "/userinfo",
			"jwks_uri":               f.srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		f.jwksCalls++
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{jwkFor(&f.key.PublicKey, f.kid)}})
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func jwkFor(pub *rsa.PublicKey, kid string) map[string]any {
	return map[string]any{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

// sign mints an ID token. Every field a test might want to corrupt is a
// parameter, so a negative case differs from the positive one by exactly the
// thing it is testing.
func (f *fakeIDP) sign(claims jwt.MapClaims, method jwt.SigningMethod, key any) string {
	f.t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	tok.Header["kid"] = f.kid
	s, err := tok.SignedString(key)
	if err != nil {
		f.t.Fatalf("sign: %v", err)
	}
	return s
}

func (f *fakeIDP) goodClaims(aud, nonce string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":   f.srv.URL,
		"aud":   aud,
		"sub":   "user-1",
		"email": "user@example.com",
		"nonce": nonce,
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Unix(),
	}
}

// pluginFor returns a Plugin whose HTTP client trusts the fake IdP's TLS cert.
func pluginFor(t *testing.T, f *fakeIDP) *Plugin {
	t.Helper()
	return &Plugin{
		logger:     slog.New(slog.NewTextHandler(io_Discard{}, nil)),
		httpClient: f.srv.Client(),
	}
}

type io_Discard struct{}

func (io_Discard) Write(p []byte) (int, error) { return len(p), nil }

// ---- discovery ----

func TestDiscoveryRejectsADocumentThatClaimsADifferentIssuer(t *testing.T) {
	f := newFakeIDP(t)
	f.issuerOver = "https://evil.example"
	p := pluginFor(t, f)

	_, _, err := p.discover(context.Background(), f.srv.URL)
	if err == nil {
		t.Fatal("a discovery document declaring someone else's issuer was accepted; " +
			"that lets whatever answers for the issuer host redirect token and userinfo to a third party")
	}
	if !strings.Contains(err.Error(), "declares issuer") {
		t.Fatalf("rejected for the wrong reason: %v", err)
	}
}

func TestDiscoveryRequiresHTTPS(t *testing.T) {
	p := pluginFor(t, newFakeIDP(t))
	if _, _, err := p.discover(context.Background(), "http://idp.example"); err == nil {
		t.Fatal("a plain-http issuer was accepted; everything cleat trusts about the IdP arrives over that connection")
	}
}

// The well-known path is APPENDED to the issuer, path included. Resolving it as
// a relative reference would discover the wrong provider for a multi-tenant IdP
// that gives each tenant its own path.
func TestDiscoveryURLKeepsTheIssuerPath(t *testing.T) {
	got, err := discoveryURL("https://idp.example/tenant1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "https://idp.example/tenant1/.well-known/openid-configuration"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDiscoveryIsCachedAndNotRefetchedPerLogin(t *testing.T) {
	f := newFakeIDP(t)
	p := pluginFor(t, f)
	for i := 0; i < 3; i++ {
		if _, _, err := p.discover(context.Background(), f.srv.URL); err != nil {
			t.Fatalf("discover %d: %v", i, err)
		}
	}
	if f.jwksCalls != 1 {
		t.Fatalf("JWKS fetched %d times for 3 logins, want 1", f.jwksCalls)
	}
}

// ---- ID token validation ----

// The positive control. Every negative case below differs from THIS by one
// thing, so a negative that passes for an unrelated reason shows up as this
// test failing too.
func TestAValidIDTokenIsAccepted(t *testing.T) {
	f := newFakeIDP(t)
	p := pluginFor(t, f)

	raw := f.sign(f.goodClaims("client-1", "nonce-1"), jwt.SigningMethodRS256, f.key)
	claims, err := p.validateIDToken(context.Background(), raw, f.srv.URL, "client-1", "nonce-1")
	if err != nil {
		t.Fatalf("a correctly signed token was rejected: %v", err)
	}
	if claims.Email != "user@example.com" {
		t.Fatalf("email = %q", claims.Email)
	}
}

func TestAnUnsignedIDTokenIsRejected(t *testing.T) {
	f := newFakeIDP(t)
	p := pluginFor(t, f)

	// alg=none: the classic. Without an algorithm allowlist this parses as a
	// perfectly valid token carrying whatever the attacker wrote.
	raw := f.sign(f.goodClaims("client-1", "nonce-1"), jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType)
	_, err := p.validateIDToken(context.Background(), raw, f.srv.URL, "client-1", "nonce-1")
	if err == nil {
		t.Fatal("alg=none was accepted: anyone can mint an identity")
	}
	// ASSERTING THE REASON, not just the rejection, and that distinction was
	// found by sabotage rather than by reading. With jwt.WithValidMethods
	// deleted this test still PASSED, because golang-jwt has its own refusal
	// for alg=none -- so "it was rejected" said nothing about our allowlist.
	// Requiring the allowlist's own wording makes the test fail when the
	// allowlist goes away, which is the property it is here to protect.
	if !strings.Contains(err.Error(), "signing method none is invalid") {
		t.Fatalf("rejected, but not by the algorithm allowlist: %v", err)
	}
}

func TestAnHMACIDTokenSignedWithThePublicKeyIsRejected(t *testing.T) {
	f := newFakeIDP(t)
	p := pluginFor(t, f)

	// Algorithm confusion. The RSA public key is published in the JWKS, so if
	// HS256 were accepted anyone who can read the JWKS -- everyone -- could
	// sign a token with it and be believed.
	pubDER := f.key.PublicKey.N.Bytes()
	raw := f.sign(f.goodClaims("client-1", "nonce-1"), jwt.SigningMethodHS256, pubDER)
	_, err := p.validateIDToken(context.Background(), raw, f.srv.URL, "client-1", "nonce-1")
	if err == nil {
		t.Fatal("an HMAC token keyed with the public key was accepted: the JWKS is public, so this is a full bypass")
	}
	// Same lesson as the alg=none case, and here it matters MORE. With the
	// allowlist removed this token is still rejected -- but only because the
	// key in hand is an *rsa.PublicKey and HMAC wants []byte. That is a type
	// accident, not a defence: it would stop protecting us the moment a key
	// set yielded a byte slice. (parseJWK refusing `oct` keys is the other
	// half of keeping that true.) Assert the allowlist's own reason.
	if !strings.Contains(err.Error(), "signing method HS256 is invalid") {
		t.Fatalf("rejected, but only by a key-type mismatch rather than the algorithm allowlist: %v", err)
	}
}

func TestAnIDTokenSignedByADifferentKeyIsRejected(t *testing.T) {
	f := newFakeIDP(t)
	p := pluginFor(t, f)

	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	raw := f.sign(f.goodClaims("client-1", "nonce-1"), jwt.SigningMethodRS256, other)
	if _, err := p.validateIDToken(context.Background(), raw, f.srv.URL, "client-1", "nonce-1"); err == nil {
		t.Fatal("a token signed by a key the issuer does not publish was accepted")
	}
}

func TestAnIDTokenForAnotherAudienceIsRejected(t *testing.T) {
	f := newFakeIDP(t)
	p := pluginFor(t, f)

	// A token minted for a DIFFERENT client of the same IdP. Without the aud
	// check, any relying party sharing this issuer could replay its users'
	// tokens into cleat.
	raw := f.sign(f.goodClaims("someone-elses-client", "nonce-1"), jwt.SigningMethodRS256, f.key)
	if _, err := p.validateIDToken(context.Background(), raw, f.srv.URL, "client-1", "nonce-1"); err == nil {
		t.Fatal("a token issued to another audience was accepted")
	}
}

func TestAnExpiredIDTokenIsRejected(t *testing.T) {
	f := newFakeIDP(t)
	p := pluginFor(t, f)

	c := f.goodClaims("client-1", "nonce-1")
	c["exp"] = time.Now().Add(-time.Minute).Unix()
	raw := f.sign(c, jwt.SigningMethodRS256, f.key)
	if _, err := p.validateIDToken(context.Background(), raw, f.srv.URL, "client-1", "nonce-1"); err == nil {
		t.Fatal("an expired token was accepted")
	}
}

func TestAnIDTokenWithoutAnExpiryIsRejected(t *testing.T) {
	f := newFakeIDP(t)
	p := pluginFor(t, f)

	c := f.goodClaims("client-1", "nonce-1")
	delete(c, "exp")
	raw := f.sign(c, jwt.SigningMethodRS256, f.key)
	if _, err := p.validateIDToken(context.Background(), raw, f.srv.URL, "client-1", "nonce-1"); err == nil {
		t.Fatal("a token with no exp was accepted, so it never expires")
	}
}

func TestAnIDTokenCarryingAnotherLoginsNonceIsRejected(t *testing.T) {
	f := newFakeIDP(t)
	p := pluginFor(t, f)

	// Replay: a token that is genuine, unexpired, correctly signed and for the
	// right audience, but was issued for a DIFFERENT login.
	raw := f.sign(f.goodClaims("client-1", "nonce-from-another-flow"), jwt.SigningMethodRS256, f.key)
	if _, err := p.validateIDToken(context.Background(), raw, f.srv.URL, "client-1", "nonce-1"); err == nil {
		t.Fatal("a token minted for another login was accepted: that is a replay")
	}
}

func TestAnIDTokenFromAnotherIssuerIsRejected(t *testing.T) {
	f := newFakeIDP(t)
	p := pluginFor(t, f)

	c := f.goodClaims("client-1", "nonce-1")
	c["iss"] = "https://evil.example"
	raw := f.sign(c, jwt.SigningMethodRS256, f.key)
	if _, err := p.validateIDToken(context.Background(), raw, f.srv.URL, "client-1", "nonce-1"); err == nil {
		t.Fatal("a token claiming a different issuer was accepted")
	}
}

// A rotated signing key must recover without a restart, and must do so without
// weakening anything: the retry refetches the JWKS and verifies again, it does
// not skip verification.
func TestARotatedSigningKeyRecoversWithinTheTTL(t *testing.T) {
	f := newFakeIDP(t)
	p := pluginFor(t, f)

	if _, _, err := p.discover(context.Background(), f.srv.URL); err != nil {
		t.Fatalf("prime cache: %v", err)
	}

	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	f.key = newKey // the IdP now serves, and signs with, a different key

	raw := f.sign(f.goodClaims("client-1", "nonce-1"), jwt.SigningMethodRS256, newKey)
	if _, err := p.validateIDToken(context.Background(), raw, f.srv.URL, "client-1", "nonce-1"); err != nil {
		t.Fatalf("a token signed with the issuer's rotated key was rejected: %v", err)
	}
}

// ---- JWK parsing ----

func TestAnUndersizedRSAKeyIsRefused(t *testing.T) {
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	raw, _ := json.Marshal(jwkFor(&small.PublicKey, "small"))
	if _, _, err := parseJWK(raw); err == nil {
		t.Fatal("a 1024-bit RSA key was accepted; it is forgeable with commodity hardware")
	}
}

func TestASymmetricJWKIsRefused(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"kty": "oct", "kid": "shared", "k": "c2VjcmV0"})
	if _, _, err := parseJWK(raw); err == nil {
		t.Fatal("an oct (shared secret) key was accepted from a PUBLISHED key set")
	}
}

func TestAnEncryptionOnlyJWKIsRefused(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	j := jwkFor(&key.PublicKey, "enc-key")
	j["use"] = "enc"
	raw, _ := json.Marshal(j)
	if _, _, err := parseJWK(raw); err == nil {
		t.Fatal("a use=enc key was accepted for signature verification")
	}
}
