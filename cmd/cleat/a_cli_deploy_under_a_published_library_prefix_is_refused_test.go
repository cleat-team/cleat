package main

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestCLIDeployUnderAPublishedLibraryPrefixIsRefused is the wiring test for cleat#3629 (owner
// decision D59) on the one deploy path that does not go through a store: `cleat deploy --db` opens
// its own transaction and calls engine.CheckWorkflowDefReplacementPostgres directly. The refusal
// itself is tested on both dialects in engine/; this proves the command reaches it and exits
// non-zero with nothing written, and that a name outside the prefix still deploys.
func TestCLIDeployUnderAPublishedLibraryPrefixIsRefused(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	lib := fmt.Sprintf("clipfx_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := db.Exec(`INSERT INTO library.bundles (library, bundle_version, published_by) VALUES ($1, '1.0.0', 'test')`, lib); err != nil {
		t.Fatalf("publishing the library: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_defs WHERE name LIKE $1`, "clipfx%")
		_, _ = db.Exec(`DELETE FROM library.bundles WHERE library = $1`, lib)
	})

	deploy := func(name string) (string, error) {
		wasmPath := writeFakeWasm(t, t.TempDir(), "wf.wasm")
		out, err := exec.Command(cleatBinary, "deploy", "--db", testutil.PostgresTestDSN(), "--name", name, wasmPath).CombinedOutput()
		return string(out), err
	}
	rows := func(name string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM workflow_defs WHERE name = $1`, name).Scan(&n); err != nil {
			t.Fatalf("UNMEASURED: counting rows for %q: %v", name, err)
		}
		return n
	}

	under := lib + ".checkout"
	out, err := deploy(under)
	if err == nil {
		t.Fatalf("cleat deploy of %q exited 0, want a refusal: %s", under, out)
	}
	for _, want := range []string{under, lib, "D59"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal %q does not mention %q", out, want)
		}
	}
	if n := rows(under); n != 0 {
		t.Errorf("the refused deploy wrote %d row(s) for %q, want 0", n, under)
	}

	// Control: a name outside the prefix deploys, so the refusal above is the prefix and not the rig.
	other := "clipfx-unrelated-" + lib
	if out, err := deploy(other); err != nil {
		t.Fatalf("cleat deploy of %q, which is not under the prefix, failed: %v\n%s", other, err, out)
	}
	if n := rows(other); n != 1 {
		t.Errorf("the control deploy wrote %d row(s) for %q, want 1", n, other)
	}
}
