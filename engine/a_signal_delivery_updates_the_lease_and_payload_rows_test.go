package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3245 Phase 3 step 2, piece 5a: RequestCancellation, ConsumeSignal,
// DeliverSignal/DeliverSignalIdempotent (via deliverSignalTx) and
// SetAllowedSignalCallers now mirror their writes onto workflow_leases
// (cancellation_requested, cancellation_reason, signal_seq,
// signal_consumed_seq, next_wake_at) and workflow_payloads (allowed_signals),
// on the same tx as the workflow_instances write. allowed_signals was a scope
// addition found and disclosed on the issue before this was written -- it
// wasn't named in step 2's original sequencing plan.
//
// Seeds workflow_instances/workflow_leases/workflow_payloads directly via SQL
// -- the same independent-testability pattern pieces 1-4d established --
// rather than depending on any other piece's code.
func signalLeaseTestStore(t *testing.T) (*PostgresStore, *sql.DB) {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	applyPostgresProcedures(t, db)
	testutil.CleanupPostgresTestData(t, db)
	t.Cleanup(func() {
		testutil.CleanupPostgresTestData(t, db)
		db.Close()
	})
	return NewPostgresStore(db), db
}

// seedSignalTriple creates matching workflow_instances/workflow_leases/
// workflow_payloads rows with the given status -- standing in for what
// piece 1's dual-write (already merged) would produce by the time a real
// caller delivers a signal, requests cancellation, or sets allowed callers.
// next_wake_at is seeded an hour out on both tables, so a test can tell
// "moved to now" apart from "left alone".
func seedSignalTriple(t *testing.T, store *PostgresStore, db *sql.DB, id, defName, status string) {
	t.Helper()
	ctx := context.Background()
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, next_wake_at)
		VALUES ($1, $2, 1, $3, '{}', 'default', $4, now() + interval '1 hour')
	`, id, defName, status, DefaultTenantUUID); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_leases (id, tenant_id, task_queue, status, next_wake_at)
		VALUES ($1, $2, 'default', $3, now() + interval '1 hour')
	`, id, DefaultTenantUUID, status); err != nil {
		t.Fatalf("seed workflow_leases: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_payloads (id, tenant_id, input)
		VALUES ($1, $2, '{}')
	`, id, DefaultTenantUUID); err != nil {
		t.Fatalf("seed workflow_payloads: %v", err)
	}
}

type leaseSignalFields struct {
	signalSeq             int64
	signalConsumedSeq     int64
	cancellationRequested bool
	cancellationReason    sql.NullString
	nextWakeAt            time.Time
}

func readLeaseSignalFields(t *testing.T, db *sql.DB, id string) leaseSignalFields {
	t.Helper()
	var f leaseSignalFields
	if err := db.QueryRow(`
		SELECT signal_seq, signal_consumed_seq, cancellation_requested, cancellation_reason, next_wake_at
		FROM workflow_leases WHERE id = $1
	`, id).Scan(&f.signalSeq, &f.signalConsumedSeq, &f.cancellationRequested, &f.cancellationReason, &f.nextWakeAt); err != nil {
		t.Fatalf("read workflow_leases signal fields for %s: %v", id, err)
	}
	return f
}

func TestRequestCancellationUpdatesTheLeaseRow(t *testing.T) {
	store, db := signalLeaseTestStore(t)
	ctx := context.Background()

	const id = "signal-lease-cancel-1"
	seedSignalTriple(t, store, db, id, "cancel-lease-def", "running")

	if err := store.RequestCancellation(ctx, id, "operator requested stop"); err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}

	lease := readLeaseSignalFields(t, db, id)
	if !lease.cancellationRequested {
		t.Errorf("workflow_leases.cancellation_requested = false, want true")
	}

	var instReason sql.NullString
	if err := db.QueryRow(`SELECT cancellation_reason FROM workflow_instances WHERE id = $1`, id).Scan(&instReason); err != nil {
		t.Fatalf("read workflow_instances: %v", err)
	}
	// Byte-identical rather than decrypted-and-compared: whatever
	// RequestCancellation sealed (cleat#2312) is reused, not re-sealed, so
	// the two tables carry identical bytes regardless of whether encryption
	// is configured in this test environment.
	if lease.cancellationReason.Valid != instReason.Valid || lease.cancellationReason.String != instReason.String {
		t.Errorf("workflow_leases.cancellation_reason = %v, want byte-identical to workflow_instances' %v",
			lease.cancellationReason, instReason)
	}
}

func TestConsumeSignalUpdatesTheLeaseRow(t *testing.T) {
	store, db := signalLeaseTestStore(t)
	ctx := context.Background()

	const id = "signal-lease-consume-1"
	seedSignalTriple(t, store, db, id, "consume-lease-def", "running")

	var signalID int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO workflow_signals (workflow_id, signal_name, payload, tenant_id)
		VALUES ($1, 'sig1', '{}', $2) RETURNING id
	`, id, DefaultTenantUUID).Scan(&signalID); err != nil {
		t.Fatalf("seed workflow_signals: %v", err)
	}

	if err := store.ConsumeSignal(ctx, id, signalID); err != nil {
		t.Fatalf("ConsumeSignal: %v", err)
	}

	lease := readLeaseSignalFields(t, db, id)
	if lease.signalConsumedSeq != 1 {
		t.Errorf("workflow_leases.signal_consumed_seq = %d, want 1", lease.signalConsumedSeq)
	}

	var instConsumedSeq int64
	if err := db.QueryRow(`SELECT signal_consumed_seq FROM workflow_instances WHERE id = $1`, id).Scan(&instConsumedSeq); err != nil {
		t.Fatalf("read workflow_instances: %v", err)
	}
	if lease.signalConsumedSeq != instConsumedSeq {
		t.Errorf("workflow_leases.signal_consumed_seq (%d) disagrees with workflow_instances (%d)",
			lease.signalConsumedSeq, instConsumedSeq)
	}
}

