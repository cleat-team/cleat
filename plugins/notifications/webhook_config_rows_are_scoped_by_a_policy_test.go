package notifications

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// TestWebhookConfigRowsAreScopedByAPolicyAndTheLoopSaysSo is cleat#1512.
//
// Every handler in this plugin already carries `WHERE tenant_id = $1`, and that
// is the reason this test exists rather than a reason it does not: a
// hand-written predicate is a convention, and a convention's miss rate does not
// improve on its own. A policy is the thing that fails closed.
//
// IT CONNECTS AS PostgresRLSTestRole, AND THAT IS THE WHOLE TEST. The rest of
// this package's suite connects as the table owner or a superuser, and
// PostgreSQL lets both past a policy -- a superuser unconditionally, the owner
// unless the table is FORCEd. So every existing test here would pass against a
// table with no policy on it at all, which is exactly the state this plugin was
// in before this change. The precondition is asserted rather than assumed.
//
// THE LAST ARM DRIVES Run ITSELF, and it is the only one that can see whether
// the MARKING is where it has to be: every other arm supplies its own marked
// context, so every other arm stays green with the AcrossAllTenants line
// deleted from Run. Falsified that way -- only the last arm fails, which is
// what localises a defect to the marking rather than to the policy.
//
// WHAT IT DELIBERATELY DOES NOT CLAIM. webhook_delivery is NOT covered, and
// cannot be by this mechanism: it has no tenant_id column, so the policy the
// runtime emits has nothing to filter on. Its isolation still rests on the
// joins in hand-written SQL. Asserting anything about it here would read as
// coverage this change does not provide.
func TestWebhookConfigRowsAreScopedByAPolicyAndTheLoopSaysSo(t *testing.T) {
	pg := postgresPluginBackend(t)
	defer pg.Cleanup()

	ctx := context.Background()
	loaded := []*plugin.LoadedPlugin{{Plugin: &Plugin{}, Healthy: true}}
	if err := plugin.RunMigrations(ctx, pg.DB, plugin.DialectPostgres, nil, loaded); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	// After the migrations, so the GRANT covers the tables they created.
	rlsDB := testutil.OpenPostgresRLSTestDB(t, pg.DB)
	defer func() { _ = rlsDB.Close() }()

	// PRECONDITION, CHECKED NOT ASSUMED. Both must be false or nothing below is
	// a measurement: PostgreSQL exempts a superuser from every policy, and
	// rolbypassrls does the same without the rest of superuser.
	var isSuper, canBypass bool
	if err := rlsDB.QueryRow(
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`,
	).Scan(&isSuper, &canBypass); err != nil {
		t.Fatalf("reading the connecting role's RLS exemptions: %v", err)
	}
	if isSuper || canBypass {
		t.Fatalf("UNMEASURED: connected as a role with rolsuper=%v rolbypassrls=%v, "+
			"which PostgreSQL exempts from row-level security. Every assertion below "+
			"would pass against a table with no policy at all.", isSuper, canBypass)
	}

	db := &engine.SQLDBAdapter{DB: rlsDB, Dialect: plugin.DialectPostgres}
	tenantA, tenantB := uuid.New(), uuid.New()

	// The plugin under test, wired to the RLS connection. Its HTTP client has a
	// short timeout because the delivery below goes to example.invalid and is
	// MEANT to fail: what the last arm watches for is attempt_count moving,
	// which happens on the failure path as well as the success one. The default
	// client's timeout would make the arm's 20s deadline a race.
	var logs bytes.Buffer
	secrets := plugintest.NewFakeSecrets()
	p := &Plugin{
		db:         db,
		dialect:    plugin.DialectPostgres,
		logger:     slog.New(slog.NewTextHandler(&logs, nil)),
		httpClient: &http.Client{Timeout: 500 * time.Millisecond},
		secrets:    secrets,
	}

	// A URL UNIQUE TO THIS RUN, so the counts below are about rows this test
	// seeded and not about whatever a previous run left in a shared database.
	// Measured on a sibling plugin: a fixed key passed on a pristine database
	// and reported "sees 4 rows, want 2" on a reused one, which is a property
	// of the database rather than of the policy -- the cleat#1479 shape.
	url := "https://example.invalid/" + uuid.NewString()

	t.Run("no tenant in context is refused, not silently emptied", func(t *testing.T) {
		_, err := db.Exec(context.Background(),
			`INSERT INTO webhook_config (tenant_id, id, url, secret_configured, events, enabled)
			 VALUES ($1, $2, $3, true, '[]', true)`,
			tenantA.String(), uuid.New().String(), url+"/unscoped")
		if err == nil {
			t.Fatal("an INSERT with no tenant in context succeeded.\n\n" +
				"Nothing in the database is scoping webhook_config: either the policy " +
				"is absent, or the connection is exempt from it, or the adapter is " +
				"setting a tenant that was never in the context.")
		}
		if !strings.Contains(err.Error(), "cleat.tenant_id is not set") {
			t.Fatalf("refused, but not by the tenant policy: %v", err)
		}
	})

	// Seed BOTH tenants. tenantB's row is the one the policy has to exclude
	// from the per-tenant read, and it is written BEFORE anything is read: a
	// USING clause over rows that do not exist is never evaluated, so a read
	// against an empty table succeeds whether the policy is right, wrong or
	// missing. That empty green is what cleat#1488 paid for.
	webhookOf := map[uuid.UUID]uuid.UUID{}
	for _, tenant := range []uuid.UUID{tenantA, tenantB} {
		id := uuid.New()
		webhookOf[tenant] = id
		c := auth.WithTenantID(context.Background(), tenant)
		// secret_configured is true: cleat#1992/#2172, owner decision (b),
		// made a signing secret mandatory, so every real row now carries one
		// and deliver() would refuse a false row outright rather than sign
		// with an empty key. The Plugin above carries a FakeSecrets, not a
		// real store -- this test's focus is the RLS-scoped db.Query/Exec
		// calls, not tenant_secrets, and a fake needs no DB round trip to
		// let the last arm's delivery reach its HTTP attempt.
		if _, err := db.Exec(c,
			`INSERT INTO webhook_config (tenant_id, id, url, secret_configured, events, enabled)
			 VALUES ($1, $2, $3, true, '[]', true)`,
			tenant.String(), id.String(), url); err != nil {
			t.Fatalf("seeding tenant %s: %v", tenant, err)
		}
		secrets.Seed(tenant.String(), WebhookSecretName(id), "test-secret")
	}

	t.Run("a tenant sees its own row and not the other tenant's", func(t *testing.T) {
		var n int
		ctxA := auth.WithTenantID(context.Background(), tenantA)
		if err := db.QueryRow(ctxA,
			`SELECT count(*) FROM webhook_config WHERE url = $1`, url).Scan(&n); err != nil {
			t.Fatalf("count under tenant A: %v", err)
		}
		if n != 1 {
			t.Fatalf("tenant A sees %d rows for a url both tenants have, want 1.\n"+
				"  2 means the policy is not filtering -- it is absent, or this "+
				"connection is exempt from it.\n"+
				"  0 means it filtered on the wrong tenant.", n)
		}
	})

	t.Run("the statement the loop depends on is refused on a bare context", func(t *testing.T) {
		// This is deliver()'s config lookup, verbatim: by delivery's webhook_id,
		// with no tenant predicate of its own, because a webhook_delivery row
		// does not know its tenant. It is the one statement in the loop that
		// needs the bypass -- queryDueDeliveries reads webhook_delivery, which
		// has no policy and would run unmarked.
		var gotURL, gotTenant string
		var gotSecretConfigured bool
		err := db.QueryRow(context.Background(),
			`SELECT url, tenant_id, secret_configured FROM webhook_config WHERE id = $1`,
			webhookOf[tenantA].String()).Scan(&gotURL, &gotTenant, &gotSecretConfigured)
		if err == nil {
			t.Fatal("deliver()'s config lookup succeeded with no tenant in context")
		}
		if !strings.Contains(err.Error(), "cleat.tenant_id is not set") {
			t.Fatalf("refused, but not by the tenant policy: %v", err)
		}
	})

	t.Run("Run marks its own loop, not just the context a test hands it", func(t *testing.T) {
		// Everything above would pass with the AcrossAllTenants line deleted
		// from Run, because everything above supplies the context itself. This
		// arm supplies only a plain context and lets Run do it -- and it
		// observes the marking through the production delivery path: a due
		// delivery whose config belongs to a tenant nobody named.
		deliveryID := uuid.New()
		sweep := plugin.AcrossAllTenants(context.Background(), "test: seeding a due delivery")
		if _, err := db.Exec(sweep,
			`INSERT INTO webhook_delivery (id, webhook_id, event_type, payload, status,
				attempt_count, next_attempt_at, created_at)
			 VALUES ($1, $2, 'test.event', '{}', 'pending', 0, now() - interval '1 minute', now())`,
			deliveryID.String(), webhookOf[tenantA].String()); err != nil {
			t.Fatalf("seeding a due delivery: %v", err)
		}

		restore := deliveryInterval
		deliveryInterval = 10 * time.Millisecond
		defer func() { deliveryInterval = restore }()

		runCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- p.Run(runCtx) }()

		// Polled rather than slept: a fixed sleep would have to be long enough
		// for the slowest machine and would still be a wall-clock assertion.
		// The delivery POST goes to example.invalid and cannot succeed, so what
		// is watched for is attempt_count moving off zero -- which only happens
		// once deliver() has read webhook_config, which is the statement the
		// marking exists for.
		deadline := time.Now().Add(20 * time.Second)
		attempts := 0
		for time.Now().Before(deadline) {
			attempts = deliveryAttempts(t, db, deliveryID)
			if attempts > 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		<-done

		if attempts == 0 {
			t.Fatalf("Run ran for 20s at a 10ms interval and the due delivery was " +
				"never attempted.\n\nThe loop is running and the delivery is due, " +
				"so deliver() is failing at its webhook_config lookup. That is what " +
				"an unmarked loop looks like: cleat.assert_tenant_set() RAISEs, the " +
				"error is logged, and the loop keeps ticking.")
		}
	})
}

// deliveryAttempts reads one delivery's attempt_count through a named bypass,
// because webhook_delivery is reached here without a tenant in scope.
func deliveryAttempts(t *testing.T, db *engine.SQLDBAdapter, id uuid.UUID) int {
	t.Helper()
	sweep := plugin.AcrossAllTenants(context.Background(), "test: reading a delivery's attempt count")
	var n int
	if err := db.QueryRow(sweep,
		`SELECT attempt_count FROM webhook_delivery WHERE id = $1`, id.String()).Scan(&n); err != nil {
		t.Fatalf("reading attempt_count: %v", err)
	}
	return n
}

// postgresPluginBackend returns the PostgreSQL backend and fails if there is
// not one. NewPluginTestBackends always attempts PostgreSQL, so its absence is
// a change in the backend set rather than an unset DSN -- which is why this
// fails rather than skipping.
func postgresPluginBackend(t *testing.T) testutil.PluginTestBackend {
	t.Helper()
	for _, be := range testutil.NewPluginTestBackends(t) {
		if plugin.Dialect(string(be.Dialect)) == plugin.DialectPostgres {
			return be
		}
		be.Cleanup()
	}
	t.Fatal("NewPluginTestBackends returned no PostgreSQL backend")
	return testutil.PluginTestBackend{}
}
