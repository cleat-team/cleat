package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#2947: `cleatctl deploy workflow` has a dedup guard that skips a redeploy
// of the binary it already holds. It compared against WorkflowDef.WASMBytes, and
// no dialect's ListWorkflowDefs selects wasm_bytes, so the field was always
// empty and the branch never ran -- against any store, on any dialect. Two tests
// covered it and both passed, because the fake returned the field no real store
// supplies.
//
// The tests here are the ones that would have caught it, and between them they
// pin the two things that make the wired-up guard correct:
//
//   - it must NORMALISE cleat.metadata's workflow_version before comparing, or
//     an identical artifact cannot match its own stored copy; and
//   - it must compare against a REAL store, because that is exactly the
//     property the old tests lacked.

// dedupTestDB is a migrated Postgres database for these tests, cleaned the same
// way dropTenantTestDB cleans its own. SuiteTestDB applies the shipped
// migrations on first use.
func dedupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := testutil.SuiteTestDB(t, "cleatctl")
	testutil.CleanupPostgresTestData(t, db)
	t.Cleanup(func() { testutil.CleanupPostgresTestData(t, db) })
	return db
}

// stampedArtifact is what `cleat build --version <version>` leaves on disk: a
// core module whose cleat.metadata carries that version, plus a body marker so
// two artifacts can share a stamp and still differ.
func stampedArtifact(t *testing.T, name string, version int, bodyMarker string) []byte {
	t.Helper()
	return artifactWithMetadata(t, &wasm.Metadata{
		WorkflowName:         name,
		WorkflowVersion:      version,
		ABIVersion:           wasm.CurrentABIVersion,
		MinCompatibleVersion: wasm.CurrentABIVersion,
		Language:             "go",
	}, bodyMarker)
}

// TestDeployWorkflow_DedupIgnoresTheWorkflowVersionStamp.
//
// The stamp is not part of the artifact's identity: two binaries differing only
// in cleat.metadata's workflow_version are the same workflow binary.
//
// This is what makes the normalisation load-bearing rather than decorative. The
// stored row carries the version the previous deploy ASSIGNED, and the file on
// disk carries whatever `cleat build --version` wrote. Compared as stored, an
// identical artifact never matches its own copy -- so the guard would not merely
// fail to fire, it would be WRONG, and would look wired while doing nothing.
func TestDeployWorkflow_DedupIgnoresTheWorkflowVersionStamp(t *testing.T) {
	dir := t.TempDir()

	// Same name, same ABI, same body -- only the stamp differs. 1 is what the
	// previous deploy assigned; 7 is what the build wrote.
	storedV1 := stampedArtifact(t, "provision", 1, "body")
	incoming := stampedArtifact(t, "provision", 7, "body")
	path := writeWASM(t, dir, incoming)

	store := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, name string) ([]engine.WorkflowDef, error) {
			return []engine.WorkflowDef{
				{Name: name, Version: 1, CreatedAt: time.Now().Add(-24 * time.Hour)},
			}, nil
		},
		loadWASMFn: func(_ context.Context, _ string, _ int) ([]byte, error) {
			return storedV1, nil
		},
		deployWorkflowDefFn: func(_ context.Context, def *engine.WorkflowDef) error {
			t.Error("DeployWorkflowDef called for a binary differing from the stored one only in its workflow_version stamp.\n\n" +
				"The stored bytes carry the version the deploy ASSIGNED; the file carries the one the build wrote. " +
				"Normalise the stamp out of both sides before comparing. cleat#2947.")
			return nil
		},
	}

	stdout := captureStdout(t, func() {
		deployWorkflow(context.Background(), store, nil, []string{"provision", path})
	})
	if !strings.Contains(stdout, "WASM unchanged") {
		t.Errorf("expected the deploy to be skipped as unchanged, got: %s", stdout)
	}
}

// TestDeployWorkflow_SameArtifactTwiceSkipsAgainstARealStore is cleat#2947's
// acceptance, driven against a real store rather than a fake.
//
// Insisting on a real store is the whole point: the old guard's failure was
// invisible precisely BECAUSE a fake supplied WorkflowDef.WASMBytes, which no
// dialect's ListWorkflowDefs selects. A test that stubbed the bytes again would
// reproduce that blindness exactly.
func TestDeployWorkflow_SameArtifactTwiceSkipsAgainstARealStore(t *testing.T) {
	db := dedupTestDB(t)
	ctx := context.Background()
	store := engine.NewPostgresStore(db)

	const name = "cleatctl-dedup-2947"
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM workflow_defs WHERE name = $1`, name)
	})

	dir := t.TempDir()

	// The file declares version 7 while the deploy assigns it v1, so the stored
	// row's stamp and the file's stamp DISAGREE. That is what makes the
	// normalisation load-bearing here rather than incidental: on a first
	// deploy-and-redeploy where both stamps happen to be 1, the comparison would
	// pass with or without it.
	path := writeWASM(t, dir, stampedArtifact(t, name, 7, "body-a"))

	stdout, stderr := withExitPanicOutput(t, func() {
		deployWorkflow(ctx, store, db, []string{name, path})
	})
	if !strings.Contains(stdout, "Deployed "+name+" v1") {
		t.Fatalf("the first deploy did not create v1.\nstdout: %s\nstderr: %s", stdout, stderr)
	}

	// The precondition, asserted rather than assumed. If the first deploy wrote
	// nothing, the second would "skip" for the wrong reason -- there would be no
	// latest version to compare against -- and this test would report a working
	// guard on an empty table.
	var stored int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM workflow_defs WHERE name = $1`, name).Scan(&stored); err != nil {
		t.Fatalf("counting definitions after the first deploy: %v", err)
	}
	if stored != 1 {
		t.Fatalf("expected 1 stored definition after the first deploy, got %d -- "+
			"the test did not set up the state it goes on to assert about", stored)
	}

	stdout, stderr = withExitPanicOutput(t, func() {
		deployWorkflow(ctx, store, db, []string{name, path})
	})
	if !strings.Contains(stdout, "WASM unchanged") {
		t.Errorf("deploying the same artifact twice against a real store did not skip.\n\n"+
			"The stored binary carries the version this command assigned (v1) and the file carries the "+
			"one the build wrote (7), so a comparison that does not normalise the stamp out cannot see the "+
			"match. This is cleat#2947.\nstdout: %s\nstderr: %s", stdout, stderr)
	}

	var afterSecond int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM workflow_defs WHERE name = $1`, name).Scan(&afterSecond); err != nil {
		t.Fatalf("counting definitions after the second deploy: %v", err)
	}
	if afterSecond != 1 {
		t.Errorf("the second deploy added a version anyway: %d rows, want 1", afterSecond)
	}

	// The converse clause, so the guard is not simply refusing everything: a
	// DIFFERENT artifact still creates a new version.
	pathB := writeWASM(t, dir, stampedArtifact(t, name, 7, "body-b"))
	stdout, stderr = withExitPanicOutput(t, func() {
		deployWorkflow(ctx, store, db, []string{name, pathB})
	})
	if !strings.Contains(stdout, "Deployed "+name+" v2") {
		t.Errorf("a DIFFERENT artifact did not create a new version.\nstdout: %s\nstderr: %s", stdout, stderr)
	}

	var afterThird int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM workflow_defs WHERE name = $1`, name).Scan(&afterThird); err != nil {
		t.Fatalf("counting definitions after the third deploy: %v", err)
	}
	if afterThird != 2 {
		t.Errorf("expected 2 versions after deploying a different artifact, got %d", afterThird)
	}
}
