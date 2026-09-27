package engine

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// The reaper reclaims anything whose heartbeat predates
// max(2*heartbeat, 10s). Any stall that outlasts that window ages EVERY
// running instance past it at once, because they all heartbeat through the
// same table -- a migration holding ACCESS EXCLUSIVE at worker boot, a
// failover, a paused volume. On release the sweep reclaimed the whole running
// set in one statement and every in-flight workflow replayed simultaneously,
// against a database that had just finished whatever stalled it.
//
// No data is lost; fencing sees to that. The cost is a self-inflicted
// thundering herd at the moment the database can least absorb one. cleat#1320.
//
// The bound changes the RATE of recovery, never whether it happens, and this
// asserts both halves: a bounded tick reclaims exactly the limit, and the
// remainder is reclaimed by the next one.
func TestTheReaperDoesNotReclaimEverythingAtOnce(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	ctx := context.Background()
	deployFaultTestDef(t, store)

	const total = 7
	const limit = 3

	// Seven running instances, each with a heartbeat old enough to be stale,
	// staggered so "oldest first" is observable. i=0 is the oldest.
	ids := make([]string, total)
	for i := range ids {
		ids[i] = fmt.Sprintf("reap-bound-%d-%d", time.Now().UnixNano(), i)
		if _, err := db.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input,
			                                assigned_to, heartbeat_at, tenant_id)
			VALUES ($1, 'test', 1, 'running', '{}', 'dead-worker',
			        now() - make_interval(secs => $2), $3)`,
			ids[i], 3600-i, store.tenantID); err != nil {
			t.Fatalf("seeding instance %d: %v", i, err)
		}
	}
	t.Cleanup(func() {
		for _, id := range ids {
			db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id)
		}
	})

	running := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM workflow_instances WHERE id = ANY($1) AND status = 'running'`,
			pgTextArray(ids)).Scan(&n); err != nil {
			t.Fatalf("counting running: %v", err)
		}
		return n
	}

	// The control. A sweep that reclaims `limit` rows and a sweep against a
	// table where only `limit` rows were ever stale are the same number.
	if got := running(); got != total {
		t.Fatalf("PRECONDITION FAILED: %d of %d seeded instances are running -- the bound below "+
			"would be measuring something else", got, total)
	}

	first, err := store.ReapStaleInstances(ctx, time.Second, limit)
	if err != nil {
		t.Fatalf("first reap: %v", err)
	}
	if first != limit {
		t.Errorf("the first tick reclaimed %d, want exactly the limit %d.\n\n"+
			"More than the limit means the bound does not bind and a whole-set stall still "+
			"reclaims everything at once; fewer means it is reclaiming less than it was "+
			"asked to.", first, limit)
	}
	if got, want := running(), total-limit; got != want {
		t.Errorf("%d instances still running after the first tick, want %d", got, want)
	}

	// Oldest first. Without this the bound is satisfied by an arbitrary
	// subset, and a genuinely long-dead workflow could be starved behind
	// fresher ones tick after tick.
	for i := 0; i < limit; i++ {
		var status string
		if err := db.QueryRowContext(ctx,
			`SELECT status FROM workflow_instances WHERE id = $1`, ids[i]).Scan(&status); err != nil {
			t.Fatalf("reading status of instance %d: %v", i, err)
		}
		if status == "running" {
			t.Errorf("instance %d (the %d-oldest heartbeat) is still running after a bounded "+
				"tick, so the sweep is not ordered by staleness", i, i+1)
		}
	}

	// And the rest follow. This is what makes the bound a rate limit rather
	// than a cap on how much is ever recovered.
	second, err := store.ReapStaleInstances(ctx, time.Second, limit)
	if err != nil {
		t.Fatalf("second reap: %v", err)
	}
	third, err := store.ReapStaleInstances(ctx, time.Second, limit)
	if err != nil {
		t.Fatalf("third reap: %v", err)
	}
	if got := running(); got != 0 {
		t.Errorf("%d instances still running after three bounded ticks (%d + %d + %d reclaimed); "+
			"the bound must delay recovery, not prevent it", got, first, second, third)
	}
}

