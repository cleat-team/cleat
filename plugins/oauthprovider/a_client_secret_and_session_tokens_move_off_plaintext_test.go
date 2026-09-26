package oauthprovider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestClientSecretFlowsFromTenantSecretsIntoTheTokenExchange proves
// getConfig's client secret genuinely comes from plugin.Secrets and reaches
// the token exchange request, not merely that the callback returns 200 --
// TestOA_Callback_Success already showed that, but its mock /token handler
// never inspects the request body, so it would pass identically if
// client_secret were sent empty. cleat#1992.
func TestClientSecretFlowsFromTenantSecretsIntoTheTokenExchange(t *testing.T) {
	store := newFakeDBStore()
	p, handler := setupTestPlugin(t, store)

	var gotClientSecret string
	mockMux := http.NewServeMux()
	mockMux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token exchange form: %v", err)
		}
		gotClientSecret = r.FormValue("client_secret")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"mock-at","expires_in":3600}`))
	})
	mockMux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"email":"user@example.com","email_verified":true}`))
	})
	mockSrv := httptest.NewServer(mockMux)
	defer mockSrv.Close()

	origEndpoints := endpoints
	endpoints = map[string]providerEndpoints{
		"google": {tokenURL: mockSrv.URL + "/token", userinfoURL: mockSrv.URL + "/userinfo"},
	}
	defer func() { endpoints = origEndpoints }()
	p.httpClient = mockSrv.Client()

	const wantSecret = "the-real-client-secret-9f2c"
	sessionID := uuid.MustParse("00000000-0000-0000-0000-000000000910")
	store.mu.Lock()
	store.sessions[sessionID] = &fakeSession{
		ID: sessionID, TenantID: testTenantID, Provider: "google",
		State: "secret-flow-state", CodeVerifier: "verifier",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	store.mu.Unlock()
	store.AddOAuthConfig(testTenantID, "google", "cid", wantSecret, "http://localhost/cb", "", true)
	// The login has to be admitted by the allowlist before this test can observe
	// the client_secret at all: since cleat#2371 the check is unconditional, so
	// an empty allowlist refuses with 403 and the token exchange never runs.
	store.AddAllowedIdentity(testTenantID, "google", identityTypeEmail, "user@example.com")

	req := httptest.NewRequest("GET", "/oauth/google/callback?code=x&state=secret-flow-state", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotClientSecret != wantSecret {
		t.Errorf("token exchange client_secret = %q, want %q (the value seeded into tenant secrets, "+
			"not oauth_config -- if this reads empty, getConfig stopped calling Secrets.Get)",
			gotClientSecret, wantSecret)
	}
}

// TestMissingClientSecretDoesNotHandTheBrowserToTheProvider is the negative
// control for the test above: an oauth_config row whose provider has no
// matching entry in tenant secrets must not proceed with an empty
// client_secret, which would silently send an unauthenticated-looking token
// exchange to the IdP. Falsified by construction: AddOAuthConfig always seeds
// a secret, so this seeds the config row directly (bypassing it) with nothing
// behind OAuthClientSecretName("google") for this tenant.
//
// Until cleat#2368 this asserted a FAILURE STATUS, and that assertion was the
// thing #2368 removed: a status separating "configured but broken" from "never
// configured" is the enumeration oracle an unauthenticated caller reads with
// ?tenant_id=<guess>. The property worth keeping is narrower and survives the
// change -- a login that cannot succeed must not hand the browser to the
// provider -- so the assertion moved to the Location, which is where that
// distinction still legitimately lives.
func TestMissingClientSecretDoesNotHandTheBrowserToTheProvider(t *testing.T) {
	store := newFakeDBStore()
	_, handler := setupTestPlugin(t, store)

	store.mu.Lock()
	store.configs[testTenantID.String()+":google"] = &oauthConfigRow{
		TenantID: testTenantID, Provider: "google", ClientID: "cid",
		RedirectURL: "http://localhost/cb", Enabled: true,
	}
	store.mu.Unlock()
	// Deliberately NOT calling store.secrets.Seed for this (tenant, name).

	req := httptest.NewRequest("GET", "/oauth/google/login?tenant_id="+testTenantID.String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// cleat#2368: the uniform 302, not a failure status.
	if rec.Code != http.StatusFound {
		t.Fatalf("expected the uniform 302, got %d: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if strings.Contains(loc, "accounts.google.com") || strings.Contains(loc, "client_id=") {
		t.Fatalf("a login with no client secret behind it was handed to the provider anyway: Location %q", loc)
	}
	if loc != "/" {
		t.Errorf("want the harmless same-origin root, got Location %q", loc)
	}
}

// TestMissingClientSecretDoesNotLeakToAnUnauthenticatedCaller is a negative
// control on the response BODY, not just the status. handleLogin accepts
// ?tenant_id= from anyone -- there is no session yet, that is the whole
// point of a login route -- so an operator-actionable message distinguishing
// "this provider is configured but has no secret" from "this provider was
// never configured at all" would hand an anonymous caller an enumeration
// oracle plus detail about cleat's own tooling (cleatctl, the secret naming
// scheme). cleat-review flagged this on cleat#2295 before it shipped.
//
// Asserts the two cases are byte-identical at the HTTP layer: a config row
// that exists but has no secret behind it, versus a tenant/provider with no
// config row at all. If a future change reintroduces
// writeConfigLookupError-style branching, this fails by construction rather
// than requiring someone to notice a response body got more specific.
func TestMissingClientSecretDoesNotLeakToAnUnauthenticatedCaller(t *testing.T) {
	configuredNoSecret := newFakeDBStore()
	configuredNoSecret.mu.Lock()
	configuredNoSecret.configs[testTenantID.String()+":google"] = &oauthConfigRow{
		TenantID: testTenantID, Provider: "google", ClientID: "cid",
		RedirectURL: "http://localhost/cb", Enabled: true,
	}
	configuredNoSecret.mu.Unlock()
	_, handlerA := setupTestPlugin(t, configuredNoSecret)

	neverConfigured := newFakeDBStore()
	_, handlerB := setupTestPlugin(t, neverConfigured)

	req := func() *http.Request {
		return httptest.NewRequest("GET", "/oauth/google/login?tenant_id="+testTenantID.String(), nil)
	}

	recA := httptest.NewRecorder()
	handlerA.ServeHTTP(recA, req())
	recB := httptest.NewRecorder()
	handlerB.ServeHTTP(recB, req())

	if recA.Code != recB.Code {
		t.Errorf("status differs: configured-no-secret=%d, never-configured=%d -- an unauthenticated "+
			"caller can distinguish the two cases", recA.Code, recB.Code)
	}
	if recA.Body.String() != recB.Body.String() {
		t.Errorf("response body differs:\n  configured-no-secret:  %s\n  never-configured:      %s\n"+
			"-- an unauthenticated caller can tell these apart", recA.Body.String(), recB.Body.String())
	}
	if strings.Contains(recA.Body.String(), "cleatctl") || strings.Contains(recA.Body.String(), OAuthClientSecretName("google")) {
		t.Errorf("response body names cleat's internal tooling to an unauthenticated caller: %s", recA.Body.String())
	}
}

// TestSessionAccessRefreshTokensAreNotPersisted proves finishLogin stores
// NONE of session_token/access_token/refresh_token -- not plaintext, not
// sealed. cleat#2295/#2296.
//
// An earlier version of this change sealed the three via plugin.Payloads
// (cleat#1992). cleat-review found that broke every login on a deployment
// with no --encryption-key-file set, which was every deployment shipped so
// far: a nil Payloads makes Seal fail closed, so finishLogin returned 500
// after the caller had already completed the IdP round trip. Since nothing
// reads these three back (session lookup is by token_hash, not
// session_token; access_token/refresh_token have no read path at all), the
// owner's fix was to stop storing them, not to fix the seal.
//
// setupTestPlugin no longer wires ANY plugin.Payloads into the Plugin under
// test -- so if finishLogin (or anything else in this package) called
// p.payloads again, every test in this file would nil-pointer-panic, not
// just this one. That is the regression guard cleat-review asked for: a
// real callback path exercised with no encryption key configured at all.
func TestSessionAccessRefreshTokensAreNotPersisted(t *testing.T) {
	store := newFakeDBStore()
	p, handler := setupTestPlugin(t, store)

	const plainAccessToken = "provider-issued-access-token-xyz"
	const plainRefreshToken = "provider-issued-refresh-token-abc"

	mockMux := http.NewServeMux()
	mockMux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"` + plainAccessToken + `","refresh_token":"` + plainRefreshToken + `","expires_in":3600}`))
	})
	mockMux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"email":"user@example.com","email_verified":true}`))
	})
	mockSrv := httptest.NewServer(mockMux)
	defer mockSrv.Close()

	origEndpoints := endpoints
	endpoints = map[string]providerEndpoints{
		"google": {tokenURL: mockSrv.URL + "/token", userinfoURL: mockSrv.URL + "/userinfo"},
	}
	defer func() { endpoints = origEndpoints }()
	p.httpClient = mockSrv.Client()

	sessionID := uuid.MustParse("00000000-0000-0000-0000-000000000920")
	store.mu.Lock()
	store.sessions[sessionID] = &fakeSession{
		ID: sessionID, TenantID: testTenantID, Provider: "google",
		State: "not-persisted-state", CodeVerifier: "verifier",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	store.mu.Unlock()
	store.AddOAuthConfig(testTenantID, "google", "cid", "cs", "http://localhost/cb", "", true)
	// Same precondition as the sibling above: cleat#2371 made the allowlist check
	// unconditional, so a callback that expects 200 has to name who is admitted.
	store.AddAllowedIdentity(testTenantID, "google", identityTypeEmail, "user@example.com")

	req := httptest.NewRequest("GET", "/oauth/google/callback?code=x&state=not-persisted-state", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var callbackResp struct {
		SessionToken string `json:"session_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &callbackResp); err != nil {
		t.Fatalf("decode callback response: %v", err)
	}
	if callbackResp.SessionToken == "" {
		t.Fatal("callback response carries no session_token -- the caller has no way to authenticate")
	}

	store.mu.RLock()
	s, ok := store.sessions[sessionID]
	store.mu.RUnlock()
	if !ok {
		t.Fatalf("session %s vanished after finishLogin", sessionID)
	}

	if s.AccessTokenAtRest != nil {
		t.Errorf("access_token at rest = %#v, want nil -- it must not be stored in any form", s.AccessTokenAtRest)
	}
	if s.RefreshTokenAtRest != nil {
		t.Errorf("refresh_token at rest = %#v, want nil -- it must not be stored in any form", s.RefreshTokenAtRest)
	}
	if s.SessionTokenAtRest != nil {
		t.Errorf("session_token at rest = %#v, want nil -- it must not be stored in any form", s.SessionTokenAtRest)
	}

	// token_hash IS how a session is looked up (middleware/extractSession),
	// so it must still be set even though session_token itself is not.
	if _, ok := s.TokenHash.(string); !ok {
		t.Errorf("token_hash = %#v, want a string -- session lookup depends on it", s.TokenHash)
	}
}
