package engine

// IMPROVEMENT-PLAN 3.77 step 4, D7: deployment tags are per-tenant. Last of the
// three tables that key on a user-chosen name.
//
// THIS CLOSES A GAP STEP 2 OPENED. Before migration 035, two tenants could not
// both hold a definition called "order-processor", so the question of both
// tagging it "stable" never came up. After 035 they can hold the definition --
// and still could not tag it, because workflow_tags kept a global
// (workflow_name, tag) key. So the tree was briefly in a state where D7 was
// half true: names were per-tenant, the pointers into them were not.
//
// The three dialects answered that state three different ways, which is the
// part worth keeping:
//
//   postgres  ERROR: new row violates row-level security policy (USING
//             expression) for table "workflow_tags" (42501) -- the ON CONFLICT
//             DO UPDATE tried to update the other tenant's row and the policy
//             refused the result. Reads like a misconfiguration; was a key.
//   mssql     Violation of PRIMARY KEY constraint 'pk_workflow_tags'. The
//             duplicate key value is (order-processor, stable). Loud, honest.
//   mysql     NO ERROR. B's write silently became an UPDATE of A's row, so A's
//             "stable" moved to B's version -- and B's own next read returned
//             nothing, because the row carries A's tenant_id and B's SELECT is
//             scoped. The tenant causing the damage is the one least able to
//             see it. Bounded by D1 (MySQL is single-tenant-only), but the
//             shape is 3.12's defect on a different table.
//
// A tag decides WHICH CODE RUNS -- ResolveVersionByTag turns "stable" into a
// version number at start time -- so every fixture here is lopsided on version
// rather than symmetric. Both tenants tagging the same version would pass
// against a key that ignored the tenant entirely.
//
// Tenant isolation of the tag STATEMENTS is 3.86, not this. On SQL Server that
// is the whole of the separation, because dbo.fn_tenant_filter is off for the
// cleat_admin login a multi-tenant deployment must use. Those predicates
// landed first, deliberately.

import (
	"context"
	"testing"
)

const (
	sharedDefName = "order-processor"
	sharedTag     = "stable"
)

// seedLopsidedTagFixture gives tenant A the name at v1 and v2 tagged to v2, and
// tenant B the same name at v1 only. The version each tenant resolves is then
// the evidence of whose row it read.
func seedLopsidedTagFixture(t *testing.T, storeA, storeB WorkflowStore) {
	t.Helper()
	ctx := context.Background()
	for _, v := range []int{1, 2} {
		if err := storeA.DeployWorkflowDef(ctx, &WorkflowDef{
			Name: sharedDefName, Version: v, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d, byte(v)},
			ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("tenant A deploy v%d: %v", v, err)
		}
	}
	if err := storeB.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: sharedDefName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d, 0xB1},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("tenant B deploy v1: %v", err)
	}
}

// TestTwoTenantsEachHoldTheirOwnTagOfOneName is the feature.
func TestTwoTenantsEachHoldTheirOwnTagOfOneName(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, storeB, _ := twoTenantStores(t, backend)
			ctx := context.Background()
			seedLopsidedTagFixture(t, storeA, storeB)

			if err := storeA.SetWorkflowTag(ctx, sharedDefName, 2, sharedTag); err != nil {
				t.Fatalf("tenant A SetWorkflowTag: %v", err)
			}
			if err := storeB.SetWorkflowTag(ctx, sharedDefName, 1, sharedTag); err != nil {
				t.Fatalf("tenant B SetWorkflowTag: %v -- both tenants must be able to "+
					"tag their own definition of a name they both hold", err)
			}

			gotA, err := storeA.GetWorkflowTag(ctx, sharedDefName, sharedTag)
			if err != nil {
				t.Fatalf("tenant A GetWorkflowTag: %v", err)
			}
			if gotA != 2 {
				t.Errorf("tenant A's %q resolves to v%d, want its own v2 -- tenant B tagged "+
					"v1, so %d is B's number", sharedTag, gotA, gotA)
			}

			gotB, err := storeB.GetWorkflowTag(ctx, sharedDefName, sharedTag)
			if err != nil {
				t.Fatalf("tenant B GetWorkflowTag: %v", err)
			}
			if gotB != 1 {
				t.Errorf("tenant B's %q resolves to v%d, want its own v1", sharedTag, gotB)
			}
		})
	}
}

// TestResolveVersionByTagStaysWithinTheCallersTenant covers the path that
// actually starts a workflow, rather than the accessor.
//
// GetWorkflowTag and ResolveVersionByTag run different SQL on all three
// dialects and only the second is on the run-start path, so a test of the
// first says nothing about which code a tenant's next run executes.
func TestResolveVersionByTagStaysWithinTheCallersTenant(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, storeB, _ := twoTenantStores(t, backend)
			ctx := context.Background()
			seedLopsidedTagFixture(t, storeA, storeB)

			if err := storeA.SetWorkflowTag(ctx, sharedDefName, 2, sharedTag); err != nil {
				t.Fatalf("tenant A SetWorkflowTag: %v", err)
			}
			if err := storeB.SetWorkflowTag(ctx, sharedDefName, 1, sharedTag); err != nil {
				t.Fatalf("tenant B SetWorkflowTag: %v", err)
			}

			for _, tc := range []struct {
				who   string
				store WorkflowStore
				want  int
			}{{"A", storeA, 2}, {"B", storeB, 1}} {
				got, err := tc.store.ResolveVersionByTag(ctx, sharedDefName, sharedTag)
				if err != nil {
					t.Fatalf("ResolveVersionByTag(%s): %v", tc.who, err)
				}
				if got != tc.want {
					t.Errorf("tenant %s starting %q by tag %q would run v%d, want v%d",
						tc.who, sharedDefName, sharedTag, got, tc.want)
				}
			}
		})
	}
}

