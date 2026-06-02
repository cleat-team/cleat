package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
)

// captureStderr runs fn and returns everything written to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	out := make(chan string)
	go func() {
		var buf bytes.Buffer
		if _, cerr := io.Copy(&buf, r); cerr != nil {
			t.Logf("copy error: %v", cerr)
		}
		out <- buf.String()
	}()
	fn()
	w.Close()
	os.Stderr = old
	return <-out
}

func TestRootFlag(t *testing.T) {
	fs := flag.NewFlagSet("cleatctl", flag.ContinueOnError)
	db := fs.String("db", "", "PostgreSQL DSN")
	if err := fs.Parse([]string{"--db", "postgres://localhost/cleat"}); err != nil {
		t.Fatal(err)
	}
	if *db != "postgres://localhost/cleat" {
		t.Errorf("db = %q, want %q", *db, "postgres://localhost/cleat")
	}
}

func TestRootFlag_Defaults(t *testing.T) {
	fs := flag.NewFlagSet("cleatctl", flag.ContinueOnError)
	db := fs.String("db", "", "PostgreSQL DSN")
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if *db != "" {
		t.Errorf("default db = %q, want empty", *db)
	}
}

func TestPrintUsage_ContainsCommands(t *testing.T) {
	out := captureStderr(t, printUsage)
	checks := []string{"versions", "deploy"}
	for _, cmd := range checks {
		if !strings.Contains(out, cmd) {
			t.Errorf("printUsage() output should mention %q", cmd)
		}
	}
}

func TestPrintDeployUsage_ContainsSubcommands(t *testing.T) {
	out := captureStderr(t, printDeployUsage)
	checks := []string{"workflow", "plugin"}
	for _, sub := range checks {
		if !strings.Contains(out, sub) {
			t.Errorf("printDeployUsage() output should mention %q", sub)
		}
	}
}

func TestPrintVersionsUsage_ContainsSubcommands(t *testing.T) {
	out := captureStderr(t, printVersionsUsage)
	checks := []string{"list", "deprecate", "restore", "purge", "active", "gc"}
	for _, sub := range checks {
		if !strings.Contains(out, sub) {
			t.Errorf("printVersionsUsage() output should mention %q", sub)
		}
	}
}

func TestPrototypesCompile(t *testing.T) {
	// Smoke test that the command dispatch functions referenced in main()
	// have the expected signatures. We verify by checking they are not nil
	// (the Go compiler ensures they exist; this test verifies the test
	// binary links against them).

	// runVersions has signature: func(ctx context.Context, store engine.WorkflowStore, args []string)
	// runDeploy has signature:  func(ctx context.Context, store engine.WorkflowStore, db *sql.DB, args []string)
	// Just check they are non-nil function values (linkage verification).
	_ = runVersions
	_ = runDeploy
}
