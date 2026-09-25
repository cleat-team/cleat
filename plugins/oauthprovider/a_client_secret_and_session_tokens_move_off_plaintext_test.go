package oauthprovider

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cleat-team/cleat/plugin"
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

// TestSessionAccessRefreshTokensAreSealedAtRest proves finishLogin's three
// Payloads.Seal calls actually change what lands in the database, and that
// the sealed form is tenant-bound -- not merely that Seal was CALLED (a
// call that discarded the result and stored plaintext anyway would still
// "call Seal"). cleat#1992.
func TestSessionAccessRefreshTokensAreSealedAtRest(t *testing.T) {
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
		State: "seal-at-rest-state", CodeVerifier: "verifier",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	store.mu.Unlock()
	store.AddOAuthConfig(testTenantID, "google", "cid", "cs", "http://localhost/cb", "", true)

	req := httptest.NewRequest("GET", "/oauth/google/callback?code=x&state=seal-at-rest-state", nil)
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

	store.mu.RLock()
	s, ok := store.sessions[sessionID]
	store.mu.RUnlock()
	if !ok {
		t.Fatalf("session %s vanished after finishLogin", sessionID)
	}

	atRestAccess, ok := s.AccessTokenAtRest.(string)
	if !ok {
		t.Fatalf("AccessTokenAtRest = %#v, want a string", s.AccessTokenAtRest)
	}
	atRestRefresh, ok := s.RefreshTokenAtRest.(string)
	if !ok {
		t.Fatalf("RefreshTokenAtRest = %#v, want a string", s.RefreshTokenAtRest)
	}
	atRestSession, ok := s.SessionTokenAtRest.(string)
	if !ok {
		t.Fatalf("SessionTokenAtRest = %#v, want a string", s.SessionTokenAtRest)
	}

	// The plaintext must not appear at rest, in any of the three columns --
	// this is the known-negative half: a Seal call that silently no-ops
	// (returns its input unchanged) would pass every other assertion in this
	// test file but fail here.
	if atRestAccess == plainAccessToken {
		t.Error("access_token stored identical to the plaintext the provider returned -- not sealed")
	}
	if atRestRefresh == plainRefreshToken {
		t.Error("refresh_token stored identical to the plaintext the provider returned -- not sealed")
	}
	if atRestSession == callbackResp.SessionToken {
		t.Error("session_token stored identical to the plaintext returned to the caller -- not sealed")
	}
	if _, err := base64.StdEncoding.DecodeString(atRestAccess); err != nil {
		t.Errorf("access_token at rest is not valid base64: %v", err)
	}

	// Opened under the SAME tenant, the sealed value must recover the exact
	// plaintext (known-positive).
	openCtx := plugin.ForTenant(t.Context(), testTenantID)
	sealedAccess, err := base64.StdEncoding.DecodeString(atRestAccess)
	if err != nil {
		t.Fatalf("decode sealed access token: %v", err)
	}
	opened, err := p.payloads.Open(openCtx, sealedAccess)
	if err != nil {
		t.Fatalf("Open access token under its own tenant: %v", err)
	}
	if string(opened) != plainAccessToken {
		t.Errorf("Open recovered %q, want %q", opened, plainAccessToken)
	}

	// Opened under a DIFFERENT tenant, it must refuse -- the same binding
	// property engine.PayloadEncryption's real SealForPlugin/OpenForPlugin
	// provide via AEAD, faked here via plugintest.FakePayloads' tenant
	// prefix. A Seal that did not bind to the tenant at all (e.g. stored the
	// plaintext with a fixed, tenant-independent transform) would let this
	// succeed.
	wrongTenant := uuid.MustParse("00000000-0000-0000-0000-0000000000ff")
	wrongCtx := plugin.ForTenant(t.Context(), wrongTenant)
	if _, err := p.payloads.Open(wrongCtx, sealedAccess); err == nil {
		t.Error("Open succeeded under a different tenant than the value was sealed for")
	}
}
