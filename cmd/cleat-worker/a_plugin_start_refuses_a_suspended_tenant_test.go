package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// cleat#3594. A suspended tenant starts nothing, and that has to hold for the
// callers that are not an HTTP request: the scheduler, event-triggers and the job
// queue all reach the engine through startPluginWorkflow. handleStart refused a
// suspended tenant; this path did not, so those three kept creating runs that
// sat `ready` and unclaimed (the rotating claim omits a suspended tenant).

// startPluginAgainst starts a run through startPluginWorkflow on a mock store
// and reports whether the store was asked to create one.
func startPluginAgainst(t *testing.T, st *suspendableStore) (created int, err error) {
	t.Helper()
	st.mockStore.startNewRunFn = func(context.Context, string, string, int, json.RawMessage, string, string, int) (string, bool, error) {
		created++
		return "run-1", false, nil
	}
	_, err = startPluginWorkflow(context.Background(), st, plugin.StartRequest{
		DefName: "d", Input: json.RawMessage(`{}`), IdempotencyKey: "k", TenantID: uuid.NewString(),
	})
	return created, err
}

func TestAPluginStartIsRefusedForASuspendedTenantAndCreatesNothing(t *testing.T) {
	// The control comes first and is the same call with only the flag changed.
	// Without it the refusal below passes against a function that refuses everything.
	t.Run("control: not suspended starts normally", func(t *testing.T) {
		st := &suspendableStore{mockStore: &mockStore{}, suspended: false}
		created, err := startPluginAgainst(t, st)
		if err != nil {
			t.Fatalf("an unsuspended tenant's plugin start failed: %v", err)
		}
		if created != 1 || st.asked != 1 {
			t.Errorf("created=%d asked=%d; want 1 and 1 (one run, and the flag read once)", created, st.asked)
		}
	})

	t.Run("suspended is refused and the store is never asked to create a run", func(t *testing.T) {
		st := &suspendableStore{mockStore: &mockStore{}, suspended: true}
		created, err := startPluginAgainst(t, st)
		if err == nil {
			t.Fatal("a suspended tenant's plugin start succeeded")
		}
		if !strings.Contains(err.Error(), "suspended") {
			t.Errorf("the refusal does not say why: %v", err)
		}
		if created != 0 {
			t.Errorf("StartNewRun was called %d time(s) for a suspended tenant", created)
		}
	})

	t.Run("an unreadable flag is a refusal, not a pass", func(t *testing.T) {
		boom := errors.New("admin.tenants unreachable")
		st := &suspendableStore{mockStore: &mockStore{}, err: boom}
		created, err := startPluginAgainst(t, st)
		if !errors.Is(err, boom) {
			t.Fatalf("got %v; want the read error", err)
		}
		if created != 0 {
			t.Errorf("StartNewRun was called %d time(s) although the flag could not be read", created)
		}
	})
}

// The same property against a real store, where "creates nothing" is a row
// count rather than a mock's call counter, and where the two states are
// reached the way an operator reaches them (SetTenantSuspended).
func TestAPluginStartIsRefusedForASuspendedTenantOnAStoreWithRows(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping database-backed cleat#3594 test")
	}
	db := testutil.SuiteTestDB(t, "cleat_worker")
	rlsDB := testutil.OpenPostgresRLSTestDB(t, db)
	factory := engine.NewPostgresStoreFactory(rlsDB, "public")
	ctx := context.Background()

	ts := auth.NewTenantStore(db)
	orgID, err := ts.CreateOrg(ctx, "susp-start-"+uuid.NewString())
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	tid, err := ts.CreateTenant(ctx, "susp-start-"+uuid.NewString(), "t", orgID)
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	tenant := tid.String()

	tenantStore, closer, err := factory.OpenStore(ctx, tenant, "default")
	if err != nil {
		t.Fatalf("OpenStore(tenant): %v", err)
	}
	defer closer.Close()
	if err := tenantStore.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: "plugin-start-3594", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	processStore, pcloser, err := factory.OpenStore(ctx, engine.DefaultTenantUUID, "default")
	if err != nil {
		t.Fatalf("OpenStore(default): %v", err)
	}
	defer pcloser.Close()

	// workflow_instances holds one row per run on every dialect; the superuser
	// connection counts them without RLS in the way.
	runs := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM workflow_instances WHERE tenant_id = $1::uuid`, tenant).Scan(&n); err != nil {
			t.Fatalf("count runs: %v", err)
		}
		return n
	}
	start := func(key string) (string, error) {
		return startPluginWorkflow(ctx, processStore, plugin.StartRequest{
			DefName: "plugin-start-3594", Input: json.RawMessage(`{}`), IdempotencyKey: key, TenantID: tenant,
		})
	}

	// CONTROL: unsuspended, the start creates exactly one row.
	if id, err := start("before"); err != nil || id == "" {
		t.Fatalf("CONTROL FAILED: an unsuspended tenant's plugin start: id=%q err=%v", id, err)
	}
	if got := runs(); got != 1 {
		t.Fatalf("CONTROL FAILED: %d run rows after one start; want 1", got)
	}

	if err := ts.SetTenantSuspended(ctx, tid, true); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if id, err := start("during"); err == nil {
		t.Fatalf("a suspended tenant's plugin start succeeded: run %q", id)
	}
	if got := runs(); got != 1 {
		t.Errorf("%d run rows after a refused start; want still 1", got)
	}

	// Resuming restores it: the refusal is the flag, not something the first
	// refusal left behind.
	if err := ts.SetTenantSuspended(ctx, tid, false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if id, err := start("after"); err != nil || id == "" {
		t.Fatalf("a resumed tenant's plugin start: id=%q err=%v", id, err)
	}
	if got := runs(); got != 2 {
		t.Errorf("%d run rows after resume and one more start; want 2", got)
	}
}
