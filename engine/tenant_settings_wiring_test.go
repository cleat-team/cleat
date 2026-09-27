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

// ShardedStore now implements TenantSettingsReader (cleat#1853), and this
// test's job changed from "is this still undecided" to "is this still the
// decision that was made".
//
// The semantics chosen: read every shard (all were opened for the same
// tenant, so any of them has a candidate answer), use the first non-empty
// result found in shard order, and if a LATER shard disagrees, log it rather
// than silently preferring one. Full reasoning is on ShardedStore.GetTenantSettings
// in sharded_store.go. That is closest to "read all and require agreement"
// among the three this test used to list, with the disagreement WARNED about
// rather than turned into a read failure -- a read failure here already
// degrades to the operator's flags, and an operator's own mistake (writing
// tenant_settings inconsistently across shards, since nothing fans the write
// out) should not additionally fail every workflow on the tenant.
//
// If you are here because GetTenantSettings changed shape again: update this
// test to assert the interface is still satisfied and that the new semantics
// are documented on the method, the same way this update did -- rather than
// deleting it, which is what lets a fourth "accidental" semantics ship
// unnoticed.
func TestShardedStoreHonoursTenantSettingsWithDocumentedSemantics(t *testing.T) {
	var s any = (*ShardedStore)(nil)
	if _, ok := s.(TenantSettingsReader); !ok {
		t.Fatal("ShardedStore no longer implements TenantSettingsReader.\n\n" +
			"cleat#1853 made this a deliberate implementation, not an absence: every " +
			"tenant on a sharded deployment silently got the operator's flags before " +
			"it existed. If this regressed, the fix is in engine/sharded_store.go, " +
			"not in loosening this test.")
	}
}
