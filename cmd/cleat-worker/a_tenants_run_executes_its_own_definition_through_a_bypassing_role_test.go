package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#3740, end to end: a real PostgreSQL, a real wasmtime guest and a real worker.
//
// Two tenants deploy the SAME workflow name and version with different code, and a run in one
// tenant is executed. The worker reads the module through the store's LoadWASM. That statement
// carried no tenant predicate on PostgreSQL, so it relied on row-level security alone, and
// through a connection the policy does not apply to (a superuser, or a role with BYPASSRLS) it
// answered with whichever tenant's row the planner met first. A run then executed ANOTHER
// tenant's code with its own tenant's input.
//
// The guest says which build it is by the operation of the one call it makes: MARKER-AAAAAA is
// tenant A's, MARKER-BBBBBB is tenant B's. The test patches the compiled module's data segment
// rather than building twice, so the two are otherwise identical.
//
// Each subtest runs a different tenant's run against a different insertion order, because
// there is no ORDER BY in the statement and the winner is whatever the plan reaches first: a
// test with one order would pass against the bug whenever that order happened to favour the
// run's own tenant.
func TestATenantsRunExecutesItsOwnDefinitionEvenThroughARoleThatBypassesRLS(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping the database-backed cross-tenant definition test")
	}
	const tenantA = engine.DefaultTenantUUID
	const tenantB = "f3740000-0000-4000-8000-00000000000b"

	db := testutil.SuiteTestDB(t, "cleat_worker")
	// PRECONDITION: the connection BYPASSES the policy. On a connection the policy applies to,
	// a pass says nothing about the statement, because the policy is then doing the scoping.
	var bypass bool
	if err := db.QueryRow(`SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil {
		t.Fatalf("reading the test role: %v", err)
	}
	if !bypass {
		t.Fatal("this test needs a connection that BYPASSES row-level security (a superuser): the statement is its only " +
			"scope there, and on a connection the policy applies to it would pass without measuring the statement at all")
	}
	if _, err := db.Exec(`INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES ($1, 'cleat-3740-b', false) ON CONFLICT (tenant_id) DO NOTHING`, tenantB); err != nil {
		t.Fatalf("registering tenant B: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_defs WHERE name LIKE 'cross-tenant-3740-%'`)
		_, _ = db.Exec(`DELETE FROM workflow_instances WHERE def_name LIKE 'cross-tenant-3740-%'`)
		_, _ = db.Exec(`DELETE FROM admin.tenants WHERE tenant_id = $1`, tenantB)
	})

	wasmA := buildMarkerFixture(t)
	wasmB := bytes.ReplaceAll(wasmA, []byte("MARKER-AAAAAA"), []byte("MARKER-BBBBBB"))
	if bytes.Equal(wasmA, wasmB) || len(wasmA) != len(wasmB) {
		t.Fatalf("UNMEASURED: the fixture's marker is not in the module, or the patch changed its length (%d vs %d bytes)", len(wasmA), len(wasmB))
	}
	meta, err := wasm.ReadMetadata(wasmA)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}

	for _, c := range []struct {
		name         string
		firstDeploy  string // the tenant whose row is inserted first
		runTenant    string
		wantMarker   string
		otherMarker  string
		runTenantTag string
	}{
		{"tenant B deployed first, the run is tenant A's", tenantB, tenantA, "marker/MARKER-AAAAAA", "marker/MARKER-BBBBBB", "A"},
		{"tenant A deployed first, the run is tenant B's", tenantA, tenantB, "marker/MARKER-BBBBBB", "marker/MARKER-AAAAAA", "B"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()

			calls := &recordedCalls{}
			svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.add(strings.TrimPrefix(r.URL.Path, "/call/"))
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"ok":true}`)
			}))
			defer svc.Close()
			oldSvcURL := *benchSvcURL
			*benchSvcURL = svc.URL
			defer func() { *benchSvcURL = oldSvcURL }()

			storeA := engine.NewPostgresStore(db).WithTenant(tenantA)
			storeB := engine.NewPostgresStore(db).WithTenant(tenantB)
			stores := map[string]*engine.PostgresStore{tenantA: storeA, tenantB: storeB}
			code := map[string][]byte{tenantA: wasmA, tenantB: wasmB}

			defName := fmt.Sprintf("cross-tenant-3740-%d", time.Now().UnixNano())
			second := tenantA
			if c.firstDeploy == tenantA {
				second = tenantB
			}
			for _, tenant := range []string{c.firstDeploy, second} {
				if err := stores[tenant].DeployWorkflowDef(ctx, &engine.WorkflowDef{
					Name: defName, Version: meta.WorkflowVersion, WASMBytes: code[tenant],
					ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
				}); err != nil {
					t.Fatalf("deploying for tenant %s: %v", tenant[:8], err)
				}
			}
			// PREMISE, measured by the test itself with the tenant named in the SQL, so the
			// reading cannot be wrong in the way the statement under test is.
			for tenant, want := range code {
				var got []byte
				if err := db.QueryRow(`SELECT wasm_bytes FROM workflow_defs WHERE tenant_id = $1 AND name = $2 AND version = $3`,
					tenant, defName, meta.WorkflowVersion).Scan(&got); err != nil || !bytes.Equal(got, want) {
					t.Fatalf("UNMEASURED: tenant %s does not hold its own build of %s (err=%v)", tenant[:8], defName, err)
				}
			}

			runStore := stores[c.runTenant]
			id := fmt.Sprintf("%s-run-%d", defName, time.Now().UnixNano())
			if _, _, err := runStore.StartNewRun(ctx, id, defName, meta.WorkflowVersion,
				json.RawMessage(`{"__entry_point":"marker"}`), "", c.runTenant, 0); err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			logs := &syncBuffer{}
			w := newRealDeferPhaseWorker(t, db, runStore, logs)
			w.storeTenantID = c.runTenant
			w.egress = &engine.EgressGuard{
				AllowLoopback:  true,
				TenantOptional: func(context.Context) bool { return true },
			}
			runExecuteWorkflow(w, claimOne(t, ctx, runStore, w.id, id))

			if calls.has(c.otherMarker) {
				t.Errorf("tenant %s's run EXECUTED THE OTHER TENANT'S CODE: the guest made %s (cleat#3740)", c.runTenantTag, c.otherMarker)
			}
			if !calls.has(c.wantMarker) {
				t.Errorf("tenant %s's run did not execute its own build: the guest made %v, want %s (logs:\n%s)", c.runTenantTag, calls.all(), c.wantMarker, logs.String())
			}
		})
	}
}

// cleat#3740, the cache half. A warm worker validates its cached bytes against the store before
// serving them: by length on every hit, and by digest at most once per wasmRevalidateInterval.
// Both reads carried no tenant predicate, so through a bypassing role they compared the cached
// bytes with ANOTHER tenant's row and judged them stale.
//
// WHAT THE TEST ASSERTS ON, and why it is not the bytes. With LoadWASM scoped, a cache entry
// wrongly judged stale is simply reloaded from the right row, so the worker still serves its own
// bytes: the defect shows up as a pointless reload (and a read of the whole row), not as wrong
// code. Asserting on the bytes alone stayed green with the digest predicate removed, which is how
// this was found. The worker says when it throws an entry away, so the test asserts it did not.
//
// One case per read. The digest is the read that decides when the two builds are the SAME
// length, and the length is the read that decides when they differ.
func TestAWarmWorkerValidatesItsCachedDefinitionAgainstItsOwnTenantsRow(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping the database-backed cross-tenant cache test")
	}
	const tenantA = engine.DefaultTenantUUID
	const tenantB = "f3740000-0000-4000-8000-0000000000cc"
	old := wasmRevalidateInterval
	wasmRevalidateInterval = 0
	t.Cleanup(func() { wasmRevalidateInterval = old })

	db := testutil.SuiteTestDB(t, "cleat_worker")
	var bypass bool
	if err := db.QueryRow(`SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil {
		t.Fatalf("reading the test role: %v", err)
	}
	if !bypass {
		t.Fatal("this test needs a connection that BYPASSES row-level security (a superuser): the statement is its only " +
			"scope there, and on a connection the policy applies to it would pass without measuring the statement at all")
	}
	if _, err := db.Exec(`INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES ($1, 'cleat-3740-c', false) ON CONFLICT (tenant_id) DO NOTHING`, tenantB); err != nil {
		t.Fatalf("registering tenant B: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_defs WHERE name LIKE 'cross-tenant-3740-cache-%'`)
		_, _ = db.Exec(`DELETE FROM admin.tenants WHERE tenant_id = $1`, tenantB)
	})

	bytesA := []byte{0x00, 0x61, 0x73, 0x6d, 0xaa, 0xaa}
	for _, c := range []struct {
		name   string
		bytesB []byte
		reason string
	}{
		{"same length, so the digest decides", []byte{0x00, 0x61, 0x73, 0x6d, 0xbb, 0xbb}, "content"},
		{"different length, so the length decides", []byte{0x00, 0x61, 0x73, 0x6d, 0xbb, 0xbb, 0xbb, 0xbb}, "length"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			defName := fmt.Sprintf("cross-tenant-3740-cache-%d", time.Now().UnixNano())
			storeA := engine.NewPostgresStore(db).WithTenant(tenantA)
			storeB := engine.NewPostgresStore(db).WithTenant(tenantB)
			// Tenant B first, so a statement with no predicate answers from B's row.
			for _, d := range []struct {
				st *engine.PostgresStore
				b  []byte
			}{{storeB, c.bytesB}, {storeA, bytesA}} {
				if err := d.st.DeployWorkflowDef(ctx, &engine.WorkflowDef{Name: defName, Version: 1, WASMBytes: d.b, ABIVersion: 1, MinVersion: 1}); err != nil {
					t.Fatalf("DeployWorkflowDef: %v", err)
				}
			}

			logs := &syncBuffer{}
			w := newRealDeferPhaseWorker(t, db, storeA, logs)
			w.storeTenantID = tenantA
			// The cache holds tenant A's own bytes, verified, as a worker that has already run them would.
			key := tenantA + "/" + defName + ":1"
			w.wasmCache.putVerified(key, bytesA, engine.WASMDigest(bytesA), time.Now())

			got, err := w.loadWASM(storeA, tenantA, defName, 1)
			if err != nil {
				t.Fatalf("loadWASM: %v", err)
			}
			if !bytes.Equal(got, bytesA) {
				t.Errorf("a warm worker for tenant A served %x for %s, which is not its own build %x (cleat#3740)", got, defName, bytesA)
			}
			if strings.Contains(logs.String(), "WASM cache stale") {
				t.Errorf("a warm worker for tenant A threw away its own cached build (reason %q) because it compared it with another "+
					"tenant's row (cleat#3740); log:\n%s", c.reason, logs.String())
			}
		})
	}
}
