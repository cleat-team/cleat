package main

// cleat#1307. The worker builds no per-tenant connection pools, on any dialect,
// and the obvious repair to the bug that revealed this would be worse than the
// bug.
//
// What was there: a `tenantPools = plugin.NewTenantPools(...)` guarded by
// `*driver != "postgres"` -- INSIDE the `case "postgres":` arm. Unreachable, so
// tenantPools was nil everywhere, so cmd/cleat-worker/setup.go's
// `if w.tenantPools != nil` has never once been true.
//
// THE TEMPTING FIX IS THE DANGEROUS ONE. Moving the block out of the switch so
// MySQL and SQL Server reach it reads as obviously right -- the comment above
// it said pools "are only created for those drivers". But plugin.TenantPools is
// PostgreSQL-only in its implementation:
//
//	plugin/tenant_db.go   sql.Open("postgres", tenantDSN)
//	                      fmt.Sprintf("%s user=%s password=%s", ...)   libpq keyword DSN
//	                      SELECT ... WHERE tenant_id = $1              libpq placeholder
//
// A MySQL worker would be handed a postgres connection builder. And MySQL is
// single-tenant BY DECISION -- tiers.yaml, "DECIDED 2026-09-03", enforced by
// migrations/mysql/038_single_tenant_guard.sql -- so making it reachable there
// also contradicts the manifest CLAUDE.md names as the source of truth.
//
// This test exists so that repair cannot land quietly. It is a source-level
// assertion on purpose: the failure it guards is a line of wiring, it needs no
// database, and so it runs in every job rather than only where a DSN is set.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func workerRepoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// newTenantPoolsCall matches a construction of the pool manager in worker code.
var newTenantPoolsCall = regexp.MustCompile(`(?m)^[^/]*\bplugin\.NewTenantPools\(`)

func TestTheWorkerBuildsNoTenantPools(t *testing.T) {
	root := workerRepoRoot(t)

	out, err := exec.Command("git", "-C", root, "ls-files", "cmd/cleat-worker/*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Fields(string(out))
	// A scan with nothing to scan reports a clean tree; that reading belongs
	// on the success path, not left implicit.
	if len(files) < 5 {
		t.Fatalf("git ls-files matched %d files under cmd/cleat-worker; the scan did "+
			"not see the package", len(files))
	}

	for _, rel := range files {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		// Matched OUTSIDE comments. The replacement comment in main.go quotes
		// the removed line, so a scan that cannot tell a call from a sentence
		// about the call fires on the explanation -- the trap CLAUDE.md records
		// as "a grep a retraction satisfies", and this file is full of prose
		// about the very construct it forbids.
		for _, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimLeft(line, " \t")
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if !newTenantPoolsCall.MatchString(line) {
				continue
			}
			t.Errorf("%s constructs plugin.NewTenantPools:\n\n\t%s\n\n"+
				"plugin.TenantPools is PostgreSQL-only -- it calls sql.Open(\"postgres\", ...) "+
				"with a libpq keyword DSN and $1 placeholders -- so wiring it up for MySQL or "+
				"SQL Server hands those workers a postgres connection builder. MySQL is also "+
				"single-tenant by decision (tiers.yaml, enforced by "+
				"migrations/mysql/038_single_tenant_guard.sql).\n\n"+
				"If tenant pools are being built deliberately, settle cleat#1307 first and "+
				"then delete this test.", rel, strings.TrimSpace(line))
		}
	}
}
