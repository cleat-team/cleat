package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestBlobIndexRowsAreScopedByAPolicyNotOnlyByTheQuery is cleat#1512.
//
// The name says NotOnlyByTheQuery where its siblings say AndTheSweepSaysSo,
// and now UNDERSTATES rather than overstates: the sweep is covered, by the
// last arm. Left as it is because the file is cited by name from cleat#1529
// and cleat#1528, and a name that understates costs a reader nothing while
// one that overstates is how a green run comes to mean the wrong thing.
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
// WHAT IS SCOPED IS THE NAMESPACE, NOT THE BYTES. blob_index is the only one of
// this plugin's three tables with a tenant_id. blob_content is content-addressed
// and deduplicated on purpose -- two tenants storing identical bytes share one
// row -- and workflow_blob_refs is keyed by workflow. Neither can carry this
// policy, and this test asserts nothing about them, so that "blobstore is
// tenant-scoped" is not read as more than the change does.
//
// THE LAST ARM DRIVES Run ITSELF, and it could not be written until cleat#1528
// was fixed. cleanupExpired's PHASE 1 reads workflow_instances, a core table
// whose policy is the older inline `tenant_id = cleat.assert_tenant_set()`
// form -- it RAISEs on an unset tenant and does NOT honour a named bypass, so
// the sweep died before reaching blob_index whatever the marking said. That was
// pre-existing, measured with blob_index carrying no policy at all:
//
//	cleanupExpired(context.Background()) -> pq: cleat.tenant_id is not set (P0001)
//
// Phase 1 now asks admin.in_flight_workflow_ids() instead, so the loop reaches
// its own table and the marking becomes observable. Every arm before this one
// supplies its own marked context and therefore stays green with the
// AcrossAllTenants line deleted from Run; only this one fails. That asymmetry
// is what localises a defect to the marking rather than to the policy.
func TestBlobIndexRowsAreScopedByAPolicyNotOnlyByTheQuery(t *testing.T) {
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

	// A KEY UNIQUE TO THIS RUN, so the counts below are about rows this test
	// seeded and not about whatever a previous run left in a shared database.
	// Measured on a sibling plugin: a fixed key passed on a pristine database
	// and reported "sees 4 rows, want 2" on a reused one -- a property of the
	// database rather than of the policy, the cleat#1479 shape.
	key := "blob-" + uuid.NewString()
	digest := sha256.Sum256([]byte(key))

	// blob_content has no tenant_id and therefore no policy, so this runs on a
	// bare context. blob_index.sha256 REFERENCES it, so it has to exist first.
	if _, err := db.Exec(context.Background(),
		`INSERT INTO blob_content (sha256, size, data, ref_count)
		 VALUES ($1, 4, '\x74657374'::bytea, 2)
		 ON CONFLICT (sha256) DO NOTHING`, digest[:]); err != nil {
		t.Fatalf("seeding blob_content: %v", err)
	}

	t.Run("no tenant in context is refused, not silently emptied", func(t *testing.T) {
		// A DIFFERENT KEY from the seed below. When this arm fails -- when the
		// insert is admitted because no policy is there -- the row it leaves
		// must not collide with the seed, or one missing policy produces one
		// verdict instead of four.
		_, err := db.Exec(context.Background(),
			`INSERT INTO blob_index (key, tenant_id, sha256, size) VALUES ($1, $2, $3, 4)`,
			key+"-unscoped", tenantA.String(), digest[:])
		if err == nil {
			t.Fatal("an INSERT with no tenant in context succeeded.\n\n" +
				"Nothing in the database is scoping blob_index: either the policy " +
				"is absent, or the connection is exempt from it, or the adapter is " +
				"setting a tenant that was never in the context.")
		}
		if !strings.Contains(err.Error(), "cleat.tenant_id is not set") {
			t.Fatalf("refused, but not by the tenant policy: %v", err)
		}
	})

	// Seed BOTH tenants, already expired so the sweep will claim them.
	// tenantB's row is the one the policy has to exclude from the per-tenant
	// read, and it is written BEFORE anything is read: a USING clause over rows
	// that do not exist is never evaluated, so a read against an empty table
	// succeeds whether the policy is right, wrong or missing. That empty green
	// is what cleat#1488 paid for.
	seed := func(t *testing.T) {
		t.Helper()
		for _, tenant := range []uuid.UUID{tenantA, tenantB} {
			c := auth.WithTenantID(context.Background(), tenant)
			if _, err := db.Exec(c,
				`INSERT INTO blob_index (key, tenant_id, sha256, size, expires_at)
				 VALUES ($1, $2, $3, 4, now() - interval '1 hour')
				 ON CONFLICT (tenant_id, key) DO NOTHING`,
				key, tenant.String(), digest[:]); err != nil {
				t.Fatalf("seeding tenant %s: %v", tenant, err)
			}
		}
	}
	seed(t)

	t.Run("a tenant sees its own row and not the other tenant's", func(t *testing.T) {
		var n int
		ctxA := auth.WithTenantID(context.Background(), tenantA)
		if err := db.QueryRow(ctxA,
			`SELECT count(*) FROM blob_index WHERE key = $1`, key).Scan(&n); err != nil {
			t.Fatalf("count under tenant A: %v", err)
		}
		if n != 1 {
			t.Fatalf("tenant A sees %d rows for a key both tenants have, want 1.\n"+
				"  2 means the policy is not filtering -- it is absent, or this "+
				"connection is exempt from it.\n"+
				"  0 means it filtered on the wrong tenant.", n)
		}
	})

	t.Run("the production expiry statement is refused bare and admitted marked", func(t *testing.T) {
		// deleteChunksReturning is cleanupExpired's phase 2, verbatim and by
		// reference rather than retyped -- a copy would keep passing after the
		// original changed. It is deliberately NOT reached through
		// cleanupExpired: that function dies in phase 1 on workflow_instances
		// (cleat#1528), so an assertion through it would pass for a reason that
		// has nothing to do with blob_index. A test that fails for the right
		// verdict and the wrong reason is the one this repo keeps paying for.
		stmt := plugin.Rebind(deleteChunksReturning.For(plugin.DialectPostgres), plugin.DialectPostgres)

		_, err := db.Exec(context.Background(), stmt)
		if err == nil {
			t.Fatal("the expiry statement ran with no tenant in context")
		}
		if !strings.Contains(err.Error(), "cleat.tenant_id is not set") {
			t.Fatalf("refused, but not by the tenant policy: %v", err)
		}
		assertSeededRowsRemaining(t, db, key, 2)

		sweep := plugin.AcrossAllTenants(context.Background(),
			"test: the expiry statement is global for the same reason Run says it is")
		if _, err := db.Exec(sweep, stmt); err != nil {
			t.Fatalf("the expiry statement was refused even when named cross-tenant, "+
				"so the policy does not admit the bypass this plugin relies on: %v", err)
		}
		// Both tenants' expired rows, not just the caller's -- which is what
		// separates a working bypass from a policy that is simply absent.
		assertSeededRowsRemaining(t, db, key, 0)
	})

	t.Run("Run marks its own sweep, not just the context a test hands it", func(t *testing.T) {
		// Arm 3 consumed the seeded rows, so put them back. The point of this
		// arm is that it supplies a PLAIN context and lets Run mark it.
		seed(t)
		assertSeededRowsRemaining(t, db, key, 2)

		// CAPTURED, NOT DISCARDED. cleanupExpired's error is the only thing
		// that says WHY the sweep swept nothing, and Run logs it rather than
		// returning it -- so discarding the logger leaves this test able to
		// report "rows are still there" and nothing else. Two different causes
		// (a missing table grant and a missing schema grant, cleat#1490)
		// produce byte-identical failures without it, and diagnosing either
		// meant re-running with the logger rewired by hand.
		var sweepLog syncBuffer
		p := &Plugin{
			db:      db,
			dialect: plugin.DialectPostgres,
			logger:  slog.New(slog.NewTextHandler(&sweepLog, nil)),
		}

		restore := cleanupInterval
		cleanupInterval = 10 * time.Millisecond
		defer func() { cleanupInterval = restore }()

		runCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- p.Run(runCtx) }()

		// Polled rather than slept: a fixed sleep would have to be long enough
		// for the slowest machine and would still be a wall-clock assertion.
		deadline := time.Now().Add(20 * time.Second)
		var remaining int
		for time.Now().Before(deadline) {
			remaining = countSeededRows(t, db, key)
			if remaining == 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		<-done

		if remaining != 0 {
			// The log, not a guess at the cause. An earlier version of this
			// message asserted that the sweep was unmarked and
			// assert_tenant_set() was raising -- true when it was written, and
			// wrong for both of the causes cleat#1490 introduced. What the
			// sweep actually logged is the evidence; the rest is a hint.
			t.Fatalf("Run swept for 20s at a 10ms interval and %d of this run's "+
				"expired rows are still there.\n\nWhat the sweep logged:\n%s\n"+
				"If that is empty the loop never ran; if it names a refused "+
				"statement, read the refusal rather than assuming the sweep is "+
				"unmarked.", remaining, sweepLog.String())
		}
	})
}

func assertSeededRowsRemaining(t *testing.T, db *engine.SQLDBAdapter, key string, want int) {
	t.Helper()
	if n := countSeededRows(t, db, key); n != want {
		t.Fatalf("%d of this run's rows remain, want %d", n, want)
	}
}

// countSeededRows counts this run's rows across both tenants, through a named
// bypass so the count is of what EXISTS rather than of what one tenant can see.
func countSeededRows(t *testing.T, db *engine.SQLDBAdapter, key string) int {
	t.Helper()
	sweep := plugin.AcrossAllTenants(context.Background(), "test: counting what exists")
	var n int
	if err := db.QueryRow(sweep,
		`SELECT count(*) FROM blob_index WHERE key = $1`, key).Scan(&n); err != nil {
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

// syncBuffer is a bytes.Buffer safe for the sweep goroutine to write while the
// test goroutine reads it. slog handlers do not serialise access to their
// io.Writer, and Run logs from a goroutine this test starts, so an unguarded
// buffer is a data race the race detector fails on rather than a flake.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
