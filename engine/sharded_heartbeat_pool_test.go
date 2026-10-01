package engine

// cleat#2195: cleat#2192's reserved heartbeat pool (cleat#2009) was wired for
// the three non-sharded WorkflowStore implementations only.
// TestHeartbeatReservedPoolSeesARealClaimedRunOnAllThreeDialects
// (heartbeat_reserved_pool_sees_a_real_claimed_run_test.go) proves that for
// PostgresStore/MySQLStore/MSSQLStore directly. This is the same proof for
// ShardedStore, built the way cmd/cleat-worker/main.go now builds it: a
// second ShardedStore, wrapping one PostgresStoreFactory.OpenIsolatedStore
// per shard, over the SAME ShardConfig slice and order as the execution
// ShardedStore.
//
// Real shards on one shared database, same pattern as
// sharded_overclaim_db_test.go: what this test exercises is the per-shard
// POOL and its routing, not cross-database topology, and sharding is
// PostgreSQL-only in production (every shard opened via PostgresStoreFactory).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestShardedHeartbeatPoolSeesRealClaimedRunsAcrossShards deploys one def,
// starts enough runs that (by pigeonhole over a real sha256 hash) every shard
// claims at least one, and heartbeats each through the sharded heartbeat
// store built the main.go way. Every one must come back not-lost: if a
// shard's heartbeat pool were pointed at the wrong shard (or at nothing),
// only the runs on THAT shard would fail, which is why this claims from
// every shard rather than asserting on a single run as the non-sharded test
// does -- a single-shard version of this test cannot tell "routes correctly"
// from "got lucky and only exercised shard 0".
func TestShardedHeartbeatPoolSeesRealClaimedRunsAcrossShards(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping a real sharded-heartbeat-pool reproduction in short mode")
	}
	const shardCount = 3
	const runCount = 24 // P(some shard gets zero of 24 draws over 3 shards) = 3*(2/3)^24 ~= 1.3e-4

	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { adminDB.Close() })
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	t.Cleanup(func() { testutil.CleanupPostgresTestData(t, adminDB) })

	ctx := context.Background()
	const defName = "sharded-hb-pool-def"
	adminStore := NewPostgresStore(adminDB)
	if err := adminStore.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	dsn := testutil.PostgresTestDSN()
	configs := make([]ShardConfig, shardCount)
	execStores := make([]WorkflowStore, shardCount)
	execClosers := make([]func() error, shardCount)
	hbStores := make([]WorkflowStore, shardCount)
	hbClosers := make([]func() error, shardCount)
	for i := 0; i < shardCount; i++ {
		sdb, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Fatalf("open shard %d: %v", i, err)
		}
		if err := sdb.PingContext(ctx); err != nil {
			sdb.Close()
			t.Fatalf("ping shard %d: %v", i, err)
		}
		t.Cleanup(func() { sdb.Close() })

		f := NewPostgresStoreFactory(sdb, "public").WithDSN(dsn)
		configs[i] = ShardConfig{Name: fmt.Sprintf("shard%d", i)}

		es, ecloser, err := f.OpenStore(ctx, DefaultTenantUUID, "default")
		if err != nil {
			t.Fatalf("OpenStore shard %d: %v", i, err)
		}
		t.Cleanup(func() { ecloser.Close() })
		execStores[i] = es
		execClosers[i] = ecloser.Close

		// The call this test exists to exercise: cmd/cleat-worker/main.go
		// builds the per-shard heartbeat pool through the SAME factory f as
		// the execution store above, not a fresh sql.Open -- that is what
		// guarantees the two agree on which physical shard they are talking
		// to.
		hs, hcloser, err := f.OpenIsolatedStore(ctx, DefaultTenantUUID, 2, "default")
		if err != nil {
			t.Fatalf("OpenIsolatedStore shard %d: %v", i, err)
		}
		t.Cleanup(func() { hcloser.Close() })
		hbStores[i] = hs
		hbClosers[i] = hcloser.Close
	}

	execSharded, err := NewShardedStore(configs, execStores, execClosers)
	if err != nil {
		t.Fatalf("NewShardedStore(exec): %v", err)
	}
	hbSharded, err := NewShardedStore(configs, hbStores, hbClosers)
	if err != nil {
		t.Fatalf("NewShardedStore(heartbeat): %v", err)
	}

	seenShards := map[string]bool{}
	for i := 0; i < runCount; i++ {
		runID, _, err := execSharded.StartNewRun(ctx, "", defName, 1,
			[]byte(`{}`), fmt.Sprintf("sharded-hb-pool-key-%d", i), DefaultTenantUUID, 0)
		if err != nil {
			t.Fatalf("StartNewRun[%d]: %v", i, err)
		}
		seenShards[execSharded.getShard(runID).Config.Name] = true

		wf, err := execSharded.ClaimWorkflow(ctx, "sharded-hb-pool-worker")
		if err != nil {
			t.Fatalf("ClaimWorkflow[%d]: %v", i, err)
		}
		if wf == nil || wf.ID != runID {
			t.Fatalf("ClaimWorkflow[%d] claimed %v, want %s", i, wf, runID)
		}

		assertNotLost(t, hbSharded, "sharded-hb-pool-worker", wf, fmt.Sprintf("run %d (shard %s)", i, execSharded.getShard(runID).Config.Name))
	}

	if len(seenShards) != shardCount {
		t.Fatalf("got %d of a run on %d distinct shards after %d runs (%v) -- "+
			"this test proves nothing about shard N's heartbeat routing for any "+
			"shard it never exercised; raise runCount or re-seed", len(seenShards), shardCount, runCount, seenShards)
	}
}
