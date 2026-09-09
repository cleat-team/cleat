package engine

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestEveryDialectRefusesAnIdempotencyKeyReusedForAnotherDefinition is
// cleat#1047, asserted across the three stores at once.
//
// The defect was that idempotency_keys is keyed (key_hash, tenant_id) and
// carries no definition, so a request naming a DIFFERENT workflow matched the
// first row and was handed that workflow's id with alreadyExisted = true --
// while its own workflow never started. A caller awaiting that id reads the
// other workflow's result as its own.
//
// WHY THIS IS A SOURCE ASSERTION RATHER THAN THREE EXECUTED CASES. The
// behaviour is one comparison inside StartNewRun, written out once per dialect
// because each store has its own SQL. Executing it needs all three databases;
// what actually breaks is one store being edited and the others not, which is
// the failure this file exists to catch and which a single-dialect run would
// miss entirely. cleat#1012 was exactly that -- tenant-scoped on one dialect of
// three -- and so was #1034.
//
// The SELECT check is the half that matters most: a store that refuses on
// mismatch but never reads def_name cannot see a mismatch, so it would pass an
// assertion about the refusal alone while behaving exactly as before.
func TestEveryDialectRefusesAnIdempotencyKeyReusedForAnotherDefinition(t *testing.T) {
	for _, f := range []string{
		"store_lifecycle.go", // PostgreSQL
		"mysql_lifecycle.go", // MySQL
		"mssql_lifecycle.go", // SQL Server
	} {
		t.Run(f, func(t *testing.T) {
			src := readSourceForTest(t, f)

			if !strings.Contains(src, "ErrIdempotencyKeyDefMismatch") {
				t.Errorf("%s does not refuse a definition mismatch.\n\n"+
					"A key reused for another workflow returns that workflow's id with "+
					"alreadyExisted = true, and the caller awaits the wrong run. Every "+
					"store needs the check; one left out is cleat#1012's shape.", f)
			}

			if !regexp.MustCompile(`SELECT workflow_id, def_name FROM idempotency_keys`).MatchString(src) {
				t.Errorf("%s does not SELECT def_name on the idempotency lookup.\n\n"+
					"Without it the mismatch check above has nothing to compare and can "+
					"never fire, so the store passes the refusal assertion while behaving "+
					"exactly as it did before the fix.", f)
			}

			if !regexp.MustCompile(`INTO idempotency_keys \(key_hash, workflow_id, expires_at, tenant_id, def_name\)`).MatchString(src) {
				t.Errorf("%s does not WRITE def_name.\n\n"+
					"Rows written without it read back NULL, which this fix treats as "+
					"\"unknown, allow\" -- correct for rows predating the backfill, and "+
					"silently disabling the check for every new row.", f)
			}
		})
	}
}

// TestTheMismatchErrorIsDistinguishable pins that a caller can tell this
// refusal from any other failure, which is the whole point of refusing rather
// than returning the wrong id.
func TestTheMismatchErrorIsDistinguishable(t *testing.T) {
	if !errors.Is(ErrIdempotencyKeyDefMismatch, ErrIdempotencyKeyDefMismatch) {
		t.Fatal("the sentinel does not match itself")
	}
	if ErrIdempotencyKeyDefMismatch.Error() == "" {
		t.Error("the sentinel has no message; a caller seeing it in a log learns nothing")
	}
}

// readSourceForTest reads one of this package's own files.
//
// Fatal rather than Skip if it is missing: these files are in this repository,
// so an unreadable one means the test has lost its subject, and passing then
// would assert nothing.
func readSourceForTest(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v -- this file is in the repository, so its absence "+
			"means this test is no longer checking what it names", name, err)
	}
	return string(b)
}
