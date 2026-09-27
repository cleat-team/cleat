package engine

// IMPROVEMENT-PLAN 3.86, second table: workflow_tags.
//
// 3.86 fixed the five schedule statements and said in as many words that the
// same audit was owed on every other SQL Server statement, and that the count
// was unknown because those five were found by reading one file. This is the
// next file, and it had the same defect in five more statements.
//
// The mechanism is unchanged and is documented above ClaimDueSchedule in
// engine/mssql_schedules.go: dbo.fn_tenant_filter admits any connection whose
// login is a member of dbo.cleat_admin regardless of SESSION_CONTEXT, a
// multi-tenant deployment must grant that role for cross-tenant dispatch to
// work at all, and WithTenant shares one pool. So on such a deployment the
// Go-level `AND tenant_id` is the whole of the isolation.
//
// WHY TAGS ARE WORTH THEIR OWN FILE RATHER THAN A LINE IN A SWEEP. A tag is
// not data about a workflow, it is the pointer that decides WHICH VERSION
// RUNS: ResolveVersionByTag turns "stable" into a version number at start
// time. Repointing another tenant's "stable" does not corrupt a row, it
// changes the code their next run executes. That is the same class of outcome
// as 3.12, reached through a different table.
//
// Names differ between the tenants throughout, so none of this depends on
// 3.77 step 4 -- tenant B names a workflow it does not own and the statement
// finds it anyway.

import (
	"context"
	"testing"
)

// seedTagForA gives tenant A a definition at two versions with "stable"
// pointing at v2, and returns the name.
//
// Two versions, not one, so the assertions can tell "B repointed A's tag" from
// "B did nothing": with a single version every answer is the same number.
func seedTagForA(t *testing.T, storeA *MSSQLStore, name string) {
	t.Helper()
	ctx := context.Background()
	for _, v := range []int{1, 2} {
		if err := storeA.DeployWorkflowDef(ctx, &WorkflowDef{
			Name: name, Version: v, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d, byte(v)},
			ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("tenant A deploy %s v%d: %v", name, v, err)
		}
	}
	if err := storeA.SetWorkflowTag(ctx, name, 2, "stable"); err != nil {
		t.Fatalf("tenant A SetWorkflowTag: %v", err)
	}
}

// TestAdminLoginGetWorkflowTagCannotCrossTenants — the read. Which version a
// competitor has promoted, and when, is deployment intelligence.
func TestAdminLoginGetWorkflowTagCannotCrossTenants(t *testing.T) {
	storeA, storeB := adminLoginStores(t)
	const name = "tenant-a-only-workflow"
	seedTagForA(t, storeA, name)

	v, err := storeB.GetWorkflowTag(context.Background(), name, "stable")
	if err == nil && v != 0 {
		t.Errorf("tenant B resolved tenant A's %q tag \"stable\" to v%d", name, v)
	}
}

// TestAdminLoginGetWorkflowTagsCannotCrossTenants — the listing. Worse than
// the single read: it enumerates every tag without needing to guess a name.
func TestAdminLoginGetWorkflowTagsCannotCrossTenants(t *testing.T) {
	storeA, storeB := adminLoginStores(t)
	const name = "tenant-a-only-listing"
	seedTagForA(t, storeA, name)

	tags, err := storeB.GetWorkflowTags(context.Background(), name)
	if err != nil {
		t.Fatalf("tenant B GetWorkflowTags: %v", err)
	}
	if len(tags) != 0 {
		t.Errorf("tenant B enumerated tenant A's tags for %q: %v", name, tags)
	}
}

// TestAdminLoginResolveVersionByTagCannotCrossTenants is the one that decides
// which code runs, so it is the read with a side effect elsewhere.
func TestAdminLoginResolveVersionByTagCannotCrossTenants(t *testing.T) {
	storeA, storeB := adminLoginStores(t)
	const name = "tenant-a-only-resolve"
	seedTagForA(t, storeA, name)

	v, err := storeB.ResolveVersionByTag(context.Background(), name, "stable")
	if err == nil && v != 0 {
		t.Errorf("tenant B resolved %q \"stable\" to v%d from tenant A's tag", name, v)
	}
}

// TestAdminLoginRemoveWorkflowTagCannotCrossTenants — destructive. A workflow
// started by tag after this fails to resolve; a deploy pipeline that promotes
// by tag has lost its pointer.
func TestAdminLoginRemoveWorkflowTagCannotCrossTenants(t *testing.T) {
	storeA, storeB := adminLoginStores(t)
	ctx := context.Background()
	const name = "tenant-a-only-remove"
	seedTagForA(t, storeA, name)

	if err := storeB.RemoveWorkflowTag(ctx, name, "stable"); err != nil {
		t.Fatalf("tenant B RemoveWorkflowTag: %v", err)
	}

	v, err := storeA.GetWorkflowTag(ctx, name, "stable")
	if err != nil || v != 2 {
		t.Errorf("after tenant B removed it, tenant A's %q tag \"stable\" reads v%d (err %v), want v2",
			name, v, err)
	}
}

// TestAdminLoginSetWorkflowTagCannotCrossTenants is the quietest and the
// worst: the MERGE matches another tenant's row and updates it in place, so
// nothing is created, nothing errors, and tenant A's "stable" now points at a
// version tenant A never promoted.
//
// Note this one cannot be seen from a tenant-scoped connection. There the
// security policy hides A's row, the MERGE falls to WHEN NOT MATCHED, and the
// INSERT trips the primary key -- loud, and the opposite conclusion. Testing
// this on the wrong connection would report the defect as absent.
func TestAdminLoginSetWorkflowTagCannotCrossTenants(t *testing.T) {
	storeA, storeB := adminLoginStores(t)
	ctx := context.Background()
	const name = "tenant-a-only-set"
	seedTagForA(t, storeA, name)

	// B deploys its own definition of a DIFFERENT name, then tries to point
	// "stable" at v1 of A's workflow name. A tagged v2, so v1 is the tell.
	_ = storeB.SetWorkflowTag(ctx, name, 1, "stable")

	v, err := storeA.GetWorkflowTag(ctx, name, "stable")
	if err != nil {
		t.Fatalf("tenant A GetWorkflowTag after B's write: %v", err)
	}
	if v != 2 {
		t.Errorf("tenant A's %q tag \"stable\" now points at v%d; tenant A promoted v2, "+
			"so tenant B has repointed the tag that decides which code A runs", name, v)
	}
}
