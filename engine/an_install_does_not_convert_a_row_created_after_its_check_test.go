package engine

// cleat#3540, PR 3a: two guards in PostgresStore.InstallLibraryBundle are written for a
// window no ordinary test reaches: a definition created under one of the bundle's keys
// AFTER the collision check has looked and BEFORE the write.
//
//   - the refresh arm's `WHERE workflow_defs.library = EXCLUDED.library`, which keeps
//     ON CONFLICT DO UPDATE off a row that is not this library's. Without it the
//     tenant's own definition keeps its own bytes and has its METADATA (plugin
//     dependencies, schemas, exposure, task queue) overwritten with the member's: its
//     code under someone else's description;
//   - the check that the rows written equal the bundle's members, which turns the
//     now-skipped member into an error instead of a "successful" install with a member
//     missing and an install recorded.
//
// Review of #3599 found that no test reached either (dropping either left every test
// green). The window is made deterministic here rather than raced: an ordinary row is
// inserted and left UNCOMMITTED, so the collision check (READ COMMITTED, one snapshot
// per statement) cannot see it, and the write then blocks on its unique-index entry;
// the row is committed only once the install is seen waiting on a lock.
//
// PostgreSQL only. On SQL Server the collision check is a read under READ COMMITTED
// without snapshot isolation, so it BLOCKS on the same uncommitted row instead of
// missing it, and the guard in the MERGE is unreachable this way; it is not tested.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestAnInstallDoesNotConvertARowCreatedAfterItsCollisionCheck(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { adminDB.Close() })
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
	t.Cleanup(func() { appDB.Close() })
	assertNotSuperuserBypass(t, appDB)

	const tenant = "f6000000-0000-4000-8000-0000000035a0"
	library := fmt.Sprintf("lib-3540-race-%d", time.Now().UnixNano())
	cleanup := func() {
		_, _ = adminDB.Exec(`DELETE FROM tenant_library_installs WHERE tenant_id = $1`, tenant)
		_, _ = adminDB.Exec(`DELETE FROM workflow_defs WHERE tenant_id = $1`, tenant)
		_, _ = adminDB.Exec(`DELETE FROM library.bundle_members WHERE library = $1`, library)
		_, _ = adminDB.Exec(`DELETE FROM library.bundles WHERE library = $1`, library)
		_, _ = adminDB.Exec(`DELETE FROM library.members WHERE library = $1`, library)
	}
	cleanup()
	t.Cleanup(cleanup)

	// Two members, and the tenant is about to create its OWN definition of the second.
	good := libMember{name: "race-good", version: 1, exposure: "auth", variant: 1}
	clash := libMember{name: "race-clash", version: 1, exposure: "auth", variant: 2}
	for _, m := range []libMember{good, clash} {
		sum := sha256.Sum256(m.wasm())
		// task_queue and abi_version are NOT the column defaults, so an install that overwrote
		// the tenant's row with the member's metadata is visible below.
		if _, err := adminDB.Exec(`INSERT INTO library.members (library, name, version, wasm_bytes, wasm_sha256, wasm_length, abi_version, task_queue)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, library, m.name, m.version, m.wasm(), hex.EncodeToString(sum[:]), len(m.wasm()), m.abi(), m.taskQueue()); err != nil {
			t.Fatalf("publish member: %v", err)
		}
	}
	if _, err := adminDB.Exec(`INSERT INTO library.bundles (library, bundle_version, published_by) VALUES ($1, '1.0.0', 'test')`, library); err != nil {
		t.Fatal(err)
	}
	for _, m := range []libMember{good, clash} {
		if _, err := adminDB.Exec(`INSERT INTO library.bundle_members (library, bundle_version, name, version) VALUES ($1, '1.0.0', $2, $3)`, library, m.name, m.version); err != nil {
			t.Fatal(err)
		}
	}

	// The tenant's own definition, inserted and NOT committed.
	ownBytes := []byte("the tenant's own code")
	holder, err := adminDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			_ = holder.Rollback()
		}
	})
	if _, err := holder.Exec(`INSERT INTO workflow_defs (tenant_id, name, version, wasm_bytes, abi_version) VALUES ($1, $2, $3, $4, 1)`,
		tenant, clash.name, clash.version, ownBytes); err != nil {
		t.Fatalf("hold an uncommitted definition: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := NewPostgresStore(appDB).WithTenant(tenant).InstallLibraryBundle(context.Background(),
			InstallLibraryRequest{Library: library, BundleVersion: "1.0.0"})
		done <- err
	}()

	// Wait until the install is blocked on a lock, which is the window: it has finished its
	// collision check and is waiting to write. If it never blocks, the test did not reach
	// what it is for, and says so instead of passing.
	blocked := false
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		var n int
		if err := adminDB.QueryRow(`SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			blocked = true
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the install finished (%v) without ever waiting on the uncommitted row: the test did not reach the window", err)
		default:
		}
	}
	if !blocked {
		t.Fatal("UNMEASURED: the install was never seen waiting on a lock within 20s, so the window was not reached")
	}
	if err := holder.Commit(); err != nil {
		t.Fatalf("commit the tenant's definition: %v", err)
	}
	released = true

	var installErr error
	select {
	case installErr = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the install did not return after the blocking row was committed")
	}

	if !errors.Is(installErr, ErrLibraryDefinitionCollision) {
		// Errorf, not Fatalf: the state checks below say what the install did to the
		// tenant's rows, which is the damage this test exists to rule out.
		t.Errorf("install = %v, want an error wrapping ErrLibraryDefinitionCollision: the member whose key the tenant took was skipped, "+
			"and the install must not report success with a member missing", installErr)
	}
	var libCol, wasmPresent string
	if err := adminDB.QueryRow(`SELECT COALESCE(library, '<NULL>'), CASE WHEN wasm_bytes IS NULL THEN 'no' ELSE 'yes' END
		FROM workflow_defs WHERE tenant_id = $1 AND name = $2`, tenant, clash.name).Scan(&libCol, &wasmPresent); err != nil {
		t.Fatal(err)
	}
	if libCol != "<NULL>" || wasmPresent != "yes" {
		t.Errorf("the tenant's own definition is now library=%s with bytes=%s: an install turned it into a library row", libCol, wasmPresent)
	}
	// And its metadata is still its own: the member's task queue and ABI (seeded above)
	// differ from the column defaults the tenant's row was created with.
	var taskQueue string
	var abi int
	if err := adminDB.QueryRow(`SELECT task_queue, abi_version FROM workflow_defs WHERE tenant_id = $1 AND name = $2`, tenant, clash.name).Scan(&taskQueue, &abi); err != nil {
		t.Fatal(err)
	}
	if taskQueue != "default" || abi != 1 {
		t.Errorf("the tenant's own definition now has task_queue=%q abi_version=%d, the member's values: an install overwrote what the tenant deployed", taskQueue, abi)
	}
	var n int
	if err := adminDB.QueryRow(`SELECT count(*) FROM workflow_defs WHERE tenant_id = $1 AND name = $2`, tenant, good.name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the other member was installed although the install failed: the transaction did not roll back")
	}
	if err := adminDB.QueryRow(`SELECT count(*) FROM tenant_library_installs WHERE tenant_id = $1 AND library = $2`, tenant, library).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a failed install recorded itself")
	}
}
