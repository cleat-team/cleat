package engine

// IMPROVEMENT-PLAN 3.86, the control-plane statements -- the group where the id
// CROSSES A TRUST BOUNDARY.
//
// The three files beside this one (schedules, tags, definitions) cover
// statements keyed on a name the tenant chose. These are keyed on a generated
// workflow id, and 3.77 argued that such statements need no tenant predicate
// because a UUID cannot be guessed. That argument is why these five were left
// out of the earlier passes, and it does not hold for them:
//
//   - Every one is reachable from an HTTP handler that reads the id straight
//     out of the URL path -- cmd/cleat-worker/app.go's dead-letter terminate
//     and workflow retry, server.go's query, signal and cancel routes.
//   - Unguessability is a claim about what an ATTACKER KNOWS, not about the
//     code. A workflow id travels: into logs, a support ticket, a URL, a user
//     who has since left the tenant. None of those are attacks.
//   - The statements the argument DOES cover are the plumbing ones, whose ids
//     the engine read back from a row it had already scoped. Those run on a
//     store cmd/cleat-worker/setup.go:storeFor already scoped to the
//     instance's own tenant, so they are safe by construction rather than by
//     unguessability. Two different arguments had been doing one job.
//
// Every case runs on a dbo.cleat_admin login, which is what a multi-tenant SQL
// Server deployment must use (adminLoginStores explains why, and Fatals rather
// than skips if the pool is not one -- on a filtered connection every assertion
// here passes without measuring anything).
//
// EACH CASE CARRIES ITS OWN POSITIVE CONTROL, and that is not decoration. The
// failure mode of "add AND tenant_id" is not a leak, it is a statement that now
// matches nothing at all: TerminateWorkflow does not check rows affected, so a
// predicate naming the wrong column would make every terminate a silent no-op
// and the cross-tenant half of these tests would still pass. Asserting only
// "the other tenant's row is untouched" cannot tell a fix from a breakage.
//
// Measured 2026-09-03 by removing each predicate on its own and reading the
// failure, tenant B acting on tenant A's workflow id. Each removal turned
// exactly one case red -- the cases are independent, not one shared fixture
// failing together:
//
//     TerminateWorkflow(idA)    -> A's status is "terminated"
//     RetryWorkflow(idA)        -> A's dead-lettered workflow: status is "ready"
//     RequestCancellation(idA)  -> A's cancellation_requested is "1"
//     GetQueryState(idA, k)     -> B read "tenant-a-only"
//     DeliverSignal(idA, held)  -> "tenant B overwrote tenant A's pending signal
//                                  payload: A reads {"from":"tenant-b"}"
//     DeliverSignal(idA, unheld)-> A's next_wake_at moved to now: A was SCHEDULED
//                                  TO RUN on a signal it cannot read
//
// The last two are one method and two fixtures, and they have to be, because
// each statement hides the other: with the MERGE scoped, a delivery to a signal
// A already holds is refused by the primary key and rolls back before the wake
// runs, so the wake's predicate could be deleted with every assertion still
// green. That is what the first falsification pass reported, and it is why
// DeliverSignalWake exists as a separate case.
//
// The DeliverSignal one is the one to read twice. Its MERGE already named
// tenant_id -- in the INSERT column list, which scopes the row the call
// CREATES and says nothing about the row it MATCHES -- so
// the substring audit script counted it as already predicated and it is not in
// the 34 that script reported. (That script has since been replaced by
// engine/mssql_tenant_predicate_test.go, which asks where the column appears.) A substring check cannot see the
// difference between a filter and a projection; the gate 3.86 describes needs
// to ask WHERE the column appears.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const (
	cpDefName = "test-workflow"
	// Deliberately not UUIDs. workflow_instances.id is a string column, and
	// naming the ids after their owner makes a cross-tenant hit legible in the
	// failure message rather than a pair of hex blobs to diff.
	cpWorkflowA = "cp-tenant-a-workflow"
	cpWorkflowB = "cp-tenant-b-workflow"
)

