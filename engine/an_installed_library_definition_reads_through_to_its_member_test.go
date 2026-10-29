package engine

// cleat#3540, PR 2: a definition installed from a library holds no bytes of its
// own, it names an immutable member of a published library, and the readers have
// to follow that reference.
//
// Four readers do, not the three the design listed: LoadWASM, GetWASMLength and
// GetWASMDigest, and GetWorkflowDef. The fourth was missed by reading, and what
// it costs is quiet: the start route resolves an entry point from
// WorkflowDef.WASMBytes to choose an input schema, and an empty slice made that
// resolution fail without saying so, which skipped input validation for every
// library definition whose input did not name its entry point. So the assertion
// on GetWorkflowDef below is that its bytes are the member's, not that the call
// succeeds.
//
// One fixture throughout: two tenants install the SAME member, a third tenant
// has an ordinary definition under the same name and version, and each is read
// through its own tenant's store over a connection the policy applies to (a
// non-superuser on PostgreSQL; SQL Server has no bypass). The ordinary tenant is
// the control: a reader that followed every row's library column would break it,
// and one that never followed it would pass it.
//
// The write guard is asserted on the same rows: bytes cannot be written over a
// library definition, identical or not, forced or not, and the refusal says why
// rather than naming a constraint.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

type libraryReadRig struct {
	name string
	// store is the tenant's store over a connection the tenant policy applies to.
	store func(tenant string) WorkflowStore
	// seedMember publishes a member as the operator would (the publisher role is
	// not under test here).
	seedMember func(t *testing.T, library, name string, version int, wasm []byte)
	// seedReference writes a tenant's definition row that names the member and
	// holds no bytes.
	seedReference func(t *testing.T, tenant, library, name string, version int)
	// seedOrdinary writes a tenant's ordinary definition row.
	seedOrdinary func(t *testing.T, tenant, name string, version int, wasm []byte)
	// cleanup removes everything the test wrote, tenant rows before the member
	// they reference.
	cleanup func(tenants []string, library string)
}

