package engine

import "testing"

// TenantSettingsReader is an optional interface, discovered by type assertion
// in Engine.tenantSettings. That buys not editing ten WorkflowStore mocks, and
// it costs a failure mode: a store that stops satisfying it does not fail to
// compile, does not error at runtime, and does not log -- every tenant on it
// quietly gets the operator's flags instead of its own settings.
//
// That is "a per-tenant limit that is never exercised", the specific hazard
// IMPROVEMENT-PLAN 3.94 warns about, and these three lines are what close it.
// They are compile-time assertions, so the build breaks rather than the
// behaviour.
var (
	_ TenantSettingsReader = (*PostgresStore)(nil)
	_ TenantSettingsReader = (*MySQLStore)(nil)
	_ TenantSettingsReader = (*MSSQLStore)(nil)
)

// ShardedStore deliberately does NOT implement it, and this test is here so
// that stays a decision rather than an oversight.
//
// A sharded deployment splits one tenant's workflows across several databases,
// each with its own tenant_settings table and no mechanism keeping them in
// step. Reading from one shard silently ignores what an operator wrote to the
// others; reading from all of them costs N queries on the dispatch path;
// requiring agreement means deciding what to do when they disagree. None of
// those is obviously right, and none was needed for 3.94 step 3, so the answer
// today is that a sharded deployment resolves to the flag defaults.
//
// If you are here because you just made ShardedStore implement the interface:
// good, but pick one of those three semantics on purpose and write down which,
// because the failure mode of picking by accident is settings that are honoured
// for some of a tenant's workflows and not others.
func TestShardedStoreDeliberatelyDoesNotReadTenantSettings(t *testing.T) {
	var s any = (*ShardedStore)(nil)
	if _, ok := s.(TenantSettingsReader); ok {
		t.Fatal("ShardedStore now implements TenantSettingsReader.\n\n" +
			"That is a real decision with three defensible answers (read one " +
			"shard, read all and require agreement, or read all and merge) and " +
			"the wrong one gives a tenant its settings on some workflows and the " +
			"flags on others. Update this test with the semantics chosen and why, " +
			"rather than deleting it.")
	}
}
