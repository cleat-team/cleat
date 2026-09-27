package oauthprovider

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
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
// session row, no bypassed getConfig -- against real Postgres, with a real
// (if fixed-key) plugin.Secrets store, so the client secret lookup that made
// the pre-fix version return 500 with no --encryption-key-file
// (cleat-review's BROKEN verdict on 48b4c1f7) is exercised for real rather
// than assumed fixed by a change to a different file.
//
// Against real MySQL and SQL Server this file asserts a DIFFERENT thing, and
// says so rather than skipping: cleat#2340 makes OAuth login Postgres-only
// for this release, so p.pgOnly (plugin.go) is the first statement in both
// handlers and these two dialects get 501. The leg asserts the refusal and
// that it refuses cleanly -- no oauth_sessions row left behind -- because
// "refuse cleanly rather than half-work" is the design decision, and an
// assertion on it is what makes it one. What it does NOT assert there is the
// NULL-at-rest property, which is unreachable on those dialects by
// construction: nothing can write the row. See the branch comment below.
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
			// RunMigrations creates oauth_config, oauth_sessions and
			// oauth_allowed_identities (and, on MSSQL, their tenant_isolation
			// security policies) directly in be.DB, which PluginTestBackend
			// reuses across runs rather than recreating -- and be.Cleanup only
			// closes the pool, it never runs this plugin's Down SQL. Left behind, the next
			// `go test ./...` against this same shared test database fails
			// engine.TestEveryTenantOwnedTableIsEmptiedByDropTenant on every
			// one of them (cleat-review, #2295 at 831a66fb) -- the same shape
			// #2271 found and fixed for slacknotify's slack_config.
			// Registered after RunMigrations but before be.Cleanup so it
			// always runs first (defers unwind LIFO), whether this subtest
			// reaches t.Fatalf or the end.
			defer cleanupOauthproviderSchema(t, be.DB, be.Dialect)

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

			// THE REAL MINTER, not the recording fake the behavioural tests
			// use. cleat#2340: this is the ONLY place auth.TenantStore's
			// per-dialect INSERT runs against a real database -- everywhere
			// else wires a fake that never emits SQL, so a wrong column list or
			// a dialect-specific spelling would pass every one of them and fail
			// only here. That is the class of defect this file already exists
			// for on the read side.
			realKeyStore, ksErr := auth.NewTenantStoreForDialect(be.DB, string(dialect))
			if ksErr != nil {
				t.Fatalf("build the real API key store: %v", ksErr)
			}
			var mintedKey string
			p.mintOAuthAPIKey = func(ctx context.Context, req plugin.MintOAuthAPIKeyRequest) (string, error) {
				raw := auth.GenerateAPIKey()
				if err := realKeyStore.CreateOAuthAPIKey(ctx, req.TenantID,
					req.Description, raw, req.ExpiresAt, req.OAuthIdentity); err != nil {
					return "", err
				}
				mintedKey = raw
				return raw, nil
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
				"oauthprovider real-dialect login fixture: seeds the config and allowlist rows a real /login reads")
			const redirectURL = "http://localhost/oauth/google/callback"

			// cleat#2340: OAuth login is Postgres-only in this release, so on
			// MySQL and SQL Server both handlers refuse at their first statement
			// (p.pgOnly, plugin.go). That is designed behaviour and gets an
			// assertion of its own -- never a t.Skip, which would report a
			// deliberate decision as an untested gap.
			//
			// The second assertion is the one worth having: a refusal that
			// happened AFTER the row was inserted would pass a status-code check
			// and still be exactly the "half-work" the design set out to avoid.
			// Counting the rows is what distinguishes refused-before-writing from
			// refused-after.
			if dialect != plugin.DialectPostgres {
				for _, path := range []string{
					"/oauth/google/login?tenant_id=" + tenantID.String(),
					"/oauth/google/callback?code=mock-code&state=mock-state",
				} {
					req := httptest.NewRequest("GET", path, nil)
					rec := httptest.NewRecorder()
					p.mux.ServeHTTP(rec, req)
					if rec.Code != http.StatusNotImplemented {
						t.Errorf("GET %s on %s: want 501 (OAuth login is Postgres-only in this "+
							"release), got %d: %s", path, be.Name, rec.Code, rec.Body.String())
					}
				}

				var rows int
				if err := plugintest.QueryRowRebound(t, ctx, fixtureDB, dialect,
					`SELECT COUNT(*) FROM oauth_sessions`).Scan(&rows); err != nil {
					t.Fatalf("count oauth_sessions on %s: %v", be.Name, err)
				}
				if rows != 0 {
					t.Errorf("on %s: a refused login left %d oauth_sessions row(s) behind -- the "+
						"refusal must happen before any state is written, not after", be.Name, rows)
				}
				return
			}

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

			// The allowlist is a PRECONDITION of the 200 below, not an
			// incidental fixture: since cleat#2371 the check runs on every
			// deployment, so a (tenant, provider) pair with no rows here is
			// refused 403 before the token exchange and none of the at-rest
			// assertions further down can be observed. Written through
			// fixtureDB for the same reason oauth_config is -- the table is
			// TenantScoped (migration 6) and be.DB carries no tenant context.
			//
			// An `email` row, against a mock IdP that now publishes
			// email_verified: an unverified address is carried but cannot
			// match an email row (identity.go's resolvedIdentity), so the
			// mock's claim and this row's type are one decision between them.
			if _, err := plugintest.ExecRebound(t, ctx, fixtureDB, dialect,
				`INSERT INTO oauth_allowed_identities
					   (tenant_id, provider, identity_type, identity_value)
					 VALUES ($1, $2, $3, $4)`,
				tenantID.String(), "google", identityTypeEmail, "real-dialect-user@example.com",
			); err != nil {
				t.Fatalf("seed oauth_allowed_identities: %v", err)
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
				// email_verified is what makes the address eligible to match the
				// `email` allowlist row seeded above; the sibling mock in
				// oauthprovider_behavioral_test.go carries it for the same reason.
				w.Write([]byte(`{"email":"real-dialect-user@example.com","email_verified":true}`))
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
			// cleat#2340: the body is the key page, not JSON, and the assertion
			// that matters is the one the whole issue is about -- the
			// credential a login hands back must authenticate through core
			// auth's OWN resolver. Before this change the login returned a
			// 64-hex session token that ResolveTenantFromAPIKey could never
			// find, so every request carrying it 401'd at the outermost
			// middleware, before this plugin saw the request at all.
			//
			// Resolving is also a real SELECT of the row the mint wrote, so it
			// proves the INSERT landed in the table the reader reads -- the
			// writer/reader agreement cleat#866 lost on MySQL, checked here on
			// the dialect whose spelling differs.
			if mintedKey == "" {
				t.Fatal("the callback minted nothing")
			}
			if !strings.Contains(callbackRec.Body.String(), mintedKey) {
				t.Fatalf("the page does not carry the key that was minted (%s), so the caller has "+
					"no way to authenticate:\n%s", mintedKey, callbackRec.Body.String())
			}
			mintedHash := sha256.Sum256([]byte(mintedKey))
			resolved, resErr := realKeyStore.ResolveTenantFromAPIKey(ctx, mintedHash[:])
			if resErr != nil {
				t.Fatalf("the minted key does not resolve through core auth on %s: %v", be.Name, resErr)
			}
			if resolved != tenantID {
				t.Errorf("the minted key resolved to tenant %s, want %s", resolved, tenantID)
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

// cleanupOauthproviderSchema undoes oauth-provider's Migrations() against
// this test's shared, persistent test database: RunMigrations has no
// matching teardown call anywhere in this file, and plugin.RunDownMigrations
// would not help here even if called -- oauth_config and oauth_sessions are
// TenantScoped (migrations.go:172), so their MSSQL security policies are
// applied through plugin.RunMigrations' own runtime side effect
// (applyTenantScoping), not through any Up/Down SQL the migration declares.
// A Down pass never drops the policy, and DROP TABLE would fail on SQL
// Server while it still references either table. Same shape, same fix, as
// plugins/slacknotify's cleanupSlackConfigSchema (cleat#2230, #2271): drop
// each policy by the "<table>_tenant_isolation" name
// plugin/migration.go's applyTenantScopingMSSQL constructs, then the table,
// then the plugin_migrations row so a re-run of this test applies the
// migration fresh rather than finding it already recorded against tables
// that are gone.
func cleanupOauthproviderSchema(t *testing.T, conn *sql.DB, dialect testutil.Dialect) {
	t.Helper()
	ctx := context.Background()
	const pluginName = "oauth-provider"
	tables := createdTables()

	exec := func(query string) {
		if _, err := conn.ExecContext(ctx, query); err != nil {
			t.Errorf("cleanupOauthproviderSchema: %s: %v", query, err)
		}
	}
	exists := func(query string, args ...any) bool {
		var n int
		if err := conn.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Errorf("cleanupOauthproviderSchema: existence check %s: %v", query, err)
			return false
		}
		return n > 0
	}

	if dialect == testutil.DialectMSSQL {
		for _, table := range tables {
			policy := table + "_tenant_isolation"
			if exists(`SELECT COUNT(*) FROM sys.security_policies WHERE name = @p1`, policy) {
				exec(`DROP SECURITY POLICY dbo.` + policy)
			}
			if exists(`SELECT COUNT(*) FROM sys.tables WHERE name = @p1`, table) {
				exec(`DROP TABLE ` + table)
			}
		}
		exec(`DELETE FROM plugin_migrations WHERE plugin_name = '` + pluginName + `'`)
		return
	}

	for _, table := range tables {
		exec(`DROP TABLE IF EXISTS ` + table)
	}
	exec(`DELETE FROM plugin_migrations WHERE plugin_name = '` + pluginName + `'`)
	if dialect == testutil.DialectPostgres {
		exec(`DELETE FROM admin.plugin_tables WHERE plugin_name = '` + pluginName + `'`)
	}
}

// createdTables names every table this plugin's migrations create, read out of
// the migration DDL rather than listed by hand.
//
// THE LIST USED TO BE THE LITERAL []string{"oauth_sessions", "oauth_config"},
// and the two halves of this cleanup disagreed from the moment a migration
// added a third table. The ledger row is deleted so that a re-run applies the
// migrations "fresh rather than finding them already recorded against tables
// that are gone" -- but each CREATE TABLE IF NOT EXISTS is then a no-op against
// the table the PREVIOUS run left standing, and the migration never runs again.
// PostgreSQL is the backend that reuses a database across runs, so it is the
// one where that is reachable.
//
// That is what happened here: oauth_allowed_identities survived a run with a
// later-revised column name, migration v6 re-ran as a no-op, and a real
// /callback answered 500 {"error":"failed to evaluate the identity allowlist"}
// -- `column "identity_value" does not exist`, against a table whose primary
// key still said `identity`. Reproduced directly:
//
//	docker exec <pg> psql -U test -d test -c \
//	  "SELECT identity_type, identity_value FROM oauth_allowed_identities ..."
//	ERROR:  column "identity_value" does not exist
//
// The fake driver in oauthprovider_behavioral_test.go had been answering that
// same SELECT happily, because a fake matches the Go string it is handed rather
// than a real schema; only a real dialect can disagree with the DDL, which is
// the whole reason this file exists.
//
// The scan over-matches deliberately: a CREATE TABLE inside a comment, or one
// this plugin only ever drops, both land in the result, and both are harmless
// because every drop below is DROP TABLE IF EXISTS. Missing a table is the
// direction that costs, and it is the direction a hand-written list cannot
// detect in itself.
func createdTables() []string {
	re := regexp.MustCompile(
		`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)
	var out []string
	seen := map[string]bool{}
	for _, m := range (&Plugin{}).Migrations() {
		for _, up := range []string{m.Up, m.UpMySQL, m.UpMSSQL} {
			for _, match := range re.FindAllStringSubmatch(up, -1) {
				if seen[match[1]] {
					continue
				}
				seen[match[1]] = true
				out = append(out, match[1])
			}
		}
	}
	return out
}
