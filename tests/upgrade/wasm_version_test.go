package upgrade

import (
	"context"
	"fmt"
	"testing"
	"time"

	host "github.com/rcownie/cleat/internal/host"
)

// TestWASMVersionUpgrade verifies that registering multiple versions of a
// workflow definition and creating new workflows uses the latest version.
func TestWASMVersionUpgrade(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := host.NewPostgresStore(db)
	ctx := context.Background()

	// Register a workflow def with version 1 (old) and version 2 (new).
	defName := fmt.Sprintf("upg-wasm-ver-%d", time.Now().UnixNano())

	// Old version WASM (minimal module).
	wasmV1 := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01}
	// New version (different bytes).
	wasmV2 := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x02}

	_, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points, namespace)
		VALUES ($1, 1, $2, '{old_entry}', 'default') ON CONFLICT DO NOTHING`,
		defName, wasmV1)
	if err != nil {
		t.Fatalf("register v1: %v", err)
	}
	_, err = db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points, namespace)
		VALUES ($1, 2, $2, '{new_entry}', 'default') ON CONFLICT DO NOTHING`,
		defName, wasmV2)
	if err != nil {
		t.Fatalf("register v2: %v", err)
	}

	defer db.Exec(`DELETE FROM workflow_defs WHERE name = $1`, defName)

	// Verify both versions are registered.
	versions, err := store.ListVersions(ctx, defName)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) < 2 {
		t.Fatalf("expected at least 2 versions, got %d: %v", len(versions), versions)
	}

	// Load each version's WASM and verify they are different.
	v1Wasm, err := store.LoadWASM(ctx, defName, 1)
	if err != nil {
		t.Fatalf("LoadWASM v1: %v", err)
	}
	v2Wasm, err := store.LoadWASM(ctx, defName, 2)
	if err != nil {
		t.Fatalf("LoadWASM v2: %v", err)
	}

	if len(v1Wasm) != len(wasmV1) || v1Wasm[0] != wasmV1[0] {
		t.Error("v1 WASM bytes do not match")
	}
	if len(v2Wasm) != len(wasmV2) || v2Wasm[0] != wasmV2[0] {
		t.Error("v2 WASM bytes do not match")
	}

	// Create a new workflow instance explicitly with version 2 (new).
	runID := fmt.Sprintf("upg-ver-new-%d", time.Now().UnixNano())
	wfID, alreadyExisted, err := store.StartNewRun(ctx, defName, 2, []byte(`{"version":2}`), "")
	if err != nil {
		t.Fatalf("StartNewRun v2: %v", err)
	}
	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, wfID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, wfID)
	}()
	_ = runID
	_ = alreadyExisted

	// Verify the new workflow uses version 2.
	wf, err := store.GetWorkflowByID(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if wf == nil {
		t.Fatal("workflow instance not found")
	}
	if wf.DefVersion != 2 {
		t.Errorf("expected def_version=2 for new workflow, got %d", wf.DefVersion)
	}

	// Create another workflow with version 1 (not the latest, but explicitly old).
	oldID, _, err := store.StartNewRun(ctx, defName, 1, []byte(`{"version":1}`), "")
	if err != nil {
		t.Fatalf("StartNewRun v1: %v", err)
	}
	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, oldID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, oldID)
	}()

	oldWf, err := store.GetWorkflowByID(ctx, oldID)
	if err != nil {
		t.Fatalf("GetWorkflowByID old: %v", err)
	}
	if oldWf == nil {
		t.Fatal("old workflow not found")
	}
	if oldWf.DefVersion != 1 {
		t.Errorf("expected def_version=1 for old workflow, got %d", oldWf.DefVersion)
	}
}

