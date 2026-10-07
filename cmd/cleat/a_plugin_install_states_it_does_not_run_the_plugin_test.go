package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#2518. `cleat plugin install` reported bare success, and
// `cleat plugin list`'s STATUS column showed only active/deprecated --
// neither said that no cleat-worker today loads an installed WASM plugin
// (engine/engine.go:271-272, engine/engine_option_reachability_test.go:101,
// IMPROVEMENT-PLAN.d/3.315: PluginLoader.LoadPlugin has no non-test callers).
// A user who completed the documented install-and-run path got no signal
// that the host function they wrote was unreachable.
//
// Both tests below drive the real binary against a real, migrated database
// so they exercise the literal output a user sees, not a unit's return
// value.

func pluginTestDSN(t *testing.T) string {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)
	var dbName string
	if err := db.QueryRow(`SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatalf("reading current_database: %v", err)
	}
	superDSN := testutil.PostgresTestDSN()
	u, err := url.Parse(superDSN)
	if err != nil {
		t.Fatalf("parse %q: %v", superDSN, err)
	}
	u.Path = "/" + dbName
	return u.String()
}

func TestPluginInstallStatesThePluginIsNotRun(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	dsn := pluginTestDSN(t)

	wasmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not a real wasm module"))
	}))
	defer wasmSrv.Close()

	dir := t.TempDir()
	indexPath := filepath.Join(dir, "index.yaml")
	index := fmt.Sprintf(`plugins:
  - name: example/hello-world
    description: test fixture
    author: test
    versions:
      - version: "0.1.0"
        wasm_url: %q
`, wasmSrv.URL)
	if err := os.WriteFile(indexPath, []byte(index), 0o644); err != nil {
		t.Fatalf("write index.yaml: %v", err)
	}

	cmd := exec.Command(cleatBinary, "--db", dsn, "plugin", "install",
		"--index-url", indexPath, "--yes", "example/hello-world@0.1.0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat plugin install: %v\n%s", err, out)
	}

	got := string(out)
	if !strings.Contains(got, "Successfully installed example/hello-world v0.1.0") {
		t.Fatalf("missing the success line: %s", got)
	}
	if !strings.Contains(got, "3.315") {
		t.Errorf("install output does not cite the tracking reference for the unwired "+
			"loader (IMPROVEMENT-PLAN 3.315): %s", got)
	}
	if !strings.Contains(got, "does not run it") && !strings.Contains(got, "will not reach it") {
		t.Errorf("install output reports success but says nothing about the plugin not "+
			"actually running: %s", got)
	}
}

func TestPluginListStatesInstalledPluginsAreNotLoaded(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	dsn := pluginTestDSN(t)

	wasmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not a real wasm module"))
	}))
	defer wasmSrv.Close()

	dir := t.TempDir()
	indexPath := filepath.Join(dir, "index.yaml")
	index := fmt.Sprintf(`plugins:
  - name: example/hello-world
    description: test fixture
    author: test
    versions:
      - version: "0.2.0"
        wasm_url: %q
`, wasmSrv.URL)
	if err := os.WriteFile(indexPath, []byte(index), 0o644); err != nil {
		t.Fatalf("write index.yaml: %v", err)
	}

	install := exec.Command(cleatBinary, "--db", dsn, "plugin", "install",
		"--index-url", indexPath, "--yes", "example/hello-world@0.2.0")
	if out, err := install.CombinedOutput(); err != nil {
		t.Fatalf("cleat plugin install (setup): %v\n%s", err, out)
	}

	list := exec.Command(cleatBinary, "--db", dsn, "plugin", "list")
	out, err := list.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat plugin list: %v\n%s", err, out)
	}

	got := string(out)
	if !strings.Contains(got, "example/hello-world") {
		t.Fatalf("list output does not show the installed plugin: %s", got)
	}
	if !strings.Contains(got, "3.315") {
		t.Errorf("list output does not cite the tracking reference for the unwired "+
			"loader (IMPROVEMENT-PLAN 3.315): %s", got)
	}
	if !strings.Contains(got, "none of the rows above run") && !strings.Contains(got, "not loaded") {
		t.Errorf("list output shows installed rows but says nothing about none of them "+
			"actually running: %s", got)
	}
}
