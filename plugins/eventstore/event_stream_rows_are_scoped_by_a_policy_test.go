package eventstore

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestEventStreamRowsAreScopedByAPolicyAndTheSweepSaysSo is cleat#1512.
//
// Every statement in this plugin already carries `WHERE tenant_id = $1`, and
// that is the reason this test exists rather than a reason it does not: a
// hand-written predicate is a convention, and a convention's miss rate does not
// improve on its own. A policy is the thing that fails closed.
//
// IT CONNECTS AS PostgresRLSTestRole, AND THAT IS THE WHOLE TEST. The rest of
// this package's suite connects as the table owner or a superuser, and
// PostgreSQL lets both past a policy -- a superuser unconditionally, the owner
// unless the table is FORCEd. So every existing test here would pass against a
// table with no policy on it at all, which is exactly the state this plugin was
// in before this change. The precondition is asserted rather than assumed,
// because a later change to the harness's connection would mute this test
// silently and it would keep reporting green.
//
// THE SWEEP ARMS RUN THE PRODUCTION FUNCTION, not a re-declared copy of it.
// p.cleanup is what Run calls on every tick, and it is exercised twice: once on
// a bare context, which must be REFUSED, and once on a context named through
// plugin.AcrossAllTenants, which must reach both tenants. The first arm is the
// trap cleat#1512 describes -- an unmarked loop does not degrade, it fails
// outright -- demonstrated on the real function rather than argued.
//
// AND THE LAST ARM DRIVES Run ITSELF, which is the only one that can see
// whether the MARKING is where it has to be. The first draft stopped at
// p.cleanup and said so in this header: at an hour the ticker cannot fire in a
// test, so removing the AcrossAllTenants line from Run left the whole file
// green. A confession is better than a false claim and worse than a test, so
// background.go's interval became a var and the last arm drives the real loop.
// Falsified by deleting that line: the arm fails, and the three before it still
// pass -- which is what localises the defect to the marking rather than to the
// policy.
func TestEventStreamRowsAreScopedByAPolicyAndTheSweepSaysSo(t *testing.T) {
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

	// A STREAM UNIQUE TO THIS RUN. The counts below are about rows this test
	// seeded, not about whatever a previous run left in a shared database --
	// and the retention DELETE is deliberately unscoped by stream, so a count
	// of rows it affected would include strangers. Measured on the sibling
	// plugin: a fixed key passed on a pristine database and reported "sees 4
	// rows, want 2" on a reused one, which is the cleat#1479 shape.
	streamID := "es-" + uuid.NewString()

	// The plugin under test, wired to the RLS connection. Its logger is
	// captured because cleanup() reports a failed sweep by LOGGING it and
	// returning 0 -- a refusal is not visible in its return value.
	var logs bytes.Buffer
	p := &Plugin{
		db:      db,
		dialect: plugin.DialectPostgres,
		logger:  slog.New(slog.NewTextHandler(&logs, nil)),
		config:  Config{RetentionDays: 1},
	}

	t.Run("no tenant in context is refused, not silently emptied", func(t *testing.T) {
		// sequence 99 rather than 1, so that when this arm FAILS -- when the
		// insert is admitted because no policy is there -- the row it leaves
		// does not collide with the seed below. Measured: with sequence 1 the
		// falsification aborted at "duplicate key violates event_stream_pkey"
		// and the three arms after it never reported at all, so one missing
		// policy produced one verdict instead of four.
		_, err := db.Exec(context.Background(),
			`INSERT INTO event_stream (tenant_id, stream_id, sequence, event, created_at)
			 VALUES ($1, $2, 99, '{}', now())`,
			tenantA.String(), streamID)
		if err == nil {
			t.Fatal("an INSERT with no tenant in context succeeded.\n\n" +
				"Nothing in the database is scoping event_stream: either the policy " +
				"is absent, or the connection is exempt from it, or the adapter is " +
				"setting a tenant that was never in the context.")
		}
		if !strings.Contains(err.Error(), "cleat.tenant_id is not set") {
			t.Fatalf("refused, but not by the tenant policy: %v", err)
		}
	})

	// Seed BOTH tenants, with a created_at old enough for the retention sweep
	// to claim. tenantB's row is the one the policy has to exclude from the
	// per-tenant read, and it is written BEFORE anything is read: a USING
	// clause over rows that do not exist is never evaluated, so a read against
	// an empty table succeeds whether the policy is right, wrong or missing.
	// That empty green is what cleat#1488 paid for.
	for _, tenant := range []uuid.UUID{tenantA, tenantB} {
		c := auth.WithTenantID(context.Background(), tenant)
		if _, err := db.Exec(c,
			`INSERT INTO event_stream (tenant_id, stream_id, sequence, event, created_at)
			 VALUES ($1, $2, 1, '{}', now() - interval '30 days')`,
			tenant.String(), streamID); err != nil {
			t.Fatalf("seeding tenant %s: %v", tenant, err)
		}
	}

	t.Run("a tenant sees its own row and not the other tenant's", func(t *testing.T) {
		var n int
		ctxA := auth.WithTenantID(context.Background(), tenantA)
		if err := db.QueryRow(ctxA,
			`SELECT count(*) FROM event_stream WHERE stream_id = $1`, streamID).Scan(&n); err != nil {
			t.Fatalf("count under tenant A: %v", err)
		}
		if n != 1 {
			t.Fatalf("tenant A sees %d rows for a stream both tenants have, want 1.\n"+
				"  2 means the policy is not filtering -- it is absent, or this "+
				"connection is exempt from it.\n"+
				"  0 means it filtered on the wrong tenant.", n)
		}
	})

	t.Run("the production sweep on a bare context is refused, not silently empty", func(t *testing.T) {
		logs.Reset()
		if n := p.cleanup(context.Background()); n != 0 {
			t.Fatalf("an unmarked sweep deleted %d rows; it must be refused by the policy", n)
		}
		if !strings.Contains(logs.String(), "cleat.tenant_id is not set") {
			t.Fatalf("the unmarked sweep returned 0, but not because the policy "+
				"refused it -- 0 is also what a sweep that matched nothing "+
				"returns, and the two are the same value. Log was:\n%s", logs.String())
		}
		// And it really did nothing: the rows are still there.
		assertSeededRowsRemaining(t, db, streamID, 2)
	})

	t.Run("the production sweep, named cross-tenant, reaches both", func(t *testing.T) {
		sweep := plugin.AcrossAllTenants(context.Background(),
			"test: the retention sweep is global for the same reason Run says it is")
		// The return value counts every tenant's expired rows, including any a
		// concurrent test seeded, so it is not asserted on. What is asserted is
		// that THIS run's two rows are gone.
		p.cleanup(sweep)
		assertSeededRowsRemaining(t, db, streamID, 0)
	})

	t.Run("Run marks its own sweep, not just the context a test hands it", func(t *testing.T) {
		// Everything above would pass with the AcrossAllTenants line deleted
		// from Run, because everything above supplies the marked context
		// itself. This arm supplies only a plain context and lets Run do it.
		for _, tenant := range []uuid.UUID{tenantA, tenantB} {
			c := auth.WithTenantID(context.Background(), tenant)
			if _, err := db.Exec(c,
				`INSERT INTO event_stream (tenant_id, stream_id, sequence, event, created_at)
				 VALUES ($1, $2, 2, '{}', now() - interval '30 days')`,
				tenant.String(), streamID); err != nil {
				t.Fatalf("re-seeding tenant %s: %v", tenant, err)
			}
		}
		assertSeededRowsRemaining(t, db, streamID, 2)

		restore := cleanupInterval
		cleanupInterval = 10 * time.Millisecond
		defer func() { cleanupInterval = restore }()

		runCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- p.Run(runCtx) }()

		// Polled rather than slept. A fixed sleep would have to be long enough
		// for the slowest machine and would still be a wall-clock assertion;
		// this one fails on the CONDITION and reports what it saw.
		deadline := time.Now().Add(10 * time.Second)
		var remaining int
		for time.Now().Before(deadline) {
			remaining = countSeededRows(t, db, streamID)
			if remaining == 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		<-done

		if remaining != 0 {
			t.Fatalf("Run swept for 10s and %d of this run's expired rows are still "+
				"there.\n\nThe loop is running -- the ticker fires every 10ms here -- "+
				"so the statement is being refused. That is what an unmarked sweep "+
				"looks like: cleat.assert_tenant_set() RAISEs, cleanup() logs and "+
				"returns 0, and the loop keeps ticking.", remaining)
		}
	})
}

// assertSeededRowsRemaining counts this run's rows across both tenants, through
// a named bypass so the count is of what EXISTS rather than of what one tenant
// can see.
func assertSeededRowsRemaining(t *testing.T, db *engine.SQLDBAdapter, streamID string, want int) {
	t.Helper()
	if n := countSeededRows(t, db, streamID); n != want {
		t.Fatalf("%d of this run's rows remain, want %d", n, want)
	}
}

// countSeededRows counts this run's rows across both tenants.
func countSeededRows(t *testing.T, db *engine.SQLDBAdapter, streamID string) int {
	t.Helper()
	sweep := plugin.AcrossAllTenants(context.Background(), "test: counting what exists")
	var n int
	if err := db.QueryRow(sweep,
		`SELECT count(*) FROM event_stream WHERE stream_id = $1`, streamID).Scan(&n); err != nil {
		t.Fatalf("counting remaining rows: %v", err)
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
