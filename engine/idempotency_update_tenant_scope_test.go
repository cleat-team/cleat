package engine

// TestIdempotencyResultUpdatesAreScopedToTenant is the regression test for
// the Finding S1 residual: CompleteWorkflow, FailWorkflow, and
// MoveToDeadLetterQueue each write a best-effort UPDATE to idempotency_keys
// filtered on workflow_id alone (engine/store_lifecycle.go:361,418,526 before
// this fix), with no tenant_id predicate at all.
//
// workflow_instances.id is a bare TEXT PRIMARY KEY with no tenant component,
// so two tenants cannot simultaneously hold a workflow with the same id --
// but idempotency_keys.workflow_id carries no foreign key to
// workflow_instances(id) and is not cleaned up when the workflow it names is
// deleted (DeleteDeadLetteredWorkflows deletes only workflow_instances rows;
// the CASCADE on concurrency_keys/workflow_update_requests does not reach
// idempotency_keys, which has no FK at all). So a stale idempotency_keys row
// naming a workflow_id that a *different* tenant later reuses -- a real risk
// once ids are freed by deletion, and trivially reproducible directly, since
// nothing enforces global uniqueness of idempotency_keys.workflow_id itself
// -- is exactly the case an unscoped UPDATE corrupts: tenant B completing
// its own workflow silently overwrites tenant A's already-recorded
// idempotency result.
//
// This test does not go through DeleteDeadLetteredWorkflows to manufacture
// that state; it inserts the colliding row directly, which isolates the
// property under test (does the UPDATE respect tenant_id) from how such a
// row could come to exist.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestIdempotencyResultUpdatesAreScopedToTenant(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, db)
	defer testutil.CleanupPostgresTestData(t, db)

	ctx := context.Background()
	const tenantB = "d1d1d1d1-d1d1-4d1d-9d1d-d1d1d1d1d1d1"
	const decoyTenant = "d2d2d2d2-d2d2-4d2d-9d2d-d2d2d2d2d2d2"

	// One shared definition (workflow_defs is tenant-global by design; see
	// migrations/postgres/001_schema.sql's comment on tenant_isolation_defs).
	storeAdmin := NewPostgresStore(db)
	const defName = "idem-scope-def"
	if err := storeAdmin.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	storeB := NewPostgresStore(db).WithTenant(tenantB)

	wfID := fmt.Sprintf("idem-scope-wf-%d", time.Now().UnixNano())
	gotID, alreadyExisted, err := storeB.StartNewRun(ctx, wfID, defName, 1, json.RawMessage(`{}`), "tenant-b-key", tenantB, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	if alreadyExisted {
		t.Fatalf("fresh idempotency key reported already-existing")
	}
	if gotID != wfID {
		t.Fatalf("StartNewRun returned %q, want %q", gotID, wfID)
	}

	// The decoy: a different tenant's idempotency_keys row that happens to
	// name the SAME workflow_id. On the real schema this key_hash+tenant_id
	// pair is a valid, distinct primary key (010_idempotency_keys_tenant_id.sql
	// widened the PK to (key_hash, tenant_id)), so this is a row the decoy
	// tenant could legitimately have -- it is only the shared workflow_id
	// that is contrived, standing in for the id-reuse-after-deletion window
	// described above.
	decoyHash := sha256.Sum256([]byte("decoy-tenant-a-key"))
	if _, err := db.ExecContext(ctx, `
		INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id)
		VALUES ($1, $2, now() + INTERVAL '1 hour', $3)
	`, decoyHash[:], wfID, decoyTenant); err != nil {
		t.Fatalf("insert decoy idempotency_keys row: %v", err)
	}

	// Claim and complete as tenant B -- the real path CompleteWorkflow is
	// reached from.
	claimed, err := storeB.ClaimWorkflows(ctx, "worker-scope-test", 1)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != wfID {
		t.Fatalf("ClaimWorkflows: got %+v, want exactly [%s]", claimed, wfID)
	}
	wf := claimed[0]

	if err := storeB.CompleteWorkflow(ctx, wfID, wf.AssignedTo, wf.Generation, `{"ok":true}`, nil); err != nil {
		t.Fatalf("CompleteWorkflow: %v", err)
	}

	// Tenant B's own row must have been updated.
	var bResult []byte
	if err := db.QueryRowContext(ctx,
		`SELECT result FROM idempotency_keys WHERE workflow_id = $1 AND tenant_id = $2`,
		wfID, tenantB,
	).Scan(&bResult); err != nil {
		t.Fatalf("read tenant B's idempotency_keys row: %v", err)
	}
	if bResult == nil {
		t.Errorf("tenant B's own idempotency_keys row was not updated by its own CompleteWorkflow")
	}

	// The point of the test: the decoy row, which names the same
	// workflow_id but belongs to a different tenant, must be untouched.
	// Against the unfixed UPDATE (WHERE workflow_id = $1, no tenant_id
	// predicate) this row is also matched and overwritten with tenant B's
	// result -- a cross-tenant write to data tenant B never owned.
	var decoyResult []byte
	if err := db.QueryRowContext(ctx,
		`SELECT result FROM idempotency_keys WHERE workflow_id = $1 AND tenant_id = $2`,
		wfID, decoyTenant,
	).Scan(&decoyResult); err != nil {
		t.Fatalf("read decoy idempotency_keys row: %v", err)
	}
	if decoyResult != nil {
		t.Errorf("a different tenant's idempotency_keys row (same workflow_id) was overwritten by "+
			"tenant B's CompleteWorkflow: result = %s -- the UPDATE is not scoped to tenant_id", decoyResult)
	}
}
