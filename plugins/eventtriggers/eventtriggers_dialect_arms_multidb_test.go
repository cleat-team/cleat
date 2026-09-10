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
	"context"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

func TestEveryQueryArmRunsOnItsOwnDialect(t *testing.T) {
	queries := []struct {
		name string
		q    plugin.Query
		args int
	}{
		{"queryUnprocessedEvents", queryUnprocessedEvents, 0},
		{"queryLatestUnprocessedEvent", queryLatestUnprocessedEvent, 2},
	}

	for _, be := range testutil.NewPluginTestBackends(t) {
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			ctx := context.Background()
			dialect := plugin.Dialect(be.Dialect)
			p := &Plugin{dialect: dialect}

			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("eventtriggers migrations on %s: %v", be.Name, err)
			}

			for _, tc := range queries {
				t.Run(tc.name, func(t *testing.T) {
					sqlText := plugin.Rebind(tc.q.For(dialect), dialect)
					args := make([]any, tc.args)
					for i := range args {
						// Values that bind on every dialect. The question is
						// whether the STATEMENT is valid, not what it returns.
						args[i] = "00000000-0000-0000-0000-000000000001"
					}
					rows, err := be.DB.QueryContext(ctx, sqlText, args...)
					if err != nil {
						t.Errorf("%s arm for %s was rejected by a real %s server:\n  %v\n  %s",
							tc.name, be.Name, be.Name, err, sqlText)
						return
					}
					_ = rows.Close()
				})
			}
		})
	}
}
