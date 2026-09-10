// The batch query, executed on each database it can reach.
//
// cleat#1133 part 4. processBatch issued one raw literal to all three backends
// and it was valid on exactly one of them. Three independent faults in a single
// statement:
//
//	NOT e.processed                     T-SQL: Msg 4145, non-boolean type
//	NOW() - INTERVAL '10 seconds'       MySQL: Error 1064, syntax
//	LIMIT 100                           T-SQL: no LIMIT
//
// WHY THE ERROR MESSAGE WOULD NOT HAVE LED ANYONE HERE. Measured against SQL
// Server 2022 while writing this:
//
//	NOT processed, no OFFSET/FETCH    -> Msg 4145 (the cause)
//	NOT processed, with OFFSET/FETCH  -> Msg 4145 AND Msg 153, and the Go
//	                                     driver surfaces only the LAST:
//	                                     "Invalid usage of the option NEXT in
//	                                     the FETCH statement"
//
// So the reported error names the row-limit clause, which is correct T-SQL,
// while the defect is the boolean above it. A worker log census that groups by
// error text therefore attributes these to the wrong construct -- the message
// is the consequence, not the cause.
//
// And none of it fails an assertion anywhere: processBatch logs and returns, so
// a statement no database accepts is indistinguishable from "no pending events"
// to every caller and to a green nightly.
package webhookingest

import (
	"context"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

func TestTheBatchQueryRunsOnEveryDialect(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			ctx := context.Background()
			dialect := plugin.Dialect(be.Dialect)
			p := &Plugin{dialect: dialect}

			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("webhookingest migrations on %s: %v", be.Name, err)
			}

			sqlText := plugin.Rebind(queryUnprocessedWebhookEvents.For(dialect), dialect)
			rows, err := be.DB.QueryContext(ctx, sqlText)
			if err != nil {
				t.Fatalf("the batch query was rejected by a real %s server:\n  %v\n  %s",
					be.Name, err, sqlText)
			}
			defer rows.Close()

			// The statement is valid. Confirm it also SELECTS what the caller
			// scans -- a valid statement returning the wrong shape fails later,
			// at Scan, in the same silent background loop.
			cols, err := rows.Columns()
			if err != nil {
				t.Fatalf("columns on %s: %v", be.Name, err)
			}
			if len(cols) != 8 {
				t.Errorf("on %s the batch query returns %d columns, and processBatch "+
					"scans 8: %v", be.Name, len(cols), cols)
			}
		})
	}
}
