package engine

// A sharded listing has to MERGE its shards' pages, not concatenate them.
// cleat#1197.
//
// Two shards on two SEPARATE DATABASES, which is the part that matters. The
// repo's other real-database sharded test (sharded_overclaim_db_test.go) runs
// three shards against one database and says so explicitly -- that is the right
// trade for a claim test, where FOR UPDATE SKIP LOCKED is what makes the rows
// distinct. It is the wrong trade here: every shard would return the same rows,
// so a concatenation bug would surface as duplicates rather than as the
// ordering and offset defects this is about, and a merge that happened to
// dedupe would look correct.
//
// So each shard gets its own database via testutil.SuiteTestDB, and the rows
// are seeded with INTERLEAVED created_at values. Interleaving is what makes the
// test able to fail: with shard A holding every newer row, concatenate and merge
// return the same thing, and the test would pass against the defect.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

const shardListDefName = "sharded-list-merge-def"

// TestShardedListWorkflows_MergesRatherThanConcatenates is the regression test
// for both halves of cleat#1197, asserted separately.
//
// Separately is deliberate. The fix has two parts -- merge on the sort key, and
// stop passing the caller's Offset to every shard -- and a single assertion
// that goes red for the whole change cannot tell you which part is load
// bearing. cleat#1138 is the precedent: a correct fix was implemented, judged
// ineffective because the symptom did not move, and reverted, because the
// symptom was being held up by a second defect that was still present.
func TestShardedListWorkflows_MergesRatherThanConcatenates(t *testing.T) {
	ctx := context.Background()
	sharded, want := twoShardsWithInterleavedRows(t, ctx)

	t.Run("page one is the globally newest rows", func(t *testing.T) {
		got, err := sharded.ListWorkflows(ctx, WorkflowFilter{Limit: 4})
		if err != nil {
			t.Fatalf("ListWorkflows: %v", err)
		}
		assertIDs(t, got, want[:4],
			"page 1 is not the four globally newest workflows.\n\n"+
				"Concatenating the shards' pages returns shard A's rows first, so a caller "+
				"asking for the most recent N gets shard A's most recent N -- and never sees "+
				"shard B at all when shard A can fill the page.")
	})

	t.Run("page two continues where page one stopped", func(t *testing.T) {
		got, err := sharded.ListWorkflows(ctx, WorkflowFilter{Limit: 4, Offset: 4})
		if err != nil {
			t.Fatalf("ListWorkflows: %v", err)
		}
		assertIDs(t, got, want[4:8],
			"page 2 does not continue page 1.\n\n"+
				"Offset is passed to every shard unchanged, so Offset: 4 skips 4 rows PER "+
				"SHARD -- 8 overall across two shards. The rows in between are not reordered, "+
				"they are absent, and the gap grows with the shard count.")
	})

	t.Run("paging the whole set returns every row exactly once", func(t *testing.T) {
		// The property the two cases above are instances of, and the one a
		// caller actually depends on. It is here because an off-by-one in the
		// merge could satisfy both pages above and still lose or repeat a row
		// further in.
		var paged []WorkflowInstance
		for off := 0; off < len(want); off += 3 {
			page, err := sharded.ListWorkflows(ctx, WorkflowFilter{Limit: 3, Offset: off})
			if err != nil {
				t.Fatalf("ListWorkflows(offset=%d): %v", off, err)
			}
			paged = append(paged, page...)
		}
		assertIDs(t, paged, want,
			"walking the whole set in pages of 3 did not return every row exactly once, "+
				"in order.")
	})
}

// twoShardsWithInterleavedRows returns a ShardedStore over two real stores on
// two separate databases, and the workflow ids in the globally correct order
// (created_at DESC, id DESC) that a correct listing must produce.
func twoShardsWithInterleavedRows(t *testing.T, ctx context.Context) (*ShardedStore, []string) {
	t.Helper()

	const rowsPerShard = 6

	dbA := testutil.SuiteTestDB(t, "shardlista")
	dbB := testutil.SuiteTestDB(t, "shardlistb")
	for _, db := range []*sql.DB{dbA, dbB} {
		testutil.CleanupPostgresTestData(t, db)
	}
	t.Cleanup(func() {
		testutil.CleanupPostgresTestData(t, dbA)
		testutil.CleanupPostgresTestData(t, dbB)
	})

	storeA, storeB := NewPostgresStore(dbA), NewPostgresStore(dbB)

	// Ordered newest first, which is the order a correct listing returns.
	// Seeded oldest first so the timestamps below ascend with the loop.
	var newestFirst []string

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	seq := 0
	for i := 0; i < rowsPerShard; i++ {
		for _, s := range []struct {
			name  string
			store *PostgresStore
			db    *sql.DB
		}{{"a", storeA, dbA}, {"b", storeB, dbB}} {
			if i == 0 {
				if err := s.store.DeployWorkflowDef(ctx, &WorkflowDef{
					Name: shardListDefName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
					ABIVersion: 1, MinVersion: 1,
				}); err != nil {
					t.Fatalf("DeployWorkflowDef on shard %s: %v", s.name, err)
				}
			}
			id := fmt.Sprintf("shardlist-%s-%d", s.name, i)
			if _, _, err := s.store.StartNewRun(ctx, id, shardListDefName, 1,
				json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun %s: %v", id, err)
			}
			// created_at defaults to now(), and every row in this loop lands
			// inside the same second -- which would make the sort key a tie and
			// the expected order a coin flip. Setting it explicitly is what
			// makes "interleaved" true rather than hoped for.
			seq++
			at := base.Add(time.Duration(seq) * time.Second)
			if _, err := s.db.ExecContext(ctx,
				`UPDATE workflow_instances SET created_at = $1 WHERE id = $2`, at, id); err != nil {
				t.Fatalf("set created_at for %s: %v", id, err)
			}
			newestFirst = append([]string{id}, newestFirst...)
		}
	}

	configs := []ShardConfig{{Name: "a"}, {Name: "b"}}
	stores := []WorkflowStore{storeA, storeB}
	closers := []func() error{func() error { return nil }, func() error { return nil }}
	sharded, err := NewShardedStore(configs, stores, closers)
	if err != nil {
		t.Fatalf("NewShardedStore: %v", err)
	}
	return sharded, newestFirst
}

func assertIDs(t *testing.T, got []WorkflowInstance, want []string, why string) {
	t.Helper()
	gotIDs := make([]string, len(got))
	for i, wf := range got {
		gotIDs[i] = wf.ID
	}
	if len(gotIDs) != len(want) {
		t.Errorf("%s\n\ngot %d row(s): %v\nwant %d: %v", why, len(gotIDs), gotIDs, len(want), want)
		return
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Errorf("%s\n\ngot:  %v\nwant: %v\n(first difference at index %d)",
				why, gotIDs, want, i)
			return
		}
	}
}
