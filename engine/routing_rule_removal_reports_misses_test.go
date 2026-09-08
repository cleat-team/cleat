package engine

import (
	"context"
	"errors"
	"testing"
)

// TestRemovingARuleThatIsNotThereIsReportedByEveryDialect is cleat#946's second
// half, and it needs real databases because it is a claim about what a DELETE
// reports when it matches nothing.
//
// #948 fixed the first half -- ShardedStore routed the removal by rule ID while
// rows are placed by workflow name, so it deleted from the wrong shard. It left
// this one: no implementation checked rows-affected, so the store returned nil
// either way and the handler answered `200 {"status":"removed"}` for a rule that
// never existed.
//
// Every mock in the suite returns nil too, which is why nothing saw it. A Go
// nil has no opinion about how many rows were touched; only a database does.
func TestRemovingARuleThatIsNotThereIsReportedByEveryDialect(t *testing.T) {
	for _, backend := range pluginDepsBackends() {
		t.Run(backend.name, func(t *testing.T) {
			_, store := setupPluginDepsDB(t, backend)

			// A well-formed UUID naming nothing. SQL Server parses the ID before
			// querying, so a non-UUID would fail earlier and this test would
			// pass without ever reaching the DELETE.
			err := store.RemoveRoutingRule(context.Background(),
				"6b1f7a4e-0000-4000-8000-0000000000ff")

			if err == nil {
				t.Fatalf("%s: removing a rule that does not exist returned nil.\n\n"+
					"The handler turns nil into 200 {\"status\":\"removed\"}, so an operator "+
					"tearing down a canary is told it is gone while it goes on shifting live "+
					"traffic.", backend.name)
			}
			if !errors.Is(err, ErrRoutingRuleNotFound) {
				t.Errorf("%s: error = %v, want ErrRoutingRuleNotFound.\n\n"+
					"ShardedStore tests this with errors.Is to decide whether to keep walking, "+
					"and the handler tests it to answer 404 rather than 500. An error that is "+
					"merely non-nil satisfies neither.", backend.name, err)
			}
		})
	}
}

// TestTheNotFoundSentinelDoesNotAbortTheShardWalk pins the interaction between
// cleat#946's two halves, which is the thing this change could plausibly break.
//
// #948 has ShardedStore ask EVERY shard, because the rule ID is not the shard
// key. forEachShard stops at the first error. So the moment a store started
// returning ErrRoutingRuleNotFound -- which n-1 shards legitimately do -- a
// naive pass-through would abort the walk at shard 0 and never reach the holder,
// reintroducing #948's defect by way of fixing the reporting.
//
// Held by the LAST shard on purpose: with the sentinel passed through, shard 0
// aborts and the failure is guaranteed rather than dependent on where the rule
// happens to sit.
func TestTheNotFoundSentinelDoesNotAbortTheShardWalk(t *testing.T) {
	const ruleID = "6b1f7a4e-0000-4000-8000-000000000001"

	ss, mocks := makeShardedStore(t, 4)
	for i, m := range mocks {
		m.heldRoutingRules = map[string]bool{}
		if i == len(mocks)-1 {
			m.heldRoutingRules[ruleID] = true
		}
	}

	if err := ss.RemoveRoutingRule(context.Background(), ruleID); err != nil {
		t.Fatalf("RemoveRoutingRule: %v\n\n"+
			"The rule is on the LAST shard. An error here means the walk stopped early -- "+
			"almost certainly because ErrRoutingRuleNotFound from an earlier shard was "+
			"passed through to forEachShard, which returns on the first error.", err)
	}
	if mocks[len(mocks)-1].heldRoutingRules[ruleID] {
		t.Error("the holding shard still has the rule after a successful removal")
	}
}
