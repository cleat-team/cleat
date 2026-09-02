package engine

// IMPROVEMENT-PLAN 3.77, step 1: every store method that looks a workflow
// definition up BY NAME must answer only about the caller's own tenant.
//
// This is a property over a boundary rather than a test per method, and that is
// deliberate. There are ~65 statements naming workflow_defs across the three
// stores; auditing them by reading is the sweep CLAUDE.md exists to prevent
// ("ask whether the answer is a sweep or a mechanism"), and it cannot tell you
// which of them are already covered by a layer underneath. PostgreSQL runs most
// of these inside beginTxWithRLS, SQL Server has security policies, and MySQL
// has neither and carries the predicate in Go. Reading the SQL tells you which
// statements have `AND tenant_id`; only running them tells you which ones leak.
//
// The property: with tenant A holding a definition, a tag, a routing rule and a
// running instance under a name, every by-name read issued by tenant B answers
// as though none of it exists.
//
// It is a *lookup* property, not a deployment one. §3.12 covers what happens
// when B deploys over A's name; this covers what B can learn about A's without
// deploying anything.
//
// THE PROPERTY HOLDS TODAY -- 42 cases, three dialects, no leaks. That is the
// useful result, and it is worth saying why it is not a wasted test: it is what
// makes the §3.77 key change safe to make. Changing workflow_defs' primary key
// to (tenant_id, name, version) is only correct if no lookup depends on the
// name alone being unique, and this is the check that says so.
//
// Three things it established, each of which bears on that change:
//
//   - PostgreSQL FAILS CLOSED rather than leaking. Bypassing the RLS
//     transaction on one of these reads does not return another tenant's rows;
//     it raises "cleat.tenant_id is not set -- tenant context required for
//     RLS-scoped query (P0001)", because the policy is
//     `tenant_id = assert_tenant_set()`. Falsified that way, this file trips its
//     own fixture check rather than the property.
//   - MySQL leaks silently, and is the reason the property is worth having.
//     Removing `AND tenant_id = ?` from GetWASMLength turns exactly one case red
//     -- "tenant B learned another tenant's WASM is non-empty" -- with no error
//     anywhere. MySQL has no policy underneath, so the Go predicate is the whole
//     of the isolation (§3.11).
//   - The one way PostgreSQL DOES hand a definition across tenants is by design.
//     `tenant_isolation_defs` is
//     `USING (tenant_id = assert_tenant_set() OR tenant_id = '00000000-...')`,
//     so a default-tenant definition is readable by every tenant. That OR clause
//     is the read side of the adoption window and goes when canAdoptDef goes.
//     Deliberately NOT pinned here: a test that enshrines behaviour the next
//     change deletes is churn.
//
// Also noted while building this: an index on exactly the tuple the new key
// needs -- `idx_defs_tenant_name_version ON workflow_defs(tenant_id, name,
// version)` -- already exists on all three dialects, so the migration has one to
// promote rather than one to build over a populated table.

import (
	"context"
	"testing"
)

const (
	// One name, held by tenant A, asked for by tenant B. A name a customer
	// would plausibly choose, because that is the whole point: two tenants
	// naming a workflow "order-processor" is ordinary, not an attack.
	probeDefName = "order-processor"
	probeTag     = "stable"
)

// seedTenantADefinition gives tenant A a definition at version 1 and 2, a tag,
// a routing rule and one running instance, all under probeDefName.
func seedTenantADefinition(t *testing.T, storeA WorkflowStore) {
	t.Helper()
	ctx := context.Background()
	for _, v := range []int{1, 2} {
		if err := storeA.DeployWorkflowDef(ctx, &WorkflowDef{
			Name: probeDefName, Version: v,
			WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d, byte(v)},
			ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("tenant A DeployWorkflowDef v%d: %v", v, err)
		}
	}
	if err := storeA.SetWorkflowTag(ctx, probeDefName, 2, probeTag); err != nil {
		t.Fatalf("tenant A SetWorkflowTag: %v", err)
	}
	if err := storeA.SetRoutingRule(ctx, probeDefName, 2, 1.0); err != nil {
		t.Fatalf("tenant A SetRoutingRule: %v", err)
	}
	seedReadyRuns(t, storeA, unscopedTenantA, probeDefName, 1)
}

