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
//   - The one way PostgreSQL DID hand a definition across tenants was by design.
//     `tenant_isolation_defs` was
//     `USING (tenant_id = assert_tenant_set() OR tenant_id = '00000000-...')`,
//     so a default-tenant definition was readable by every tenant -- the read
//     side of the adoption window. **That clause is gone**, dropped by
//     migration 035 along with canAdoptDef when D7 landed, exactly as this
//     comment predicted. The policy is now plain
//     `tenant_id = cleat.assert_tenant_set()`. Confirm with
//     `grep -rn "assert_tenant_set() OR" migrations/postgres/*.sql`, which
//     matches nothing. Not pinning it was right: a test enshrining it would
//     have had to be deleted with it.
//
// Also noted while building this: an index on exactly the tuple the new key
// needs -- `idx_defs_tenant_name_version ON workflow_defs(tenant_id, name,
// version)` -- already existed on all three dialects, so the migration had one
// to promote rather than one to build over a populated table. It is now the
// primary key.
//
// WHAT THIS FILE ASSERTS CHANGED MEANING WHEN D7 LANDED, and that is the part
// worth reading before editing it. The property above is "tenant B sees
// NOTHING", and its fixture seeds only tenant A -- so it is still true and
// still worth having. But it is no longer the whole property, because B is now
// entitled to its OWN definition of the same name. "Sees nothing" and "sees its
// own" are different claims, and a test asserting the first will pass happily
// while the second is broken: that is exactly how 3.86's LoadWASM defect
// survived, handing tenant B tenant A's compiled code while every assertion
// here stayed green.
//
// TestDefinitionLookupsAnswerAboutTheCallersOwnRowsWhenBothHoldTheName below is
// the second half. It runs the same fourteen methods with BOTH tenants holding
// the name, so a lookup that answers about the wrong tenant is caught rather
// than merely a lookup that answers when it should not.

import (
	"context"
	"fmt"
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

// TestLatestVersionResolvesWithinTheCallersTenant covers the failure mode the
// read property above cannot see, and it is the more dangerous of the two.
//
// "Resolve the latest version of this name" is an aggregate -- MAX(version) --
// so it returns exactly one row whether or not the tenant is in the predicate.
// Unscoped it does not error and does not hand back somebody else's row: it
// hands back somebody else's NUMBER. The caller then starts a workflow on a
// version of its own definition that may not exist, or runs code its tenant
// never deployed, and nothing anywhere reports a problem.
//
// So the fixture is deliberately lopsided. Tenant A holds v1, v2 and v3 of a
// name; tenant B holds only v1. An unscoped MAX picks 3; B's own answer is 1. A
// symmetric fixture -- both tenants at the same version -- could not tell the
// two apart, which is the trap this file's header is about in miniature.
func TestLatestVersionResolvesWithinTheCallersTenant(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, storeB, _ := twoTenantStores(t, backend)
			ctx := context.Background()
			const name = "version-skew"

			for _, v := range []int{1, 2, 3} {
				if err := storeA.DeployWorkflowDef(ctx, &WorkflowDef{
					Name: name, Version: v, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d, byte(v)},
					ABIVersion: 1, MinVersion: 1,
				}); err != nil {
					t.Fatalf("tenant A deploy v%d: %v", v, err)
				}
			}
			if err := storeB.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: name, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d, 0xB1},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("tenant B deploy v1: %v", err)
			}

			gotB, err := storeB.ResolveLatestVersion(ctx, name)
			if err != nil {
				t.Fatalf("ResolveLatestVersion(B): %v", err)
			}
			if gotB != 1 {
				t.Errorf("tenant B resolved %q to version %d, want its own 1 -- "+
					"tenant A holds v3 of that name, so %d is A's number", name, gotB, gotB)
			}

			// The control, so a bug that forced every answer to 1 could not
			// pass this test: A's own answer really is 3.
			gotA, err := storeA.ResolveLatestVersion(ctx, name)
			if err != nil {
				t.Fatalf("ResolveLatestVersion(A): %v", err)
			}
			if gotA != 3 {
				t.Errorf("tenant A resolved %q to version %d, want 3", name, gotA)
			}
		})
	}
}