// TestRetaggingOneTenantsNamesakeLeavesTheOther is the silent case, pinned.
//
// This is the assertion MySQL failed before the migration: B's SetWorkflowTag
// returned no error and moved A's pointer. Nothing about B's own subsequent
// reads would have told anyone -- B saw nothing, A saw the wrong version, and
// neither saw an error.
func TestRetaggingOneTenantsNamesakeLeavesTheOther(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, storeB, _ := twoTenantStores(t, backend)
			ctx := context.Background()
			seedLopsidedTagFixture(t, storeA, storeB)

			if err := storeA.SetWorkflowTag(ctx, sharedDefName, 2, sharedTag); err != nil {
				t.Fatalf("tenant A SetWorkflowTag: %v", err)
			}
			// B repoints its own tag twice, which under a global key would
			// walk A's row rather than B's.
			for _, v := range []int{1, 1} {
				if err := storeB.SetWorkflowTag(ctx, sharedDefName, v, sharedTag); err != nil {
					t.Fatalf("tenant B SetWorkflowTag v%d: %v", v, err)
				}
			}

			gotA, err := storeA.GetWorkflowTag(ctx, sharedDefName, sharedTag)
			if err != nil {
				t.Fatalf("tenant A GetWorkflowTag: %v", err)
			}
			if gotA != 2 {
				t.Errorf("tenant B retagging its own %q moved tenant A's to v%d; A promoted v2",
					sharedTag, gotA)
			}
		})
	}
}

// TestRemovingOneTenantsTagLeavesTheOthersNamesake — the destructive case,
// which only becomes reachable once both tenants can hold the name.
func TestRemovingOneTenantsTagLeavesTheOthersNamesake(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, storeB, _ := twoTenantStores(t, backend)
			ctx := context.Background()
			seedLopsidedTagFixture(t, storeA, storeB)

			if err := storeA.SetWorkflowTag(ctx, sharedDefName, 2, sharedTag); err != nil {
				t.Fatalf("tenant A SetWorkflowTag: %v", err)
			}
			if err := storeB.SetWorkflowTag(ctx, sharedDefName, 1, sharedTag); err != nil {
				t.Fatalf("tenant B SetWorkflowTag: %v", err)
			}

			if err := storeB.RemoveWorkflowTag(ctx, sharedDefName, sharedTag); err != nil {
				t.Fatalf("tenant B RemoveWorkflowTag: %v", err)
			}

			gotA, err := storeA.GetWorkflowTag(ctx, sharedDefName, sharedTag)
			if err != nil || gotA != 2 {
				t.Errorf("tenant B removing its own %q took tenant A's with it: A reads v%d (err %v), want v2",
					sharedTag, gotA, err)
			}

			// And B's own really is gone, so this cannot pass by the delete
			// having done nothing at all.
			if v, err := storeB.GetWorkflowTag(ctx, sharedDefName, sharedTag); err == nil && v != 0 {
				t.Errorf("tenant B's own %q survived its own delete, reading v%d", sharedTag, v)
			}
		})
	}
}

// TestGetWorkflowTagsListsOnlyTheCallersTags covers the listing, which returns
// a map keyed by tag name -- so two tenants' rows for one name do not merely
// add an entry, they collide on the key and one silently wins.
func TestGetWorkflowTagsListsOnlyTheCallersTags(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, storeB, _ := twoTenantStores(t, backend)
			ctx := context.Background()
			seedLopsidedTagFixture(t, storeA, storeB)

			if err := storeA.SetWorkflowTag(ctx, sharedDefName, 2, sharedTag); err != nil {
				t.Fatalf("tenant A SetWorkflowTag: %v", err)
			}
			if err := storeB.SetWorkflowTag(ctx, sharedDefName, 1, sharedTag); err != nil {
				t.Fatalf("tenant B SetWorkflowTag: %v", err)
			}

			for _, tc := range []struct {
				who   string
				store WorkflowStore
				want  int
			}{{"A", storeA, 2}, {"B", storeB, 1}} {
				tags, err := tc.store.GetWorkflowTags(ctx, sharedDefName)
				if err != nil {
					t.Fatalf("GetWorkflowTags(%s): %v", tc.who, err)
				}
				if len(tags) != 1 {
					t.Errorf("tenant %s sees %d tags for %q (%v), want exactly its own one",
						tc.who, len(tags), sharedDefName, tags)
				}
				if tags[sharedTag] != tc.want {
					t.Errorf("tenant %s's listing maps %q to v%d, want v%d",
						tc.who, sharedTag, tags[sharedTag], tc.want)
				}
			}
		})
	}
}
