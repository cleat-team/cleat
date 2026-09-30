package eventtriggers

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestVersion7ReplacesTheCorrelateIndexAcrossDialects is cleat#2669: the
// claim query's forced/preferred index, idx_ingested_events_correlate, had
// no `processed` column, so it could not exclude a processed row -- the
// claim walked, and on MySQL under REPEATABLE READ locked, every processed
// row for its key tuple before reaching the first unprocessed one. Since
// nothing deletes from ingested_events, that walk was the tenant's entire
// history of the event type for an unkeyed claim (today's only usage).
// Measured directly (not assumed) against real Postgres and SQL Server
// containers before this migration existed: 20,000 rows of processed
// history cost 2.5ms/193ms respectively, dropping to 0.46ms/5.5ms once a
// candidate index of this shape existed -- see migrations.go's Version 7
// comment for the full measurement.
//
// This test proves the SCHEMA change landed correctly on every dialect,
// reading each dialect's own catalog rather than trusting a nil error from
// RunMigrations: idx_ingested_events_correlate is gone,
// idx_ingested_events_claim exists, and it is PARTIAL/FILTERED on
// Postgres/MSSQL (excludes processed rows by construction, which is what
// the 5.5x/35x measurement above credits) and carries `processed` as a real
// column on MySQL (which has no partial index support at all).
func TestVersion7ReplacesTheCorrelateIndexAcrossDialects(t *testing.T) {
	for _, tc := range []struct {
		name string
		td   testutil.Dialect
	}{
		{"postgres", testutil.DialectPostgres},
		{"mysql", testutil.DialectMySQL},
		{"mssql", testutil.DialectMSSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.TestDB(t, tc.td)
			dialect := plugin.Dialect(string(tc.td))

			p := &Plugin{dialect: dialect}
			if err := plugin.RunMigrations(context.Background(), db, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("apply migrations: %v", err)
			}

			var correlateExists, claimExists bool
			var claimIsPartial bool
			var query1, query2, query3 string
			switch tc.td {
			case testutil.DialectPostgres:
				query1 = `SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE indexname = 'idx_ingested_events_correlate')`
				query2 = `SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE indexname = 'idx_ingested_events_claim')`
				query3 = `SELECT indpred IS NOT NULL FROM pg_index WHERE indexrelid = 'idx_ingested_events_claim'::regclass`
			case testutil.DialectMySQL:
				query1 = `SELECT COUNT(*) > 0 FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'ingested_events' AND index_name = 'idx_ingested_events_correlate'`
				query2 = `SELECT COUNT(*) > 0 FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'ingested_events' AND index_name = 'idx_ingested_events_claim'`
				// MySQL has no partial index -- the equivalent proof is that
				// `processed` is one of the index's own columns.
				query3 = `SELECT COUNT(*) > 0 FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'ingested_events' AND index_name = 'idx_ingested_events_claim' AND column_name = 'processed'`
			case testutil.DialectMSSQL:
				query1 = `SELECT CASE WHEN EXISTS(SELECT 1 FROM sys.indexes WHERE name = 'idx_ingested_events_correlate' AND object_id = OBJECT_ID('ingested_events')) THEN 1 ELSE 0 END`
				query2 = `SELECT CASE WHEN EXISTS(SELECT 1 FROM sys.indexes WHERE name = 'idx_ingested_events_claim' AND object_id = OBJECT_ID('ingested_events')) THEN 1 ELSE 0 END`
				query3 = `SELECT has_filter FROM sys.indexes WHERE name = 'idx_ingested_events_claim' AND object_id = OBJECT_ID('ingested_events')`
			}
			if err := db.QueryRowContext(context.Background(), query1).Scan(&correlateExists); err != nil {
				t.Fatalf("check idx_ingested_events_correlate: %v", err)
			}
			if correlateExists {
				t.Errorf("on %s, idx_ingested_events_correlate still exists -- Version 7 should have dropped it "+
					"(nothing else in this package references it; see migrations.go's Version 7 comment)", tc.name)
			}
			if err := db.QueryRowContext(context.Background(), query2).Scan(&claimExists); err != nil {
				t.Fatalf("check idx_ingested_events_claim: %v", err)
			}
			if !claimExists {
				t.Fatalf("on %s, idx_ingested_events_claim does not exist -- Version 7 did not create it", tc.name)
			}
			if err := db.QueryRowContext(context.Background(), query3).Scan(&claimIsPartial); err != nil {
				t.Fatalf("check idx_ingested_events_claim shape: %v", err)
			}
			if !claimIsPartial {
				wantDesc := "partial (WHERE NOT processed)"
				if tc.td == testutil.DialectMSSQL {
					wantDesc = "filtered (WHERE processed = 0)"
				} else if tc.td == testutil.DialectMySQL {
					wantDesc = "carrying processed as a column"
				}
				t.Errorf("on %s, idx_ingested_events_claim is not %s -- it cannot exclude processed rows "+
					"the way the Version 7 measurement depends on", tc.name, wantDesc)
			}
		})
	}
}

