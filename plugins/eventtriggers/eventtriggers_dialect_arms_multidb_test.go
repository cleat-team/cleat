// Every arm of every eventtriggers query, executed on the database it names.
//
// cleat#1133 part 4. queryUnprocessedEvents' MSSQL arm carried
// `WHERE NOT processed` -- while, in the same literal, LIMIT 100 had been
// translated to OFFSET/FETCH and NOW() - INTERVAL to DATEADD. Someone
// translated this arm carefully and stopped at the constructs they were
// thinking about. That is the failure this plugin.Query shape invites: naming
// what a variant is FOR narrows the reviewer to that purpose, and the rest of
// the literal inherits the primary dialect unexamined.
//
// WHY NOTHING CAUGHT IT. T-SQL has no boolean type, so `NOT processed` is a
// BINDING error (Msg 4145) rather than a syntax error. `SET PARSEONLY ON`
// accepts the statement; only `SET NOEXEC ON`, which binds, rejects it. So a
// parse-based sweep reports this clean -- and the runtime failure lands in a
// background loop that logs and returns, where it is indistinguishable from
// "no unprocessed events".
//
// This test does the one thing that cannot be fooled by either: it runs the
// statement against a real server of that dialect, on the schema the plugin's
// own migrations build. A fixture built by hand to suit the query cannot
// disagree with the query (cleat#1141 is what that costs).
package eventtriggers

import (
	"testing"

	"github.com/cleat-team/cleat/plugins/plugintest"
)

func TestEveryQueryArmRunsOnItsOwnDialect(t *testing.T) {
	plugintest.RunEveryArm(t, &Plugin{}, []plugintest.Arm{
		{Name: "queryUnprocessedEvents", Q: queryUnprocessedEvents, WantCols: 5},
		{
			Name:     "queryLatestUnprocessedEvent",
			Q:        queryLatestUnprocessedEvent,
			Args:     []any{"00000000-0000-0000-0000-000000000001", "some.event"},
			WantCols: 4,
		},
	})
}
