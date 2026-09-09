package engine

// TestIdempotencyOutcomeUpdatesAreScopedToTenant is the regression test for
// the Finding S1 residual: CompleteWorkflow, FailWorkflow, and
// MoveToDeadLetterQueue each wrote a best-effort UPDATE to idempotency_keys
// filtered on workflow_id alone, with no tenant_id predicate at all.
//
// Two of those three sites remain. cleat#1049 dropped idempotency_keys.result
// -- the column CompleteWorkflow's write filled and nothing read -- so the
// success path no longer touches this table and there is no third statement
// left to scope. FailWorkflow and MoveToDeadLetterQueue still write
// error_msg, and this test now drives that arm. Same UPDATE shape, same
// predicate, same three dialects; only the column changed.
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
// -- is exactly the case an unscoped UPDATE corrupts: tenant B failing its
// own workflow silently overwrites tenant A's already-recorded idempotency
// outcome.
//
// This test does not go through DeleteDeadLetteredWorkflows to manufacture
// that state; it inserts the colliding row directly, which isolates the
// property under test (does the UPDATE respect tenant_id) from how such a
// row could come to exist.
//
// WHY THIS RUNS ON THREE DIALECTS, AND WHY THAT IS THE POINT.
//
// Until 2026-09-08 this test hardcoded testutil.DialectPostgres, and the fix
// it guards had been applied to PostgreSQL only. MySQL and SQL Server kept
// the unscoped UPDATE at all three sites for as long as this test was green.
//
// That is the failure mode worth naming: not an absent test, but a *passing*
// one covering a third of the surface. An absent test looks like a gap. A
// green test on one dialect of three looks like the property is pinned, and
// nothing in the suite distinguishes them. idempotency_keys has no row-level
// security on any dialect -- PostgreSQL's RLS covers 11 tables and SQL
// Server's SECURITY POLICY covers 7, and it is on neither list -- so these
// predicates are the whole of the protection, on every backend, and the
// per-dialect assertion is the only thing that can say so.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// idempotencyScopeDialect carries the per-backend differences: the store
// constructor and the two statements this test issues outside the store.
// Placeholder syntax and interval arithmetic are the only things that vary;
// the assertions below are identical on every backend, which is the property
// being claimed.
type idempotencyScopeDialect struct {
	name    string
	dialect testutil.Dialect

	// newStore returns a store bound to tenantID.
	newStore func(db *sql.DB, tenantID string) idempotencyScopeStore

	// insertDecoy inserts an idempotency_keys row for (keyHash, wfID, tenant)
	// expiring an hour out.
	insertDecoy string

	// selectOutcome reads idempotency_keys.error_msg for (wfID, tenant).
	//
	// error_msg, not result: cleat#1049 dropped idempotency_keys.result,
	// which every write site filled and no reader ever selected. The
	// property under test is unchanged -- error_msg is written by the same
	// UPDATE shape, under the same `AND tenant_id` predicate, from the other
	// branch of the same IF -- and it is the branch two of the three sites
	// named above (FailWorkflow, MoveToDeadLetterQueue) always took.
	selectOutcome string
}