// seedTwoTenantWorkflows gives each tenant its own definition and one workflow.
//
// Both definitions carry the same name: since D7 (3.77) workflow_instances' FK
// carries tenant_id, so a run under tenant B needs B's own row of the
// definition, and giving them different names would make the fixture answer an
// easier question than the one asked.
func seedTwoTenantWorkflows(t *testing.T, storeA, storeB *MSSQLStore) {
	t.Helper()
	ctx := context.Background()
	for _, tc := range []struct {
		who    string
		store  *MSSQLStore
		tenant string
		runID  string
	}{
		{"A", storeA, unscopedTenantA, cpWorkflowA},
		{"B", storeB, unscopedTenantB, cpWorkflowB},
	} {
		if err := tc.store.DeployWorkflowDef(ctx, &WorkflowDef{
			Name: cpDefName, Version: 1,
			WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
			ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("tenant %s deploy: %v", tc.who, err)
		}
		if _, _, err := tc.store.StartNewRun(ctx, tc.runID, cpDefName, 1, json.RawMessage(`{}`), "", tc.tenant, 0); err != nil {
			t.Fatalf("tenant %s StartNewRun: %v", tc.who, err)
		}
	}
}

// instanceField reads one column of one workflow directly, naming the tenant in
// the SQL.
//
// Deliberately not routed through the store: the assertions below are about
// whether the store's own statements are scoped, so verifying them with another
// of the store's statements would let one unscoped statement vouch for another.
// This is the "watch which layer is holding the test up" rule -- the read has
// to be independent of the thing being read about.
func instanceField(t *testing.T, s *MSSQLStore, column, id, tenant string) string {
	t.Helper()
	var v *string
	// #nosec G202 -- column is a literal from the call sites below, never input.
	q := "SELECT CAST(" + column + " AS NVARCHAR(MAX)) FROM workflow_instances WHERE id = @p1 AND tenant_id = @p2"
	if err := s.db.QueryRow(q, id, tenant).Scan(&v); err != nil {
		t.Fatalf("read %s of %s: %v", column, id, err)
	}
	if v == nil {
		return ""
	}
	return *v
}

func setInstanceStatus(t *testing.T, s *MSSQLStore, id, tenant, status string) {
	t.Helper()
	res, err := s.db.Exec(
		`UPDATE workflow_instances SET status = @p3 WHERE id = @p1 AND tenant_id = @p2`,
		id, tenant, status)
	if err != nil {
		t.Fatalf("set status of %s to %s: %v", id, status, err)
	}
	// A fixture that quietly updated nothing would leave every assertion below
	// measuring a workflow still in 'ready', which several of them would pass.
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("set status of %s to %s affected %d rows, want 1; the fixture is broken", id, status, n)
	}
}

