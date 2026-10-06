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
			// pass without ever reaching the DELETE. The workflow name is
			// likewise arbitrary -- cleat#3168 added it as a DELETE filter, but
			// the rule id alone already matches nothing, so no name makes this
			// a miss.
			err := store.RemoveRoutingRule(context.Background(), "some-workflow",
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

// TestRemovingARoutingRuleByNamingADifferentWorkflowIsNotFound is cleat#3168's
// regression test against real databases: it proves the WHERE clause itself
// scopes by workflow_name, on all three dialects, rather than trusting a mock
// to simulate that scoping correctly.
//
// Two workflows, two rules. Naming the wrong one must report
// ErrRoutingRuleNotFound -- the same answer a nonexistent rule id gets, which
// is the point: from the caller's side of the name in the URL, a rule
// belonging to someone else should be indistinguishable from a rule that was
// never created.
func TestRemovingARoutingRuleByNamingADifferentWorkflowIsNotFound(t *testing.T) {
	for _, backend := range pluginDepsBackends() {
		t.Run(backend.name, func(t *testing.T) {
			_, store := setupPluginDepsDB(t, backend)
			ctx := context.Background()

			const (
				holder, holderVersion = "checkout", 2
				other, otherVersion   = "refunds", 3
			)
			// workflow_routing's FK is (tenant_id, workflow_name, target_version)
			// -> workflow_defs, so a rule cannot exist without a matching deployed
			// VERSION -- the DELETE scoping this test proves is between two real
			// definitions, not two bare names.
			for name, version := range map[string]int{holder: holderVersion, other: otherVersion} {
				if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
					Name: name, Version: version,
					WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
				}); err != nil {
					t.Fatalf("DeployWorkflowDef(%s): %v", name, err)
				}
			}

			if err := store.SetRoutingRule(ctx, holder, holderVersion, 0.25); err != nil {
				t.Fatalf("SetRoutingRule(%s): %v", holder, err)
			}
			if err := store.SetRoutingRule(ctx, other, otherVersion, 0.5); err != nil {
				t.Fatalf("SetRoutingRule(%s): %v", other, err)
			}

			rules, err := store.GetRoutingRules(ctx, holder)
			if err != nil {
				t.Fatalf("GetRoutingRules(%s): %v", holder, err)
			}
			if len(rules) != 1 {
				t.Fatalf("GetRoutingRules(%s): got %d rules, want 1", holder, len(rules))
			}
			ruleID := rules[0].ID

			// The defect: removal under the OTHER workflow's name must not
			// reach checkout's rule, even though ruleID is a real, existing id.
			if err := store.RemoveRoutingRule(ctx, other, ruleID); !errors.Is(err, ErrRoutingRuleNotFound) {
				t.Fatalf("%s: removing %s's rule by naming %s = %v, want ErrRoutingRuleNotFound.\n\n"+
					"A caller naming any deployed workflow could delete a rule belonging to a "+
					"different one.", backend.name, holder, other, err)
			}

			// The control: it is still there, and the real owner can remove it.
			if remaining, err := store.GetRoutingRules(ctx, holder); err != nil || len(remaining) != 1 {
				t.Fatalf("%s's rule did not survive the misnamed removal attempt: rules=%v err=%v",
					holder, remaining, err)
			}
			if err := store.RemoveRoutingRule(ctx, holder, ruleID); err != nil {
				t.Fatalf("removing %s's own rule under its own name: %v", holder, err)
			}
		})
	}
}
