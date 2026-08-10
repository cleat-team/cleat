package engine

// IMPROVEMENT-PLAN.md 2.17 is marked ✅ FIXED: ShardedStore used to hand every
// shard the full limit and strand the excess as `running` with no executor,
// "demonstrated at 3 shards / limit 2: claimed 6, returned 2, stranded 4".
//
// That evidence was a *database* observation -- rows left `running`. The
// regression test guarding the fix, TestShardedClaimWorkflows_DoesNotOverClaim,
// runs entirely against mockShardStore: its claim function returns exactly the
// limit it is handed and increments a counter. So it verifies ShardedStore's
// fan-out arithmetic against a mock that always cooperates, one layer above
// where the defect was seen.
//
// That is the §1.1 lesson in a different package -- an assertion that holds
// because of a layer other than the one under test. This adds the missing
// layer: real PostgresStores, the real claim SQL, and the assertion §2.17
// actually made, which is about rows in the database rather than the length of
// a returned slice.
//
// The three shards share one database. That is not the production topology --
// what it exercises is the fan-out against real claim SQL and real row state,
// not cross-database routing. Rows are still distinct per shard because the
// claim uses FOR UPDATE SKIP LOCKED, so each store takes different rows, which
// is precisely the condition under which the excess got stranded.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestShardedClaimWorkflows_DoesNotOverClaim_RealDB(t *testing.T) {
	const shardCount = 3
	const limit = 2
	const seeded = shardCount * limit * 2 // plenty of claimable work per shard

	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { adminDB.Close() })
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	t.Cleanup(func() { testutil.CleanupPostgresTestData(t, adminDB) })

	ctx := context.Background()
	store := NewPostgresStore(adminDB)

	const defName = "shard-overclaim-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	stamp := time.Now().UnixNano()
	for i := 0; i < seeded; i++ {
		id := fmt.Sprintf("shard-overclaim-%d-%d", stamp, i)
		if _, _, err := store.StartNewRun(ctx, id, defName, 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
			t.Fatalf("StartNewRun[%d]: %v", i, err)
		}
	}

	// One real store per shard, each on its own pool, all against the same
	// database. NewShardedStore takes pre-built stores, so no sharded
	// deployment is needed to exercise the fan-out.
	dsn := testutil.PostgresTestDSN()
	stores := make([]WorkflowStore, shardCount)
	configs := make([]ShardConfig, shardCount)
	closers := make([]func() error, shardCount)
	for i := 0; i < shardCount; i++ {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Fatalf("open shard %d: %v", i, err)
		}
		if err := db.PingContext(ctx); err != nil {
			db.Close()
			t.Fatalf("ping shard %d: %v", i, err)
		}
		t.Cleanup(func() { db.Close() })
		stores[i] = NewPostgresStore(db)
		configs[i] = ShardConfig{Name: fmt.Sprintf("shard%d", i)}
		closers[i] = func() error { return nil }
	}

	sharded, err := NewShardedStore(configs, stores, closers)
	if err != nil {
		t.Fatalf("NewShardedStore: %v", err)
	}

	claimed, err := sharded.ClaimWorkflows(ctx, "worker-sharded", limit)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}

	if len(claimed) > limit {
		t.Errorf("ClaimWorkflows returned %d workflows for a limit of %d", len(claimed), limit)
	}

	// The assertion 2.17 actually made. Truncating the returned slice does
	// not un-claim a row: anything updated to 'running' beyond what the
	// caller received is held by a worker that will never execute it, until
	// the lease expires.
	var running int
	if err := adminDB.QueryRowContext(ctx,
		`SELECT count(*) FROM workflow_instances WHERE def_name = $1 AND status = 'running'`,
		defName).Scan(&running); err != nil {
		t.Fatalf("count running: %v", err)
	}
	if running != len(claimed) {
		t.Errorf("a claim for %d left %d row(s) 'running' in the database but returned %d -- "+
			"%d row(s) are claimed by a worker that will never run them and stay that way until "+
			"their lease expires (IMPROVEMENT-PLAN.md 2.17)",
			limit, running, len(claimed), running-len(claimed))
	}

	// Every row the caller was given must be one of the rows actually
	// claimed, so a fix cannot pass by returning rows it never took.
	byID := map[string]bool{}
	for _, wf := range claimed {
		byID[wf.ID] = true
	}
	rows, err := adminDB.QueryContext(ctx,
		`SELECT id FROM workflow_instances WHERE def_name = $1 AND status = 'running'`, defName)
	if err != nil {
		t.Fatalf("list running: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan running id: %v", err)
		}
		if !byID[id] {
			t.Errorf("workflow %s is 'running' in the database but was not returned to the caller", id)
		}
	}
}