// TestClaimQueryPlanExcludesProcessedRowsOnPostgres is the same claim as
// above, proven behaviourally rather than by reading the catalog: EXPLAIN
// against a real mix of processed and unprocessed rows for one key must show
// ZERO rows removed by the "NOT processed" filter, because idx_ingested_events_claim
// excludes them from the index by construction -- a processed row is never a
// candidate the scan has to reject, unlike idx_ingested_events_correlate,
// which named every processed row in "Rows Removed by Filter" (measured at
// 20000 in migrations.go's Version 7 comment). A small, fast, deterministic
// count here (not a timing assertion, which would be flaky under CI load)
// is the structural signature of the fix.
//
// Postgres only: EXPLAIN's plan-text format is dialect-specific, and this is
// the one dialect where "Rows Removed by Filter" is a stable, parseable
// line. The catalog-shape test above covers all three dialects; this is the
// deeper, behavioural half for the one dialect it is cheap to parse on.
func TestClaimQueryPlanExcludesProcessedRowsOnPostgres(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	dialect := plugin.DialectPostgres

	p := &Plugin{dialect: dialect}
	if err := plugin.RunMigrations(context.Background(), db, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	p.db = &engine.SQLDBAdapter{DB: db, Dialect: dialect}

	tenantID := uuid.New()
	now := time.Now()
	seedCtx := plugin.ForTenant(context.Background(), tenantID)

	// 20,000 processed rows for the target key, older than the one
	// unprocessed row a claim should find -- the same order of magnitude
	// this file's own Version-7 comment measured (20,000 rows, 2.5ms/193ms
	// seq-scanned vs 0.46ms/5.5ms indexed) as the point at which the index
	// is cheaper for a REAL reason, not merely because default,
	// never-analyzed page-count estimates happen to favor it.
	//
	// This replaces a fixed count of 200, which cleat#2822 exposed as
	// fragile rather than wrong: at ~200 rows this table is small enough
	// that whether the planner prefers the index depends on whether
	// pg_class.reltuples for ingested_events has been set in this run (by
	// ANALYZE, VACUUM or an index build), and that is an accident of what
	// else has run in the shared database beforehand, not a property of
	// the index. In CI at cleat#2822 it was (reltuples=4, relpages=1) with
	// no ANALYZE/autoanalyze ever run (both NULL, job 110053370908),
	// consistent with v8's CREATE INDEX idx_ingested_events_dispatch
	// setting it as a side effect of building on a near-empty table --
	// the log shows stats set with no analyze, not which operation set
	// them. develop leaves it at -1, which happens to still favor the
	// index. 20,000 rows removes the dependency in both directions: the
	// index is genuinely cheaper at this size whether or not stats are
	// fresh.
	//
	// One batched INSERT ... SELECT rather than 20,000 round trips, which
	// at one row per exec would make this test slow enough that nobody
	// would want it at this size. tenant_id, event_type and key1 are
	// inlined as literals (test-generated values, not user input) so the
	// statement needs one bound parameter (a row count) rather than
	// 20,000 x N; id and received_at are computed per generated row.
	const n = 20000
	seedSQL := fmt.Sprintf(`
		INSERT INTO ingested_events (id, tenant_id, event_type, event_data, key1, received_at, processed, status)
		SELECT gen_random_uuid(), %s, 'order.paid', '{}', 'K-1',
		       $1::timestamptz - ((%d - g) * interval '1 second'), true, 'consumed'
		FROM generate_series(0, %d) AS g
	`, "'"+tenantID.String()+"'::uuid", n, n-1)
	if _, err := p.db.Exec(seedCtx, seedSQL, now); err != nil {
		t.Fatalf("seed %d processed rows: %v", n, err)
	}
	if _, err := p.db.Exec(seedCtx, `INSERT INTO ingested_events (id, tenant_id, event_type, event_data, key1, received_at, processed, status) VALUES ($1,$2,$3,$4,$5,$6,false,'pending')`,
		uuid.New(), tenantID, "order.paid", "{}", "K-1", now); err != nil {
		t.Fatalf("seed unprocessed row: %v", err)
	}

	tx, err := p.db.Begin(seedCtx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	explainSQL := "EXPLAIN (ANALYZE, FORMAT TEXT) " + queryOldestUnprocessedEventForClaim.For(dialect)
	rows, err := tx.Query(seedCtx, explainSQL, tenantID, "order.paid", "K-1", "", "")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()

	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan explain line: %v", err)
		}
		plan = append(plan, line)
	}

	// FLOOR: confirm this actually used idx_ingested_events_claim, not some
	// other index that would make the absence of "Rows Removed" meaningless
	// for the reason this test cares about.
	usedClaimIndex := false
	sawRowsRemoved := false
	for _, line := range plan {
		if contains(line, "idx_ingested_events_claim") {
			usedClaimIndex = true
		}
		if contains(line, "Rows Removed by Filter") {
			sawRowsRemoved = true
		}
	}
	if !usedClaimIndex {
		t.Fatalf("UNMEASURED: EXPLAIN did not name idx_ingested_events_claim -- this run says nothing "+
			"about that index's behaviour. Full plan:\n%s", joinLines(plan))
	}
	if sawRowsRemoved {
		t.Errorf("EXPLAIN shows rows removed by the NOT processed filter -- idx_ingested_events_claim is "+
			"not excluding processed rows structurally, which defeats the point of Version 7. Full plan:\n%s",
			joinLines(plan))
	}
}

