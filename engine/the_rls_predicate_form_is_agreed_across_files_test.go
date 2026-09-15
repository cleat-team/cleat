package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cleat#1541. Three files have to agree on two string literals: this package's
// constants, migration 074, and migrations/mssql/optional/cross_tenant_claim.sql.
//
// A typo in a migration does not fail loudly. `form = N'admn'` is refused by the
// table's CHECK constraint at apply time, which is the good case -- but a
// migration writing a VALID-but-wrong value ('plain' where 'admin' was meant)
// applies cleanly and reads here as "the predicate admits nobody", so a
// deployment that opted in would be told to opt in again. These tests are the
// only thing standing between that and a silent misreport.
//
// Static, so they run without a SQL Server. The behavioural half needs
// CLEAT_TEST_MSSQL and lives in the file next to it.

func migrationsDirForRLSTest(t *testing.T) string {
	t.Helper()
	// engine/ -> repo root -> migrations/mssql
	dir := filepath.Join("..", "migrations", "mssql")
	if _, err := os.Stat(dir); err != nil {
		// Fatal, not Skip: the directory is in this repository, and a skip here
		// would mean the guard silently stops running.
		t.Fatalf("migrations/mssql not readable from the engine package: %v", err)
	}
	return dir
}

func TestTheMarkerValuesAreSpelledTheSameEverywhere(t *testing.T) {
	dir := migrationsDirForRLSTest(t)

	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		return string(b)
	}

	seventyFour := read(filepath.Join(dir, "074_the_admin_bypass_is_opt_in.sql"))
	optional := read(filepath.Join(dir, "optional", "cross_tenant_claim.sql"))

	// The value each file WRITES, taken from the statement that writes it rather
	// than from anywhere the word happens to appear -- both files discuss both
	// values in their comments, so a substring search would pass on prose.
	// Both orders, because the two files write it differently: 074 MERGEs with
	// `N'plain' AS form` (literal first) and the opt-in UPDATEs with
	// `SET form = N'admin'` (literal second). The first version of this pattern
	// matched only the second order and reported 074 as writing nothing -- which
	// is what a migration that had stopped recording would also look like.
	writes := regexp.MustCompile(`(?i)(?:SET\s+form\s*=\s*N'([a-z]+)'|N'([a-z]+)'\s+AS\s+form)`)

	for _, tc := range []struct {
		name string
		src  string
		want string
	}{
		{"074_the_admin_bypass_is_opt_in.sql", seventyFour, rlsPredicatePlain},
		{"optional/cross_tenant_claim.sql", optional, rlsPredicateAdmin},
	} {
		m := writes.FindAllStringSubmatch(tc.src, -1)
		if len(m) == 0 {
			t.Errorf("%s: found no statement writing admin.rls_predicate_form.form. "+
				"Either the migration stopped recording the predicate it installs -- which "+
				"makes CheckCrossTenantCapability unable to answer -- or this test's pattern "+
				"no longer matches how it is written.", tc.name)
			continue
		}
		for _, hit := range m {
			got := hit[1]
			if got == "" {
				got = hit[2]
			}
			if got != tc.want {
				t.Errorf("%s writes form=%q, want %q. A valid-but-wrong value applies "+
					"cleanly and makes CheckCrossTenantCapability report the opposite of "+
					"the truth.", tc.name, got, tc.want)
			}
		}
	}

	// And the CHECK constraint has to admit exactly the two the Go code knows,
	// or a third value could be written that this package reads as "not admin".
	for _, want := range []string{rlsPredicatePlain, rlsPredicateAdmin} {
		if !strings.Contains(seventyFour, "N'"+want+"'") {
			t.Errorf("074 does not mention %q, so its CHECK constraint cannot be admitting it", want)
		}
	}
}

// The opt-in migration must NOT be picked up by the runner.
//
// migration.Runner.readMigrations skips directory entries, so a file under
// optional/ is never applied automatically -- that is the whole mechanism, and
// it is invisible: nothing about the file says "I am not applied". If someone
// moves it up a level to give it a version number, every deployment silently
// gets the slow predicate and the bypass back, which is the change this issue
// exists to undo.
func TestTheOptInMigrationIsNotInTheAutoAppliedSet(t *testing.T) {
	dir := migrationsDirForRLSTest(t)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	versioned := regexp.MustCompile(`^\d+_.*\.sql$`)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if versioned.MatchString(e.Name()) && strings.Contains(e.Name(), "cross_tenant_claim") {
			t.Errorf("%s is a versioned migration, so every deployment applies it. The "+
				"cross-tenant bypass is opt-in (cleat#1541); it belongs under "+
				"migrations/mssql/optional/, which the runner skips because it skips "+
				"directories.", e.Name())
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "optional", "cross_tenant_claim.sql")); err != nil {
		t.Errorf("migrations/mssql/optional/cross_tenant_claim.sql is missing (%v). Without "+
			"it there is no way to opt in, and --claim-across-tenants cannot work on SQL "+
			"Server at all.", err)
	}
}

// Neither migration may hard-code the policy list.
//
// The first draft of 074 listed eight policy names read out of 012. Thirteen are
// bound to the predicate on a fresh database, and CREATE OR ALTER FUNCTION fails
// while ANY policy still references it -- so the migration would have failed at
// apply time. A hand-written list is also the list that goes stale the next time
// a migration adds a policy.
func TestNeitherPredicateMigrationHardCodesThePolicyList(t *testing.T) {
	dir := migrationsDirForRLSTest(t)
	for _, name := range []string{
		filepath.Join(dir, "074_the_admin_bypass_is_opt_in.sql"),
		filepath.Join(dir, "optional", "cross_tenant_claim.sql"),
	} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(b)
		// Strip comments: both files name policies in prose explaining why they
		// are not listed, and a search that cannot tell code from a sentence
		// about code is the fault these very files were fixed for.
		var code strings.Builder
		for _, line := range strings.Split(src, "\n") {
			if i := strings.Index(line, "--"); i >= 0 {
				line = line[:i]
			}
			code.WriteString(line)
			code.WriteString("\n")
		}
		if m := regexp.MustCompile(`(?i)DROP\s+SECURITY\s+POLICY\s+dbo\.\[?TenantFilter`).
			FindString(code.String()); m != "" {
			t.Errorf("%s names a policy literally (%q). Derive the set from "+
				"sys.security_predicates instead: the literal list was wrong by five "+
				"entries when it was written by hand, and goes stale when a migration "+
				"adds a policy.", filepath.Base(name), m)
		}
		if !strings.Contains(code.String(), "sys.security_predicates") {
			t.Errorf("%s does not read sys.security_predicates, so it is not deriving "+
				"the policy set", filepath.Base(name))
		}
	}
}
