package engine

// cleat#3540, PR 3a: WorkflowLoader.ListVersions returns WorkflowDef values whose
// WASMBytes is "the code the version runs, wherever it is stored" (PR 2). A version
// installed from a library holds no bytes of its own, so a listing that scanned the
// column alone returned an empty slice, which reads as "no code" rather than "code
// held in the member". Nothing in production calls it today (NewWorkflowLoader has no
// non-test caller), which is why it was found by reading and not by a failure.
//
// The control is an ordinary version of the same name in the same listing: a fix that
// followed every row's library column, or none, would break one of the two.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestAListedInstalledVersionCarriesItsMembersBytes(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { adminDB.Close() })
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)

	const tenant = "f3000000-0000-4000-8000-0000000035a0"
	stamp := time.Now().UnixNano()
	library := fmt.Sprintf("lib-3540-list-%d", stamp)
	name := fmt.Sprintf("listed-def-%d", stamp)
	cleanup := func() {
		_, _ = adminDB.Exec(`DELETE FROM workflow_defs WHERE tenant_id = $1`, tenant)
		_, _ = adminDB.Exec(`DELETE FROM library.members WHERE library = $1`, library)
	}
	cleanup()
	t.Cleanup(cleanup)

	memberBytes := make([]byte, 200)
	for i := range memberBytes {
		memberBytes[i] = byte(i*3 + 1)
	}
	ownBytes := []byte("an ordinary version's own bytes")
	sum := sha256.Sum256(memberBytes)
	if _, err := adminDB.Exec(`INSERT INTO library.members (library, name, version, wasm_bytes, wasm_sha256, wasm_length, abi_version)
		VALUES ($1, $2, 1, $3, $4, $5, 1)`, library, name, memberBytes, hex.EncodeToString(sum[:]), len(memberBytes)); err != nil {
		t.Fatalf("publish member: %v", err)
	}
	// Version 1 is installed from the library (a reference, no bytes); version 2 is ordinary.
	if _, err := adminDB.Exec(`INSERT INTO workflow_defs (tenant_id, name, version, library, abi_version) VALUES ($1, $2, 1, $3, 1)`, tenant, name, library); err != nil {
		t.Fatalf("install reference: %v", err)
	}
	if _, err := adminDB.Exec(`INSERT INTO workflow_defs (tenant_id, name, version, wasm_bytes, abi_version) VALUES ($1, $2, 2, $3, 1)`, tenant, name, ownBytes); err != nil {
		t.Fatalf("seed ordinary version: %v", err)
	}

	defs, err := NewWorkflowLoader(adminDB, nil, nil).ListVersions(context.Background(), name)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("ListVersions returned %d versions, want 2", len(defs))
	}
	byVersion := map[int][]byte{}
	for _, d := range defs {
		byVersion[d.Version] = d.WASMBytes
	}
	if string(byVersion[1]) != string(memberBytes) {
		t.Errorf("the installed version's WASMBytes is %d bytes, want the member's %d. A listing that scans the column alone returns nothing for it",
			len(byVersion[1]), len(memberBytes))
	}
	if string(byVersion[2]) != string(ownBytes) {
		t.Errorf("the ordinary version's WASMBytes = %q, want its own", byVersion[2])
	}
}
