package main

// The two claims of cleat#2339 that live entirely inside main(), and that no
// test in plugins/oauthprovider can reach.
//
// Both are about the worker COLLECTING and CONFIGURING the plugin -- one wires
// it into the background loop, one fills in plugin.Environment. A test that
// builds a *Plugin by hand and calls its methods exercises the plugin, and
// deleting the wiring leaves it green: the plugin is still discovered, still
// well-formed, still correct in everything it controls itself.
//
//  1. THE SWEEP THE WORKER RUNS. Four links:
//
//	init() registration
//	  -> main.go  plugin.Discover()
//	  -> main.go  lp.Plugin.(plugin.HasBackground)
//	  -> main.go  bgPlugins = append(bgPlugins, p)
//	  -> setup.go w.startPluginBackground() -> bg.Run(w.ctx)
//
//     The first two are covered -- TestTheWorkerGetsAPluginThatCanRunTheSweep
//     calls Discover() and makes the same type assertion, and
//     TestRunSweepsOnStartupAndReturnsOnCancel calls p.Run(ctx) directly. The
//     append between them was not, and that test's own doc comment says so. It
//     is the invisible kind of missing: delete it and the plugin is still
//     registered, still satisfies HasBackground, still sweeps correctly when
//     called by hand -- the worker simply never calls it. No error, no log, no
//     failing test. TestABootedWorkerSweepsAnAbandonedLogin observes the append
//     by what only it can cause instead of naming it.
//
//  2. THE HOST BINDING --require-host-match PROMISES. Added to pluginEnv:
//
//	HostResolver:     authResolver,
//	RequireHostMatch: *requireHostMatch,
//
//     and handleLogin reads p.requireHostMatch before it consults
//     p.hostResolver. TestLoginIsBoundToTheHost in plugins/oauthprovider covers
//     the CHECK -- five cases, including the ones that are not refusals -- but
//     it builds the Plugin with setupTestPlugin and sets the field itself. It
//     never goes through pluginEnv, so it is green with BOTH lines deleted: the
//     field still reads true in the fixture and the resolver is still non-nil.
//     The plugin is only ever as host-bound as pluginEnv made it, and that is
//     the half that was unpinned. TestABootedWorkerBindsLoginToTheHost boots
//     the real binary with --require-host-match and asks over HTTP.
//
// BOTH ARE BOOT TESTS, for the reason an_anonymous_oauth_login_is_not_401_test.go
// gives: the wiring is closures inside main(), and a shim for it would be a
// second implementation of the collection step -- so the shim would go on
// passing while the real one broke, which is the defect these exist to catch.
//
// BOTH ARE SINGLE-DIALECT (Postgres), the scope a_require_host_match_boots_as_
// the_app_role_test.go and this package's own TestMainRefusesToStartOnAStale-
// RouteSignature already use for a dialect-independent property. What is under
// test here is main()'s plugin collection and environment construction; neither
// varies by database. The sweep QUERY varies by dialect and is covered
// per-dialect by TestSweepExpiredSessionsOnlyDeletesAbandonedLogins.
//
// Both boot on --db=<the cleat_app DSN> rather than the owner's, matching
// a_require_host_match_boots_as_the_app_role_test.go. A superuser bypasses
// row-level security, so a boot test served by one would not be measuring the
// worker a deployment runs -- and on PostgreSQL the sweep's own AcrossAllTenants
// and the request-time TenantForHost lookup are both policy-scoped, so running
// as cleat_app is part of what the wiring has to work under.
//
// Each test carries the two conditional skips its neighbours carry (no binary
// build under -short; no DSN), both of which are environmental preconditions.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugins/oauthprovider"
)

// buildWorker is the shared prologue: the real binary, built from the repo
// root, because every property here is about what the PROCESS does. It returns
// the owner handle as well, so a caller can seed rows and then read back what
// the worker -- on its own connection, as a different role -- did with them.
//
// The DIALECT IS PASSED IN, and both callers have already checked it, because
// the two environmental skips live inline in each test the way every other test
// in this package writes them. Hoisting them into this helper would work, but
// scripts/check-skips.sh attributes a skip to its enclosing function -- so they
// would be recorded against `buildWorker` instead of against the two tests that
// actually need the resource, and the baseline would stop saying which test is
// skipping.
func buildWorker(t *testing.T, c deployDialect) (bin, ownerDSN string, owner *sql.DB) {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(t.TempDir(), "cleat-worker")
	build := exec.Command("go", "build", "-o", bin, "./cmd/cleat-worker")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/cleat-worker: %v\n%s", err, out)
	}
	ownerDSN, owner = deployScratch(t, c)
	if code, out := runWorker(t, bin, nil, "--driver=postgres", "--db="+ownerDSN, "--migrate-only"); code != 0 {
		t.Fatalf("--migrate-only exited %d:\n%s", code, out)
	}
	return bin, ownerDSN, owner
}

