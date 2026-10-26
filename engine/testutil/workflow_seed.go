package testutil

import (
	"database/sql"
	"testing"
)

// These three match workflow_leases' and workflow_instances' own column
// defaults (migrations/postgres/001 and 016) exactly -- COALESCE below falls
// back to these literals rather than the table's DEFAULT clause because a
// column given an explicit (even empty-string) argument never reaches its
// DEFAULT. Re-derive if either migration's defaults ever change.
const (
	defaultLeaseTenantID  = "00000000-0000-0000-0000-000000000000"
	defaultLeaseTaskQueue = "default"
	defaultLeaseStatus    = "ready"
)

// WorkflowLeaseSeed describes the workflow_leases row to create alongside a
// raw-seeded workflow_instances row of the same id. A zero-value field takes
// workflow_leases' own column default (migrations/postgres/016, folded
// into migrations/postgres/001_schema.sql by #3358), which
// agrees with workflow_instances' default for every field here -- both
// tables default status to 'ready' and tenant_id to the zero UUID.
type WorkflowLeaseSeed struct {
	TenantID   string
	TaskQueue  string
	Status     string
	AssignedTo string
}

// SeedWorkflowLease inserts the workflow_leases AND workflow_payloads rows
// that must accompany a raw-seeded workflow_instances row sharing its id --
// mirroring StartNewRun's dual-write (engine/store_lifecycle.go's
// insertLeaseAndPayloadRows). Call it immediately after the
// workflow_instances INSERT, with the same id and whatever of
// TenantID/TaskQueue/Status/AssignedTo that INSERT set explicitly.
//
// cleat#3245 Phase 3: ClaimWorkflows' lease write is a plain UPDATE (the
// only claim write since B3c), which is a silent no-op against a row that was never
// inserted here -- so a fixture that skips this call does not fail loudly,
// it fails the moment something reads the candidate set from
// workflow_leases instead of workflow_instances (piece A's
// ReapStaleInstances/ReapStaleInstancesExcept/ListStaleHolders cutover, and
// piece B's claim-path cutover next). See cleat#3245's tracking issue for
// the three rounds this was found the hard way before becoming a helper.
//
// The workflow_payloads row was missing from this function until Phase 3
// step 3 piece F, which is the first thing to make its absence loud: piece
// F's ListWorkflows/CountWorkflows/GetWorkflowByID read workflow_leases
// JOIN workflow_payloads, an INNER join, so a lease with no matching
// payload row is invisible to all three -- every one of this function's 25+
// callers across engine/, tests/ and plugins/ seeded a lease-only row and
// would otherwise have reported every seeded workflow "not found" the
// moment any of their tests reached GetWorkflowByID. This function's own
// doc comment already called itself a "dual-write" mirror before piece F;
// it only became one here.
//
// PostgreSQL only: workflow_leases and workflow_payloads do not exist on
// MySQL or SQL Server. A fixture that seeds more than one dialect must gate
// this call itself -- see tests/crash/dialects_test.go's
// crashTarget.startWorkflowEntry for the pattern. This function does not
// check the dialect.
func SeedWorkflowLease(t *testing.T, db *sql.DB, id string, seed WorkflowLeaseSeed) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO workflow_leases (id, tenant_id, task_queue, status, assigned_to)
		VALUES (
			$1,
			COALESCE(NULLIF($2, ''), $6)::uuid,
			COALESCE(NULLIF($3, ''), $7),
			COALESCE(NULLIF($4, ''), $8),
			NULLIF($5, '')
		)
		ON CONFLICT (id) DO NOTHING
	`,
		id, seed.TenantID, seed.TaskQueue, seed.Status, seed.AssignedTo,
		defaultLeaseTenantID, defaultLeaseTaskQueue, defaultLeaseStatus,
	); err != nil {
		t.Fatalf("seeding %s's workflow_leases row: %v", id, err)
	}
	// The workflow_payloads row copies every payload column from the
	// caller's workflow_instances INSERT, with the same tenant_id COALESCE as
	// the lease insert above so the two rows agree under RLS. It used to
	// copy only def_name/def_version and hardcode input = '{}', which was
	// harmless while every reader read workflow_instances; cleat#3245 B1a
	// moved the readers to workflow_payloads, and the claim then handed
	// tests/exhaustion's guest an empty input.
	syncWorkflowPayload(t, db, id, seed.TenantID, false)
}

// SyncWorkflowPayload copies every workflow_payloads column of one run from
// its workflow_instances row, inserting the payload row if it is missing.
//
// compacted_at and plugin_vers exist only on workflow_payloads and keep
// whatever that row already holds (its defaults, for a new row).
//
// Call it after a fixture writes a payload column -- completed_at, result,
// compaction_state, and so on -- straight into workflow_instances. Since
// cleat#3245 B1a every PostgreSQL reader of those columns reads
// workflow_payloads, so a value written only to workflow_instances is
// invisible to the code under test. Writing through the store keeps both
// copies in step and needs no call here.
//
// PostgreSQL only, like SeedWorkflowLease.
func SyncWorkflowPayload(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	syncWorkflowPayload(t, db, id, "", true)
}

func syncWorkflowPayload(t *testing.T, db *sql.DB, id, tenantID string, overwrite bool) {
	t.Helper()
	// SeedWorkflowLease's path takes the lease row's tenant rule (the seed's,
	// else the default), so the two rows it writes agree; a later sync takes
	// the instance's own tenant.
	tenantExpr := `COALESCE(NULLIF($2, ''), $3)::uuid`
	args := []any{id, tenantID, defaultLeaseTenantID}
	onConflict := `DO NOTHING`
	if overwrite {
		tenantExpr = `wi.tenant_id`
		args = []any{id}
		onConflict = `DO UPDATE SET
			input = EXCLUDED.input, result = EXCLUDED.result,
			query_state = EXCLUDED.query_state, compaction_state = EXCLUDED.compaction_state,
			compaction_step = EXCLUDED.compaction_step,
			allowed_signals = EXCLUDED.allowed_signals,
			error_msg = EXCLUDED.error_msg, error_code = EXCLUDED.error_code,
			error_op = EXCLUDED.error_op, def_name = EXCLUDED.def_name,
			def_version = EXCLUDED.def_version, trace_id = EXCLUDED.trace_id,
			completed_at = EXCLUDED.completed_at`
	}
	res, err := db.Exec(`
		INSERT INTO workflow_payloads (id, tenant_id, input, result, query_state,
			compaction_state, compaction_step,
			allowed_signals, error_msg, error_code, error_op, def_name,
			def_version, trace_id, completed_at)
		SELECT $1, `+tenantExpr+`,
			COALESCE(wi.input, '{}'), wi.result, wi.query_state,
			wi.compaction_state, wi.compaction_step,
			wi.allowed_signals, wi.error_msg,
			wi.error_code, wi.error_op, wi.def_name, wi.def_version,
			wi.trace_id, wi.completed_at
		FROM workflow_instances wi WHERE wi.id = $1
		ON CONFLICT (id) `+onConflict, args...)
	if err != nil {
		t.Fatalf("seeding %s's workflow_payloads row: %v", id, err)
	}
	if overwrite {
		if n, _ := res.RowsAffected(); n == 0 {
			t.Fatalf("SyncWorkflowPayload(%s): no workflow_instances row with that id", id)
		}
	}
}

// SyncWorkflowLease copies a workflow_instances row's lease columns onto its
// workflow_leases row, creating that row if it is missing.
//
// Call it after a fixture writes a lease column -- status above all, or
// assigned_to, generation, continued_from, a run_* limit -- straight into
// workflow_instances. Since cleat#3245 B3a the engine's PostgreSQL readers of
// those columns read workflow_leases, so a value written only to
// workflow_instances is invisible to the code under test. Writing through the
// store keeps both copies in step and needs no call here.
//
// heartbeat_at is deliberately not copied: since B2 nothing writes it to
// workflow_instances, so the instance copy is stale by construction.
//
// PostgreSQL only, like SeedWorkflowLease.
func SyncWorkflowLease(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	res, err := db.Exec(`
		INSERT INTO workflow_leases (id, tenant_id, task_queue, priority, status,
			assigned_to, next_wake_at, generation, pending_terminal_status,
			defer_phase_deadline, completed_by, continued_from, parent_workflow_id,
			run_wasm_instance_timeout_ms, run_wasm_wall_clock_ceiling_ms,
			run_host_retry_budget_ms, run_max_workflow_duration_ms)
		SELECT wi.id, wi.tenant_id, wi.task_queue, wi.priority, wi.status,
			wi.assigned_to, wi.next_wake_at, wi.generation, wi.pending_terminal_status,
			wi.defer_phase_deadline, wi.completed_by, wi.continued_from, wi.parent_workflow_id,
			wi.run_wasm_instance_timeout_ms, wi.run_wasm_wall_clock_ceiling_ms,
			wi.run_host_retry_budget_ms, wi.run_max_workflow_duration_ms
		FROM workflow_instances wi WHERE wi.id = $1
		ON CONFLICT (id) DO UPDATE SET
			tenant_id = EXCLUDED.tenant_id, task_queue = EXCLUDED.task_queue,
			priority = EXCLUDED.priority, status = EXCLUDED.status,
			assigned_to = EXCLUDED.assigned_to, next_wake_at = EXCLUDED.next_wake_at,
			generation = EXCLUDED.generation,
			pending_terminal_status = EXCLUDED.pending_terminal_status,
			defer_phase_deadline = EXCLUDED.defer_phase_deadline,
			completed_by = EXCLUDED.completed_by, continued_from = EXCLUDED.continued_from,
			parent_workflow_id = EXCLUDED.parent_workflow_id,
			run_wasm_instance_timeout_ms = EXCLUDED.run_wasm_instance_timeout_ms,
			run_wasm_wall_clock_ceiling_ms = EXCLUDED.run_wasm_wall_clock_ceiling_ms,
			run_host_retry_budget_ms = EXCLUDED.run_host_retry_budget_ms,
			run_max_workflow_duration_ms = EXCLUDED.run_max_workflow_duration_ms`, id)
	if err != nil {
		t.Fatalf("syncing %s's workflow_leases row: %v", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		t.Fatalf("SyncWorkflowLease(%s): no workflow_instances row with that id", id)
	}
}
