// Every dialect arm of this plugin's structural queries, executed on the
// database it names. cleat#1133 part 4.
//
// These are the statements plugin.Rebind deliberately does not rewrite: LIMIT,
// ON CONFLICT and the like change the SHAPE of a statement rather than a token
// in it, so they are written out per dialect. Nothing checked that the written
// arms were valid, and the fake-driver tests that drive these paths
// pattern-match the query string, so they accept SQL no database would.
package notifications

import (
	"testing"

	"github.com/cleat-team/cleat/plugins/plugintest"
)

func TestEveryArmRunsOnItsOwnDialect(t *testing.T) {
	plugintest.RunEveryArm(t, &Plugin{}, []plugintest.Arm{
		{Name: "queryDueDeliveries", Q: queryDueDeliveries, WantCols: 5},
	})
}