func TestAnInstalledLibraryDefinitionReadsThroughToItsMember(t *testing.T) {
	t.Run("postgres as a role the policy applies to", func(t *testing.T) {
		adminDB := testutil.TestDB(t, testutil.DialectPostgres)
		// t.Cleanup, not defer: the rig's own cleanup is a t.Cleanup and uses this
		// handle, and cleanups run last-registered first, so this one runs after it.
		t.Cleanup(func() { adminDB.Close() })
		testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
		testutil.CleanupPostgresTestData(t, adminDB)
		appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
		t.Cleanup(func() { appDB.Close() })
		assertNotSuperuserBypass(t, appDB)
		runAnInstalledLibraryDefinitionReads(t, &libraryReadRig{
			name:  "postgres",
			store: func(tenant string) WorkflowStore { return NewPostgresStore(appDB).WithTenant(tenant) },
			seedMember: func(t *testing.T, library, name string, version int, wasm []byte) {
				t.Helper()
				sum := sha256.Sum256(wasm)
				if _, err := adminDB.Exec(`INSERT INTO library.members (library, name, version, wasm_bytes, wasm_sha256, wasm_length, abi_version)
					VALUES ($1, $2, $3, $4, $5, $6, 1)`, library, name, version, wasm, hex.EncodeToString(sum[:]), len(wasm)); err != nil {
					t.Fatalf("publish member: %v", err)
				}
			},
			seedReference: func(t *testing.T, tenant, library, name string, version int) {
				t.Helper()
				if _, err := adminDB.Exec(`INSERT INTO workflow_defs (name, version, library, abi_version, min_version, tenant_id)
					VALUES ($1, $2, $3, 1, 0, $4)`, name, version, library, tenant); err != nil {
					t.Fatalf("install reference for %s: %v", tenant, err)
				}
			},
			seedOrdinary: func(t *testing.T, tenant, name string, version int, wasm []byte) {
				t.Helper()
				if _, err := adminDB.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
					VALUES ($1, $2, $3, 1, 0, $4)`, name, version, wasm, tenant); err != nil {
					t.Fatalf("seed ordinary definition for %s: %v", tenant, err)
				}
			},
			cleanup: func(tenants []string, library string) {
				for _, tenant := range tenants {
					_, _ = adminDB.Exec(`DELETE FROM workflow_defs WHERE tenant_id = $1`, tenant)
				}
				_, _ = adminDB.Exec(`DELETE FROM library.members WHERE library = $1`, library)
			},
		})
	})

	t.Run("sqlserver", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMSSQL)
		t.Cleanup(func() { db.Close() })
		testutil.SetupFullSchema(t, db, testutil.DialectMSSQL)
		ctx := context.Background()
		// asTenant runs fn on one connection whose SESSION_CONTEXT tenant is set, which
		// is what every statement of the store's own transactions does.
		asTenant := func(t *testing.T, tenant string, fn func(c *sql.Conn)) {
			t.Helper()
			c, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := c.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tenant); err != nil {
				t.Fatalf("scoping the connection to %s: %v", tenant, err)
			}
			fn(c)
		}
		runAnInstalledLibraryDefinitionReads(t, &libraryReadRig{
			name:  "sqlserver",
			store: func(tenant string) WorkflowStore { return NewMSSQLStore(db).WithTenant(tenant) },
			seedMember: func(t *testing.T, library, name string, version int, wasm []byte) {
				t.Helper()
				sum := sha256.Sum256(wasm)
				if _, err := db.ExecContext(ctx, `INSERT INTO library.members (library, name, version, wasm_bytes, wasm_sha256, wasm_length, abi_version)
					VALUES (@p1, @p2, @p3, @p4, @p5, @p6, 1)`, library, name, version, wasm, hex.EncodeToString(sum[:]), len(wasm)); err != nil {
					t.Fatalf("publish member: %v", err)
				}
			},
			seedReference: func(t *testing.T, tenant, library, name string, version int) {
				t.Helper()
				asTenant(t, tenant, func(c *sql.Conn) {
					if _, err := c.ExecContext(ctx, `INSERT INTO workflow_defs (name, version, library, abi_version, min_version, tenant_id)
						VALUES (@p1, @p2, @p3, 1, 0, @p4)`, name, version, library, tenant); err != nil {
						t.Fatalf("install reference for %s: %v", tenant, err)
					}
				})
			},
			seedOrdinary: func(t *testing.T, tenant, name string, version int, wasm []byte) {
				t.Helper()
				asTenant(t, tenant, func(c *sql.Conn) {
					if _, err := c.ExecContext(ctx, `INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
						VALUES (@p1, @p2, @p3, 1, 0, @p4)`, name, version, wasm, tenant); err != nil {
						t.Fatalf("seed ordinary definition for %s: %v", tenant, err)
					}
				})
			},
			cleanup: func(tenants []string, library string) {
				for _, tenant := range tenants {
					asTenant(t, tenant, func(c *sql.Conn) {
						_, _ = c.ExecContext(ctx, `DELETE FROM workflow_defs WHERE tenant_id = @p1`, tenant)
					})
				}
				_, _ = db.ExecContext(ctx, `DELETE FROM library.members WHERE library = @p1`, library)
			},
		})
	})
}

func runAnInstalledLibraryDefinitionReads(t *testing.T, rig *libraryReadRig) {
	t.Helper()
	ctx := context.Background()
	const (
		installerA = "f1000000-0000-4000-8000-0000000035a0"
		installerB = "f1000000-0000-4000-8000-0000000035b0"
		ordinary   = "f1000000-0000-4000-8000-0000000035c0"
	)
	tenants := []string{installerA, installerB, ordinary}
	stamp := time.Now().UnixNano()
	library := fmt.Sprintf("lib-3540-%d", stamp)
	name := fmt.Sprintf("lib-def-%d", stamp)
	const version = 1

	// Bytes that are not a valid module and need not be: these readers do not
	// interpret them. Distinct and long enough that a length mix-up shows.
	memberBytes := make([]byte, 300)
	for i := range memberBytes {
		memberBytes[i] = byte(i*7 + 3)
	}
	ordinaryBytes := []byte("an ordinary tenant's own bytes, a different length")

	rig.cleanup(tenants, library)
	t.Cleanup(func() { rig.cleanup(tenants, library) })

	rig.seedMember(t, library, name, version, memberBytes)
	rig.seedReference(t, installerA, library, name, version)
	rig.seedReference(t, installerB, library, name, version)
	rig.seedOrdinary(t, ordinary, name, version, ordinaryBytes)

	wantDigest := WASMDigest(memberBytes)

	for _, tenant := range []string{installerA, installerB} {
		st := rig.store(tenant)

		got, err := st.LoadWASM(ctx, name, version)
		if err != nil {
			t.Fatalf("%s: LoadWASM for an installed definition of tenant %s: %v", rig.name, tenant, err)
		}
		if string(got) != string(memberBytes) {
			t.Errorf("%s: LoadWASM returned %d bytes for tenant %s, want the member's %d. A library definition holds no bytes "+
				"of its own, so a reader that does not follow the reference returns nothing", rig.name, len(got), tenant, len(memberBytes))
		}

		length, err := st.GetWASMLength(ctx, name, version)
		if err != nil {
			t.Fatalf("%s: GetWASMLength for tenant %s: %v", rig.name, tenant, err)
		}
		if length != int64(len(memberBytes)) {
			t.Errorf("%s: GetWASMLength = %d for tenant %s, want the member's %d", rig.name, length, tenant, len(memberBytes))
		}

		digester, ok := st.(WASMDigester)
		if !ok {
			t.Fatalf("%s: the store does not implement WASMDigester", rig.name)
		}
		digest, err := digester.GetWASMDigest(ctx, name, version)
		if err != nil {
			t.Fatalf("%s: GetWASMDigest for tenant %s: %v", rig.name, tenant, err)
		}
		if digest != wantDigest {
			t.Errorf("%s: GetWASMDigest = %q for tenant %s, want the member's %q", rig.name, digest, tenant, wantDigest)
		}

		def, err := st.GetWorkflowDef(ctx, name, version)
		if err != nil || def == nil {
			t.Fatalf("%s: GetWorkflowDef for tenant %s: %v (def %v)", rig.name, tenant, err, def)
		}
		if string(def.WASMBytes) != string(memberBytes) {
			t.Errorf("%s: GetWorkflowDef.WASMBytes is %d bytes for tenant %s, want the member's %d. The start route resolves "+
				"an entry point from these to choose an input schema; empty, that fails silently and input validation is skipped",
				rig.name, len(def.WASMBytes), tenant, len(memberBytes))
		}
	}

	// THE CONTROL: an ordinary definition under the very same name and version.
	ost := rig.store(ordinary)
	if got, err := ost.LoadWASM(ctx, name, version); err != nil || string(got) != string(ordinaryBytes) {
		t.Errorf("%s: an ordinary definition's LoadWASM = %q, %v; want its own bytes", rig.name, got, err)
	}
	if length, err := ost.GetWASMLength(ctx, name, version); err != nil || length != int64(len(ordinaryBytes)) {
		t.Errorf("%s: an ordinary definition's GetWASMLength = %d, %v; want %d", rig.name, length, err, len(ordinaryBytes))
	}
	if digest, err := ost.(WASMDigester).GetWASMDigest(ctx, name, version); err != nil || digest != WASMDigest(ordinaryBytes) {
		t.Errorf("%s: an ordinary definition's GetWASMDigest = %q, %v; want the hash of its own bytes", rig.name, digest, err)
	}

	// THE WRITE GUARD. Nothing can be written to a library definition's row: not
	// the member's own bytes, not under ReplaceUnderOpenRuns, and the refusal says
	// why instead of naming a constraint.
	ast := rig.store(installerA)
	for _, replace := range []bool{false, true} {
		err := ast.DeployWorkflowDef(ctx, &WorkflowDef{
			Name: name, Version: version, WASMBytes: memberBytes, ABIVersion: 1, ReplaceUnderOpenRuns: replace,
		})
		if !errors.Is(err, ErrWorkflowDefIsLibraryMember) {
			t.Errorf("%s: deploying bytes over an installed library definition (replace=%v) gave %v, want ErrWorkflowDefIsLibraryMember",
				rig.name, replace, err)
		}
	}
	if got, err := ast.LoadWASM(ctx, name, version); err != nil || string(got) != string(memberBytes) {
		t.Errorf("%s: after the refused deploys the installed definition reads %d bytes, %v; want the member's", rig.name, len(got), err)
	}
	// The control for the guard: an ordinary definition redeployed with identical
	// bytes is still the idempotent upsert it always was.
	if err := ost.DeployWorkflowDef(ctx, &WorkflowDef{Name: name, Version: version, WASMBytes: ordinaryBytes, ABIVersion: 1}); err != nil {
		t.Errorf("%s: redeploying an ordinary definition with identical bytes: %v", rig.name, err)
	}
}
