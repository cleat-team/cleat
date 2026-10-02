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
// starts runs whose ids are CHOSEN so that every shard gets at least one --
// getShard hashes the id (sharded_store.go:218), so ids drawn at random could
// leave a shard untouched, which is what cleat#2963 removes -- and heartbeats
// each through the sharded heartbeat store built the main.go way. Every one must come back not-lost: if a
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
	// runCount is how many runs exercise the pool. Coverage of all shards is
	// GUARANTEED by the id selection below rather than hoped for from a draw
	// count -- see the comment there (cleat#2963).
	const runCount = 24

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

	// CHOOSE the run ids so every shard is exercised, rather than drawing random
	// ones and hoping. getShard hashes the run id (sharded_store.go:218), so a
	// fixed number of random draws has a real chance of never touching some
	// shard -- 3*(2/3)^24 ~= 1.8e-4 at this runCount, small per run and still
	// enough to fail a REQUIRED Tier 1 context for a PR that cannot possibly
	// have caused it (cleat#2963: a docs-only change ate it, and cleat-review
	// had hit the same flake independently).
	//
	// "Raise runCount or re-seed" -- what the failure message used to say -- only
	// lowers the probability, and re-seeding is not even available: the ids are
	// generated by the store, not the test. Passing our own ids removes the
	// class. StartNewRun honours a caller-supplied id (sharded_store.go:724-726),
	// so the first shardCount candidates are required to land somewhere new and
	// the rest may fall where they like; the coverage check below stays as a
	// check on THIS selection, which can now only fail if it is broken.
	runIDs, err := shardCoveringRunIDs(execSharded, runCount)
	if err != nil {
		t.Fatalf("shardCoveringRunIDs: %v", err)
	}

	seenShards := map[string]bool{}
	for i, runID := range runIDs {
		if _, _, err := execSharded.StartNewRun(ctx, runID, defName, 1,
			[]byte(`{}`), fmt.Sprintf("sharded-hb-pool-key-%d", i), DefaultTenantUUID, 0); err != nil {
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
		t.Fatalf("the id selection covered only %d of %d shards (%v) -- that selection is "+
			"deterministic, so this is a bug in it, not an unlucky draw; this test would "+
			"otherwise prove nothing about the heartbeat routing of a shard it never exercised",
			len(seenShards), shardCount, seenShards)
	}
}

// shardCoveringRunIDs returns `count` run ids whose shards between them cover
// every shard s knows about, so a test can exercise each shard's routing
// deterministically instead of drawing random ids and hoping the draw covers
// everything.
//
// WHY THIS EXISTS (cleat#2963). getShard hashes the run id
// (sharded_store.go:218), so with random ids a fixed number of draws leaves a
// real chance of never touching some shard: 3*(2/3)^24 ~= 1.8e-4 for three
// shards over 24 runs. Small per run, and enough to fail a REQUIRED Tier 1
// context for a PR that cannot have caused it -- a docs-only change ate it, and
// cleat-review hit the same flake independently on the same PR.
//
// (The comment this replaced -- and cleat#2963's own title -- said 1.3e-4. That
// is wrong: by inclusion-exclusion the miss probability is
// 3*(2/3)^24 - 3*(1/3)^24 = 1.78e-4, and 700k simulated draws gave 1.63e-4 and
// 1.95e-4 -- two runs, both on the closed form. The flake was slightly likelier
// than the number everyone was quoting.)
//
// The failure message used to say "raise runCount or re-seed", and both only
// lower the probability; re-seeding is not even available, because the ids are
// generated by the store rather than chosen by the test. StartNewRun honours a
// caller-supplied id (sharded_store.go:724-726), so the test can ask for ids
// that land where it needs them.
//
// The first len(s.shards) ids are required to land on distinct shards; the
// remainder may fall wherever, because the point is coverage rather than a
// particular distribution.
func shardCoveringRunIDs(s *ShardedStore, count int) ([]string, error) {
	if len(s.shards) == 0 {
		return nil, fmt.Errorf("shardCoveringRunIDs: no shards configured")
	}
	if count < len(s.shards) {
		return nil, fmt.Errorf("shardCoveringRunIDs: count %d is fewer than the %d shards it must cover", count, len(s.shards))
	}
	ids := make([]string, 0, count)
	covered := map[string]bool{}
	for i := 0; len(ids) < count; i++ {
		if i > 10_000 {
			return nil, fmt.Errorf("shardCoveringRunIDs: no ids covering all %d shards after %d candidates -- getShard is not distributing over the id space this assumes", len(s.shards), i)
		}
		candidate := fmt.Sprintf("sharded-hb-pool-run-%d", i)
		name := s.getShard(candidate).Config.Name
		if len(ids) < len(s.shards) && covered[name] {
			continue
		}
		covered[name] = true
		ids = append(ids, candidate)
	}
	return ids, nil
}

// TestShardCoveringRunIDsCoverEveryShard guards the selection above, and needs
// no database: getShard reads only the shard names, so the store can be built
// directly. This is what makes the coverage assertion in the sharded-heartbeat
// test a check on ITS OWN selection rather than on a draw -- the thing
// cleat#2963 exists to remove.
func TestShardCoveringRunIDsCoverEveryShard(t *testing.T) {
	const shardCount = 3
	for _, count := range []int{shardCount, 24, 60} {
		t.Run(fmt.Sprintf("count=%d", count), func(t *testing.T) {
			s := &ShardedStore{}
			for i := 0; i < shardCount; i++ {
				s.shards = append(s.shards, &Shard{Config: ShardConfig{Name: fmt.Sprintf("shard%d", i)}})
			}

			ids, err := shardCoveringRunIDs(s, count)
			if err != nil {
				t.Fatalf("shardCoveringRunIDs: %v", err)
			}
			if len(ids) != count {
				t.Fatalf("got %d ids, want %d", len(ids), count)
			}

			seenShards := map[string]bool{}
			seenIDs := map[string]bool{}
			for _, id := range ids {
				if seenIDs[id] {
					t.Fatalf("id %q returned twice -- two runs would share a run id", id)
				}
				seenIDs[id] = true
				shard := s.getShard(id)
				if shard == nil {
					t.Fatalf("getShard(%q) = nil", id)
				}
				seenShards[shard.Config.Name] = true
			}
			if len(seenShards) != shardCount {
				t.Errorf("ids cover %d of %d shards (%v) -- every shard must be exercised, which is the whole point of choosing ids rather than drawing them", len(seenShards), shardCount, seenShards)
			}
		})
	}
}

// And the impossible requests must be refused rather than silently satisfied
// short: a caller asking for fewer ids than there are shards cannot be covered,
// and returning a short list would move that failure to the caller's loop.
func TestShardCoveringRunIDsRefusesImpossibleRequests(t *testing.T) {
	s := &ShardedStore{}
	for i := 0; i < 3; i++ {
		s.shards = append(s.shards, &Shard{Config: ShardConfig{Name: fmt.Sprintf("shard%d", i)}})
	}
	if ids, err := shardCoveringRunIDs(s, 2); err == nil {
		t.Errorf("shardCoveringRunIDs(count=2) over 3 shards returned (%v, nil) -- the request cannot be satisfied", ids)
	}
	if ids, err := shardCoveringRunIDs(&ShardedStore{}, 3); err == nil {
		t.Errorf("shardCoveringRunIDs with no shards returned (%v, nil)", ids)
	}
}