// A limit of zero means unbounded, and that is what every existing caller and
// test passes. Asserted rather than assumed, because the alternative reading
// -- zero means "reclaim nothing" -- is a sweep that silently does nothing,
// which is the failure the bound exists to make visible rather than to cause.
func TestAZeroReapLimitIsUnbounded(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	ctx := context.Background()
	deployFaultTestDef(t, store)

	const total = 5
	ids := make([]string, total)
	for i := range ids {
		ids[i] = fmt.Sprintf("reap-unbounded-%d-%d", time.Now().UnixNano(), i)
		if _, err := db.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input,
			                                assigned_to, heartbeat_at, tenant_id)
			VALUES ($1, 'test', 1, 'running', '{}', 'dead-worker',
			        now() - interval '1 hour', $2)`, ids[i], store.tenantID); err != nil {
			t.Fatalf("seeding instance %d: %v", i, err)
		}
	}
	t.Cleanup(func() {
		for _, id := range ids {
			db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id)
		}
	})

	n, err := store.ReapStaleInstances(ctx, time.Second, 0)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n < total {
		t.Errorf("an unbounded reap reclaimed %d of %d seeded stale instances", n, total)
	}
}

// pgTextArray renders ids as a PostgreSQL text[] literal for = ANY($1).
func pgTextArray(ids []string) any {
	out := "{"
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += `"` + id + `"`
	}
	return out + "}"
}

// The bound is written three times in three dialects -- a LIMIT in a
// subquery on PostgreSQL, the same wrapped in a derived table on MySQL
// (which rejects LIMIT inside IN), and TOP (n) inside a subquery on SQL
// Server (whose UPDATE TOP takes no ORDER BY). None of those is a
// rewrite of the others, and a syntax error in any of them shows up only at
// runtime, on the dialect nobody ran.
func TestTheReapBoundBindsOnEveryDialect(t *testing.T) {
	for _, d := range []struct {
		dialect testutil.Dialect
		setup   func(*testing.T, *sql.DB)
		insert  string
	}{
		{testutil.DialectPostgres, func(t *testing.T, db *sql.DB) {
			testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
		}, `INSERT INTO workflow_instances (id, def_name, def_version, status, input,
		        assigned_to, heartbeat_at, tenant_id)
		    VALUES ($1, 'reap-bound', 1, 'running', '{}', 'dead', now() - interval '1 hour', $2)`},
		{testutil.DialectMySQL, testutil.SetupMySQLFullSchema,
			`INSERT INTO workflow_instances (id, def_name, def_version, status, input,
		        assigned_to, heartbeat_at, tenant_id)
		     VALUES (?, 'reap-bound', 1, 'running', '{}', 'dead', NOW(6) - INTERVAL 1 HOUR, ?)`},
		{testutil.DialectMSSQL, testutil.SetupMSSQLFullSchema,
			`INSERT INTO workflow_instances (id, def_name, def_version, status, input,
		        assigned_to, heartbeat_at, tenant_id)
		     VALUES (@p1, 'reap-bound', 1, 'running', '{}', 'dead', DATEADD(HOUR, -1, SYSUTCDATETIME()), @p2)`},
	} {
		t.Run(string(d.dialect), func(t *testing.T) {
			db := testutil.TestDB(t, d.dialect)
			defer db.Close()
			d.setup(t, db)

			admin := testutil.AdminDB(t, db, d.dialect)
			defStmt := map[testutil.Dialect]string{
				testutil.DialectPostgres: `INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id)
				                            VALUES ('reap-bound', 1, '', '` + DefaultTenantUUID + `')`,
				testutil.DialectMySQL: `INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id)
				                         VALUES ('reap-bound', 1, '', '` + DefaultTenantUUID + `')`,
				testutil.DialectMSSQL: `INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id)
				                         VALUES ('reap-bound', 1, 0x, '` + DefaultTenantUUID + `')`,
			}[d.dialect]
			if _, err := admin.Exec(defStmt); err != nil && !isDuplicateKey(err) {
				t.Fatalf("seeding workflow_defs on %s: %v", d.dialect, err)
			}

			const total, limit = 5, 2
			prefix := fmt.Sprintf("reap-bound-%s-%d-", d.dialect, time.Now().UnixNano())
			for i := 0; i < total; i++ {
				if _, err := admin.Exec(d.insert, prefix+fmt.Sprint(i), DefaultTenantUUID); err != nil {
					t.Fatalf("seeding instance %d on %s: %v", i, d.dialect, err)
				}
			}
			t.Cleanup(func() {
				for i := 0; i < total; i++ {
					admin.Exec(deleteInstanceSQL(d.dialect), prefix+fmt.Sprint(i))
				}
			})

			store := storeFor(t, d.dialect, db)
			n, err := store.ReapStaleInstances(context.Background(), time.Second, limit)
			if err != nil {
				t.Fatalf("bounded reap on %s: %v\n\n"+
					"Each dialect writes this bound differently and a syntax error in one of "+
					"them reaches nothing but that dialect's runtime.", d.dialect, err)
			}
			// >= limit rather than == : this database is shared with other
			// tests, so other stale rows may exist and be reclaimed first.
			// What must not happen is reclaiming MORE than the limit.
			if n > limit {
				t.Errorf("a bounded reap reclaimed %d on %s, want at most %d -- the bound does "+
					"not bind on this dialect", n, d.dialect, limit)
			}
			if n == 0 {
				t.Errorf("a bounded reap reclaimed nothing on %s, with %d stale instances seeded; "+
					"a bound that reclaims zero is the failure it exists to prevent", d.dialect, total)
			}
		})
	}
}

func deleteInstanceSQL(d testutil.Dialect) string {
	switch d {
	case testutil.DialectMySQL:
		return "DELETE FROM workflow_instances WHERE id = ?"
	case testutil.DialectMSSQL:
		return "DELETE FROM workflow_instances WHERE id = @p1"
	default:
		return "DELETE FROM workflow_instances WHERE id = $1"
	}
}
