package engine

// cleat#3785. The HTTP layer asks a definition's exposure class on nearly every request, and it used to ask
// GetWorkflowDef, which returns the whole row: the module's bytes (19 MB for a Python one) to learn one short
// string. GetWorkflowDefExposure asks only for the class.
//
// Two things are pinned. The answer is the same one GetWorkflowDef gives, per version and per tenant, on every
// backend -- "narrower" must not become "different". And the statement selects nothing else, which is the
// point of the change and which no behavioural test can see: a statement that also read wasm_bytes would
// return the same answer, only slower.

import (
	"context"
	"strings"
	"testing"
)

func TestTheExposureReadAnswersWhatGetWorkflowDefAnswersAndOnlyThat(t *testing.T) {
	const (
		tenantA = "a1a1a1a1-1111-4111-8111-a1a1a1a1a1a1"
		tenantB = "b2b2b2b2-2222-4222-8222-b2b2b2b2b2b2"
	)
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			base, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			sa := storeForTenant(t, base, tenantA)
			sb := storeForTenant(t, base, tenantB)

			reader := func(s WorkflowStore) DefExposureReader {
				r, ok := s.(DefExposureReader)
				if !ok {
					t.Fatalf("%T does not implement DefExposureReader, so the HTTP layer reads whole definitions on this backend", s)
				}
				return r
			}
			deploy := func(s WorkflowStore, name string, version int, class ExposureClass) {
				t.Helper()
				if err := s.DeployWorkflowDef(ctx, &WorkflowDef{
					Name: name, Version: version, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
					ABIVersion: 1, MinVersion: 1, Exposure: class,
				}); err != nil {
					t.Fatalf("deploy %s v%d: %v", name, version, err)
				}
			}

			// Each class, and two versions of one name that differ, so a read that ignored the version would
			// answer one of them for both.
			deploy(sa, "expo-internal", 1, ExposureInternal)
			deploy(sa, "expo-public", 1, ExposurePublic)
			deploy(sa, "expo-auth", 1, ExposureAuth)
			deploy(sa, "expo-unset", 1, "") // the store writes the default
			deploy(sa, "expo-versioned", 1, ExposureInternal)
			deploy(sa, "expo-versioned", 2, ExposureAuth)
			// Tenant B holds a SAME-NAMED, differently-classed definition, so a read not scoped to the tenant
			// answers with the wrong tenant's class instead of a miss.
			deploy(sb, "expo-internal", 1, ExposurePublic)

			type want struct {
				name    string
				version int
				store   WorkflowStore
				class   ExposureClass
				found   bool
			}
			for _, w := range []want{
				{"expo-internal", 1, sa, ExposureInternal, true},
				{"expo-public", 1, sa, ExposurePublic, true},
				{"expo-auth", 1, sa, ExposureAuth, true},
				{"expo-unset", 1, sa, ExposureAuth, true},
				{"expo-versioned", 1, sa, ExposureInternal, true},
				{"expo-versioned", 2, sa, ExposureAuth, true},
				{"expo-internal", 1, sb, ExposurePublic, true}, // its own tenant's, not A's
				{"expo-public", 1, sb, "", false},              // A's definition is not visible to B
				{"expo-versioned", 3, sa, "", false},           // no such version
				{"no-such-definition", 1, sa, "", false},
			} {
				class, found, err := reader(w.store).GetWorkflowDefExposure(ctx, w.name, w.version)
				if err != nil {
					t.Errorf("%s v%d: %v", w.name, w.version, err)
					continue
				}
				if found != w.found || class.OrDefault() != w.class.OrDefault() && w.found {
					t.Errorf("%s v%d: GetWorkflowDefExposure = (%q, found=%v), want (%q, found=%v)",
						w.name, w.version, class, found, w.class, w.found)
				}
				// The narrow read must agree with the wide one it replaces, wherever the wide one has an answer.
				def, derr := w.store.GetWorkflowDef(ctx, w.name, w.version)
				if derr != nil {
					t.Errorf("%s v%d: GetWorkflowDef: %v", w.name, w.version, derr)
					continue
				}
				if (def != nil) != found || (def != nil && def.Exposure.OrDefault() != class.OrDefault()) {
					t.Errorf("%s v%d: the narrow read says (%q, found=%v) but GetWorkflowDef says %+v", w.name, w.version, class, found, def)
				}
			}
		})
	}
}

// The statements are constants so this can be asserted without a database. wasm_bytes is the column the
// change exists to stop reading; `*` would read it too.
func TestTheExposureStatementsSelectOnlyTheClass(t *testing.T) {
	for name, stmt := range map[string]string{"postgres": pgDefExposureSQL, "mysql": mysqlDefExposureSQL, "mssql": mssqlDefExposureSQL} {
		sel := strings.ToLower(stmt[:strings.Index(strings.ToLower(stmt), " from ")])
		if sel != "select exposure" {
			t.Errorf("%s: the statement selects %q; want exactly `exposure`, so the module bytes are never read:\n%s", name, sel, stmt)
		}
		if !strings.Contains(strings.ToLower(stmt), "tenant_id") {
			t.Errorf("%s: the statement does not name the tenant; on a backend with no row-level security that is the whole scope:\n%s", name, stmt)
		}
	}
}
