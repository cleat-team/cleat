package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#3597: `cleatctl library publish` and `library list`.

// stampedMember writes a stamped binary named by its own cleat.metadata to dir and returns its path.
func stampedMember(t *testing.T, dir, name string, version int, children map[string]int, body string) string {
	t.Helper()
	// body varies the stamp, not the module: bytes appended after the header would be a corrupt
	// section, and the stamp is part of the bytes a member is digested over.
	b, err := wasm.WriteMetadata([]byte("\x00asm\x01\x00\x00\x00"), &wasm.Metadata{
		Language: body, WorkflowName: name, WorkflowVersion: version, ABIVersion: 1, MinCompatibleVersion: 1,
		PluginDeps: map[string]string{"slack-notify": ">=1"}, ChildVersions: children,
		EntryPoints: []string{"handle_" + name}, Exposure: "internal",
	})
	if err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%d.wasm", name, version))
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestALibraryMemberIsNamedByItsOwnStamp(t *testing.T) {
	dir := t.TempDir()
	path := stampedMember(t, dir, "checkout", 3, map[string]int{"pay": 2}, "x")
	if err := os.WriteFile(path+".schema.json", []byte(`{"handle_checkout":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := libraryMemberFromFile(path)
	if err != nil {
		t.Fatalf("libraryMemberFromFile: %v", err)
	}
	if m.Name != "checkout" || m.Version != 3 || m.ABIVersion != 1 || m.MinVersion != 1 || m.Exposure != "internal" ||
		m.PluginDeps["slack-notify"] != ">=1" || m.ChildVersions["pay"] != 2 || len(m.EntryPoints) != 1 || len(m.EntryPointSchemas) != 1 || len(m.WasmBytes) == 0 {
		t.Fatalf("the member does not carry what its stamp says: %+v", m)
	}
}

func TestALibraryMemberWithoutAUsableStampIsRefused(t *testing.T) {
	dir := t.TempDir()
	bare := filepath.Join(dir, "bare.wasm")
	if err := os.WriteFile(bare, []byte("\x00asm\x01\x00\x00\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.wasm")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	unnamed, err := wasm.WriteMetadata([]byte("\x00asm\x01\x00\x00\x00"), &wasm.Metadata{WorkflowVersion: 1, ABIVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	unnamedPath := filepath.Join(dir, "unnamed.wasm")
	if err := os.WriteFile(unnamedPath, unnamed, 0o644); err != nil {
		t.Fatal(err)
	}
	badSidecar := stampedMember(t, dir, "sidecar", 1, nil, "s")
	if err := os.WriteFile(badSidecar+".schema.json", []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ path, want string }{
		"no stamp":                   {bare, "no readable cleat.metadata stamp"},
		"an empty file":              {empty, "is empty"},
		"a stamp with no name":       {unnamedPath, "no workflow_name"},
		"a missing file":             {filepath.Join(dir, "nope.wasm"), "reading"},
		"a sidecar that is not JSON": {badSidecar, "not valid JSON"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := libraryMemberFromFile(tc.path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestLibraryCommandsRefuseTheWrongArguments(t *testing.T) {
	for name, args := range map[string][]string{
		"no subcommand":           {},
		"an unknown subcommand":   {"delete"},
		"publish with no members": {"publish", "acme", "1"},
		"list with two libraries": {"list", "a", "b"},
	} {
		t.Run(name, func(t *testing.T) {
			stderr := withExitPanic(t, func() { runLibrary(context.Background(), nil, engine.DialectPostgres, args) })
			if !strings.Contains(stderr, "sage") && !strings.Contains(stderr, "unknown library command") {
				t.Errorf("no usage or refusal on stderr: %q", stderr)
			}
		})
	}
}

func TestPublishingThroughThePublisherLoginAndListing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tdialect testutil.Dialect
		edialect engine.Dialect
	}{
		{"postgres", testutil.DialectPostgres, engine.DialectPostgres},
		{"mssql", testutil.DialectMSSQL, engine.DialectMSSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := testutil.TestDB(t, tc.tdialect)
			t.Cleanup(func() { owner.Close() })
			testutil.SetupFullSchema(t, owner, tc.tdialect)
			pub := testutil.OpenLibraryLoginDB(t, owner, tc.tdialect, true)
			lib := fmt.Sprintf("cli-%d", time.Now().UnixNano()%1_000_000_000_000)
			cleanupCLILibrary(t, owner, tc.edialect, lib)
			dir := t.TempDir()
			// D59 (cleat#3629): members are named "<library>.<name>".
			parent := stampedMember(t, dir, lib+".parent", 1, map[string]int{lib + ".child": 2}, "p")
			child := stampedMember(t, dir, lib+".child", 2, nil, "c")
			ctx := context.Background()

			out := captureStdout(t, func() {
				libraryPublish(ctx, pub, tc.edialect, []string{lib, "1.0.0", parent, child, "--published-by", "ci"})
			})
			if !strings.Contains(out, "2 member(s) written, 0 already published") {
				t.Fatalf("publish output: %q", out)
			}
			out = captureStdout(t, func() { libraryPublish(ctx, pub, tc.edialect, []string{lib, "1.0.0", parent, child}) })
			if !strings.Contains(out, "already published with exactly these 2 member(s); nothing written") {
				t.Fatalf("republish output: %q", out)
			}

			var listed []engine.LibraryBundleSummary
			js := captureStdout(t, func() { libraryList(ctx, pub, tc.edialect, []string{lib, "--json"}) })
			if err := json.Unmarshal([]byte(js), &listed); err != nil || len(listed) != 1 || len(listed[0].Members) != 2 || listed[0].PublishedBy != "ci" {
				t.Fatalf("list --json = %q (%v, %+v), want one bundle of two members published by ci", js, err, listed)
			}
			table := captureStdout(t, func() { libraryList(ctx, pub, tc.edialect, []string{lib}) })
			if !strings.Contains(table, lib+".child@2, "+lib+".parent@1") || !strings.Contains(table, "1.0.0") {
				t.Fatalf("list table: %q", table)
			}
			if empty := captureStdout(t, func() { libraryList(ctx, pub, tc.edialect, []string{"nothing-here"}) }); !strings.Contains(empty, "no published bundles") {
				t.Fatalf("listing an unknown library: %q", empty)
			}

			// A pin outside the bundle is refused before anything is written.
			orphan := stampedMember(t, dir, lib+".orphan", 1, map[string]int{lib + ".ghost": 1}, "o")
			stderr := withExitPanic(t, func() { libraryPublish(ctx, pub, tc.edialect, []string{lib, "2.0.0", orphan}) })
			if !strings.Contains(stderr, `no member named "`+lib+`.ghost"`) {
				t.Fatalf("stderr = %q, want the pin refused", stderr)
			}
			var n int
			if err := owner.QueryRow(`SELECT count(*) FROM library.bundles WHERE library = `+cliPlaceholder(tc.edialect), lib).Scan(&n); err != nil || n != 1 {
				t.Fatalf("a refused publish left %d bundle(s) for the library (%v), want only 1.0.0", n, err)
			}
		})
	}
}

// Run the REAL binary with only the publisher login. The control is a command that reads the
// application tables, which must fail with the same DSN, so a pass here is not a binary that
// ignores its arguments or a login that can do more than the library schema.
func TestTheBuiltCleatctlPublishesWithOnlyThePublisherRole(t *testing.T) {
	root := cleatctlRepoRoot(t)
	bin := filepath.Join(t.TempDir(), "cleatctl")
	build := exec.Command("go", "build", "-o", bin, filepath.Join(root, "cmd", "cleatctl"))
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building cleatctl: %v\n%s", err, out)
	}
	for _, tc := range []struct {
		name     string
		tdialect testutil.Dialect
		edialect engine.Dialect
	}{
		{"postgres", testutil.DialectPostgres, engine.DialectPostgres},
		{"mssql", testutil.DialectMSSQL, engine.DialectMSSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := testutil.TestDB(t, tc.tdialect)
			t.Cleanup(func() { owner.Close() })
			testutil.SetupFullSchema(t, owner, tc.tdialect)
			_, dsn := testutil.LibraryLoginDSN(t, owner, tc.tdialect, true)
			lib := fmt.Sprintf("bin-%d", time.Now().UnixNano()%1_000_000_000_000)
			cleanupCLILibrary(t, owner, tc.edialect, lib)
			member := stampedMember(t, t.TempDir(), lib+".solo", 1, nil, "s")

			run := func(args ...string) (int, string) {
				cmd := exec.Command(bin, append([]string{"--db", dsn}, args...)...)
				cmd.Dir = root
				out, err := cmd.CombinedOutput()
				if err != nil && cmd.ProcessState == nil {
					t.Fatalf("cleatctl never ran: %v\n%s", err, out)
				}
				return cmd.ProcessState.ExitCode(), string(out)
			}

			if code, out := run("versions", "list"); code == 0 {
				t.Fatalf("the CONTROL passed: `versions list` opened the application store as the publisher login (exit 0):\n%s", out)
			}
			// The warning every other command prints for a role row-level security applies to is
			// about the OPERATOR's questions ("this tool can see only one tenant's rows"); it is
			// wrong for the publisher, who asks none, so `library` must be dispatched before it.
			const rlsWarning = "row-level security applies to"
			if code, out := run("library", "publish", lib, "1.0.0", member); code != 0 || !strings.Contains(out, "1 member(s) written") || strings.Contains(out, rlsWarning) {
				t.Fatalf("library publish as the publisher login: exit %d (the tenant-scoped warning must not appear)\n%s", code, out)
			}
			if code, out := run("library", "list", lib); code != 0 || !strings.Contains(out, "solo@1") || strings.Contains(out, rlsWarning) {
				t.Fatalf("library list as the publisher login: exit %d (the tenant-scoped warning must not appear)\n%s", code, out)
			}
		})
	}
}

func cleanupCLILibrary(t *testing.T, owner *sql.DB, d engine.Dialect, lib string) {
	t.Helper()
	t.Cleanup(func() {
		for _, table := range []string{"library.bundle_members", "library.bundles", "library.members"} {
			_, _ = owner.Exec(`DELETE FROM `+table+` WHERE library = `+cliPlaceholder(d), lib)
		}
	})
}

// cliPlaceholder is the first bind parameter in the dialect's own syntax.
func cliPlaceholder(d engine.Dialect) string {
	if d == engine.DialectMSSQL {
		return "@p1"
	}
	return "$1"
}
