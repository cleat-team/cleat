package main

// cleat#1226. `cleatctl deploy plugin` wrote three statements against
// plugin_registry, a table no migration has ever created, so the command could
// not work at all.
//
// WHY THE EXISTING TESTS PASSED, which is the reason this file exists rather
// than another assertion in cleatctl_command_test.go. Every deployPlugin test
// drives a FAKE DRIVER (mockPluginConnector) that returns a canned result for
// any query, so it accepts SQL no database would -- the same failure mode
// plugins/*_dialect_arms_multidb_test.go was built for, and the reason
// TestEveryInlineStatementParsesOnPostgres (cleat#1217) exists at all. Those
// tests reported "Deployed plugin" for a statement naming a table that does
// not exist, and they still have their place: they cover argument handling,
// refusals and output. What they cannot do is notice that the write went
// nowhere.
//
// So this asserts the ROUND TRIP: deploy, then resolve. "The INSERT returned
// no error" was true of the old code's intent and false of its effect; "the
// plugin the resolver hands back is the one I deployed" is the property a
// caller actually depends on.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

func TestDeployPluginIsResolvableAfterwards(t *testing.T) {
	db := testutil.SuiteTestDB(t, "cleatctl")
	ctx := context.Background()

	name := fmt.Sprintf("cleatctl-roundtrip-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM plugin_defs WHERE name = $1`, name)
	})

	dir := t.TempDir()
	v1Bytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	v1Path := writeWASM(t, dir, v1Bytes)

	stdout, stderr := captureOutputs(t, func() {
		deployPlugin(ctx, db, []string{name, "1.0.0", v1Path})
	})
	if stderr != "" {
		t.Fatalf("deploy reported an error: %s", stderr)
	}
	if want := "Deployed plugin " + name + " v1.0.0"; !contains(stdout, want) {
		t.Fatalf("stdout %q does not contain %q", stdout, want)
	}

	loader := engine.NewPluginLoader(db, nil)

	// THE ASSERTION THE OLD CODE COULD NOT HAVE SATISFIED. Before cleat#1226
	// the row was written to a table that does not exist, so there was nothing
	// here to resolve -- and nothing said so.
	gotVersion, def, err := loader.ResolvePlugin(ctx, name, "")
	if err != nil {
		t.Fatalf("the plugin just deployed cannot be resolved: %v.\n\n"+
			"A deploy that reports success and leaves nothing a resolver can find is the "+
			"defect cleat#1226 is about, not a variation on it.", err)
	}
	if gotVersion != "1.0.0" {
		t.Errorf("resolved version %q, want 1.0.0", gotVersion)
	}
	if len(def.WASMBytes) != len(v1Bytes) {
		t.Errorf("resolved %d WASM bytes, want %d", len(def.WASMBytes), len(v1Bytes))
	}

	// A SECOND VERSION ADDS A ROW, it does not replace the first. This is the
	// whole of the decision recorded on cleat#1226: plugin_defs is keyed
	// (name, version), and the command it replaces had one row per NAME.
	v2Bytes := append(append([]byte{}, v1Bytes...), 0x0b)
	v2Path := writeWASM(t, dir, v2Bytes)
	_, stderr = captureOutputs(t, func() {
		deployPlugin(ctx, db, []string{name, "2.0.0", v2Path})
	})
	if stderr != "" {
		t.Fatalf("deploying a second version reported an error: %s", stderr)
	}

	var rows int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM plugin_defs WHERE name = $1`, name).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 2 {
		t.Errorf("after deploying 1.0.0 and 2.0.0 there are %d rows for %s, want 2 -- a new "+
			"version must not overwrite the old one", rows, name)
	}

	gotVersion, def, err = loader.ResolvePlugin(ctx, name, "")
	if err != nil {
		t.Fatalf("resolve after the second deploy: %v", err)
	}
	if gotVersion != "2.0.0" {
		t.Errorf("an unconstrained resolve returned %q, want 2.0.0 (the highest)", gotVersion)
	}
	if len(def.WASMBytes) != len(v2Bytes) {
		t.Errorf("resolved %d WASM bytes, want %d (version 2.0.0's)", len(def.WASMBytes), len(v2Bytes))
	}

	// The OLD version is still resolvable by constraint. A test that only ever
	// asks for the latest cannot tell "both rows exist" from "the second
	// overwrote the first and happens to be the one I wanted".
	//
	// THE EXACT FORM, which is the stronger assertion: "^1.0.0" would be
	// satisfied by anything in 1.x, and what this needs to show is that version
	// 1.0.0 in particular is still there.
	//
	// It carried "^1.0.0" when this test was written, because an exact
	// constraint matched NOTHING -- parseConstraint encoded both exact forms as
	// {Min: v, Max: v} and versionInRange excluded the upper bound, so the one
	// version such a constraint named was the one version it could not return
	// (cleat#1243, fixed in cleat#1253 with an explicit Exact field). Switched
	// back now that it works: a comment saying "this form is broken" outlives
	// the breakage and tells the next reader to avoid something that is fine.
	oldVersion, oldDef, err := loader.ResolvePlugin(ctx, name, "1.0.0")
	if err != nil {
		t.Fatalf("resolve 1.0.0 after 2.0.0 was deployed: %v", err)
	}
	if oldVersion != "1.0.0" || len(oldDef.WASMBytes) != len(v1Bytes) {
		t.Errorf("constrained resolve returned %q with %d bytes, want 1.0.0 with %d",
			oldVersion, len(oldDef.WASMBytes), len(v1Bytes))
	}

	// REDEPLOYING a version replaces its bytes rather than adding a row.
	v1Prime := append(append([]byte{}, v1Bytes...), 0x0c, 0x0d)
	v1PrimePath := writeWASM(t, dir, v1Prime)
	_, stderr = captureOutputs(t, func() {
		deployPlugin(ctx, db, []string{name, "1.0.0", v1PrimePath})
	})
	if stderr != "" {
		t.Fatalf("redeploying 1.0.0 reported an error: %s", stderr)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM plugin_defs WHERE name = $1`, name).Scan(&rows); err != nil {
		t.Fatalf("count rows after redeploy: %v", err)
	}
	if rows != 2 {
		t.Errorf("redeploying 1.0.0 left %d rows for %s, want 2 -- an upsert on (name, version) "+
			"must replace rather than accumulate", rows, name)
	}
	_, oldDef, err = loader.ResolvePlugin(ctx, name, "1.0.0")
	if err != nil {
		t.Fatalf("resolve 1.0.0 after redeploy: %v", err)
	}
	if len(oldDef.WASMBytes) != len(v1Prime) {
		t.Errorf("1.0.0 still has %d WASM bytes after a redeploy, want %d -- the redeploy did "+
			"not replace the binary", len(oldDef.WASMBytes), len(v1Prime))
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && stringsIndex(haystack, needle) >= 0
}

func stringsIndex(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
