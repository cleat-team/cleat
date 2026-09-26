package engine

import (
	"context"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestPartitionedEventHistorySizeIsNotZero asserts that EstimateEventHistorySize
// reports a non-zero event store once event_history holds a row.
//
// THE REGRESSION IT PINS. pg_total_relation_size against a partitioned parent
// returns 0: a partitioned table stores no rows itself and the function does
// not descend into its children. So once event_history was hash-partitioned on
// tenant_id (cleat#2059), the query this method used to run -- one bare
// pg_total_relation_size('event_history') -- reported 0 bytes forever.
//
// Nothing fails when that happens. The statement is valid, the column is
// unchanged, no RLS policy is evaluated (this reads catalog metadata, not rows),
// and the value lands in a gauge: cmd/cleat-worker/setup.go's metrics sweep
// passes it to SetEventHistorySize beside CountEventHistoryTotal, whose number
// stays correct because it does a real COUNT. The observable is a dashboard
// showing rows and zero bytes, and only if someone cross-checks.
//
// Measured 2026-09-26 on a four-way-partitioned table of 1000 rows: 0 through
// the parent, 131072 summed over the children.
//
// The emptiness trap this test avoids is the one CLAUDE.md records for the RLS
// read-side assertion: a size query over an EMPTY table is not a control,
// because per-partition page accounting alone is non-zero and a broken sum of
// zero-sized parts can still look plausible. The seed is what makes the reading
// mean anything, so the test fails loudly if it did not land.
func TestPartitionedEventHistorySizeIsNotZero(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)
	defer testutil.CleanupPostgresTestData(t, db)

	ctx := context.Background()
	const tenant = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"

	if _, err := db.Exec(
		`INSERT INTO event_history (workflow_id, step, event_type, service, operation, tenant_id)
		 VALUES ('partitioned-size-probe', 1, 'call', 'svc', 'op', $1)`, tenant); err != nil {
		t.Fatalf("UNMEASURED: seeding event_history failed, so a zero below would "+
			"be indistinguishable from an unseeded table: %v", err)
	}

	// State the shape being measured rather than assuming it. On an
	// unpartitioned event_history this test is still a valid statement about
	// the size call, but it is no longer evidence about the partition sum --
	// and the difference must be visible in the output, not inferred.
	var relkind string
	if err := db.QueryRow(`SELECT relkind::text FROM pg_class WHERE relname = 'event_history'`).
		Scan(&relkind); err != nil {
		t.Fatalf("reading event_history's relkind: %v", err)
	}
	t.Logf("event_history relkind=%s ('p' is partitioned, 'r' is an ordinary table)", relkind)

	got, err := NewPostgresStore(db).EstimateEventHistorySize(ctx)
	if err != nil {
		t.Fatalf("EstimateEventHistorySize: %v", err)
	}
	if got <= 0 {
		t.Errorf("EstimateEventHistorySize returned %d for a table with a row in it. "+
			"On a partitioned parent a single pg_total_relation_size reports 0, and "+
			"this figure reaches a gauge -- it would read as an empty event store, "+
			"with no error, on every partitioned deployment.", got)
	}

	if relkind != "p" {
		return // not the shape this test exists for; say nothing it did not measure
	}

	// A SECOND DERIVATION, through a different catalog function. pg_partition_tree
	// rather than a pg_inherits recursion, so agreement is not the same query run
	// twice. (It is only usable here: it returns zero rows for a table that is not
	// partitioned, which is exactly why the production query cannot use it.)
	var want int64
	if err := db.QueryRow(`
		SELECT COALESCE(SUM(pg_total_relation_size(c.relid)), 0)
		  FROM pg_partition_tree('event_history') AS c`).Scan(&want); err != nil {
		t.Fatalf("UNMEASURED: the cross-check query failed, so nothing was compared: %v", err)
	}
	if want <= 0 {
		t.Fatalf("UNMEASURED: the cross-check itself read %d, so it cannot confirm "+
			"or refute anything. The partition sum is what this test is about.", want)
	}
	if got != want {
		t.Errorf("EstimateEventHistorySize reported %d; summing event_history's "+
			"partition tree independently gives %d", got, want)
	}
}