// TestDefinitionLookupsAnswerAboutTheCallersOwnRowsWhenBothHoldTheName is the
// property the file above cannot express, because its fixture gives the name to
// one tenant only.
//
// D7 (3.77) made two tenants holding "order-processor" ordinary. From that
// point "tenant B sees nothing" stopped being the whole requirement: B must see
// ITS OWN definition and never A's, and a lookup that confuses the two returns
// a plausible answer rather than an empty one. 3.86 is what that looks like in
// practice -- LoadWASM handed tenant B tenant A's compiled code, and every
// assertion in the older property stayed green throughout, because its fixture
// never gave B a row to confuse.
//
// The fixture is deliberately lopsided in three ways at once, because each one
// catches a different wrong answer:
//
//   - A holds v1 and v2, B holds only v1 -- so a version-resolving lookup that
//     ignores the tenant returns 2 where B's answer is 1.
//   - the WASM bytes differ (0xAA vs 0xBB) -- so a byte-returning lookup that
//     reads the wrong row is caught even though both rows exist and are the
//     same length.
//   - only A has a tag and a routing rule -- so B's tag and routing lookups
//     must still answer empty, which is the OLD property, retained rather than
//     replaced.
//
// Runs on tenant-SCOPED stores, so on PostgreSQL and SQL Server the policy is
// doing the work and this is a check that the policy is actually bound. 3.86
// covers the same surface on a cleat_admin connection where it is not; both are
// needed, and neither substitutes for the other.
func TestDefinitionLookupsAnswerAboutTheCallersOwnRowsWhenBothHoldTheName(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			storeA, storeB, _ := twoTenantStores(t, backend)
			ctx := context.Background()

			const aByte, bByte = 0xAA, 0xBB
			for _, v := range []int{1, 2} {
				if err := storeA.DeployWorkflowDef(ctx, &WorkflowDef{
					Name: probeDefName, Version: v,
					WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d, aByte},
					ABIVersion: 1, MinVersion: 1,
				}); err != nil {
					t.Fatalf("tenant A deploy v%d: %v", v, err)
				}
			}
			if err := storeB.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: probeDefName, Version: 1,
				WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d, bByte},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("tenant B deploy v1: %v", err)
			}
			if err := storeA.SetWorkflowTag(ctx, probeDefName, 2, probeTag); err != nil {
				t.Fatalf("tenant A SetWorkflowTag: %v", err)
			}
			if err := storeA.SetRoutingRule(ctx, probeDefName, 2, 1.0); err != nil {
				t.Fatalf("tenant A SetRoutingRule: %v", err)
			}

			// Fixture check. Without it every "B sees its own" assertion could
			// hold against a fixture where A deployed nothing at all.
			if got, err := storeA.ListVersions(ctx, probeDefName); err != nil || len(got) != 2 {
				t.Fatalf("fixture is broken: tenant A sees versions %v (err %v), want two", got, err)
			}

			for _, tc := range []struct {
				name string
				// wrong reports what tenant B got wrong, or "" for correct.
				wrong func() string
			}{
				{"LoadWASM — B's own compiled code, not A's", func() string {
					b, err := storeB.LoadWASM(ctx, probeDefName, 1)
					if err != nil {
						return fmt.Sprintf("could not load its own definition: %v", err)
					}
					if len(b) == 0 {
						return "loaded nothing for a definition it deployed"
					}
					if b[len(b)-1] != bByte {
						return fmt.Sprintf("loaded WASM ending 0x%02X, want its own 0x%02X -- "+
							"its workflow would execute tenant A's code", b[len(b)-1], bByte)
					}
					return ""
				}},
				{"GetWorkflowDef — B's own record", func() string {
					d, err := storeB.GetWorkflowDef(ctx, probeDefName, 1)
					if err != nil {
						return fmt.Sprintf("errored on its own definition: %v", err)
					}
					if d == nil {
						return "got no record for a definition it deployed"
					}
					if len(d.WASMBytes) == 0 || d.WASMBytes[len(d.WASMBytes)-1] != bByte {
						return "got a record carrying tenant A's bytes"
					}
					return ""
				}},
				{"ListVersions — B holds only v1", func() string {
					v, err := storeB.ListVersions(ctx, probeDefName)
					if err != nil {
						return fmt.Sprintf("errored: %v", err)
					}
					if len(v) != 1 || v[0] != 1 {
						return fmt.Sprintf("sees versions %v, want exactly [1] -- v2 is tenant A's", v)
					}
					return ""
				}},
				{"ResolveLatestVersion — B's latest is 1, A's is 2", func() string {
					v, err := storeB.ResolveLatestVersion(ctx, probeDefName)
					if err != nil {
						return fmt.Sprintf("errored: %v", err)
					}
					if v != 1 {
						return fmt.Sprintf("resolved v%d, want its own v1 -- %d is tenant A's number", v, v)
					}
					return ""
				}},
				{"ValidateVersion — v2 exists only for A", func() string {
					ok, err := storeB.ValidateVersion(ctx, probeDefName, 2)
					if err == nil && ok {
						return "confirms v2 exists; only tenant A deployed v2"
					}
					return ""
				}},
				{"ListWorkflowDefs — one row, B's", func() string {
					ds, err := storeB.ListWorkflowDefs(ctx, probeDefName)
					if err != nil {
						return fmt.Sprintf("errored: %v", err)
					}
					if len(ds) != 1 {
						return fmt.Sprintf("lists %d definitions, want exactly its own 1", len(ds))
					}
					return ""
				}},
				{"GetWorkflowTag — A tagged, B did not", func() string {
					v, err := storeB.GetWorkflowTag(ctx, probeDefName, probeTag)
					if err == nil && v > 0 {
						return fmt.Sprintf("resolved tag %q to v%d; only tenant A tagged", probeTag, v)
					}
					return ""
				}},
				{"ResolveVersionByTag — the run-start path", func() string {
					v, err := storeB.ResolveVersionByTag(ctx, probeDefName, probeTag)
					if err == nil && v > 0 {
						return fmt.Sprintf("would start a run on v%d from tenant A's tag", v)
					}
					return ""
				}},
				{"GetWorkflowTags — A's tags are not B's", func() string {
					m, err := storeB.GetWorkflowTags(ctx, probeDefName)
					if err == nil && len(m) > 0 {
						return fmt.Sprintf("enumerated tags %v; only tenant A tagged", m)
					}
					return ""
				}},
				{"GetRoutingRules — A routed, B did not", func() string {
					rs, err := storeB.GetRoutingRules(ctx, probeDefName)
					if err == nil && len(rs) > 0 {
						return fmt.Sprintf("read %d routing rules; only tenant A routed", len(rs))
					}
					return ""
				}},
				{"PickVersionByRouting — A's split must not steer B", func() string {
					v, err := storeB.PickVersionByRouting(ctx, probeDefName)
					if err == nil && v == 2 {
						return "picked v2 from tenant A's routing rule; B has no v2"
					}
					return ""
				}},
				{"GetWASMLength — B's own", func() string {
					n, err := storeB.GetWASMLength(ctx, probeDefName, 2)
					if err == nil && n > 0 {
						return "reports a length for v2, which only tenant A deployed"
					}
					return ""
				}},
			} {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					if wrong := tc.wrong(); wrong != "" {
						t.Errorf("tenant B %s", wrong)
					}
				})
			}

			// The control. Every assertion above would also hold if B's own
			// rows had silently failed to arrive, so check A still sees its.
			if v, err := storeA.ResolveLatestVersion(ctx, probeDefName); err != nil || v != 2 {
				t.Errorf("tenant A resolves v%d (err %v), want its own v2", v, err)
			}
		})
	}
}
