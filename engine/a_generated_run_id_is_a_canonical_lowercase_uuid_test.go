package engine

// cleat#3639. workflow_instances.id compares case-INsensitively on SQL Server
// (SQL_Latin1_General_CP1_CI_AS) and MySQL (utf8mb4_0900_ai_ci) and exactly on
// PostgreSQL, so two run ids that differ only by case are one run on two
// dialects and two runs on the third. Nothing shipped can create such a pair,
// and the reason is narrow: every id a store generates is a canonical
// lowercase UUID, and no caller outside tests supplies one. This pins the first
// half, so a generator that started emitting upper case (SQL Server's NEWID()
// renders it, for one) turns this red instead of becoming a portability
// divergence nobody sees.
//
// Asserted on the STORED row, not on the string the store returned. On SQL
// Server and MySQL a lookup by the id matches in either case, so a store that
// returned a lowercase string while writing another spelling would pass a check
// on the return value alone; reading the column back and comparing bytes does
// not.

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/google/uuid"
)

// shardedChildID is the shard-aligned child id ShardedStore.StartChildWorkflowAtomic
// builds when no id is given: <root>.c<step>.<12 hex>, root being the parent's id.
var shardedChildID = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.c([0-9]+)\.([0-9a-f]{12})$`)

func TestEveryRunIdAStoreGeneratesIsACanonicalLowercaseUUID(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			truncateAll(t, store)
			ctx := context.Background()

			const defName = "run-id-case-wf"
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			// A one-shard ShardedStore over the same store, to reach the ids the
			// sharded layer generates itself (sharded_store.go StartNewRun,
			// StartNewRunWithOptions and the shard-aligned child id).
			sharded, err := NewShardedStore([]ShardConfig{{Name: "only"}}, []WorkflowStore{store},
				[]func() error{func() error { return nil }})
			if err != nil {
				t.Fatalf("NewShardedStore: %v", err)
			}

			type generated struct{ path, id string }
			var ids []generated
			record := func(path, id string, err error) string {
				t.Helper()
				if err != nil {
					t.Fatalf("%s: %v", path, err)
				}
				ids = append(ids, generated{path, id})
				return id
			}
			input := json.RawMessage(`{}`)

			// The idempotency-key variant is its own code path on MySQL
			// (tryStartNewRunUnderIdempotencyKey), so both spellings of a start run.
			parent, _, err := store.StartNewRun(ctx, "", defName, 1, input, "", DefaultTenantUUID, 0)
			record("StartNewRun", parent, err)
			keyed, _, err := store.StartNewRun(ctx, "", defName, 1, input, "run-id-case-key", DefaultTenantUUID, 0)
			record("StartNewRun with an idempotency key", keyed, err)

			shardedRun, _, err := sharded.StartNewRun(ctx, "", defName, 1, input, "", DefaultTenantUUID, 0)
			record("ShardedStore.StartNewRun", shardedRun, err)
			shardedOpts, _, err := sharded.StartNewRunWithOptions(ctx, "", defName, 1, input, "", DefaultTenantUUID, 0, StartOptions{})
			record("ShardedStore.StartNewRunWithOptions", shardedOpts, err)

			child, err := store.StartChildWorkflow(ctx, parent, defName, `{}`, 1, "ABANDON", 0)
			record("StartChildWorkflow", child, err)
			atomic, err := store.StartChildWorkflowAtomic(ctx, "", parent, defName, `{}`, 1, "ABANDON",
				EventRecord{Step: 0, EventType: "child_workflow", ChildName: defName, ChildInput: `{}`}, 0)
			record("StartChildWorkflowAtomic", atomic, err)

			// The shard-aligned child id is a different format, asserted separately below.
			shardedChild, err := sharded.StartChildWorkflowAtomic(ctx, "", shardedRun, defName, `{}`, 1, "ABANDON",
				EventRecord{Step: 7, EventType: "child_workflow", ChildName: defName, ChildInput: `{}`}, 0)
			if err != nil {
				t.Fatalf("ShardedStore.StartChildWorkflowAtomic: %v", err)
			}

			// ContinueAsNew needs a claimed run. Whichever run is claimed, its
			// successor's id is what this path generates.
			wf, err := store.ClaimWorkflow(ctx, "worker-3639")
			if err != nil || wf == nil {
				t.Fatalf("ClaimWorkflow: wf=%v err=%v", wf, err)
			}
			next, err := store.ContinueAsNew(ctx, wf.ID, "worker-3639", wf.Generation, wf.DefName, wf.DefVersion,
				input, nil, "", nil, 0)
			record("ContinueAsNew", next, err)

			d := dialectOf(t, store)
			db := rawDBOf(t, store)
			stored := func(path, id string) string {
				t.Helper()
				var n int
				if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_instances WHERE id = `+d.placeholder(1), id).Scan(&n); err != nil {
					t.Fatalf("%s: count rows for %s: %v", path, id, err)
				}
				if n != 1 {
					t.Fatalf("%s: %d workflow_instances rows match the id %q it returned, want 1", path, n, id)
				}
				var got string
				if err := db.QueryRow(`SELECT id FROM workflow_instances WHERE id = `+d.placeholder(1), id).Scan(&got); err != nil {
					t.Fatalf("%s: read back the stored id for %s: %v", path, id, err)
				}
				return got
			}

			for _, g := range ids {
				got := stored(g.path, g.id)
				if got != g.id {
					t.Errorf("%s returned %q but the row holds %q; a lookup matches either spelling on SQL Server and MySQL, so this is only visible by comparing bytes", g.path, g.id, got)
				}
				u, perr := uuid.Parse(got)
				if perr != nil || u.String() != got {
					t.Errorf("%s stored the id %q, want a canonical lowercase UUID (%v)", g.path, got, perr)
				}
			}

			// The sharded child: <root>.c<step>.<hex>, every part lowercase, and the
			// root is the parent's own id.
			got := stored("ShardedStore.StartChildWorkflowAtomic", shardedChild)
			m := shardedChildID.FindStringSubmatch(got)
			if m == nil {
				t.Fatalf("ShardedStore.StartChildWorkflowAtomic stored the id %q, want <lowercase uuid>.c<step>.<12 lowercase hex>", got)
			}
			if got != shardedChild {
				t.Errorf("ShardedStore.StartChildWorkflowAtomic returned %q but the row holds %q", shardedChild, got)
			}
			if m[1] != shardedRun {
				t.Errorf("the sharded child's root is %q, want its parent's id %q", m[1], shardedRun)
			}
			if m[2] != "7" {
				t.Errorf("the sharded child's step is %q, want 7", m[2])
			}
			if u, perr := uuid.Parse(m[1]); perr != nil || u.String() != m[1] {
				t.Errorf("the sharded child's root %q is not a canonical lowercase UUID (%v)", m[1], perr)
			}
		})
	}
}
