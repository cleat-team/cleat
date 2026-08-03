package engine

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// TestClaimWorkflows_RespectsLimitUnderConcurrency guards the invariant that a
// claim never returns more workflows than it asked for. `limit` is the
// worker's remaining concurrency budget, so over-claiming means running more
// workflows at once than the operator configured and taking work other idle
// workers should have had.
//
// Honest statement of what this does and does not establish. The cluster CI
// job produced a claim for 3 that returned 10 -- every eligible row -- and the
// diagnostic in describeClaimState confirmed all ten were left `running`.
//
// The mechanism previously suspected, and written into this comment and the
// query, was an EvalPlanQual recheck re-executing the
// `id IN (SELECT ... LIMIT n FOR UPDATE SKIP LOCKED)` sublink. **That has now
// been ruled out**, both mechanically and empirically -- see the note on
// ClaimWorkflows in store_lifecycle.go, and IMPROVEMENT-PLAN.md 2.11. The
// SQL, in either form, cannot return more rows than its limit.
//
// So this remains a regression guard for the invariant rather than a
// reproduction of the observed failure, and the cause of that failure is still
// unexplained. What it is *not* is a guard against the one over-claim that has
// since been demonstrated: ShardedStore hands every shard the full limit and
// discards the excess after the shards have already marked those rows running
// (TestShardedClaimWorkflows_OverClaimsAcrossShards, IMPROVEMENT-PLAN.md 2.17).
// This test uses a single store and cannot see that.
func TestClaimWorkflows_RespectsLimitUnderConcurrency(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		if backend.Name() != "postgres" {
			// The defect and its fix are both PostgreSQL-specific: MySQL and
			// SQL Server use different claim statements.
			continue
		}
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			const total = 40
			for i := 0; i < total; i++ {
				if _, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
					json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
					t.Fatalf("StartNewRun[%d]: %v", i, err)
				}
			}

			// A background sweep that updates the same rows without SKIP
			// LOCKED. This is the contention that matters: EvalPlanQual only
			// runs when the UPDATE meets a row a *committed* concurrent
			// transaction has changed underneath it. Claimers alone do not
			// produce it -- they skip each other's locked rows -- which is
			// why this reproduces in the cluster job, where live workers run
			// heartbeat and stale-recovery sweeps over the same table, and
			// not in a test that only runs claimers.
			ps := store.(*PostgresStore)
			sweepDone := make(chan struct{})
			var sweepWG sync.WaitGroup
			sweepWG.Add(1)
			go func() {
				defer sweepWG.Done()
				for {
					select {
					case <-sweepDone:
						return
					default:
					}
					_, _ = ps.db.Exec(
						`UPDATE workflow_instances SET heartbeat_at = now() WHERE status = 'ready'`)
				}
			}()
			defer func() {
				close(sweepDone)
				sweepWG.Wait()
			}()

			// Several claimers at once, each with a small limit.
			const claimers = 6
			const limit = 3

			var wg sync.WaitGroup
			counts := make([]int, claimers)
			errs := make([]error, claimers)
			start := make(chan struct{})
			for i := 0; i < claimers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					wfs, err := store.ClaimWorkflows(ctx, "worker-"+string(rune('a'+i)), limit)
					errs[i] = err
					counts[i] = len(wfs)
				}(i)
			}
			close(start)
			wg.Wait()

			claimed := 0
			for i, n := range counts {
				if errs[i] != nil {
					t.Fatalf("claimer %d: %v", i, errs[i])
				}
				if n > limit {
					t.Errorf("claimer %d received %d workflows for limit %d: a worker "+
						"that asks for %d must never be handed more, or it exceeds "+
						"its configured concurrency", i, n, limit, limit)
				}
				claimed += n
			}

			// No workflow may be handed to two workers, however the limit
			// behaved.
			var running int
			if err := ps.db.QueryRow(
				`SELECT count(*) FROM workflow_instances WHERE status = 'running'`).Scan(&running); err != nil {
				t.Fatalf("count running: %v", err)
			}
			if running != claimed {
				t.Errorf("claimers reported %d workflows between them but %d are running: "+
					"a row was claimed twice or a claim did not take effect", claimed, running)
			}
		})
	}
}
