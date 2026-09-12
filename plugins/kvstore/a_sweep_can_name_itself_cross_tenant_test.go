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

// A plugin's background loop has no tenant and cannot get one, so against the
// fail-closed policy #1277 installed it does not read fewer rows -- it fails
// outright. plugin.AcrossAllTenants is the exemption it asks for by name.
// cleat#1278.
//
// WHAT MAKES THIS TEST ABLE TO FAIL, which is the whole difficulty with an RLS
// test and the reason #1284 existed. Four conditions have to hold together or
// the result is green and empty:
//
//   - Connect as PostgresRLSTestRole. The owner and any superuser bypass row
//     level security, so the existing kvstore suite would pass against a table
//     with no policy at all.
//   - Seed a row the policy is supposed to EXCLUDE. A USING clause is a
//     row-level predicate and is never evaluated against zero rows, so a
//     scoped read of an empty table succeeds whether the policy is right,
//     wrong, or absent. Both tenants are seeded before anything is asserted.
//   - Assert the bypassed read sees MORE than the scoped one. "The sweep
//     worked" is satisfied by a bypass that does nothing if the scoped read
//     would have returned the same rows anyway.
//   - Pin the pool to one connection, so the reuse case below is actually
//     exercised rather than hidden behind a second connection that never
//     served the bypass.
func TestASweepCanNameItselfCrossTenant(t *testing.T) {
	pg := postgresBackend(t)
	defer pg.Cleanup()

	ctx := context.Background()
	loaded := []*plugin.LoadedPlugin{{Plugin: &Plugin{}, Healthy: true}}
	if err := plugin.RunMigrations(ctx, pg.DB, plugin.DialectPostgres, nil, loaded); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	rlsDB := testutil.OpenPostgresRLSTestDB(t, pg.DB)
	defer func() { _ = rlsDB.Close() }()

	// One connection, so that "a later statement on a connection that served a
	// bypass" is the case under test rather than an accident of pooling.
	rlsDB.SetMaxOpenConns(1)

	db := &engine.SQLDBAdapter{DB: rlsDB, Dialect: plugin.DialectPostgres}
	tenantA, tenantB := uuid.New(), uuid.New()

	// Unique per run. The scratch database outlives a single `go test`, so a
	// fixed key accumulates a pair of rows per run and the cross-tenant counts
	// below climb by two each time -- which fails, but reading as "the bypass
	// saw too much" rather than as residue.
	sharedKey := "swept-" + uuid.NewString()
	for _, seed := range []struct {
		tenant uuid.UUID
		value  string
	}{{tenantA, `"from A"`}, {tenantB, `"from B"`}} {
		tctx := auth.WithTenantID(context.Background(), seed.tenant)
		if _, err := db.Exec(tctx,
			`INSERT INTO kv_store (tenant_id, key, value) VALUES ($1, $2, $3)`,
			seed.tenant.String(), sharedKey, seed.value); err != nil {
			t.Fatalf("seeding %s: %v", seed.tenant, err)
		}
	}

	countSharedKey := func(t *testing.T, ctx context.Context) (int, error) {
		t.Helper()
		var n int
		err := db.QueryRow(ctx,
			`SELECT count(*) FROM kv_store WHERE key = $1`, sharedKey).Scan(&n)
		return n, err
	}

	// The floor for every assertion below. If the scoped read could already
	// see both rows, a bypass that did nothing at all would pass the main
	// case, and if it could see neither, the seeds did not land.
	t.Run("a scoped read sees one tenant of the two", func(t *testing.T) {
		n, err := countSharedKey(t, auth.WithTenantID(context.Background(), tenantA))
		if err != nil {
			t.Fatalf("scoped read: %v", err)
		}
		if n != 1 {
			t.Fatalf("a read scoped to tenant A saw %d rows under key %q, want 1.\n\n"+
				"2 means the policy is not filtering and every assertion below is "+
				"comparing a bypass against nothing; 0 means the seeds did not "+
				"land and the bypass would be reading an empty table.", n, sharedKey)
		}
	})

	t.Run("an unnamed sweep is refused", func(t *testing.T) {
		_, err := countSharedKey(t, context.Background())
		if err == nil {
			t.Fatal("a read with no tenant and no bypass succeeded.\n\n" +
				"That is the state a plugin background loop is in. It has to " +
				"fail, or AcrossAllTenants is decoration: cross-tenant access " +
				"would be reachable by forgetting rather than by naming.")
		}
		if !strings.Contains(err.Error(), "cleat.tenant_id is not set") {
			t.Fatalf("refused, but not by the tenant policy: %v", err)
		}
	})

	t.Run("a named sweep sees every tenant", func(t *testing.T) {
		sweep := plugin.AcrossAllTenants(context.Background(),
			"kvstore test: a sweep is global by definition")
		n, err := countSharedKey(t, sweep)
		if err != nil {
			t.Fatalf("named sweep: %v", err)
		}
		if n != 2 {
			t.Fatalf("a named cross-tenant sweep saw %d rows, want 2", n)
		}
	})

	t.Run("a named sweep widens a request context rather than narrowing it", func(t *testing.T) {
		// An admin endpoint that rebuilds something for everyone runs on a
		// request context, so both markers are present. Resolving that the
		// other way round would scope the sweep to whoever called it and
		// return a plausible, wrong answer.
		both := plugin.AcrossAllTenants(
			auth.WithTenantID(context.Background(), tenantA),
			"kvstore test: admin endpoint sweeping on behalf of all tenants")
		n, err := countSharedKey(t, both)
		if err != nil {
			t.Fatalf("named sweep on a request context: %v", err)
		}
		if n != 2 {
			t.Fatalf("a named sweep on tenant A's context saw %d rows, want 2 -- "+
				"the tenant won over the explicit bypass", n)
		}
	})

	t.Run("an empty reason is an error, not a bypass", func(t *testing.T) {
		_, err := countSharedKey(t, plugin.AcrossAllTenants(context.Background(), "  "))
		if err == nil {
			t.Fatal("an empty reason was accepted as a cross-tenant bypass")
		}
		if strings.Contains(err.Error(), "cleat.tenant_id is not set") {
			t.Fatalf("an empty reason fell through to the fail-closed path: %v\n\n"+
				"It refuses the statement, which is right, but it names the "+
				"wrong cause: the author is told a tenant is missing from a "+
				"sweep that has no tenant to supply, and will go looking for "+
				"one instead of at the argument they passed.", err)
		}
		if !strings.Contains(err.Error(), "needs a reason") {
			t.Fatalf("refused, but not for the reason given: %v", err)
		}
	})

	// The bypass is transaction-local, and a reverted is_local setting reads as
	// the empty string rather than as NULL on that connection -- the behaviour
	// migration 034 exists to handle for cleat.tenant_id. The policy therefore
	// tests cleat.cross_tenant for emptiness rather than for presence. With the
	// pool pinned to one connection above, this is the same physical connection
	// that just served the sweep.
	t.Run("the bypass does not outlive its transaction", func(t *testing.T) {
		_, err := countSharedKey(t, context.Background())
		if err == nil {
			t.Fatal("after a bypassed sweep, an unscoped read on the same " +
				"connection succeeded.\n\n" +
				"The exemption followed the connection back into the pool, so " +
				"every later borrower of it is exempt too -- and which " +
				"statements those are depends on pool scheduling, so the leak " +
				"is intermittent by nature.")
		}
		// Refused is not enough: a closed pool or a dead connection would also
		// be an error here, and would make this subtest pass while measuring
		// nothing about the bypass at all.
		if !strings.Contains(err.Error(), "cleat.tenant_id is not set") {
			t.Fatalf("refused, but not by the tenant policy: %v", err)
		}
	})

	t.Run("a read-only plugin can sweep too", func(t *testing.T) {
		ro := &engine.ReadOnlyDB{Inner: rlsDB, Dialect: plugin.DialectPostgres}
		sweep := plugin.AcrossAllTenants(context.Background(),
			"kvstore test: read-only sweep")
		var n int
		if err := ro.QueryRow(sweep,
			`SELECT count(*) FROM kv_store WHERE key = $1`, sharedKey).Scan(&n); err != nil {
			t.Fatalf("read-only named sweep: %v", err)
		}
		if n != 2 {
			t.Fatalf("read-only sweep saw %d rows, want 2", n)
		}
		// set_config's is_local form is what makes this legal inside a READ
		// ONLY transaction; a session-level SET would not be. #1285 is where
		// the second adapter was found unscoped after #1280 covered only the
		// first, and a bypass wired into one of them would repeat it.
	})
}