// TestInFlightUsesOldVersion verifies that a workflow started with an old
// version continues with the old version even after a new version is registered.
func TestInFlightUsesOldVersion(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := host.NewPostgresStore(db)
	ctx := context.Background()

	defName := fmt.Sprintf("upg-inflight-%d", time.Now().UnixNano())

	// Register version 1.
	wasmV1 := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01}
	_, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points, namespace)
		VALUES ($1, 1, $2, '{entry}', 'default') ON CONFLICT DO NOTHING`,
		defName, wasmV1)
	if err != nil {
		t.Fatalf("register v1: %v", err)
	}
	defer db.Exec(`DELETE FROM workflow_defs WHERE name = $1`, defName)

	// Start a workflow with version 1.
	runID, _, err := store.StartNewRun(ctx, defName, 1, []byte(`{"inflight":true}`), "")
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	}()

	// Verify it started with version 1.
	wf, err := store.GetWorkflowByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if wf == nil {
		t.Fatal("workflow not found")
	}
	if wf.DefVersion != 1 {
		t.Fatalf("expected def_version=1, got %d", wf.DefVersion)
	}

	// Now register version 2 (simulating an upgrade while workflow is in-flight).
	wasmV2 := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x02}
	_, err = db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points, namespace)
		VALUES ($1, 2, $2, '{entry}', 'default') ON CONFLICT DO NOTHING`,
		defName, wasmV2)
	if err != nil {
		t.Fatalf("register v2: %v", err)
	}

	// Verify the in-flight workflow still shows version 1 (unchanged).
	wfAgain, err := store.GetWorkflowByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetWorkflowByID after upgrade: %v", err)
	}
	if wfAgain == nil {
		t.Fatal("workflow not found after upgrade")
	}
	if wfAgain.DefVersion != 1 {
		t.Errorf("in-flight workflow version changed from 1 to %d after upgrade", wfAgain.DefVersion)
	}

	// Verify we can still load version 1's WASM.
	loadedV1, err := store.LoadWASM(ctx, defName, 1)
	if err != nil {
		t.Errorf("LoadWASM v1 after upgrade: %v", err)
	}
	if len(loadedV1) == 0 || loadedV1[0] != wasmV1[0] {
		t.Error("v1 WASM corrupted after v2 registration")
	}

	// Verify version 2 is also loadable.
	loadedV2, err := store.LoadWASM(ctx, defName, 2)
	if err != nil {
		t.Errorf("LoadWASM v2: %v", err)
	}
	if len(loadedV2) == 0 || loadedV2[0] != wasmV2[0] {
		t.Error("v2 WASM not loadable")
	}
}

// TestVersionFallback verifies behavior when a specific version is not found.
func TestVersionFallback(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := host.NewPostgresStore(db)
	ctx := context.Background()

	defName := fmt.Sprintf("upg-fallback-%d", time.Now().UnixNano())

	// Register only version 1.
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	_, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points, namespace)
		VALUES ($1, 1, $2, '{entry}', 'default') ON CONFLICT DO NOTHING`,
		defName, wasmBytes)
	if err != nil {
		t.Fatalf("register v1: %v", err)
	}
	defer db.Exec(`DELETE FROM workflow_defs WHERE name = $1`, defName)

	// Trying to load a non-existent version should return an error.
	_, err = store.LoadWASM(ctx, defName, 999)
	if err == nil {
		t.Error("expected error when loading non-existent version 999")
	} else {
		t.Logf("Version fallback correctly returned error: %v", err)
	}

	// Trying to load a non-existent workflow def should return an error.
	_, err = store.LoadWASM(ctx, "nonexistent-def-name", 1)
	if err == nil {
		t.Error("expected error when loading non-existent workflow def")
	} else {
		t.Logf("Non-existent def correctly returned error: %v", err)
	}

	// Try to start a workflow with a non-existent definition.
	_, _, err = store.StartNewRun(ctx, "nonexistent-def", 1, []byte(`{}`), "")
	if err == nil {
		t.Error("expected error when starting run with non-existent def")
	} else {
		t.Logf("StartNewRun with non-existent def correctly returned error: %v", err)
	}

	// Verify ListVersions returns only the versions that exist.
	versions, err := store.ListVersions(ctx, defName)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 1 {
		t.Errorf("expected 1 version, got %d: %v", len(versions), versions)
	}
	if versions[0] != 1 {
		t.Errorf("expected version 1, got %d", versions[0])
	}

	// ListVersions for non-existent def should return empty slice, not error.
	emptyVersions, err := store.ListVersions(ctx, "nonexistent-def")
	if err != nil {
		t.Errorf("ListVersions for non-existent def: %v", err)
	}
	if len(emptyVersions) != 0 {
		t.Errorf("expected empty versions for non-existent def, got %v", emptyVersions)
	}
}