func TestDeliverSignalUpdatesTheLeaseRowAndWakesASuspendedWorkflow(t *testing.T) {
	store, db := signalLeaseTestStore(t)
	ctx := context.Background()

	const id = "signal-lease-deliver-1"
	seedSignalTriple(t, store, db, id, "deliver-lease-def", "suspended")

	if err := store.DeliverSignal(ctx, id, "sig1", `"hello"`); err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	lease := readLeaseSignalFields(t, db, id)
	if lease.signalSeq != 1 {
		t.Errorf("workflow_leases.signal_seq = %d, want 1", lease.signalSeq)
	}
	if time.Since(lease.nextWakeAt) > 10*time.Second {
		t.Errorf("workflow_leases.next_wake_at = %v, want set to roughly now for a suspended workflow", lease.nextWakeAt)
	}

	var instSeq int64
	var instWake time.Time
	if err := db.QueryRow(`SELECT signal_seq, next_wake_at FROM workflow_instances WHERE id = $1`, id).Scan(&instSeq, &instWake); err != nil {
		t.Fatalf("read workflow_instances: %v", err)
	}
	if lease.signalSeq != instSeq {
		t.Errorf("workflow_leases.signal_seq (%d) disagrees with workflow_instances (%d)", lease.signalSeq, instSeq)
	}
	if lease.nextWakeAt.Sub(instWake).Abs() > time.Second {
		t.Errorf("workflow_leases.next_wake_at (%v) disagrees with workflow_instances' (%v)", lease.nextWakeAt, instWake)
	}
}

func TestDeliverSignalLeavesNextWakeAtAloneForARunningWorkflow(t *testing.T) {
	store, db := signalLeaseTestStore(t)
	ctx := context.Background()

	const id = "signal-lease-deliver-running-1"
	seedSignalTriple(t, store, db, id, "deliver-lease-running-def", "running")

	if err := store.DeliverSignal(ctx, id, "sig1", `"hello"`); err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	lease := readLeaseSignalFields(t, db, id)
	if lease.signalSeq != 1 {
		t.Errorf("workflow_leases.signal_seq = %d, want 1 (the bump is unconditional)", lease.signalSeq)
	}
	if time.Until(lease.nextWakeAt) < 30*time.Minute {
		t.Errorf("workflow_leases.next_wake_at moved for a running workflow: got %v, want left at its seeded ~1h-out value",
			lease.nextWakeAt)
	}
}

func TestSetAllowedSignalCallersUpdatesThePayloadRow(t *testing.T) {
	store, db := signalLeaseTestStore(t)
	ctx := context.Background()

	const id = "signal-payload-allowed-1"
	seedSignalTriple(t, store, db, id, "allowed-signals-def", "running")

	if err := store.SetAllowedSignalCallers(ctx, id, []string{"svc-a", "svc-b"}); err != nil {
		t.Fatalf("SetAllowedSignalCallers: %v", err)
	}

	var payloadAllowed, instAllowed sql.NullString
	if err := db.QueryRow(`SELECT allowed_signals::text FROM workflow_payloads WHERE id = $1`, id).Scan(&payloadAllowed); err != nil {
		t.Fatalf("read workflow_payloads: %v", err)
	}
	if err := db.QueryRow(`SELECT allowed_signals::text FROM workflow_instances WHERE id = $1`, id).Scan(&instAllowed); err != nil {
		t.Fatalf("read workflow_instances: %v", err)
	}
	if payloadAllowed.Valid != instAllowed.Valid || payloadAllowed.String != instAllowed.String {
		t.Errorf("workflow_payloads.allowed_signals = %v, want byte-identical to workflow_instances' %v",
			payloadAllowed, instAllowed)
	}
}
