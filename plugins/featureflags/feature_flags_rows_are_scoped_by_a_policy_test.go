package featureflags

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestFeatureFlagsRowsAreScopedByAPolicyNotOnlyByTheQuery is cleat#1512.
//
// Every statement in this plugin already carries `WHERE tenant_id = $1`, and
// that is the reason this test exists rather than a reason it does not: a
// hand-written predicate is a convention, and the miss rate of a convention
// does not improve on its own. A policy is the thing that fails closed.
//
// IT CONNECTS AS PostgresRLSTestRole, AND THAT IS THE WHOLE TEST. The rest of
// this package's suite connects as the table owner or a superuser, and
// PostgreSQL lets both past a policy -- a superuser unconditionally, the owner
// unless the table is FORCEd. So every existing test here would pass against a
// table with no policy on it at all, which is exactly the state this plugin was
// in before this change. The precondition is asserted below rather than assumed,
// because a later change to the harness's connection would mute this test
// silently and it would keep reporting green.
//
// THE THIRD CASE IS THE ONE WITH TEETH, and the first two do not substitute for
// it. "A statement with no tenant is refused" and "a cross-tenant sweep sees
// everything" are both satisfied by a policy that is absent, wrong, or bypassed.
// Only a per-tenant read returning ONE row where TWO exist distinguishes a
// working policy from no policy -- and it only does so if the row that must be
// excluded was seeded first. Seeding just the visible row is the empty green
// cleat#1488 paid for: a USING clause over rows that do not exist is never
// evaluated, so it succeeds whether the policy is right, wrong or missing.
func TestFeatureFlagsRowsAreScopedByAPolicyNotOnlyByTheQuery(t *testing.T) {
	pg := postgresPluginBackend(t)
	defer pg.Cleanup()

	ctx := context.Background()
	loaded := []*plugin.LoadedPlugin{{Plugin: &Plugin{}, Healthy: true}}
	if err := plugin.RunMigrations(ctx, pg.DB, plugin.DialectPostgres, nil, loaded); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	// After the migrations, so the GRANT covers the table they created.
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

	// A KEY UNIQUE TO THIS RUN, so the counts below are about rows this test
	// seeded and not about whatever a previous run left behind. Measured the
	// hard way: with a fixed "shared-key" this passed on a pristine database
	// and reported "a cross-tenant sweep sees 4 rows, want 2" on a reused one,
	// which is a property of the database rather than of the policy -- the same
	// shape as cleat#1479.
	key := "scoped-" + uuid.NewString()

	t.Run("no tenant in context is refused, not silently emptied", func(t *testing.T) {
		_, err := db.Exec(context.Background(),
			`INSERT INTO feature_flags (tenant_id, id, key, enabled) VALUES ($1, $2, $3, true)`,
			tenantA.String(), uuid.New().String(), "k")
		if err == nil {
			t.Fatal("an INSERT with no tenant in context succeeded.\n\n" +
				"Nothing in the database is scoping feature_flags: either the policy " +
				"is absent, or the connection is exempt from it, or the adapter is " +
				"setting a tenant that was never in the context.")
		}
		if !strings.Contains(err.Error(), "cleat.tenant_id is not set") {
			t.Fatalf("refused, but not by the tenant policy: %v", err)
		}
	})

	// Seed BOTH tenants. tenantB's row is the one the policy has to exclude,
	// and it is written before anything is read -- see the header.
	for _, seed := range []struct {
		tenant uuid.UUID
		key    string
	}{{tenantA, key}, {tenantB, key}} {
		c := auth.WithTenantID(context.Background(), seed.tenant)
		if _, err := db.Exec(c,
			`INSERT INTO feature_flags (tenant_id, id, key, enabled) VALUES ($1, $2, $3, true)`,
			seed.tenant.String(), uuid.New().String(), seed.key); err != nil {
			t.Fatalf("seeding tenant %s: %v", seed.tenant, err)
		}
	}

	t.Run("a tenant sees its own row and not the other tenant's", func(t *testing.T) {
		var n int
		ctxA := auth.WithTenantID(context.Background(), tenantA)
		if err := db.QueryRow(ctxA,
			`SELECT count(*) FROM feature_flags WHERE key = $1`, key).Scan(&n); err != nil {
			t.Fatalf("count under tenant A: %v", err)
		}
		if n != 1 {
			t.Fatalf("tenant A sees %d rows for a key both tenants have, want 1.\n"+
				"  2 means the policy is not filtering -- it is absent, or this "+
				"connection is exempt from it.\n"+
				"  0 means it filtered on the wrong tenant.", n)
		}
	})

	t.Run("a cross-tenant sweep sees both, by a named bypass", func(t *testing.T) {
		// featureflags has no background loop and never takes this path. It is
		// asserted because the policy the runtime emits honours the bypass, and
		// a policy that refused it would break the next plugin to adopt.
		var n int
		sweep := plugin.AcrossAllTenants(context.Background(), "test: the policy admits a named bypass")
		if err := db.QueryRow(sweep,
			`SELECT count(*) FROM feature_flags WHERE key = $1`, key).Scan(&n); err != nil {
			t.Fatalf("count under a cross-tenant sweep: %v", err)
		}
		if n != 2 {
			t.Fatalf("a cross-tenant sweep sees %d rows, want 2", n)
		}
	})
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