// masterKey installs a real 32-byte tenant-secrets key for this test, so the
// worker boots exactly as a deployment does rather than down some no-key path
// this test would then be measuring instead of the one it names. It returns the
// ring so a caller can seed a secret the worker will be able to read.
func masterKey(t *testing.T) *engine.KeyRing {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	ring, err := engine.NewKeyRing(engine.VersionedKey{Version: 1, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	// t.Setenv, and this is load-bearing rather than tidy: hostMatchServes
	// hands the child os.Environ() and nothing else, so a key the child is to
	// see has to be in THIS process's environment. t.Setenv puts it there and
	// restores it afterwards. (an_anonymous_oauth_login_is_not_401_test.go
	// builds its own cmd.Env instead, because it does not use this helper.)
	t.Setenv("CLEAT_SECRET_MASTER_KEY", base64.StdEncoding.EncodeToString(key))
	return ring
}

// TestABootedWorkerSweepsAnAbandonedLogin pins the append inside the
// plugin.HasBackground assertion by observing what only it can cause.
//
// The fixture is the one the plugin-level sweep tests use, so this test is
// about reachability and nothing else: an abandoned login (token_hash NULL,
// PKCE window past) that must go, a still-pending one and a completed-but-
// expired one that must stay. The two survivors are not decoration. A worker
// that deleted every expired row would also make the abandoned row disappear,
// so without them a green here would be consistent with a much worse worker
// than the one under test.
func TestABootedWorkerSweepsAnAbandonedLogin(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the worker binary")
	}
	c := deployDialect{"postgres", "CLEAT_TEST_POSTGRES", "postgres"}
	if c.admin() == "" {
		t.Skip("CLEAT_TEST_POSTGRES/CLEAT_TEST_DB not set, skipping")
	}
	bin, ownerDSN, owner := buildWorker(t, c)
	_ = masterKey(t)
	appDSN := pgAppRoleDSN(t, owner, ownerDSN)

	ctx := context.Background()
	// The default tenant: it exists already, so nothing has to be seeded for
	// ForeignKey-style constraints to be satisfied by it.
	tenantID := engine.DefaultTenantUUID
	abandonedID := uuid.New()
	stillPendingID := uuid.New()
	completedExpiredID := uuid.New()
	past := time.Now().Add(-1 * time.Hour)
	future := time.Now().Add(1 * time.Hour)

	// Seeded BEFORE the worker starts, deliberately. Run sweeps once immediately
	// on startup (plugins/oauthprovider/background.go), so a row present at boot
	// is the case this observes without waiting sweepInterval, which is 5
	// minutes. Seeding after boot would be measuring the ticker instead.
	//
	// Written on the OWNER connection, so the sweep has to find rows written by
	// a path that is not itself. The reads below are on the same handle.
	insert := func(id uuid.UUID, tokenHash any, expiresAt time.Time) {
		t.Helper()
		if _, err := owner.ExecContext(ctx, `
			INSERT INTO oauth_sessions (id, tenant_id, provider, state, token_hash, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			id, tenantID, "github", "some-state", tokenHash, expiresAt); err != nil {
			t.Fatalf("seed oauth_sessions row %s: %v", id, err)
		}
	}
	insert(abandonedID, nil, past)
	insert(stillPendingID, nil, future)
	insert(completedExpiredID, "a-real-token-hash", past)

	rowExists := func(id uuid.UUID) (bool, error) {
		var n int
		if err := owner.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM oauth_sessions WHERE id = $1`, id).Scan(&n); err != nil {
			return false, fmt.Errorf("checking existence of %s: %w", id, err)
		}
		return n > 0, nil
	}

	// observe is assembled here and run by hostMatchServes once /healthz is up;
	// it returns a diagnostic rather than calling t.Fatalf so that the caller
	// can attach the worker's own log to it. A t.Fatalf in here would abort
	// before that log was ever appended, and the log is what says whether the
	// sweep ran and errored or never ran at all.
	observe := func() error {
		// Poll rather than assert once: the append is followed by the Worker's
		// own startup, so this must not depend on the sweep having won a race
		// with /healthz. The deadline is a liveness guard, not a timing
		// assertion -- a correct worker sweeps in milliseconds, and a generous
		// bound cannot make a working one look broken, only make a broken one
		// report instead of hanging the suite.
		deadline := time.Now().Add(30 * time.Second)
		for {
			gone, err := rowExists(abandonedID)
			if err != nil {
				return err
			}
			if !gone {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("the booted worker never swept the abandoned login (row %s, token_hash NULL, "+
					"expires_at in the past) within 30s. The worker discovers the plugin and the plugin "+
					"satisfies plugin.HasBackground -- TestTheWorkerGetsAPluginThatCanRunTheSweep proves "+
					"both -- but plugin.Run never ran, so the collection inside the HasBackground assertion "+
					"in cmd/cleat-worker/main.go did not put it into a background loop. Nothing else goes red "+
					"for that: the plugin is still registered, still healthy, and still sweeps correctly when "+
					"called by hand", abandonedID)
			}
			time.Sleep(200 * time.Millisecond)
		}
		// The controls, asserted only after the sweep has demonstrably run.
		if pending, err := rowExists(stillPendingID); err != nil {
			return err
		} else if !pending {
			return fmt.Errorf("the sweep deleted a login that has NOT expired (row %s, token_hash NULL, "+
				"expires_at in the future). Its predicate is `token_hash IS NULL AND expires_at < now()`, "+
				"so this row is half of what makes the assertion above mean anything", stillPendingID)
		}
		if kept, err := rowExists(completedExpiredID); err != nil {
			return err
		} else if !kept {
			return fmt.Errorf("the sweep deleted a COMPLETED session (row %s, token_hash set) whose expiry "+
				"has passed. Session expiry is enforced at read time, not by deleting the row, and the "+
				"`token_hash IS NULL` half of the predicate is what spares it -- a worker that deleted "+
				"every expired row would also have removed the abandoned one", completedExpiredID)
		}
		return nil
	}

	args := []string{"--driver=postgres", "--db=" + appDSN, "--migrate-db=" + ownerDSN}
	var key string
	var probeErr error
	ok, out := hostMatchServes(t, bin, args, &key, func(_, _ string) { probeErr = observe() })
	if !ok {
		t.Fatalf("the worker did not boot:\n%s", out)
	}
	if probeErr != nil {
		t.Fatalf("%v\n\nWorker output:\n%s", probeErr, out)
	}
}

// TestABootedWorkerDisablesAnExpiredOAuthMintedKey pins the
// RevokeExpiredOAuthAPIKeys assignment into plugin.Environment.
//
// cleat#2412's review measured this gap rather than inferring it: deleting the
// assignment from cmd/cleat-worker/main.go leaves the whole package green
// (1206 pass / 0 fail / 2 skip), and so does deleting MintOAuthAPIKey. The
// plugin's own suite cannot see either, because its fixture wires its own host
// function -- and for THIS half the miss is silent by design: a nil host is
// treated as "this host has no key store" and the sweep returns without
// logging, which TestSweepExpiredOAuthKeysWithNoHostIsASilentNoOp deliberately
// pins. So an unwired revoke degrades invisibly rather than loudly.
//
// THE ROW IS SEEDED BEFORE THE WORKER STARTS because Run sweeps once on
// startup (plugins/oauthprovider/background.go), the same reason
// TestABootedWorkerSweepsAnAbandonedLogin does it: seeding afterwards would
// measure the 5-minute ticker instead of the wiring.
//
// TWO CONTROLS, WITHOUT WHICH THE ASSERTION IS VACUOUS. "The expired OAuth key
// is disabled" is satisfied by a worker that disables EVERY key, and by one that
// disables every EXPIRED key regardless of who minted it. So a live OAuth key
// and an expired key with no oauth_identity are seeded beside it, and both must
// survive. The second is the predicate's own boundary: the statement is scoped
// to `oauth_identity IS NOT NULL` on purpose, because un-revoking is not a
// supported operation in this release and flipping an operator's service key
// would be a surprise with no way back.
func TestABootedWorkerDisablesAnExpiredOAuthMintedKey(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the worker binary")
	}
	c := deployDialect{"postgres", "CLEAT_TEST_POSTGRES", "postgres"}
	if c.admin() == "" {
		t.Skip("CLEAT_TEST_POSTGRES/CLEAT_TEST_DB not set, skipping")
	}
	bin, ownerDSN, owner := buildWorker(t, c)
	_ = masterKey(t)
	appDSN := pgAppRoleDSN(t, owner, ownerDSN)

	ctx := context.Background()
	// The default tenant: it exists already, so nothing has to be seeded for it.
	// engine.DefaultTenantUUID is a STRING; the keys table takes a uuid.
	tenantID := uuid.MustParse(engine.DefaultTenantUUID)
	past := time.Now().Add(-1 * time.Hour)

	// Written on the OWNER connection, so the sweep has to find rows written by
	// a path that is not itself.
	seed := func(forTenant uuid.UUID, oauthIdentity any, expiresAt time.Time, why string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		hash := sha256.Sum256([]byte(id.String()))
		if _, err := owner.ExecContext(ctx, `
			INSERT INTO admin.tenant_api_keys
				(tenant_id, key_id, key_hash, description, expires_at, oauth_identity)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			forTenant, id, hash[:], "boot-test: "+why, expiresAt, oauthIdentity); err != nil {
			t.Fatalf("seed key %s: %v", id, err)
		}
		t.Cleanup(func() {
			_, _ = owner.ExecContext(context.Background(),
				`DELETE FROM admin.tenant_api_keys WHERE key_id = $1`, id)
		})
		return id
	}

	// The subject: an OAuth-minted key whose expiry has passed.
	target := seed(tenantID, "google:expired@example.com", past, "expired oauth key")
	// Control 2: expired, but this feature did not mint it.
	serviceKey := seed(tenantID, nil, past, "expired service key")

	// THERE IS NO "LIVE OAUTH KEY" CONTROL, and its absence is a property of
	// this harness rather than an oversight. A live key under ANY tenant
	// suppresses the startup API key the worker prints -- liveAPIKeyCountQuery
	// (cmd/cleat-worker) is `WHERE disabled_at IS NULL AND (expires_at IS NULL
	// OR expires_at > now())` with NO tenant predicate, and hostMatchServes
	// needs that printed key before it will run any probe. So the control cannot
	// be seeded in any tenant of this database, and the first version of this
	// test asserted "the worker did not print a startup API key" on a run where
	// the sweep had demonstrably worked -- the worker logged "disabled expired
	// OAuth-minted keys count=1" while the test failed in the harness.
	//
	// What that control would have guarded -- that the sweep does not disable
	// OAuth-minted keys wholesale, by dropping `expires_at < now()` from the
	// statement -- is covered where it can be measured, with mutations:
	// plugins/oauthprovider's TestSweepDisablesOnlyExpiredOAuthMintedKeys pins
	// the live and no-expiry cases and goes red when either clause is removed.
	// This test's job is the WIRING, which is the gap the review measured, and
	// one control is enough for that: the row below catches a sweep that empties
	// the table of expired keys without regard to who minted them.

	isDisabled := func(id uuid.UUID) (bool, error) {
		var disabled bool
		if err := owner.QueryRowContext(ctx,
			`SELECT disabled_at IS NOT NULL FROM admin.tenant_api_keys WHERE key_id = $1`,
			id).Scan(&disabled); err != nil {
			return false, fmt.Errorf("reading disabled_at for %s: %w", id, err)
		}
		return disabled, nil
	}

	// A diagnostic rather than a t.Fatalf, so the caller can attach the
	// worker's own log -- which is what says whether the sweep ran and errored
	// or never ran at all.
	observe := func() error {
		// A liveness deadline, not a timing assertion: a correct worker sweeps
		// in milliseconds, so a generous bound cannot make a working one look
		// broken, only make a broken one report instead of hanging.
		deadline := time.Now().Add(30 * time.Second)
		for {
			disabled, err := isDisabled(target)
			if err != nil {
				return err
			}
			if disabled {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("the booted worker never disabled the expired OAuth-minted key "+
					"(row %s, oauth_identity set, expires_at in the past) within 30s. Either "+
					"pluginEnv's RevokeExpiredOAuthAPIKeys assignment is gone -- a nil host is "+
					"treated as 'no key store' and the sweep returns silently, which is the "+
					"documented behaviour of that arm -- or the loop that calls it is not running. "+
					"Both leave the suite green without this test", target)
			}
			time.Sleep(200 * time.Millisecond)
		}

		// The control, asserted only once the sweep has demonstrably run.
		if disabled, err := isDisabled(serviceKey); err != nil {
			return err
		} else if disabled {
			return fmt.Errorf("the sweep disabled an expired key that OAuth login did not mint "+
				"(row %s, oauth_identity NULL). The statement is scoped to `oauth_identity IS NOT "+
				"NULL` deliberately: un-revoking is not supported in this release, so flipping an "+
				"operator's service key would be a surprise with no way back, and the read path "+
				"already refuses it", serviceKey)
		}
		return nil
	}

	args := []string{"--driver=postgres", "--db=" + appDSN, "--migrate-db=" + ownerDSN}
	var key string
	var probeErr error
	ok, out := hostMatchServes(t, bin, args, &key, func(_, _ string) { probeErr = observe() })
	if !ok {
		t.Fatalf("the worker did not boot:\n%s", out)
	}
	if probeErr != nil {
		t.Fatalf("%v\n\nWorker output:\n%s", probeErr, out)
	}
}

// TestABootedWorkerBindsLoginToTheHost pins the HostResolver and
// RequireHostMatch assignments into plugin.Environment.
//
// The property is #2339's own stated one: under --require-host-match, a request
// must not be able to start a login for a tenant its Host does not own. /login
// is auth-exempt (it has to be -- an anonymous browser starts there), so
// auth.HostBindingMiddlewareWithMux skips it, and handleLogin has to do the
// check itself. That check is only reachable if the worker handed the plugin
// both the resolver and the flag; drop either assignment and the plugin is told
// `false` or `nil`, and --require-host-match protects every route except the one
// that mints the credential.
//
// THE EXPERIMENT IS A PAIRED ONE, and the pairing is the whole design. The two
// requests differ in exactly one field -- the tenant_id -- and are otherwise
// identical: same Host, same provider, same anonymous caller. Both tenants are
// fully configured, so neither request can fail for want of a client secret, a
// redirect URL or an oauth_config row. So the only thing left that can
// distinguish a 302 from a 400 is the host check, and that is what makes the
// refusal mean something rather than being one of several ways this request
// could have been turned away.
//
// Both assertions read the BODY, not only the status. handleLogin answers 400
// from three different checks -- an unparseable tenant_id, a missing one, and
// this one -- so a status-only assertion could go green against a branch that is
// not the one under test. CLAUDE.md's "condition that never decides anything"
// case, and the reason the text is asserted rather than the code.
func TestABootedWorkerBindsLoginToTheHost(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the worker binary")
	}
	c := deployDialect{"postgres", "CLEAT_TEST_POSTGRES", "postgres"}
	if c.admin() == "" {
		t.Skip("CLEAT_TEST_POSTGRES/CLEAT_TEST_DB not set, skipping")
	}
	bin, ownerDSN, owner := buildWorker(t, c)
	ring := masterKey(t)
	appDSN := pgAppRoleDSN(t, owner, ownerDSN)

	ctx := context.Background()
	secrets := engine.NewPluginSecrets(engine.NewSecretStoreWithRing(owner, "postgres", ring))

	const provider = "github" // hardcoded endpoints, so no mock IdP and no network
	// hostA belongs to the default tenant. The victim is a second, real tenant
	// on a DIFFERENT host -- so the refusal below is not an artefact of asking
	// about a tenant that does not exist.
	hostA := "app.tenant.example.test"
	victim := "5a5a5a5a-0000-4000-8000-000000002339"
	victimHost := "victim.tenant.example.test"

	// The victim is a real second tenant, on its own host, and it is created
	// FIRST -- both tenant_secrets and oauth_config carry a foreign key to the
	// tenant, so seeding an unknown tenant fails with
	// `tenant_secrets_tenant_id_fkey` rather than quietly doing nothing. hostA
	// belongs to the default tenant, which already exists, so only its domain
	// needs inserting. hostA is owned by the default tenant ALONE, which is the
	// whole point: TenantForHost answers (false, nil) for a host another tenant
	// owns, the same answer it gives for a hostname nobody owns
	// (auth/host_binding.go -- no oracle).
	execDomain(t, owner, "postgres", hostA, engine.DefaultTenantUUID)
	seedTenantWithDomain(t, owner, "postgres", victim, "cleat-2339-victim", victimHost, false)

	// Every tenant in the experiment gets the same treatment -- a client secret
	// and a config row -- so that the victim's request would SUCCEED if the host
	// check were the only thing removed. An under-configured victim would make
	// the attack assertion below pass for an incidental reason (no client
	// secret, no config row) and would leave a deleted HostResolver looking
	// fixed.
	clientIDs := map[string]string{}
	for i, tenant := range []string{engine.DefaultTenantUUID, victim} {
		clientID := fmt.Sprintf("cleat-2339-client-id-%d", i)
		clientIDs[tenant] = clientID
		if err := secrets.ForTenant(tenant).Put(ctx,
			oauthprovider.OAuthClientSecretName(provider), fmt.Sprintf("cleat-2339-secret-%d", i)); err != nil {
			t.Fatalf("seed client secret for %s: %v", tenant, err)
		}
		// client_secret is deliberately absent: it lives in plugin.Secrets by
		// this migration, not on the row (cleat#1992), and this is the same
		// INSERT the two neighbouring boot tests use.
		if _, err := owner.ExecContext(ctx, `
			INSERT INTO oauth_config (tenant_id, provider, client_id, redirect_url, enabled)
			VALUES ($1, $2, $3, $4, $5)`,
			tenant, provider, clientID, "http://localhost/oauth/github/callback", true); err != nil {
			t.Fatalf("seed oauth_config for %s: %v", tenant, err)
		}
	}

	observe := func(base string) error {
		// A client that does NOT follow the redirect: the success case is a 302
		// to github.com, and following it would make a real network call.
		client := &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		// Anonymous, with no Authorization header -- exactly the caller /login
		// is exempt to serve.
		login := func(host, tenantID string) (int, string, string, error) {
			req, err := http.NewRequest(http.MethodGet,
				base+"/oauth/"+provider+"/login?tenant_id="+tenantID, nil)
			if err != nil {
				return 0, "", "", err
			}
			req.Host = host
			resp, err := client.Do(req)
			if err != nil {
				return 0, "", "", fmt.Errorf("GET /oauth/%s/login as Host %s: %w", provider, host, err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(body), resp.Header.Get("Location"), nil
		}

		// THE CONTROL, and it is load-bearing: without it a handler that refused
		// every request unconditionally would satisfy the assertion below. This
		// is the Host that DOES own the tenant, asking for its own tenant -- so
		// the check must pass and the login must complete.
		st, body, loc, err := login(hostA, engine.DefaultTenantUUID)
		if err != nil {
			return err
		}
		if st != http.StatusFound {
			return fmt.Errorf("Host %s asking for the tenant it owns = %d %q, want 302. Either the host "+
				"check refused a request it should have allowed, or it refused for a neighbouring reason "+
				"(a missing resolver answers 500 \"host binding is misconfigured\"), or the login path "+
				"itself is broken -- so the assertion below would prove nothing", hostA, st, body)
		}
		if !strings.HasPrefix(loc, "https://github.com/login/oauth/authorize") {
			return fmt.Errorf("Location = %q, want it to start with https://github.com/login/oauth/authorize", loc)
		}
		// Not just "redirects somewhere": the redirect must be built from THIS
		// tenant's own configured client. That is what proves getConfig resolved
		// the requested tenant's row rather than falling back to something
		// generic -- and it is also the precondition the attack case below
		// depends on, since the same lookup is what would hand the victim's
		// client_id to an attacker if the host check did not run.
		locURL, err := url.Parse(loc)
		if err != nil {
			return fmt.Errorf("parse Location %q: %w", loc, err)
		}
		if got := locURL.Query().Get("client_id"); got != clientIDs[engine.DefaultTenantUUID] {
			return fmt.Errorf("the control's Location carries client_id = %q, want %q (its own tenant's "+
				"oauth_config row)", got, clientIDs[engine.DefaultTenantUUID])
		}

		// THE ATTACK #2339 closes: the same Host, the same provider, the same
		// anonymous caller, and the victim's tenant_id -- which hostA does not
		// own. Before the fix this minted a credential for the victim tenant.
		st, body, _, err = login(hostA, victim)
		if err != nil {
			return err
		}
		if st != http.StatusBadRequest || !strings.Contains(body, "does not match the requested host") {
			return fmt.Errorf("Host %s asking for tenant %s (which owns %s, not %s) = %d %q, want 400 "+
				"\"tenant_id does not match the requested host\". The victim tenant is configured "+
				"exactly as the control's is, so nothing else in handleLogin can be turning this away "+
				"-- a 302 here IS the exploit. The check reads p.requireHostMatch and p.hostResolver, "+
				"and both come from plugin.Environment as main() builds it: a missing or wrong "+
				"HostResolver/RequireHostMatch assignment into pluginEnv tells the plugin host binding "+
				"is off (or leaves it nothing to resolve with), and an attacker-chosen tenant_id is "+
				"then accepted from any Host. That is why an anonymous caller can start a login for a "+
				"tenant whose domain they do not control",
				hostA, victim, victimHost, hostA, st, body)
		}
		return nil
	}

	args := []string{"--driver=postgres", "--db=" + appDSN, "--migrate-db=" + ownerDSN,
		"--require-host-match"}
	var key string
	var probeErr error
	ok, out := hostMatchServes(t, bin, args, &key, func(b, _ string) { probeErr = observe(b) })
	if !ok {
		t.Fatalf("the worker did not boot:\n%s", out)
	}
	if probeErr != nil {
		t.Fatalf("%v\n\nWorker output:\n%s", probeErr, out)
	}
}

// TestABootedWorkerMintsAnOAuthAPIKeyOnARealLogin pins the
// MintOAuthAPIKey assignment into plugin.Environment -- cleat#2408, the
// sibling gap TestABootedWorkerDisablesAnExpiredOAuthMintedKey's own doc
// comment names and does not cover: that test pins RevokeExpiredOAuthAPIKeys
// by seeding an already-expired key row directly in SQL and watching the
// sweep act on it, which never calls MintOAuthAPIKey at all. Deleting the
// mint assignment from cmd/cleat-worker/main.go left the whole package green
// before this test existed (cleat#2412's review measured it), because the
// plugin's own suite wires its OWN recording minter
// (plugins/oauthprovider/a_real_dialect_login_stores_no_tokens_test.go:125)
// rather than going through pluginEnv, and because a mint only happens on a
// SUCCESSFUL login, which no boot test in this file drove all the way
// through -- TestABootedWorkerBindsLoginToTheHost stops at the redirect
// (CheckRedirect returns http.ErrUseLastResponse) specifically to avoid a
// real network call.
//
// THE OBSTACLE THIS TEST HAD TO CLEAR, recorded on the issue before this was
// written (design v2 Sec(9), cleat#2340): a real /login -> /callback round
// trip needs an IdP the spawned worker can reach, and oidc.go's discoveryURL
// refuses a non-https issuer -- so the mock has to be httptest.NewTLSServer,
// and the SEPARATELY SPAWNED worker process has to trust its self-signed
// certificate, which the in-process tests' package-var swap
// (plugins/oauthprovider's `endpoints` map) cannot reach from outside at
// all. SSL_CERT_FILE, pointed at the mock's certificate, is what bridges
// that -- MEASURED here, not assumed: a standalone probe (spawn a child
// process, GET the TLS mock from inside it) failed with "certificate signed
// by unknown authority" with SSL_CERT_FILE unset, and succeeded with it set,
// on this machine (darwin, go1.27.1) -- so the single end-to-end test design
// the issue hoped for turned out not to need the documented macOS/split
// fallback at all, at least here. If this test ever needs that fallback on
// some other platform, gate it on GOOS rather than deleting this.
//
// THE SECOND OBSTACLE: the mock IdP binds loopback, which EgressGuard's floor
// refuses unconditionally regardless of any allowlist -- AllowLoopback (the
// production code already has a test-only seam for exactly this shape) is
// enforced test-only by TestNoProductionCodeAllowsLoopbackEgress, so a real
// spawned worker cannot use it. The production mechanism for the same thing
// is --plugin-egress-allow-private, PLUS a tenant_egress_allow row -- both
// are needed because the floor and the tenant-allowlist layer are separate
// gates in engine/egress_policy.go's DialContext, checked one after the
// other.
//
// WHY THE EXISTING MOCK TECHNIQUE (swapping the package-level `endpoints`
// map, used by the google/github hardcoded-provider tests) COULD NOT BE
// REUSED HERE: that map lives in the oauthprovider package's memory, and a
// spawned cmd/cleat-worker process has its own, separate copy that this test
// binary cannot reach. The generic `oidc` provider is the only one whose
// endpoint (the issuer) is externally configurable, via a DB row
// (oauth_config.issuer) the spawned worker reads for itself -- which is why
// this test is provider=oidc rather than provider=google.
//
// WHAT IS ASSERTED: a NEW row in admin.tenant_api_keys, carrying
// oauth_identity = OAuthIdentityTag("oidc", "email", <the mock's email>) --
// not merely "the callback returned 200", which a mint that silently no-ops
// would also satisfy.
func TestABootedWorkerMintsAnOAuthAPIKeyOnARealLogin(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the worker binary")
	}
	c := deployDialect{"postgres", "CLEAT_TEST_POSTGRES", "postgres"}
	if c.admin() == "" {
		t.Skip("CLEAT_TEST_POSTGRES/CLEAT_TEST_DB not set, skipping")
	}
	bin, ownerDSN, owner := buildWorker(t, c)
	ring := masterKey(t)
	appDSN := pgAppRoleDSN(t, owner, ownerDSN)

	ctx := context.Background()
	tenantID := engine.DefaultTenantUUID
	const provider = "oidc"
	const testEmail = "cleat-2408-user@example.test"

	// The mock IdP. mockURL is filled in once the server starts; the
	// discovery handler closes over the pointer rather than needing the URL
	// before NewTLSServer returns it.
	var mockURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"userinfo_endpoint":%q}`,
			mockURL, mockURL+"/authorize", mockURL+"/token", mockURL+"/userinfo")
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		// No id_token: oidc.go validates one IN ADDITION whenever the IdP
		// returns one, and userinfo alone is sufficient identity -- this
		// avoids needing to mint and sign a JWT for a mock that exists only
		// to prove the wiring.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"cleat-2408-access-token","refresh_token":"cleat-2408-refresh-token","expires_in":3600}`))
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"email":%q,"email_verified":true}`, testEmail)
	})
	mockSrv := httptest.NewTLSServer(mux)
	defer mockSrv.Close()
	mockURL = mockSrv.URL
	parsedMockURL, err := url.Parse(mockURL)
	if err != nil {
		t.Fatalf("parse mock IdP URL %q: %v", mockURL, err)
	}
	mockHostOnly := parsedMockURL.Hostname()

	// SSL_CERT_FILE: the measured bridge (see doc comment). Written to
	// t.TempDir() rather than a fixed path -- this file's siblings all use
	// it for the same reason (parallel runs, automatic cleanup).
	var certPEM []byte
	for _, c := range mockSrv.TLS.Certificates {
		for _, der := range c.Certificate {
			certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
		}
	}
	certFile := filepath.Join(t.TempDir(), "cleat-2408-mock-idp-cert.pem")
	if err := os.WriteFile(certFile, certPEM, 0644); err != nil {
		t.Fatalf("write mock IdP cert: %v", err)
	}

	// Egress, both layers (see doc comment): the tenant allowlist row for
	// the host, and the operator's private-address exemption for the same
	// host, since the mock binds loopback and the floor refuses that
	// regardless of the tenant's own list.
	if _, err := owner.ExecContext(ctx,
		`INSERT INTO admin.tenant_egress_allow (tenant_id, host) VALUES ($1, $2)`,
		tenantID, mockHostOnly); err != nil {
		t.Fatalf("seed tenant_egress_allow: %v", err)
	}
	t.Cleanup(func() {
		_, _ = owner.ExecContext(context.Background(),
			`DELETE FROM admin.tenant_egress_allow WHERE tenant_id = $1 AND host = $2`,
			tenantID, mockHostOnly)
	})

	// oauth_config, client secret, allowlist -- the same three fixtures
	// TestABootedWorkerBindsLoginToTheHost seeds, plus `issuer` (the oidc
	// provider's own column, unused by the hardcoded-endpoint providers that
	// test configures) and an oauth_allowed_identities row (neither existing
	// boot test needs one: this is the first to complete a login all the
	// way to the allowlist check).
	secrets := engine.NewPluginSecrets(engine.NewSecretStoreWithRing(owner, "postgres", ring))
	const clientSecret = "cleat-2408-client-secret"
	if err := secrets.ForTenant(tenantID).Put(ctx,
		oauthprovider.OAuthClientSecretName(provider), clientSecret); err != nil {
		t.Fatalf("seed client secret: %v", err)
	}
	t.Cleanup(func() {
		_, _ = owner.ExecContext(context.Background(),
			`DELETE FROM tenant_secrets WHERE tenant_id = $1 AND name = $2`,
			tenantID, oauthprovider.OAuthClientSecretName(provider))
	})

	if _, err := owner.ExecContext(ctx, `
		INSERT INTO oauth_config (tenant_id, provider, client_id, redirect_url, issuer, enabled)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		tenantID, provider, "cleat-2408-client-id", "http://localhost/oauth/oidc/callback",
		mockURL, true); err != nil {
		t.Fatalf("seed oauth_config: %v", err)
	}
	t.Cleanup(func() {
		_, _ = owner.ExecContext(context.Background(),
			`DELETE FROM oauth_config WHERE tenant_id = $1 AND provider = $2`, tenantID, provider)
	})

	if _, err := owner.ExecContext(ctx, `
		INSERT INTO oauth_allowed_identities (tenant_id, provider, identity_type, identity_value)
		VALUES ($1, $2, $3, $4)`,
		tenantID, provider, "email", testEmail); err != nil {
		t.Fatalf("seed oauth_allowed_identities: %v", err)
	}
	t.Cleanup(func() {
		_, _ = owner.ExecContext(context.Background(),
			`DELETE FROM oauth_allowed_identities WHERE tenant_id = $1 AND provider = $2 AND identity_value = $3`,
			tenantID, provider, testEmail)
	})

	wantIdentityTag := oauthprovider.OAuthIdentityTag(provider, "email", testEmail)
	t.Cleanup(func() {
		_, _ = owner.ExecContext(context.Background(),
			`DELETE FROM admin.tenant_api_keys WHERE tenant_id = $1 AND oauth_identity = $2`,
			tenantID, wantIdentityTag)
	})

	observe := func(base string) error {
		client := &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}

		loginResp, err := client.Get(base + "/oauth/" + provider + "/login?tenant_id=" + tenantID)
		if err != nil {
			return fmt.Errorf("GET /login: %w", err)
		}
		defer loginResp.Body.Close()
		loginBody, _ := io.ReadAll(loginResp.Body)
		if loginResp.StatusCode != http.StatusFound {
			return fmt.Errorf("GET /login: want 302, got %d: %s", loginResp.StatusCode, loginBody)
		}
		loc, err := url.Parse(loginResp.Header.Get("Location"))
		if err != nil {
			return fmt.Errorf("parse Location: %w", err)
		}
		state := loc.Query().Get("state")
		if state == "" {
			return fmt.Errorf("Location carries no state: %s", loc)
		}

		callbackResp, err := client.Get(base + "/oauth/" + provider + "/callback?code=cleat-2408-mock-code&state=" + state)
		if err != nil {
			return fmt.Errorf("GET /callback: %w", err)
		}
		defer callbackResp.Body.Close()
		callbackBody, _ := io.ReadAll(callbackResp.Body)
		if callbackResp.StatusCode != http.StatusOK {
			return fmt.Errorf("GET /callback: want 200, got %d: %s", callbackResp.StatusCode, callbackBody)
		}

		// A diagnostic deadline, not a timing assertion: finishLogin mints
		// synchronously on this same request, so the row should already be
		// there -- polling only absorbs any scheduling slack between the
		// response and this SELECT, the same shape the sibling boot tests
		// use for their own post-response checks.
		deadline := time.Now().Add(10 * time.Second)
		var count int
		for {
			if err := owner.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM admin.tenant_api_keys WHERE tenant_id = $1 AND oauth_identity = $2`,
				tenantID, wantIdentityTag).Scan(&count); err != nil {
				return fmt.Errorf("counting minted keys: %w", err)
			}
			if count > 0 {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("the callback returned 200 (body: %s) but no admin.tenant_api_keys row "+
					"carries oauth_identity = %q within 10s. Either pluginEnv's MintOAuthAPIKey assignment "+
					"is gone -- a nil mint function would make finishLogin itself fail, which this request "+
					"would also have shown as a non-200 -- or it is assigned but the real minter main.go "+
					"builds is returning success without writing a row", callbackBody, wantIdentityTag)
			}
			time.Sleep(200 * time.Millisecond)
		}
		if count != 1 {
			return fmt.Errorf("%d admin.tenant_api_keys rows carry oauth_identity = %q, want exactly 1 "+
				"-- a stale row from an earlier run that this test's cleanup did not reach, or a double mint",
				count, wantIdentityTag)
		}
		return nil
	}

	args := []string{"--driver=postgres", "--db=" + appDSN, "--migrate-db=" + ownerDSN,
		"--plugin-egress-allow-private=" + mockHostOnly}
	var key string
	var probeErr error
	ok, out := hostMatchServes(t, bin, args, &key, func(b, _ string) { probeErr = observe(b) },
		"SSL_CERT_FILE="+certFile)
	if !ok {
		t.Fatalf("the worker did not boot:\n%s", out)
	}
	if probeErr != nil {
		t.Fatalf("%v\n\nWorker output:\n%s", probeErr, out)
	}
}
