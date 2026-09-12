package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// cleat#1090: workflow_instances recorded created_at and completed_at and
// nothing in between, so completed_at - created_at measured WAIT PLUS
// EXECUTION. A run that queued for a minute and a run that executed for a
// minute produced the same number.
//
// started_at separates them. This test pins the two properties that make it
// useful, and the second is the design decision rather than the mechanism.
//
// Every assertion compares two values stamped by the DATABASE clock. None
// compares against time.Now(), which is the WORKER clock here -- a wall-clock
// tolerance would measure skew between processes rather than the column. See
// children.go's note on exactly that pair.
func TestARunRecordsWhenAWorkerFirstBeganExecutingIt(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			id, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), "started-at", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			// A run nobody has claimed has not started, and the column has to
			// say so rather than standing in with created_at.
			fresh, err := store.GetWorkflowByID(ctx, id)
			if err != nil || fresh == nil {
				t.Fatalf("GetWorkflowByID on a new run: %v (nil=%v)", err, fresh == nil)
			}
			if fresh.StartedAt != nil {
				t.Errorf("a run that has never been claimed reports StartedAt = %v, want nil.\n\n"+
					"Queue latency is started_at - created_at. If an unclaimed run "+
					"reports a start, that difference is zero for everything waiting, "+
					"which is the one answer a backlog metric must never give.",
					*fresh.StartedAt)
			}

			claimed, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil || claimed == nil {
				t.Fatalf("ClaimWorkflow: %v %v", claimed, err)
			}
			afterClaim, err := store.GetWorkflowByID(ctx, id)
			if err != nil || afterClaim == nil {
				t.Fatalf("GetWorkflowByID after claim: %v (nil=%v)", err, afterClaim == nil)
			}
			if afterClaim.StartedAt == nil {
				t.Fatalf("a claimed run records no start time.\n\n" +
					"The claim is a single UPDATE that already stamps heartbeat_at " +
					"from the database clock; started_at is stamped in the same SET " +
					"list. A nil here means this dialect's claim was not one of the " +
					"nine sites -- and the ninth is SQL, inside admin.claim_workflows, " +
					"where a Go-only change cannot reach it.")
			}
			if afterClaim.StartedAt.Before(afterClaim.CreatedAt) {
				t.Errorf("started_at %v is before created_at %v.\n\n"+
					"Both are stamped by the database, so this ordering cannot be "+
					"broken by clock skew between processes.",
					*afterClaim.StartedAt, afterClaim.CreatedAt)
			}
			first := *afterClaim.StartedAt

			// --- the design decision: FIRST claim wins -----------------------
			//
			// Reap with a negative timeout -- "every running row is stale now",
			// the established way to simulate a worker that stopped
			// heartbeating (see reclaim_count_records_reclaims_only_test.go).
			// The row returns to 'ready' and is claimed again, so the claim's
			// UPDATE runs a second time against a row that already has a
			// started_at.
			if _, err := store.ReapStaleInstances(ctx, -1*time.Second, 0); err != nil {
				t.Fatalf("ReapStaleInstances: %v", err)
			}
			claimByID(t, ctx, store, "worker-2", id)

			afterReclaim, err := store.GetWorkflowByID(ctx, id)
			if err != nil || afterReclaim == nil {
				t.Fatalf("GetWorkflowByID after reclaim: %v (nil=%v)", err, afterReclaim == nil)
			}
			if afterReclaim.StartedAt == nil {
				t.Fatalf("a reclaimed run lost its start time entirely, want it unchanged at %v", first)
			}
			if !afterReclaim.StartedAt.Equal(first) {
				t.Errorf("a reclaim moved started_at from %v to %v, want it unchanged.\n\n"+
					"The claim stamps COALESCE(started_at, <now>), so the FIRST claim "+
					"wins. Queue latency and backlog drain rate both need the first "+
					"start; only elapsed-execution-of-the-winning-attempt wants the "+
					"latest, and for a reclaimed run the longer figure is the honest "+
					"one -- reclaim_count (%d here) says why. A column that gets "+
					"rewritten is how heartbeat_at came to lose this value in the "+
					"first place, which is the whole of cleat#1090.",
					first, *afterReclaim.StartedAt, afterReclaim.ReclaimCount)
			}

			if err := store.FinalizeWorkflowSegment(ctx, id, "worker-2", afterReclaim.Generation,
				nil, "done", `{"ok":true}`, "", "", nil, time.Time{}); err != nil {
				t.Fatalf("finalize: %v", err)
			}
			done, err := store.GetWorkflowByID(ctx, id)
			if err != nil || done == nil {
				t.Fatalf("GetWorkflowByID after finalize: %v (nil=%v)", err, done == nil)
			}
			if done.CompletedAt == nil {
				t.Fatalf("a finished run records no completion time (cleat#1091)")
			}
			// The pair the issue asks for, now expressible: this ordering is
			// what makes completed_at - started_at an elapsed execution rather
			// than a number that can come out negative.
			if done.CompletedAt.Before(*done.StartedAt) {
				t.Errorf("completed_at %v is before started_at %v.\n\n"+
					"Both database-stamped. A negative elapsed execution would make "+
					"the metric worse than not having it.",
					*done.CompletedAt, *done.StartedAt)
			}
		})
	}
}

