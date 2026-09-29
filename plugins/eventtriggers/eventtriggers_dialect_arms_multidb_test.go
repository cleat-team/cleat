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
			Name:     "queryOldestUnprocessedEventForClaim",
			Q:        queryOldestUnprocessedEventForClaim,
			Args:     []any{"00000000-0000-0000-0000-000000000001", "some.event", "", "", ""},
			WantCols: 4,
		},
	})
}

// insertEventIdempotent and upsertAwaiter (Version 6, cleat#2625) are NOT
// arms here, on purpose. Both write ingested_events/event_awaiters, and both
// are TenantScoped (migrations.go v4) -- RunEveryArm executes straight
// against be.DB with no tenant session context, which SQL Server's RLS block
// predicate refuses outright ("target object ... has a block predicate that
// conflicts with this operation", error 33504), unlike Postgres and MySQL on
// this test harness's connections. Reproducing correct tenant-context setup
// per dialect inside this shared helper -- Postgres's set_config on the
// write transaction, SQL Server's sp_set_session_context, and its own
// fragility under connection-pool reuse (plugin/plugin.go's comment on
// exactly that) -- is a bigger change than this migration's own SQL
// correctness needs.
//
// Both statements ARE exercised for real, under correct tenant scoping, by
// TestPluginMigrations_AllDialects (the schema they write into, on all three
// dialects) and TestPublishEventCarriesItsOwnTenant (PublishEvent, which
// calls insertEventIdempotent, under real RLS enforcement) -- and by the
// worker's own Multi-DB/Layer-3 CI jobs once this lands, which run
// PublishEvent/registerAwaiter through the real tenant-scoped connection
// path End to end.
