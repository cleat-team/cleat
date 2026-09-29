package main

import (
	"context"
	"strings"
	"testing"
)

// TestRedactDBFlagHidesTheDSN pins redactDBFlag's one job: the value
// following "--db" never survives it. cleat-review's finding on #2716 --
// runBin formatted the raw args, DSN and password included, into an error
// that createTenant/generateAPIKey/deployWorkflowDef's callers all
// s.log.Error, so a single failed signup put the admin connection string
// into this process's logs.
func TestRedactDBFlagHidesTheDSN(t *testing.T) {
	dsn := "postgres://cleat:super-secret-password@localhost:5432/cleat?sslmode=disable"
	args := []string{"--create-tenant", "acme", "--org", "abc", "--db", dsn}

	got := redactDBFlag(args)

	for _, a := range got {
		if strings.Contains(a, "super-secret-password") {
			t.Fatalf("redactDBFlag(%v) = %v, still contains the DSN's password", args, got)
		}
	}
	if got[len(got)-1] != "REDACTED" {
		t.Errorf("redactDBFlag(%v) = %v, want the --db value replaced with REDACTED", args, got)
	}

	// The rest of the argv is unchanged -- redaction should not blind a
	// reader to which command actually ran.
	want := []string{"--create-tenant", "acme", "--org", "abc", "--db", "REDACTED"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("redactDBFlag(%v)[%d] = %q, want %q", args, i, got[i], w)
		}
	}

	// The input slice itself must not be mutated -- a caller building the
	// real argv for exec.CommandContext right after calling this for a log
	// line must still see the real DSN.
	if args[len(args)-1] != dsn {
		t.Errorf("redactDBFlag mutated its input slice: args[-1] = %q, want the original DSN", args[len(args)-1])
	}
}

// TestRunBinRedactsTheDSNOnFailure exercises runBin itself, not just the
// helper it now calls: the failure this scenario actually hits is a
// nonexistent binary or a refused invocation, and the DSN must not survive
// THAT path either.
func TestRunBinRedactsTheDSNOnFailure(t *testing.T) {
	a := &tenantAdmin{}
	dsn := "postgres://cleat:super-secret-password@localhost:5432/cleat?sslmode=disable"

	_, err := a.runBin(context.Background(), "/bin/false", "--db", dsn)
	if err == nil {
		t.Fatal("runBin with /bin/false: err = nil, want a failure")
	}
	if strings.Contains(err.Error(), "super-secret-password") {
		t.Fatalf("runBin's error leaks the DSN password: %v", err)
	}
}