// idempotencyScopeStore is the slice of each store this test drives. All
// three concrete stores satisfy it; naming it here keeps the table honest
// about what is actually exercised.
type idempotencyScopeStore interface {
	DeployWorkflowDef(ctx context.Context, def *WorkflowDef) error
	StartNewRun(ctx context.Context, workflowID, defName string, version int, input json.RawMessage, idemKey, tenantID string, priority int) (string, bool, error)
	ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error)
	FailWorkflow(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error

	// The path the worker actually takes. cmd/cleat-worker/setup.go:1935 calls
	// this, not CompleteWorkflow -- whose only non-test callers are in
	// cmd/cleat-bench. On every dialect it delegates the terminal writes to the
	// finalize_workflow_status stored procedure, so a test that drives
	// CompleteWorkflow exercises Go code production does not run.
	FinalizeWorkflowSegment(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error
}

func TestIdempotencyOutcomeUpdatesAreScopedToTenant(t *testing.T) {
	for _, d := range idempotencyScopeDialects() {
		t.Run(d.name, func(t *testing.T) {
			runIdempotencyTenantScopeCase(t, d)
		})
	}
}

func runIdempotencyTenantScopeCase(t *testing.T, d idempotencyScopeDialect) {
	t.Helper()

	db := testutil.TestDB(t, d.dialect)
	defer db.Close()
	testutil.SetupFullSchema(t, db, d.dialect)
	testutil.CleanupAllTestData(t, db, d.dialect)
	defer testutil.CleanupAllTestData(t, db, d.dialect)

	ctx := context.Background()
	const tenantB = "d1d1d1d1-d1d1-4d1d-9d1d-d1d1d1d1d1d1"
	const decoyTenant = "d2d2d2d2-d2d2-4d2d-9d2d-d2d2d2d2d2d2"

	// One definition per tenant. workflow_defs is keyed by tenant since D7
	// (IMPROVEMENT-PLAN 3.77), so the decoy tenant needs its own.
	defName := fmt.Sprintf("idem-scope-def-%s", d.name)
	for _, tn := range []string{tenantB, decoyTenant} {
		if err := d.newStore(db, tn).DeployWorkflowDef(ctx, &WorkflowDef{
			Name: defName, Version: 1,
			WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
			ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("deploy %q for tenant %s: %v", defName, tn, err)
		}
	}

	storeB := d.newStore(db, tenantB)

	wfID := fmt.Sprintf("idem-scope-wf-%s-%d", d.name, time.Now().UnixNano())
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
	// pair is a valid, distinct primary key (010_idempotency_keys_tenant_id
	// widened the PK to (key_hash, tenant_id)), so this is a row the decoy
	// tenant could legitimately have -- it is only the shared workflow_id
	// that is contrived, standing in for the id-reuse-after-deletion window
	// described above.
	decoyHash := sha256.Sum256([]byte("decoy-tenant-a-key"))
	if _, err := db.ExecContext(ctx, d.insertDecoy, decoyHash[:], wfID, decoyTenant); err != nil {
		t.Fatalf("insert decoy idempotency_keys row: %v", err)
	}

	// Claim and fail as tenant B. The failure arm, not the success arm:
	// since #1049 dropped idempotency_keys.result, a completed run writes
	// nothing to this table and there would be nothing to scope.
	claimed, err := storeB.ClaimWorkflows(ctx, "worker-scope-test", 1)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != wfID {
		t.Fatalf("ClaimWorkflows: got %+v, want exactly [%s]", claimed, wfID)
	}
	wf := claimed[0]

	if err := storeB.FailWorkflow(ctx, wfID, wf.AssignedTo, wf.Generation,
		`{"error":"tenant B's own failure"}`, "", "", nil); err != nil {
		t.Fatalf("FailWorkflow: %v", err)
	}

	// Tenant B's own row must have been updated -- the control. Without it a
	// store that updated nothing at all would pass the cross-tenant half
	// trivially.
	//
	var bErrMsg []byte
	if err := db.QueryRowContext(ctx, d.selectOutcome, wfID, tenantB).Scan(&bErrMsg); err != nil {
		t.Fatalf("read tenant B's idempotency_keys row: %v", err)
	}
	if bErrMsg == nil {
		t.Errorf("tenant B's own idempotency_keys row was not updated by its own FailWorkflow")
	}

	// The point of the test: the decoy row, which names the same
	// workflow_id but belongs to a different tenant, must be untouched.
	// Against the unfixed UPDATE (WHERE workflow_id = ?, no tenant_id
	// predicate) this row is also matched and overwritten with tenant B's
	// error -- a cross-tenant write to data tenant B never owned.
	var decoyErrMsg []byte
	if err := db.QueryRowContext(ctx, d.selectOutcome, wfID, decoyTenant).Scan(&decoyErrMsg); err != nil {
		t.Fatalf("read decoy idempotency_keys row: %v", err)
	}
	if decoyErrMsg != nil {
		t.Errorf("a different tenant's idempotency_keys row (same workflow_id) was overwritten by "+
			"tenant B's FailWorkflow: error_msg = %s -- the UPDATE is not scoped to tenant_id", decoyErrMsg)
	}
}

// idempotencyScopeDialects is the table. Each backend that TestDB can reach
// runs the identical assertions; TestDB skips the ones whose DSN is unset,
// so a single-dialect job costs two skips here rather than silently covering
// one third of the surface, which is what the previous hardcoded version did.
func idempotencyScopeDialects() []idempotencyScopeDialect {
	return []idempotencyScopeDialect{
		{
			name:    "postgres",
			dialect: testutil.DialectPostgres,
			newStore: func(db *sql.DB, tenantID string) idempotencyScopeStore {
				return NewPostgresStore(db).WithTenant(tenantID)
			},
			insertDecoy: `INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id)
			              VALUES ($1, $2, now() + INTERVAL '1 hour', $3)`,
			selectOutcome: `SELECT error_msg FROM idempotency_keys WHERE workflow_id = $1 AND tenant_id = $2`,
		},
		{
			name:    "mysql",
			dialect: testutil.DialectMySQL,
			newStore: func(db *sql.DB, tenantID string) idempotencyScopeStore {
				return NewMySQLStore(db).WithTenant(tenantID)
			},
			insertDecoy: `INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id)
			              VALUES (?, ?, DATE_ADD(NOW(6), INTERVAL 1 HOUR), ?)`,
			selectOutcome: `SELECT error_msg FROM idempotency_keys WHERE workflow_id = ? AND tenant_id = ?`,
		},
		{
			name:    "mssql",
			dialect: testutil.DialectMSSQL,
			newStore: func(db *sql.DB, tenantID string) idempotencyScopeStore {
				return NewMSSQLStore(db).WithTenant(tenantID)
			},
			insertDecoy: `INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id)
			              VALUES (@p1, @p2, DATEADD(hour, 1, SYSUTCDATETIME()), @p3)`,
			selectOutcome: `SELECT error_msg FROM idempotency_keys WHERE workflow_id = @p1 AND tenant_id = @p2`,
		},
	}
}

// TestFinalizeDoesNotWriteIdempotencyAcrossTenants is the same property as
// TestIdempotencyOutcomeUpdatesAreScopedToTenant, on the path production takes.
//
// WHY BOTH EXIST. The first drives CompleteWorkflow, a store method whose only
// non-test callers are in cmd/cleat-bench. The worker finalizes through
// FinalizeWorkflowSegment (cmd/cleat-worker/setup.go:1935), which delegates the
// terminal writes to the finalize_workflow_status stored procedure -- a
// different implementation of the same rule, in SQL, in a migration.
//
// cleat#1012 fixed the Go methods and left the procedure untouched, and the
// first test passed on all three dialects throughout, including its
// falsifications. An exhaustive check of the wrong set: three dialects felt
// like completeness, and the axis that mattered was the call graph. Nothing in
// a Go-level search can see the procedure's statement at all.
//
// So this is not redundant coverage. It is the only arm that binds the code the
// worker runs, and it is table-driven for the same reason as its sibling: the
// procedure is defined separately per dialect, so no dialect stands in for
// another.
func TestFinalizeDoesNotWriteIdempotencyAcrossTenants(t *testing.T) {
	for _, d := range idempotencyScopeDialects() {
		t.Run(d.name, func(t *testing.T) {
			db := testutil.TestDB(t, d.dialect)
			defer db.Close()
			testutil.SetupFullSchema(t, db, d.dialect)
			testutil.CleanupAllTestData(t, db, d.dialect)
			defer testutil.CleanupAllTestData(t, db, d.dialect)

			ctx := context.Background()
			const tenantB = "d4d4d4d4-d4d4-4d4d-9d4d-d4d4d4d4d4d4"
			const decoyTenant = "d5d5d5d5-d5d5-4d5d-9d5d-d5d5d5d5d5d5"

			defName := fmt.Sprintf("idem-finalize-def-%s", d.name)
			for _, tn := range []string{tenantB, decoyTenant} {
				if err := d.newStore(db, tn).DeployWorkflowDef(ctx, &WorkflowDef{
					Name: defName, Version: 1,
					WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
					ABIVersion: 1, MinVersion: 1,
				}); err != nil {
					t.Fatalf("deploy %q for tenant %s: %v", defName, tn, err)
				}
			}

			storeB := d.newStore(db, tenantB)
			wfID := fmt.Sprintf("idem-finalize-wf-%s-%d", d.name, time.Now().UnixNano())
			if _, _, err := storeB.StartNewRun(ctx, wfID, defName, 1, json.RawMessage(`{}`), "finalize-tenant-b-key", tenantB, 0); err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			// The decoy: another tenant's idempotency row naming the SAME
			// workflow_id. Legitimate on the real schema, where the primary key
			// is (key_hash, tenant_id).
			decoyHash := sha256.Sum256([]byte("finalize-decoy-key"))
			if _, err := db.ExecContext(ctx, d.insertDecoy, decoyHash[:], wfID, decoyTenant); err != nil {
				t.Fatalf("insert decoy idempotency_keys row: %v", err)
			}

			claimed, err := storeB.ClaimWorkflows(ctx, "worker-finalize-test", 1)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}
			if len(claimed) != 1 || claimed[0].ID != wfID {
				t.Fatalf("ClaimWorkflows: got %+v, want exactly [%s]", claimed, wfID)
			}
			wf := claimed[0]

			// The production call. Terminal status 'failed' takes the
			// procedure's idempotency arm -- since #1049 the 'done' arm
			// writes no idempotency row at all.
			//
			// A JSON object, not a bare sentence, because FinalizeWorkflowSegment
			// runs `result` through coerceResultJSON regardless of finalStatus
			// and a non-JSON value is replaced by `{}` before the procedure sees
			// it. Production passes a JSON error payload here, so this matches
			// what the worker sends; a plain string would still exercise the
			// scoping but every assertion message would read `error_msg = {}`.
			if err := storeB.FinalizeWorkflowSegment(ctx, wfID, wf.AssignedTo, wf.Generation,
				nil, "failed", `{"error":"tenant B's own failure"}`, "", "", nil, time.Time{}); err != nil {
				t.Fatalf("FinalizeWorkflowSegment: %v", err)
			}

			// Control: tenant B's own row must have been written, or the
			// cross-tenant assertion below passes for the wrong reason.
			var bErrMsg []byte
			if err := db.QueryRowContext(ctx, d.selectOutcome, wfID, tenantB).Scan(&bErrMsg); err != nil {
				t.Fatalf("read tenant B's idempotency_keys row: %v", err)
			}
			if bErrMsg == nil {
				t.Errorf("tenant B's own idempotency_keys row was not written by its own " +
					"FinalizeWorkflowSegment -- the decoy assertion below would pass vacuously")
			}

			var decoyErrMsg []byte
			if err := db.QueryRowContext(ctx, d.selectOutcome, wfID, decoyTenant).Scan(&decoyErrMsg); err != nil {
				t.Fatalf("read decoy idempotency_keys row: %v", err)
			}
			if decoyErrMsg != nil {
				t.Errorf("a different tenant's idempotency_keys row (same workflow_id) was "+
					"overwritten by tenant B's FinalizeWorkflowSegment: error_msg = %s -- "+
					"finalize_workflow_status's UPDATE is not scoped to tenant_id", decoyErrMsg)
			}
		})
	}
}
