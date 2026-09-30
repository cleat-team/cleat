package eventtriggers

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// seededPreV8Row names one of the four rows seedPreV8Rows writes, so the
// test can check which of ITS OWN rows the sweep selected rather than a
// bare count -- queryUnprocessedEvents is a genuinely cross-tenant, no-limit
// scan (mirroring processBatch), and this suite's other tests share the
// same database and also write ingested_events rows, so a bare row count
// is not this test's to claim.
type seededPreV8Row struct {
	id     uuid.UUID
	status string
}

// v7OnlyPlugin exposes only migrations 1-7, so a test can apply the schema
// AS IT EXISTED BEFORE Version 8, seed rows the way a real deployment's
// history would look, and only then apply Version 8 -- the only way to
// exercise a migration's backfill rather than its steady-state behaviour.
type v7OnlyPlugin struct{ *Plugin }

func (v v7OnlyPlugin) Migrations() []plugin.Migration {
	all := v.Plugin.Migrations()
	return all[:7]
}

// TestV8BackfillPreservesTheOldSweepSelection is the regression test for
// cleat-review's round-1 GAP on cleat#2663/#2822: Version 8 added
// dispatch_processed with no backfill, and queryUnprocessedEvents dropped
// the `status = 'pending' OR status IS NULL` predicate that used to be the
// ONLY thing excluding a row from the dispatch sweep. On a fresh database
// every row gets dispatch_processed correctly from birth, so no fresh-DB
// test -- including this package's other Version 8 tests -- can see the
// bug: a deployment upgrading from before Version 8 would have its entire
// ingested_events history default to dispatch_processed = false, and
// processBatch would re-dispatch every event it ever ingested, including
// ones an awaiter already consumed and ones already dead-lettered,
// restarting whatever workflows they had triggered.
//
// Measured before the fix (cleat-review, PG16, real v1-v7 SQL): develop's
// own predicate selects 1 of 4 seeded rows (the pending one); Version 8
// with no backfill selected all 4. This test seeds the same four states
// through real tenant-scoped writes (SQLDBAdapter.Exec, the same path
// production code uses -- not a raw *sql.DB.Exec, which a BLOCK security
// policy on Postgres/MSSQL would simply refuse) and asserts the SAME thing
// develop would have selected, after a real Version 8 migration with its
// backfill applies.
func TestV8BackfillPreservesTheOldSweepSelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		td   testutil.Dialect
	}{
		{"postgres", testutil.DialectPostgres},
		{"mysql", testutil.DialectMySQL},
		{"mssql", testutil.DialectMSSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sqlDB := testutil.TestDB(t, tc.td)
			dialect := plugin.Dialect(string(tc.td))
			real := &Plugin{dialect: dialect}
			db := &engine.SQLDBAdapter{DB: sqlDB, Dialect: dialect}

			if err := plugin.RunMigrations(context.Background(), sqlDB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: v7OnlyPlugin{real}, Healthy: true}}); err != nil {
				t.Fatalf("migrate to v7: %v", err)
			}

			tenantID := uuid.New()
			tenantCtx := plugin.ForTenant(context.Background(), tenantID)
			seeded := seedPreV8Rows(t, tenantCtx, db, tenantID)

			// Apply Version 8. Its backfill must run against the rows just
			// seeded, exactly as it would against a real deployment's
			// pre-existing history on upgrade.
			if err := plugin.RunMigrations(context.Background(), sqlDB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: real, Healthy: true}}); err != nil {
				t.Fatalf("migrate to v8: %v", err)
			}

			// queryUnprocessedEvents is a genuinely cross-tenant, no-limit
			// scan in production (processBatch runs it under
			// AcrossAllTenants) -- reading it through a bare, unscoped
			// connection would have RLS's FILTER predicate hide every row
			// and pass for the wrong reason. It is also, for that same
			// reason, not this test's alone to count: this suite's other
			// tests share the database and write their own ingested_events
			// rows, so the check below is keyed on the FOUR IDs this test
			// itself seeded, never on the result set's size.
			sweepCtx := plugin.AcrossAllTenants(context.Background(), "test: cross-tenant dispatch sweep")
			rows, err := db.Query(sweepCtx, queryUnprocessedEvents.For(dialect))
			if err != nil {
				t.Fatalf("queryUnprocessedEvents: %v", err)
			}
			defer rows.Close()
			selected := map[uuid.UUID]bool{}
			for rows.Next() {
				var (
					id, tid    uuid.UUID
					eventType  string
					eventData  []byte
					retryCount int
				)
				if err := plugin.ScanRow(rows, &id, &tid, &eventType, &eventData, &retryCount); err != nil {
					t.Fatalf("scan: %v", err)
				}
				selected[id] = true
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}

			// Exactly the pending row -- the one develop's own sweep would
			// also have selected. Selecting any terminal row means the
			// backfill missed it (the re-dispatch hazard); not selecting
			// the pending row means it was over-backfilled (a live event
			// silently never retried).
			for _, r := range seeded {
				want := r.status == "pending"
				if got := selected[r.id]; got != want {
					t.Errorf("%s: row seeded with status=%q, selected=%v, want %v -- "+
						"the backfill does not preserve develop's own selection",
						tc.name, r.status, got, want)
				}
			}
		})
	}
}

// seedPreV8Rows inserts one row per terminal state a real pre-Version-8
// deployment's ingested_events could hold, all older than the sweep's 10s
// cutoff, through the same tenant-scoped SQLDBAdapter.Exec path production
// code uses -- required on Postgres/MSSQL, where a raw, unscoped write is
// refused outright by the table's own BLOCK security policy.
func seedPreV8Rows(t *testing.T, ctx context.Context, db *engine.SQLDBAdapter, tenantID uuid.UUID) []seededPreV8Row {
	t.Helper()
	dialect := db.Dialect
	var receivedAtExpr string
	switch dialect {
	case plugin.DialectMySQL:
		receivedAtExpr = "NOW() - INTERVAL 1 HOUR"
	case plugin.DialectMSSQL:
		receivedAtExpr = "DATEADD(hour, -1, SYSUTCDATETIME())"
	default:
		receivedAtExpr = "NOW() - INTERVAL '1 hour'"
	}
	insertSQL := `INSERT INTO ingested_events (id, tenant_id, event_type, event_data, received_at, processed, status)
		VALUES ($1, $2, 't', '{}', ` + receivedAtExpr + `, $3, $4)`

	rows := []struct {
		processed bool
		status    string
	}{
		{true, "completed"},
		{true, "dead_letter"},
		{true, "consumed"},
		{false, "pending"},
	}
	seeded := make([]seededPreV8Row, 0, len(rows))
	for _, r := range rows {
		id := uuid.New()
		if _, err := db.Exec(ctx, insertSQL, id.String(), tenantID.String(), r.processed, r.status); err != nil {
			t.Fatalf("seed pre-v8 row (status=%s): %v", r.status, err)
		}
		seeded = append(seeded, seededPreV8Row{id: id, status: r.status})
	}
	return seeded
}
