package kvstore

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

// kv_store is scoped by a database policy, not only by the WHERE clause each
// handler writes. cleat#1277.
//
// WHY THIS NEEDS ITS OWN ROLE. Every other kvstore test connects as the owner
// or a superuser, and PostgreSQL lets both bypass row-level security -- a
// superuser unconditionally, the owner unless the table is FORCEd. So the
// whole existing suite would pass against a table with no policy on it at all,
// which is precisely the state this repository was in before this change. The
// policy is only observable through PostgresRLSTestRole, and a test that
// skipped that step would be green and empty.
//
// WHY THE PREDICATE RAISES RATHER THAN FILTERING. cleat.assert_tenant_set()
// throws when cleat.tenant_id is unset. A policy that merely filtered would
// turn "no tenant context" into "no rows", and an empty result reads as
// absence of data rather than absence of authorisation -- the failure this
// exists to prevent, wearing the costume of a normal answer.
func TestKVStoreRowsAreScopedByAPolicyNotOnlyByTheQuery(t *testing.T) {
	pg := postgresBackend(t)
	defer pg.Cleanup()

	ctx := context.Background()
	loaded := []*plugin.LoadedPlugin{{Plugin: &Plugin{}, Healthy: true}}
	if err := plugin.RunMigrations(ctx, pg.DB, plugin.DialectPostgres, nil, loaded); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	// After the migrations, so the GRANT covers the table they created.
	rlsDB := testutil.OpenPostgresRLSTestDB(t, pg.DB)
	defer func() { _ = rlsDB.Close() }()

	db := &engine.SQLDBAdapter{DB: rlsDB, Dialect: plugin.DialectPostgres}
	tenantA, tenantB := uuid.New(), uuid.New()

	t.Run("a statement with no tenant set is refused", func(t *testing.T) {
		_, err := db.Exec(context.Background(),
			`INSERT INTO kv_store (tenant_id, key, value) VALUES ($1, $2, $3)`,
			tenantA.String(), "k", `"v"`)
		if err == nil {
			t.Fatal("an INSERT with no tenant in context succeeded.\n\n" +
				"Nothing in the database is scoping kv_store: either the policy " +
				"is absent, or the connection is exempt from it, or the adapter " +
				"is setting a tenant that was never in the context.")
		}
		if !strings.Contains(err.Error(), "cleat.tenant_id is not set") {
			t.Fatalf("refused, but not by the tenant policy: %v", err)
		}
	})

	t.Run("a statement with a tenant set is allowed", func(t *testing.T) {
		ctxA := auth.WithTenantID(context.Background(), tenantA)
		if _, err := db.Exec(ctxA,
			`INSERT INTO kv_store (tenant_id, key, value) VALUES ($1, $2, $3)`,
			tenantA.String(), "shared-key", `"from A"`); err != nil {
			t.Fatalf("insert under tenant A: %v", err)
		}

		var value string
		err := db.QueryRow(ctxA,
			`SELECT value::text FROM kv_store WHERE key = $1`, "shared-key").Scan(&value)
		if err != nil {
			t.Fatalf("reading back tenant A's own row: %v", err)
		}
		if value != `"from A"` {
			t.Errorf("read %q, want %q", value, `"from A"`)
		}
	})

	// The assertion that would have caught the original defect. Note that it
	// does not mention tenant_id: the query is deliberately unscoped, so the
	// only thing that can withhold the row is the policy.
	t.Run("one tenant cannot read another's row", func(t *testing.T) {
		ctxB := auth.WithTenantID(context.Background(), tenantB)

		var count int
		if err := db.QueryRow(ctxB,
			`SELECT count(*) FROM kv_store WHERE key = $1`, "shared-key").Scan(&count); err != nil {
			t.Fatalf("counting under tenant B: %v", err)
		}
		if count != 0 {
			t.Errorf("tenant B sees %d of tenant A's rows through a query that "+
				"names no tenant at all, want 0", count)
		}
	})

	// A tenant must not be able to write a row belonging to someone else
	// either. USING covers reads; without the policy applying to the write,
	// tenant B could plant rows under tenant A's id.
	t.Run("one tenant cannot write a row under another's id", func(t *testing.T) {
		ctxB := auth.WithTenantID(context.Background(), tenantB)
		_, err := db.Exec(ctxB,
			`INSERT INTO kv_store (tenant_id, key, value) VALUES ($1, $2, $3)`,
			tenantA.String(), "planted", `"from B"`)
		if err == nil {
			t.Error("tenant B inserted a row carrying tenant A's id")
		}
	})

	// FORCE is what binds the table's OWNER, and nothing above can see it.
	// cleat#1283.
	//
	// Every assertion so far runs as PostgresRLSTestRole, which is not the
	// owner -- and a non-owner is bound by ENABLE alone. Measured before this
	// subtest existed: deleting the FORCE statement from applyTenantScoping
	// left all four green, and the only guard that noticed was a unit test
	// matching the literal DDL string. A text comparison standing in for a
	// behavioural property.
	//
	// The distinction is not academic. The owner is whoever ran the
	// migrations, so in a deployment where the worker connects as that role,
	// ENABLE without FORCE means the plugin path walks straight through the
	// policy while pg_policy still shows a perfectly good one.
	//
	// Ownership is moved for the duration because that is the only way to ask
	// the question here: the real owner of these tables is a superuser, and a
	// superuser bypasses row-level security unconditionally, FORCE or not. So
	// a test that simply connected as the owner would measure the superuser
	// bypass and report nothing about FORCE.
	t.Run("the table's owner is bound by the policy too", func(t *testing.T) {
		if _, err := pg.DB.Exec(
			`ALTER TABLE kv_store OWNER TO ` + testutil.PostgresRLSTestRole); err != nil {
			t.Fatalf("hand kv_store to the RLS role: %v", err)
		}
		// Hand it back, or every later test in this package runs against a
		// table owned by a role the suite does not expect.
		defer func() {
			var owner string
			if err := pg.DB.QueryRow(`SELECT current_user`).Scan(&owner); err != nil {
				t.Fatalf("reading the owner to restore: %v", err)
			}
			if _, err := pg.DB.Exec(`ALTER TABLE kv_store OWNER TO ` + owner); err != nil {
				t.Fatalf("restore kv_store ownership to %s: %v", owner, err)
			}
		}()

		// Same statement as the first subtest, now issued by the owner.
		_, err := db.Exec(context.Background(),
			`INSERT INTO kv_store (tenant_id, key, value) VALUES ($1, $2, $3)`,
			tenantA.String(), "owner-write", `"from the owner"`)
		if err == nil {
			t.Fatal("the table's owner wrote a row with no tenant in context.\n\n" +
				"ENABLE ROW LEVEL SECURITY does not bind the owner; FORCE does. " +
				"Without it the policy is real, visible in pg_policy, and " +
				"silently inert for whichever role ran the migrations -- which " +
				"is the role a worker may well be connecting as.")
		}
		if !strings.Contains(err.Error(), "cleat.tenant_id is not set") {
			t.Fatalf("the owner was refused, but not by the tenant policy: %v", err)
		}
	})
}

// postgresBackend returns the PostgreSQL backend from the standard set.
//
// Fatal, not Skip, and scripts/check-skips.sh is right to insist: the
// precondition is always satisfiable here. NewPluginTestBackends always
// attempts PostgreSQL, and the TestDB inside it already distinguishes
// "no DSN configured" (skip, before this function returns) from "configured
// and unreachable" (fatal). So arriving at the end of this loop does not mean
// the environment is missing something -- it means the backend set stopped
// containing PostgreSQL, which is a structural change this test should report
// rather than quietly decline to run.
func postgresBackend(t *testing.T) testutil.PluginTestBackend {
	t.Helper()
	for _, be := range testutil.NewPluginTestBackends(t) {
		if plugin.Dialect(string(be.Dialect)) == plugin.DialectPostgres {
			return be
		}
		be.Cleanup()
	}
	t.Fatal("NewPluginTestBackends returned no PostgreSQL backend; it is " +
		"always attempted, so this is a change in the backend set rather " +
		"than an unset DSN")
	return testutil.PluginTestBackend{}
}
