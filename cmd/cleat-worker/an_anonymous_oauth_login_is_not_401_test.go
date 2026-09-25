package main

// cleat#2319, found by cleat-review reviewing #2318: GET /oauth/{provider}/login
// was not in pluginAuthExemptPatterns -- only /callback was. Under the
// default --require-auth, auth.MiddlewareWithMux 401s the login request
// before oauthprovider's own handleLogin ever runs, so no browser can start
// an OAuth login on a default deployment. The fix is the one-line addition
// to pluginAuthExemptPatterns (plugin_exempt_routes.go); this proves it end
// to end, the same way a_require_host_match_boots_as_the_app_role_test.go
// and a_migration_is_a_deploy_step_test.go prove their own flags -- by
// running the REAL binary, because (as a_slack_interactive_route_is_exempt_
// test.go's doc comment explains) --require-auth is wired into a chain of
// closures inside main() that no test outside it can reach any other way.
//
// TestPluginAuthExemptPatternsAreRealRoutes (plugin_exempt_patterns_are_real_
// routes_test.go) already proves the new pattern resolves to a real route on
// the real mux; what THIS test adds is the one thing that check cannot: that
// an ANONYMOUS caller reaches it under a REAL --require-auth worker and gets
// a real redirect, not just a matching mux entry.
//
// Single dialect (Postgres): the property under test is HTTP middleware
// wiring in main.go, which does not vary by database, the same scope
// TestRunDueBackupsConcurrentWorkersProduceOneDispatch and this package's own
// TestMainRefusesToStartOnAStaleRouteSignature already use for a
// dialect-independent property.
//
// "github" as the provider, not "oidc": github's endpoints are a hardcoded
// table (routes.go), so resolveEndpoints makes no network call and this test
// needs no mock IdP -- it only has to check the Location header this
// process's own oauthprovider plugin built, never follow it.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugins/oauthprovider"
)

func TestAnonymousOAuthLoginIsNotAborted401UnderRequireAuth(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the worker binary")
	}
	c := deployDialect{"postgres", "CLEAT_TEST_POSTGRES", "postgres"}
	if c.admin() == "" {
		t.Skip("CLEAT_TEST_POSTGRES/CLEAT_TEST_DB not set, skipping")
	}

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "cleat-worker")
	build := exec.Command("go", "build", "-o", bin, "./cmd/cleat-worker")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/cleat-worker: %v\n%s", err, out)
	}

	ownerDSN, owner := deployScratch(t, c)
	if code, out := runWorker(t, bin, nil, "--driver=postgres", "--db="+ownerDSN, "--migrate-only"); code != 0 {
		t.Fatalf("--migrate-only exited %d:\n%s", code, out)
	}
	// cleat_app, not the owner connection: a superuser bypasses row-level
	// security, and the worker refuses to SERVE on such a connection
	// (see pgAppRoleDSN's own doc comment) -- --migrate-only above still
	// needs the owner's DDL rights, but the long-running serve below must
	// not run as it.
	dsn := pgAppRoleDSN(t, owner, ownerDSN)
	db := owner

	// A real 32-byte tenant-secrets key: getConfig's client-secret lookup
	// (plugin.Secrets) is exercised for real, the same lookup that used to
	// return 500 with no --encryption-key-file (cleat-review's BROKEN
	// verdict on cleat#2295/#2296) -- an exempt route that 500s on every
	// request would be just as unusable as one that 401s.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	keyB64 := base64.StdEncoding.EncodeToString(key)
	ring, err := engine.NewKeyRing(engine.VersionedKey{Version: 1, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	secretStore := engine.NewSecretStoreWithRing(db, "postgres", ring)
	realSecrets := engine.NewPluginSecrets(secretStore)

	const provider = "github"
	const clientID = "cleat-2319-test-client-id"
	tenantID := engine.DefaultTenantUUID
	ctx := context.Background()
	if err := realSecrets.ForTenant(tenantID).Put(ctx,
		oauthprovider.OAuthClientSecretName(provider), "cleat-2319-test-client-secret"); err != nil {
		t.Fatalf("seed client secret: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO oauth_config (tenant_id, provider, client_id, redirect_url, enabled)
		VALUES ($1, $2, $3, $4, $5)`,
		tenantID, provider, clientID, "http://localhost/oauth/github/callback", true); err != nil {
		t.Fatalf("seed oauth_config: %v", err)
	}

	port := freePort(t)
	args := []string{"--driver=postgres", "--db=" + dsn, "--migrate-db=" + ownerDSN, fmt.Sprintf("--api-addr=127.0.0.1:%d", port)}
	cmd := exec.Command(bin, args...)
	// NO --require-auth flag: the default is true, and this whole test is
	// about what the DEFAULT does, not an opt-in flag.
	cmd.Env = append(os.Environ(), "CLEAT_SECRET_MASTER_KEY="+keyB64)
	var out syncBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	defer func() {
		_ = cmd.Process.Kill()
		<-exited
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	deadline := time.Now().Add(45 * time.Second)
	healthy := false
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			t.Fatalf("worker exited before becoming healthy:\n%s", out.String())
		default:
		}
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				healthy = true
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !healthy {
		t.Fatalf("worker did not become healthy within 45s:\n%s", out.String())
	}

	// The request this issue is about: ANONYMOUS (no Authorization header
	// at all), against a worker running under the DEFAULT --require-auth.
	loginURL := base + "/oauth/" + provider + "/login?tenant_id=" + tenantID
	req, err := http.NewRequest(http.MethodGet, loginURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", loginURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("anonymous GET %s = 401 %q: the login route is not exempt from --require-auth "+
			"(pluginAuthExemptPatterns, plugin_exempt_routes.go), so no browser can start an OAuth "+
			"login on a default deployment (cleat#2319)", loginURL, body)
	}
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("anonymous GET %s = %d %q, want 302 (a redirect to the IdP)", loginURL, resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "https://github.com/login/oauth/authorize") {
		t.Fatalf("Location = %q, want it to start with https://github.com/login/oauth/authorize", loc)
	}
	locURL, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location %q: %v", loc, err)
	}
	// Not just "redirects somewhere": it must be a redirect built from THIS
	// tenant's own configured client, not a generic or empty one.
	if got := locURL.Query().Get("client_id"); got != clientID {
		t.Errorf("Location's client_id = %q, want %q -- the configured tenant's own oauth_config row",
			got, clientID)
	}
	if got := locURL.Query().Get("state"); got == "" {
		t.Error("Location carries no state -- the CSRF nonce handleLogin mints was not included")
	}

	// A control on the OTHER exempt OAuth route, so this test's setup is not
	// itself the reason nothing 401s: /callback was already exempt before
	// this fix, and an invalid state on it must answer 400, never 401 --
	// confirming an anonymous caller reaches ITS handler too, on the same
	// worker, under the same --require-auth.
	cbResp, err := client.Get(base + "/oauth/" + provider + "/callback?code=x&state=nonexistent")
	if err != nil {
		t.Fatalf("GET /oauth/%s/callback: %v", provider, err)
	}
	defer cbResp.Body.Close()
	if cbResp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("control: anonymous GET /oauth/%s/callback = 401 -- the callback route, exempt since "+
			"before this fix, stopped being reachable; this test's setup proves nothing", provider)
	}
}
