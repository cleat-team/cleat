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
	"testing"

	"github.com/cleat-team/cleat/plugins/plugintest"
)

func TestTheBatchQueryRunsOnEveryDialect(t *testing.T) {
	plugintest.RunEveryArm(t, &Plugin{}, []plugintest.Arm{
		// WantCols guards the second failure mode: a valid statement returning
		// the wrong shape fails at Scan, later, in the same silent loop.
		{Name: "queryUnprocessedWebhookEvents", Q: queryUnprocessedWebhookEvents, WantCols: 8},
	})
}
