package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// cleat#3545, the coordinator's condition for leaving tags and routing rules on the designated
// shard only: every reader must reach them through the NAME-routed shard, not through the shard a
// run lives on. A reader that went through the run's shard would find no rule or tag there and fall
// back to the latest version without saying so.
//
// The two readers are the HTTP start handler (cmd/cleat-worker/server.go, PickVersionByRouting on
// the worker's ShardedStore) and child version resolution (engine/children.go, ResolveVersionByTag
// on childWfStore, which the worker sets to the tenant-scoped ShardedStore from storeForTenant).
// Routing rules are not consulted for children at all; they resolve by tag, a policy, or latest.
//
// What is asserted is the version recorded on the RUN'S OWN ROW on the NON-DESIGNATED shard, read
// from that database. The latest version is made different from the routed and tagged one, so a
// silent fall back to latest is a visible failure and not a coincidence.

func (rs *realShards) runVersion(t *testing.T, i int, id string) int {
	t.Helper()
	var v int
	if err := rs.dbs[i].QueryRow(`SELECT def_version FROM workflow_payloads WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("UNMEASURED: read the version of run %s on shard %q: %v", id, rs.names[i], err)
	}
	return v
}

func TestARoutedOrTaggedVersionIsHonouredForARunOnANonDesignatedShard(t *testing.T) {
	rs := newRealShards(t, "cleat_shard_defs_routing", "a", "b")
	ctx := context.Background()
	const child = "shard-defs-routed-child"
	const parentName = "shard-defs-routed-parent"
	designated := rs.shardOf(child)
	other := otherThan(designated, len(rs.names))

	// v1 is what the rule and the tag point at; v2 is the latest.
	for _, v := range []int{1, 2} {
		if err := rs.deploy(ctx, child, v, defBytesA, false); err != nil {
			t.Fatalf("deploy %s v%d: %v", child, v, err)
		}
	}
	if err := rs.deploy(ctx, parentName, 1, defBytesA, false); err != nil {
		t.Fatalf("deploy parent: %v", err)
	}
	if err := rs.store.SetRoutingRule(ctx, child, 1, 1.0); err != nil {
		t.Fatalf("set routing rule: %v", err)
	}
	if err := rs.store.SetWorkflowTag(ctx, child, 1, "stable"); err != nil {
		t.Fatalf("set tag: %v", err)
	}
	// The rule and tag are on the designated shard and NOT on the other: the situation under test.
	for tbl, col := range map[string]string{"workflow_routing": "workflow_name", "workflow_tags": "workflow_name"} {
		var onDesignated, onOther int
		q := `SELECT COUNT(*) FROM ` + tbl + ` WHERE ` + col + ` = $1`
		if err := rs.dbs[designated].QueryRow(q, child).Scan(&onDesignated); err != nil {
			t.Fatalf("UNMEASURED: count %s: %v", tbl, err)
		}
		if err := rs.dbs[other].QueryRow(q, child).Scan(&onOther); err != nil {
			t.Fatalf("UNMEASURED: count %s: %v", tbl, err)
		}
		if onDesignated != 1 || onOther != 0 {
			t.Fatalf("UNMEASURED: %s has %d rows on the designated shard and %d on the other, want 1 and 0", tbl, onDesignated, onOther)
		}
	}
	if latest, err := rs.store.ResolveLatestVersion(ctx, child); err != nil || latest != 2 {
		t.Fatalf("UNMEASURED: the latest version of %s is %d, %v, want 2", child, latest, err)
	}

	// The start path: pick by routing through the ShardedStore, as the HTTP handler does, then start
	// a run that lives on the non-designated shard.
	picked, err := rs.store.PickVersionByRouting(ctx, child)
	if err != nil || picked != 1 {
		t.Fatalf("PickVersionByRouting = %d, %v, want the routed version 1", picked, err)
	}
	runID := rs.idOnShard(t, other, "routed-run")
	if _, _, err := rs.store.StartNewRun(ctx, runID, child, picked, json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
		t.Fatalf("start the routed run on shard %q: %v", rs.names[other], err)
	}
	if got := rs.runVersion(t, other, runID); got != 1 {
		t.Errorf("the run on shard %q recorded version %d, want the routed 1", rs.names[other], got)
	}

	// The child path: a parent on the non-designated shard resolves the tag through the same store
	// the worker hands the engine, then starts the child with it. The child lives on the parent's shard.
	parentID := rs.idOnShard(t, other, "routed-parent")
	if _, _, err := rs.store.StartNewRun(ctx, parentID, parentName, 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
		t.Fatalf("start the parent on shard %q: %v", rs.names[other], err)
	}
	tagged, err := rs.store.ResolveVersionByTag(ctx, child, "stable")
	if err != nil || tagged != 1 {
		t.Fatalf("ResolveVersionByTag = %d, %v, want the tagged version 1", tagged, err)
	}
	taggedChild, err := rs.store.StartChildWorkflow(ctx, parentID, child, `{}`, tagged, "", 0)
	if err != nil {
		t.Fatalf("start the tagged child: %v", err)
	}
	if got := rs.runVersion(t, other, taggedChild); got != 1 {
		t.Errorf("the child on shard %q recorded version %d, want the tagged 1", rs.names[other], got)
	}

	// No version asked for: resolved to the latest ON THE CHILD'S OWN SHARD, which is only possible
	// because the definition is there.
	latestChild, err := rs.store.StartChildWorkflow(ctx, parentID, child, `{}`, 0, "", 0)
	if err != nil {
		t.Fatalf("start a child with no version on shard %q: %v", rs.names[other], err)
	}
	if got := rs.runVersion(t, other, latestChild); got != 2 {
		t.Errorf("a child with no version recorded %d, want the latest 2", got)
	}
}