func setInstanceNextWake(t *testing.T, s *MSSQLStore, id, tenant, when string) {
	t.Helper()
	res, err := s.db.Exec(
		`UPDATE workflow_instances SET next_wake_at = @p3 WHERE id = @p1 AND tenant_id = @p2`,
		id, tenant, when)
	if err != nil {
		t.Fatalf("set next_wake_at of %s: %v", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("set next_wake_at of %s affected %d rows, want 1; the fixture is broken", id, n)
	}
}

// TestAdminLoginControlPlaneWritesTouchOnlyTheCallersOwnWorkflow is the group's
// core: four writes, each reachable with an id from outside.
func TestAdminLoginControlPlaneWritesTouchOnlyTheCallersOwnWorkflow(t *testing.T) {
	storeA, storeB := adminLoginStores(t)
	seedTwoTenantWorkflows(t, storeA, storeB)
	ctx := context.Background()

	t.Run("TerminateWorkflow", func(t *testing.T) {
		if err := storeB.TerminateWorkflow(ctx, cpWorkflowA, "not yours"); err != nil {
			t.Fatalf("TerminateWorkflow across tenants: %v", err)
		}
		if got := instanceField(t, storeA, "status", cpWorkflowA, unscopedTenantA); got == "terminated" {
			t.Errorf("tenant B terminated tenant A's workflow: A's status is %q", got)
		}
		// Positive control: the same call on B's own workflow must still work,
		// or the predicate has broken terminate rather than scoped it.
		if err := storeB.TerminateWorkflow(ctx, cpWorkflowB, "mine"); err != nil {
			t.Fatalf("TerminateWorkflow on own workflow: %v", err)
		}
		if got := instanceField(t, storeB, "status", cpWorkflowB, unscopedTenantB); got != "terminated" {
			t.Errorf("tenant B could not terminate its OWN workflow: status is %q, want \"terminated\"", got)
		}
	})

	t.Run("RetryWorkflow", func(t *testing.T) {
		setInstanceStatus(t, storeA, cpWorkflowA, unscopedTenantA, "dead_lettered")
		setInstanceStatus(t, storeB, cpWorkflowB, unscopedTenantB, "dead_lettered")

		if err := storeB.RetryWorkflow(ctx, cpWorkflowA); err != nil {
			t.Fatalf("RetryWorkflow across tenants: %v", err)
		}
		if got := instanceField(t, storeA, "status", cpWorkflowA, unscopedTenantA); got != "dead_lettered" {
			t.Errorf("tenant B re-queued tenant A's dead-lettered workflow: A's status is %q", got)
		}
		if err := storeB.RetryWorkflow(ctx, cpWorkflowB); err != nil {
			t.Fatalf("RetryWorkflow on own workflow: %v", err)
		}
		if got := instanceField(t, storeB, "status", cpWorkflowB, unscopedTenantB); got != "ready" {
			t.Errorf("tenant B could not retry its OWN workflow: status is %q, want \"ready\"", got)
		}
	})

	// The wake statement, on its own.
	//
	// It needs its own case because the one above CANNOT reach it. Once the
	// MERGE's ON clause is scoped, a cross-tenant delivery to a signal the
	// victim ALREADY HOLDS is refused by the primary key and the transaction
	// rolls back before the wake runs -- so removing the wake's predicate left
	// every assertion above green. Measured, not reasoned: that is exactly what
	// the falsification pass reported, and this case exists because of it.
	//
	// The reachable path is a signal name the victim does NOT hold. Then the
	// MERGE matches nothing, the INSERT succeeds under the CALLER's tenant
	// (a harmless orphan row the victim's own polls will never see), and the
	// wake is the only statement left touching the victim -- which without a
	// tenant predicate scheduled another tenant's suspended workflow to run.
	t.Run("DeliverSignalWake", func(t *testing.T) {
		const unheld = "not-a-signal-tenant-a-holds"
		const marker = "2020-01-01T00:00:00"
		setInstanceStatus(t, storeA, cpWorkflowA, unscopedTenantA, "suspended")
		setInstanceNextWake(t, storeA, cpWorkflowA, unscopedTenantA, marker)

		if err := storeB.DeliverSignal(ctx, cpWorkflowA, unheld, `{"from":"tenant-b"}`); err != nil {
			t.Fatalf("DeliverSignal of an unheld name across tenants: %v", err)
		}
		got := instanceField(t, storeA, "next_wake_at", cpWorkflowA, unscopedTenantA)
		if !strings.HasPrefix(got, marker[:10]) {
			t.Errorf("tenant B woke tenant A's suspended workflow: next_wake_at is now %q, "+
				"want it still at %q -- A is now scheduled to run on a signal it cannot even read",
				got, marker)
		}

		// Positive control: the same delivery to the caller's OWN suspended
		// workflow must still wake it, or the predicate has disabled waking
		// rather than scoped it.
		setInstanceStatus(t, storeB, cpWorkflowB, unscopedTenantB, "suspended")
		setInstanceNextWake(t, storeB, cpWorkflowB, unscopedTenantB, marker)
		if err := storeB.DeliverSignal(ctx, cpWorkflowB, unheld, `{"from":"tenant-b"}`); err != nil {
			t.Fatalf("DeliverSignal on own workflow: %v", err)
		}
		if own := instanceField(t, storeB, "next_wake_at", cpWorkflowB, unscopedTenantB); strings.HasPrefix(own, marker[:10]) {
			t.Errorf("tenant B's own workflow was NOT woken by its own signal: next_wake_at is still %q", own)
		}
	})

	t.Run("RequestCancellation", func(t *testing.T) {
		if err := storeB.RequestCancellation(ctx, cpWorkflowA, "not yours"); err != nil {
			t.Fatalf("RequestCancellation across tenants: %v", err)
		}
		if got := instanceField(t, storeA, "cancellation_requested", cpWorkflowA, unscopedTenantA); got != "0" {
			t.Errorf("tenant B flagged tenant A's workflow cancelled: A's cancellation_requested is %q", got)
		}
		if err := storeB.RequestCancellation(ctx, cpWorkflowB, "mine"); err != nil {
			t.Fatalf("RequestCancellation on own workflow: %v", err)
		}
		if got := instanceField(t, storeB, "cancellation_requested", cpWorkflowB, unscopedTenantB); got != "1" {
			t.Errorf("tenant B could not cancel its OWN workflow: cancellation_requested is %q, want \"1\"", got)
		}
	})

	// DeliverSignal is two statements and two distinct harms: the MERGE
	// overwrote the victim's pending payload, and the wake below it then
	// scheduled the victim's workflow to consume what was written.
	//
	// This case is the one whose SHAPE OF SUCCESS CHANGED, and it is worth
	// stating exactly. pk_workflow_signals is (workflow_id, signal_name) with
	// no tenant column, which is correct and should stay that way: workflow_id
	// is generated and globally unique, so "one pending signal per workflow per
	// name" is a global truth and adding tenant_id to that key would ALLOW two
	// tenants to hold a signal for the same workflow. So once the MERGE's ON
	// clause is tenant-scoped, a cross-tenant delivery no longer MATCHES the
	// victim's row -- it falls through to the INSERT branch and the primary key
	// refuses it. The call now returns an error where it used to return nil
	// having silently overwritten another tenant's payload.
	//
	// That is the same shape as SetWorkflowTag in the tags file: the refusal
	// arrives as a constraint violation rather than a considered "not yours".
	// It is safe but it is a 500, and it is distinguishable from delivering to
	// an id that exists nowhere (which still succeeds, creating a harmless
	// orphan row under the caller's own tenant) -- a weak cross-tenant
	// existence oracle. Noted in 3.86 rather than fixed here; turning it into a
	// clean not-found is a behaviour change to an HTTP contract.
	t.Run("DeliverSignal", func(t *testing.T) {
		const sig = "approve"
		if err := storeA.DeliverSignal(ctx, cpWorkflowA, sig, `{"from":"tenant-a"}`); err != nil {
			t.Fatalf("tenant A deliver own signal: %v", err)
		}
		setInstanceStatus(t, storeA, cpWorkflowA, unscopedTenantA, "suspended")
		before := instanceField(t, storeA, "next_wake_at", cpWorkflowA, unscopedTenantA)

		// nil here is the defect: it is what this call returned while it was
		// overwriting tenant A's payload.
		if err := storeB.DeliverSignal(ctx, cpWorkflowA, sig, `{"from":"tenant-b"}`); err == nil {
			t.Errorf("tenant B's delivery to tenant A's workflow was ACCEPTED; " +
				"it should not have matched A's row at all")
		}

		payload, ok, err := storeA.PollSignal(ctx, cpWorkflowA, sig)
		if err != nil {
			t.Fatalf("tenant A PollSignal: %v", err)
		}
		if !ok {
			t.Fatalf("tenant A's own signal is gone; the fixture is broken")
		}
		if strings.Contains(payload, "tenant-b") {
			t.Errorf("tenant B overwrote tenant A's pending signal payload: A reads %q", payload)
		}
		if after := instanceField(t, storeA, "next_wake_at", cpWorkflowA, unscopedTenantA); after != before {
			t.Errorf("tenant B woke tenant A's suspended workflow: next_wake_at %q -> %q", before, after)
		}

		setInstanceStatus(t, storeB, cpWorkflowB, unscopedTenantB, "suspended")
		if err := storeB.DeliverSignal(ctx, cpWorkflowB, sig, `{"from":"tenant-b"}`); err != nil {
			t.Fatalf("DeliverSignal on own workflow: %v", err)
		}
		own, ok, err := storeB.PollSignal(ctx, cpWorkflowB, sig)
		if err != nil || !ok {
			t.Fatalf("tenant B could not read the signal it sent ITSELF: ok=%v err=%v", ok, err)
		}
		if !strings.Contains(own, "tenant-b") {
			t.Errorf("tenant B's own signal payload is %q, want it to carry \"tenant-b\"", own)
		}
	})
}

// TestAdminLoginQueryStateAnswersAboutTheCallersOwnWorkflow covers the read.
//
// Separate from the writes above because the harm is a different one: nothing
// is damaged, the caller is simply handed whatever the other tenant's workflow
// published about itself.
func TestAdminLoginQueryStateAnswersAboutTheCallersOwnWorkflow(t *testing.T) {
	storeA, storeB := adminLoginStores(t)
	seedTwoTenantWorkflows(t, storeA, storeB)
	ctx := context.Background()

	if _, err := storeA.db.Exec(
		`UPDATE workflow_instances SET query_state = @p3 WHERE id = @p1 AND tenant_id = @p2`,
		cpWorkflowA, unscopedTenantA, `{"stage":"tenant-a-only"}`); err != nil {
		t.Fatalf("seed tenant A query state: %v", err)
	}

	got, err := storeB.GetQueryState(ctx, cpWorkflowA, "stage")
	if err != nil {
		t.Fatalf("GetQueryState across tenants: %v", err)
	}
	if got != "" {
		t.Errorf("tenant B read tenant A's query state: got %q", got)
	}

	// Positive control. Without it a GetQueryState that had stopped returning
	// anything at all would pass the assertion above.
	own, err := storeA.GetQueryState(ctx, cpWorkflowA, "stage")
	if err != nil {
		t.Fatalf("GetQueryState on own workflow: %v", err)
	}
	if own != "tenant-a-only" {
		t.Errorf("tenant A could not read its OWN query state: got %q, want \"tenant-a-only\"", own)
	}
}