// TestClaimQueryIsSargableOnProcessedOnMySQL is cleat-review's round-1 GAP on
// #2675: idx_ingested_events_claim puts `processed` between key3 and
// received_at (MySQL has no partial-index support, so `processed` has to be
// a real column in the key -- see migrations.go's Version 7 comment). The
// query's WHERE clause binds it with "processed = FALSE". A reasonable
// worry, raised without a MySQL environment to check it in: if MySQL's
// optimizer treated that as a post-scan FILTER rather than an equality KEY
// PART, the claim would seek to (tenant_id, event_type, key1, key2, key3)
// and then walk every row of that key's history -- read AND, under
// REPEATABLE READ, locked -- to find the first with processed = FALSE,
// which is exactly the bug this migration exists to remove, on the one
// dialect the issue was filed about.
//
// It does not: EXPLAIN's own "ref" column names SIX const-bound parts --
// tenant_id, event_type, key1, key2, key3, AND processed -- proving
// `processed` is consumed as part of the index SEEK, not a filter applied
// after it. "Extra" carries no "Using filesort": the received_at ordering
// this migration relies on (idx_ingested_events_claim: ..., key3, received_at)
// falls out of the index for free once processed is bound by equality,
// exactly as it did for the key columns alone before this migration existed.
func TestClaimQueryIsSargableOnProcessedOnMySQL(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectMySQL)
	dialect := plugin.DialectMySQL

	p := &Plugin{dialect: dialect}
	if err := plugin.RunMigrations(context.Background(), db, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	p.db = &engine.SQLDBAdapter{DB: db, Dialect: dialect}

	tenantID := uuid.New()
	now := time.Now()
	seedCtx := plugin.ForTenant(context.Background(), tenantID)

	// Same modest, fast history as the Postgres EXPLAIN test above.
	const n = 200
	for i := 0; i < n; i++ {
		if _, err := p.db.Exec(seedCtx, `INSERT INTO ingested_events (id, tenant_id, event_type, event_data, key1, received_at, processed, status) VALUES ($1,$2,$3,$4,$5,$6,true,'consumed')`,
			uuid.New(), tenantID, "order.paid", "{}", "K-1", now.Add(-time.Duration(n-i)*time.Second)); err != nil {
			t.Fatalf("seed processed row %d: %v", i, err)
		}
	}
	if _, err := p.db.Exec(seedCtx, `INSERT INTO ingested_events (id, tenant_id, event_type, event_data, key1, received_at, processed, status) VALUES ($1,$2,$3,$4,$5,$6,false,'pending')`,
		uuid.New(), tenantID, "order.paid", "{}", "K-1", now); err != nil {
		t.Fatalf("seed unprocessed row: %v", err)
	}

	// p.db.Query, not a bare db.QueryContext: p.db (engine.SQLDBAdapter) is
	// what rebinds queryOldestUnprocessedEventForClaim's "$1".."$5"
	// placeholders to MySQL's "?" -- the same reason every other query in
	// this file goes through p.db rather than the raw *sql.DB.
	//
	// Scanned POSITIONALLY, not by column name: plugin.Rows (p.db.Query's
	// return type) has no Columns() method -- it is a scoped interface for
	// plugins, not a mirror of *sql.Rows. MySQL 8's classic (non-ANALYZE)
	// EXPLAIN has a fixed 12-column shape: id, select_type, table,
	// partitions, type, possible_keys, key, key_len, ref, rows, filtered,
	// Extra. Several are nullable (partitions, possible_keys, key, key_len,
	// ref, filtered), hence sql.NullString rather than string.
	explainSQL := "EXPLAIN " + queryOldestUnprocessedEventForClaim.For(dialect)
	rows, err := p.db.Query(seedCtx, explainSQL, tenantID, "order.paid", "K-1", "", "")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatalf("UNMEASURED: EXPLAIN returned no rows")
	}
	var id, selType, table, partitions, typ, possibleKeys, key, keyLen, ref, rowsEst, filtered, extra sql.NullString
	if err := rows.Scan(&id, &selType, &table, &partitions, &typ, &possibleKeys, &key, &keyLen, &ref, &rowsEst, &filtered, &extra); err != nil {
		t.Fatalf("scan: %v", err)
	}
	row := map[string]string{
		"key": key.String, "ref": ref.String, "Extra": extra.String,
	}

	if row["key"] != "idx_ingested_events_claim" {
		t.Fatalf("UNMEASURED: EXPLAIN used key %q, not idx_ingested_events_claim -- this run says nothing "+
			"about that index's sargability. Full row: %+v", row["key"], row)
	}
	// FLOOR: six comma-separated "const" entries in `ref` means all six
	// leading columns -- tenant_id, event_type, key1, key2, key3, processed
	// -- are bound by equality as part of the index SEEK. Fewer than six
	// means processed (or an earlier column) is NOT part of the seek, and
	// whatever it filters on afterwards is exactly the walk this migration
	// removes.
	refParts := strings.Split(row["ref"], ",")
	if len(refParts) != 6 {
		t.Errorf("on mysql, EXPLAIN's ref column has %d const-bound part(s) (%q), want 6 (tenant_id, "+
			"event_type, key1, key2, key3, processed) -- processed is not being consumed as part of the "+
			"index seek, which reopens the walk-and-lock-the-history bug this migration exists to fix. "+
			"Full row: %+v", len(refParts), row["ref"], row)
	}
	if strings.Contains(row["Extra"], "Using filesort") {
		t.Errorf("on mysql, EXPLAIN's Extra contains %q -- the received_at ordering is not falling out of "+
			"the index for free, which means MySQL is materialising and sorting a candidate set rather than "+
			"seeking directly to the row wanted. Full row: %+v", row["Extra"], row)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}
