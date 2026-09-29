package eventtriggers

import (
	"context"
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

	// A modest, fast history: 200 processed rows for the target key, older
	// than the one unprocessed row a claim should find.
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