// TestDefinitionLookupsAnswerOnlyAboutTheCallersTenant is the property.
//
// Each case names what a leak would mean in the caller's terms, because
// "returned a non-zero value" is not what makes these worth fixing -- what
// makes them worth fixing is that the value belongs to somebody else.
func TestDefinitionLookupsAnswerOnlyAboutTheCallersTenant(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, storeB, _ := twoTenantStores(t, backend)
			seedTenantADefinition(t, storeA)
			ctx := context.Background()

			// Sanity: tenant A can see its own definition. Without this the
			// whole table could pass against a broken fixture -- every "B sees
			// nothing" assertion holds trivially if A deployed nothing.
			if got, err := storeA.ListVersions(ctx, probeDefName); err != nil || len(got) != 2 {
				t.Fatalf("fixture is broken: tenant A sees versions %v (err %v), want two", got, err)
			}

			for _, tc := range []struct {
				name string
				// leak reports what tenant B learned, or "" for nothing.
				leak func() string
			}{
				{"LoadWASM — the compiled code itself", func() string {
					b, err := storeB.LoadWASM(ctx, probeDefName, 1)
					if err == nil && len(b) > 0 {
						return "read another tenant's WASM bytes"
					}
					return ""
				}},
				{"GetWASMLength — the size of it", func() string {
					n, err := storeB.GetWASMLength(ctx, probeDefName, 1)
					if err == nil && n > 0 {
						return "learned another tenant's WASM is non-empty"
					}
					return ""
				}},
				{"ListVersions — how many versions exist", func() string {
					v, err := storeB.ListVersions(ctx, probeDefName)
					if err == nil && len(v) > 0 {
						return "enumerated another tenant's versions"
					}
					return ""
				}},
				{"ResolveLatestVersion — which version is current", func() string {
					v, err := storeB.ResolveLatestVersion(ctx, probeDefName)
					if err == nil && v > 0 {
						return "resolved a version from another tenant's definitions"
					}
					return ""
				}},
				{"ValidateVersion — whether a version exists", func() string {
					ok, err := storeB.ValidateVersion(ctx, probeDefName, 1)
					if err == nil && ok {
						return "confirmed another tenant's version exists"
					}
					return ""
				}},
				{"LoadWorkflowConfig — its configured limits", func() string {
					n, err := storeB.LoadWorkflowConfig(ctx, probeDefName, 1)
					if err == nil && n != 0 {
						return "read another tenant's max_history_length"
					}
					return ""
				}},
				{"GetWorkflowDef — the whole record", func() string {
					d, err := storeB.GetWorkflowDef(ctx, probeDefName, 1)
					if err == nil && d != nil {
						return "read another tenant's definition record"
					}
					return ""
				}},
				{"ListWorkflowDefs — the definition listing", func() string {
					ds, err := storeB.ListWorkflowDefs(ctx, probeDefName)
					if err == nil && len(ds) > 0 {
						return "listed another tenant's definitions"
					}
					return ""
				}},
				{"CountActiveInstances — how busy it is", func() string {
					n, err := storeB.CountActiveInstances(ctx, probeDefName, 1)
					if err == nil && n > 0 {
						return "counted another tenant's running workflows"
					}
					return ""
				}},
				{"GetWorkflowTag — where a tag points", func() string {
					v, err := storeB.GetWorkflowTag(ctx, probeDefName, probeTag)
					if err == nil && v > 0 {
						return "resolved another tenant's deployment tag"
					}
					return ""
				}},
				{"GetWorkflowTags — every tag", func() string {
					m, err := storeB.GetWorkflowTags(ctx, probeDefName)
					if err == nil && len(m) > 0 {
						return "enumerated another tenant's deployment tags"
					}
					return ""
				}},
				{"ResolveVersionByTag — a tag, resolved", func() string {
					v, err := storeB.ResolveVersionByTag(ctx, probeDefName, probeTag)
					if err == nil && v > 0 {
						return "resolved another tenant's tag to a version"
					}
					return ""
				}},
				{"GetRoutingRules — its traffic split", func() string {
					rs, err := storeB.GetRoutingRules(ctx, probeDefName)
					if err == nil && len(rs) > 0 {
						return "read another tenant's routing rules"
					}
					return ""
				}},
				{"PickVersionByRouting — the split, applied", func() string {
					v, err := storeB.PickVersionByRouting(ctx, probeDefName)
					if err == nil && v > 0 {
						return "picked a version from another tenant's routing rules"
					}
					return ""
				}},
			} {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					if leak := tc.leak(); leak != "" {
						t.Errorf("tenant B %s (%q)", leak, probeDefName)
					}
				})
			}
		})
	}
}
