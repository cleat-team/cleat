package oauthprovider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		w.Write([]byte(`{"email":"user@example.com"}`))
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

// TestMissingClientSecretRefusesLogin is the negative control for the test
// above: an oauth_config row whose provider has no matching entry in tenant
// secrets must refuse rather than proceed with an empty client_secret, which
// would silently send an unauthenticated-looking token exchange to the IdP.
// Falsified by construction: AddOAuthConfig always seeds a secret, so this
// seeds the config row directly (bypassing it) with nothing behind
// OAuthClientSecretName("google") for this tenant.
func TestMissingClientSecretRefusesLogin(t *testing.T) {
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

	if rec.Code == http.StatusFound {
		t.Fatalf("login redirected (302) with no client secret configured; want a failure status, got a working authorize redirect")
	}
	if rec.Code < 500 {
		t.Errorf("expected a server-side failure (secret lookup error), got %d: %s", rec.Code, rec.Body.String())
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
		w.Write([]byte(`{"email":"user@example.com"}`))
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