// The NINTH claim site, and the one a Go-only change cannot reach.
//
// ClaimWorkflow and ClaimWorkflows issue their UPDATE from Go; the cross-tenant
// claim calls admin.claim_workflows, a SQL function defined in a migration. A
// scan for `SET status = 'running'` over *.go finds eight sites and reports a
// symmetric-looking two/three/three -- and misses this one entirely, which
// would have left multi-tenant dispatch as the single claim path that records
// no start time.
//
// The assertion reads the COLUMN through the admin connection rather than the
// returned struct: admin.claim_workflows' RETURNING list does not carry
// started_at, and widening it would change the function's return type -- which
// is what forced migration 040 to DROP and CREATE rather than replace. Nothing
// needs the value on that path, so the column is the right thing to check.
//
// PostgreSQL-only, for the reason the header of cross_tenant_claim_test.go
// gives: admin.claim_workflows exists on no other dialect, so looping over
// registeredBackends would buy two skips for dialects that were never going to
// run this.
func TestTheCrossTenantClaimAlsoRecordsWhenTheRunStarted(t *testing.T) {
	adminDB, appDB, teardown := crossTenantClaimDB(t)
	defer teardown()
	deployCrossTenantDef(t, adminDB)
	ctx := context.Background()

	id := seedWorkflow(t, adminDB, seedWorkflowOpts{tenantID: xtcTenantA})

	var before *time.Time
	if err := adminDB.QueryRow(`SELECT started_at FROM workflow_instances WHERE id = $1`, id).
		Scan(&before); err != nil {
		t.Fatalf("reading started_at before the claim: %v", err)
	}
	if before != nil {
		t.Fatalf("a seeded, unclaimed run already has started_at = %v, so this test "+
			"cannot tell whether the claim set it", *before)
	}

	claimed, err := NewPostgresStore(appDB).ClaimWorkflowsAcrossTenants(ctx, "worker-cross", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflowsAcrossTenants: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != id {
		t.Fatalf("claimed %v, want exactly [%s]", claimedIDs(claimed), id)
	}

	var after *time.Time
	if err := adminDB.QueryRow(`SELECT started_at FROM workflow_instances WHERE id = $1`, id).
		Scan(&after); err != nil {
		t.Fatalf("reading started_at after the claim: %v", err)
	}
	if after == nil {
		t.Fatalf("the cross-tenant claim left started_at NULL.\n\n" +
			"admin.claim_workflows is a claim site like any other and has to stamp " +
			"it. A deployment using cross-tenant dispatch would otherwise be the " +
			"one configuration where cleat#1090 is still open -- and every Go-level " +
			"test would pass, because none of them goes through this function.")
	}
}
