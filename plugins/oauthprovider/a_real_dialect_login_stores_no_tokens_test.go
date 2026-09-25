package oauthprovider

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// TestARealLoginStoresNoTokensOnAnyDialect is cleat-review's should-fix (1)
// on cleat#2295/705e1fcd: TestSessionAccessRefreshTokensAreNotPersisted
// (a_client_secret_and_session_tokens_move_off_plaintext_test.go) proves the
// NULL-at-rest property against the FAKE driver, whose execUpdateSession
// decides "these columns are NULL" from `len(args) == 4` -- a shape, not the
// query text. A finishLogin that regressed to binding
// `session_token = $1, access_token = $2, refresh_token = $2` (reusing one
// placeholder for two columns, so still 4 distinct arguments) would satisfy
// that fake and this file is what would still catch it, because there is no
// fake here: session_token/access_token/refresh_token are read back with a
// real SELECT against a real server, so what is asserted is what the SQL
// standard's NULL actually is on each of the three, not what a Go struct
// field happened to be left as.
//
// Runs the real /login -> real /callback path end to end -- no seeded
// session row, no bypassed getConfig -- against real Postgres, MySQL and SQL
// Server, with a real (if fixed-key) plugin.Secrets store, so the client
// secret lookup that made the pre-fix version return 500 with no
// --encryption-key-file (cleat-review's BROKEN verdict on 48b4c1f7) is
// exercised for real rather than assumed fixed by a change to a different
// file.
func TestARealLoginStoresNoTokensOnAnyDialect(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

			// Core schema first: tenant_secrets and admin.tenants, which the
			// real SecretStore and DefaultTenantUUID below both need, live
			// there rather than in this plugin's own migrations -- the same
			// order TestARetiredSecretRefusesIngest uses.
			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: dialect, logger: quiet}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("oauthprovider migrations on %s: %v", be.Name, err)
			}

			// A real SecretStore under a fixed test key -- the same shape
			// TestARetiredSecretRefusesIngest and
			// TestTenantBCannotReadTenantAsWebhookIngestSecretThroughTheRoute
			// use, reproduced here rather than imported for the same
			// import-cycle reason plugintest.RunEveryArm's doc gives.
			key := make([]byte, 32)
			for i := range key {
				key[i] = 0x5a
			}
			ring, err := engine.NewKeyRing(engine.VersionedKey{Version: 1, Key: key})
			if err != nil {
				t.Fatalf("build key ring: %v", err)
			}
			secretStore := engine.NewSecretStoreWithRing(be.DB, string(dialect), ring)
			realSecrets := engine.NewPluginSecrets(secretStore)

			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}
			p.secrets = realSecrets
			p.mux = http.NewServeMux()
			if err := p.RegisterRoutes(p.mux); err != nil {
				t.Fatalf("RegisterRoutes: %v", err)
			}

			// engine.DefaultTenantUUID, not a freshly generated one: MySQL's
			// schema is single-tenant by constraint, and this test is not
			// about tenant isolation (that is
			// TestTenantBCannotReadTenantAsWebhookIngestSecretThroughTheRoute's
			// shape, for a different plugin) -- it is about what one real
			// login leaves at rest.
			tenantID := uuid.MustParse(engine.DefaultTenantUUID)

			// oauth_config is TenantScoped (migration 3), so writing it on
			// the raw be.DB pool -- which carries no tenant session context
			// -- needs CrossTenantConn: on SQL Server a plain INSERT here
			// would be refused by the policy's BLOCK predicate. tenant_secrets
			// cleanup below needs the same connection for the same reason.
			fixtureDB := be.CrossTenantConn(t, ctx,
				"oauthprovider real-dialect login fixture: seeds the config row a real /login reads")
			const redirectURL = "http://localhost/oauth/google/callback"

			// oauth_config's primary key is (tenant_id, provider), and
			// PostgreSQL's PluginTestBackend reuses one long-lived database
			// across runs rather than a fresh one per test (unlike MySQL and
			// SQL Server here, which start from an empty schema) -- so a
			// second run under the same tenant/provider hits a duplicate
			// key. Delete first, and delete afterward too: leaving a row
			// behind would make a LATER test in this file (or a rerun of
			// this one) collide the same way.
			//
			// tenant_secrets is the same story with a sharper consequence:
			// it is scoped by DefaultTenantUUID, which cmd/cleatctl's own
			// reseal-secrets tests also seed under -- a leftover row here
			// makes THAT suite's global sweep report an unrelated row as
			// UNREADABLE and fail on every dialect (found the hard way
			// against a persistent local SQL Server: the row was invisible
			// to a plain `DELETE ... WHERE name = ...` because RLS hides it
			// from a connection with no tenant_id in SESSION_CONTEXT, so it
			// silently reported zero rows affected -- fixtureDB is already
			// scoped past that). This delete-first-and-after must run BEFORE
			// the client secret is seeded below, or the "delete first" half
			// deletes the row this test just wrote.
			cleanup := func() {
				if _, err := plugintest.ExecRebound(t, context.Background(), fixtureDB, dialect,
					`DELETE FROM oauth_sessions WHERE tenant_id = $1 AND provider = $2`,
					tenantID.String(), "google"); err != nil {
					t.Errorf("cleanup oauth_sessions on %s: %v", be.Name, err)
				}
				if _, err := plugintest.ExecRebound(t, context.Background(), fixtureDB, dialect,
					`DELETE FROM oauth_config WHERE tenant_id = $1 AND provider = $2`,
					tenantID.String(), "google"); err != nil {
					t.Errorf("cleanup oauth_config on %s: %v", be.Name, err)
				}
				if _, err := plugintest.ExecRebound(t, context.Background(), fixtureDB, dialect,
					`DELETE FROM tenant_secrets WHERE tenant_id = $1 AND name = $2`,
					tenantID.String(), OAuthClientSecretName("google")); err != nil {
					t.Errorf("cleanup tenant_secrets on %s: %v", be.Name, err)
				}
			}
			cleanup()
			defer cleanup()

			const clientSecret = "real-dialect-test-client-secret"
			if err := realSecrets.ForTenant(tenantID.String()).Put(ctx,
				OAuthClientSecretName("google"), clientSecret); err != nil {
				t.Fatalf("seed client secret: %v", err)
			}

			if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
				`INSERT INTO oauth_config (tenant_id, provider, client_id, redirect_url, enabled)
				 VALUES ($1, $2, $3, $4, $5)`,
				tenantID.String(), "google", "real-dialect-test-client-id", redirectURL, true,
			); err != nil {
				t.Fatalf("seed oauth_config: %v", err)
			}

			// A mock IdP -- real HTTP, fake identity, the same technique
			// TestSessionAccessRefreshTokensAreNotPersisted uses, so /login's
			// redirect and /callback's token exchange both complete without
			// reaching a real network.
			const plainAccessToken = "real-dialect-access-token"
			const plainRefreshToken = "real-dialect-refresh-token"
			mockMux := http.NewServeMux()
			mockMux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"access_token":"` + plainAccessToken +
					`","refresh_token":"` + plainRefreshToken + `","expires_in":3600}`))
			})
			mockMux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"email":"real-dialect-user@example.com"}`))
			})
			mockSrv := httptest.NewServer(mockMux)
			defer mockSrv.Close()

			origEndpoints := endpoints
			endpoints = map[string]providerEndpoints{
				"google": {authURL: mockSrv.URL + "/authorize", tokenURL: mockSrv.URL + "/token",
					userinfoURL: mockSrv.URL + "/userinfo", scope: "openid email profile"},
			}
			defer func() { endpoints = origEndpoints }()
			p.httpClient = mockSrv.Client()

			// GET /login for real: this is what proves the client secret
			// lookup that returned 500 with no --encryption-key-file
			// (cleat-review's BROKEN verdict) is exercised -- getConfig runs
			// on the login path too, not only on callback.
			loginReq := httptest.NewRequest("GET",
				"/oauth/google/login?tenant_id="+tenantID.String(), nil)
			loginRec := httptest.NewRecorder()
			p.mux.ServeHTTP(loginRec, loginReq)
			if loginRec.Code != http.StatusFound {
				t.Fatalf("GET /login: want 302, got %d: %s", loginRec.Code, loginRec.Body.String())
			}
			loc, err := url.Parse(loginRec.Header().Get("Location"))
			if err != nil {
				t.Fatalf("parse Location: %v", err)
			}
			state := loc.Query().Get("state")
			if state == "" {
				t.Fatalf("Location carries no state: %s", loc)
			}

			callbackReq := httptest.NewRequest("GET",
				"/oauth/google/callback?code=mock-code&state="+state, nil)
			callbackRec := httptest.NewRecorder()
			p.mux.ServeHTTP(callbackRec, callbackReq)
			if callbackRec.Code != http.StatusOK {
				t.Fatalf("GET /callback: want 200, got %d: %s", callbackRec.Code, callbackRec.Body.String())
			}
			var callbackResp struct {
				SessionToken string `json:"session_token"`
			}
			if err := json.Unmarshal(callbackRec.Body.Bytes(), &callbackResp); err != nil {
				t.Fatalf("decode callback response: %v", err)
			}
			if callbackResp.SessionToken == "" {
				t.Fatal("callback response carries no session_token")
			}

			// The real assertion: read the row back with a real SELECT, not
			// a Go struct field. sql.NullString.Valid is false only for a
			// genuine SQL NULL -- a driver would report an empty string as
			// Valid=true, String="", so this cannot pass on an accidental
			// empty-string write the way a `== ""` check could.
			var sessionToken, accessToken, refreshToken, tokenHash sql.NullString
			row := plugintest.QueryRowRebound(t, ctx, fixtureDB, dialect,
				`SELECT session_token, access_token, refresh_token, token_hash
				 FROM oauth_sessions WHERE tenant_id = $1 AND provider = $2`,
				tenantID.String(), "google")
			if err := row.Scan(&sessionToken, &accessToken, &refreshToken, &tokenHash); err != nil {
				t.Fatalf("read back oauth_sessions row: %v", err)
			}
			if sessionToken.Valid {
				t.Errorf("session_token at rest on %s = %q, want SQL NULL", be.Name, sessionToken.String)
			}
			if accessToken.Valid {
				t.Errorf("access_token at rest on %s = %q, want SQL NULL "+
					"(plaintext was %q -- it must not reach the row in any form)",
					be.Name, accessToken.String, plainAccessToken)
			}
			if refreshToken.Valid {
				t.Errorf("refresh_token at rest on %s = %q, want SQL NULL "+
					"(plaintext was %q -- it must not reach the row in any form)",
					be.Name, refreshToken.String, plainRefreshToken)
			}
			if !tokenHash.Valid || tokenHash.String == "" {
				t.Errorf("token_hash at rest on %s = %#v, want a non-empty value -- "+
					"session lookup depends on it", be.Name, tokenHash)
			}
		})
	}
}
