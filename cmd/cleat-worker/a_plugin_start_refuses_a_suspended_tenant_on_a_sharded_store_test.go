package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// A suspended tenant's plugin start is refused on a SHARDED store too (cleat#3596).
//
// startPluginWorkflow, like handleStart, decides by a type assertion on the store it holds, and a
// *engine.ShardedStore used to implement nothing of the kind: the assertion failed and the check was
// skipped without a word. Same fixture as TestAPluginStartIsRefusedForASuspendedTenantOnAStoreWithRows,
// with the process store wrapped in a one-shard ShardedStore, which is the shape a sharded worker hands
// startPluginWorkflow.
func TestAPluginStartIsRefusedForASuspendedTenantOnAShardedStore(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping database-backed cleat#3596 test")
	}
	db := testutil.SuiteTestDB(t, "cleat_worker")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, db)
	factory := engine.NewPostgresStoreFactory(rlsDB, "public")
	ctx := context.Background()

	ts := auth.NewTenantStore(db)
	orgID, err := ts.CreateOrg(ctx, "susp-shard-"+uuid.NewString())
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	tid, err := ts.CreateTenant(ctx, "susp-shard-"+uuid.NewString(), "t", orgID)
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	tenant := tid.String()
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `SELECT admin.drop_tenant($1::uuid, 'public')`, tenant)
	})

	tenantStore, closer, err := factory.OpenStore(ctx, tenant, "default")
	if err != nil {
		t.Fatalf("OpenStore(tenant): %v", err)
	}
	defer closer.Close()
	if err := tenantStore.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: "plugin-start-3596", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	shardStore, scloser, err := factory.OpenStore(ctx, engine.DefaultTenantUUID, "default")
	if err != nil {
		t.Fatalf("OpenStore(default): %v", err)
	}
	defer scloser.Close()
	sharded, err := engine.NewShardedStore(
		[]engine.ShardConfig{{Name: "shard0"}},
		[]engine.WorkflowStore{shardStore},
		[]func() error{func() error { return nil }})
	if err != nil {
		t.Fatalf("NewShardedStore: %v", err)
	}

	runs := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM workflow_instances WHERE tenant_id = $1::uuid`, tenant).Scan(&n); err != nil {
			t.Fatalf("count runs: %v", err)
		}
		return n
	}
	start := func(key string) (string, error) {
		return startPluginWorkflow(ctx, sharded, plugin.StartRequest{
			DefName: "plugin-start-3596", Input: json.RawMessage(`{}`), IdempotencyKey: key, TenantID: tenant,
		})
	}

	// CONTROL: unsuspended, a start on the sharded store creates exactly one row. Without it a refusal
	// below could be the sharded store refusing everything.
	if id, err := start("before"); err != nil || id == "" {
		t.Fatalf("CONTROL FAILED: an unsuspended tenant's plugin start on a sharded store: id=%q err=%v", id, err)
	}
	if got := runs(); got != 1 {
		t.Fatalf("CONTROL FAILED: %d run rows after one start; want 1", got)
	}

	if err := ts.SetTenantSuspended(ctx, tid, true); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if id, err := start("during"); err == nil {
		t.Fatalf("a suspended tenant's plugin start succeeded on a sharded store: run %q", id)
	}
	if got := runs(); got != 1 {
		t.Errorf("%d run rows after a refused start; want still 1", got)
	}

	if err := ts.SetTenantSuspended(ctx, tid, false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if id, err := start("after"); err != nil || id == "" {
		t.Fatalf("a resumed tenant's plugin start on a sharded store: id=%q err=%v", id, err)
	}
	if got := runs(); got != 2 {
		t.Errorf("%d run rows after resume and one more start; want 2", got)
	}
}
