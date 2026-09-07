package auth

import (
	"regexp"
	"strings"
	"testing"
)

// TestTheAPIKeyWriteAndReadNameTheSameTable is the invariant cleat#866 broke.
//
// An API key is written by one code path and read by another. If they disagree
// about which table -- or, on MySQL, which DATABASE -- the row lands somewhere
// the reader never looks, and the symptom is not "a bug in the query" but "no
// key works, and the one you just minted is right there in the database".
//
// That is exactly what happened. Keys were written to the base database the DSN
// names, and read through a tenant-scoped store, which on MySQL is a different
// database entirely (isolation there is one database per tenant, not RLS or a
// session context). Every authenticated request answered 401 and no key existed
// that would have worked.
//
// Checking the two statements name the same table cannot catch a caller that
// hands the reader the wrong CONNECTION -- that is what cmd/cleat-worker's
// comment at the auth.Middleware call site is for, and what the port suite
// covers by running against a real MySQL. This covers the half that is
// checkable here, and it is the half that silently rots when someone adds a
// dialect or moves a schema.
func TestTheAPIKeyWriteAndReadNameTheSameTable(t *testing.T) {
	table := regexp.MustCompile(`(?i)(?:INSERT\s+INTO|FROM)\s+([A-Za-z0-9_.]+)`)

	extract := func(t *testing.T, stmt string) string {
		t.Helper()
		m := table.FindStringSubmatch(stmt)
		if m == nil {
			t.Fatalf("no table name found in %q, so this guard checked nothing", stmt)
		}
		return strings.ToLower(m[1])
	}

	for _, dialect := range []string{DialectPostgres, DialectMySQL, DialectMSSQL} {
		t.Run(dialect, func(t *testing.T) {
			writeStmt, _ := createAPIKeyStmt(dialect)
			readStmt := resolveAPIKeyStmt(dialect)

			write := extract(t, writeStmt)
			read := extract(t, readStmt)

			if write != read {
				t.Errorf("the API key writer targets %q and the reader targets %q on %s.\n\n"+
					"A key written to one and looked up in the other cannot authenticate "+
					"anything, and the failure looks like a wrong key rather than a wrong "+
					"table: the row is present, unrevoked, and its hash matches. cleat#866.",
					write, read, dialect)
			}
		})
	}
}

// TestTheResolverCoversEveryDialectTheWriterDoes stops a dialect being added to
// one of the pair and not the other. Both fall through to a PostgreSQL default,
// so an unknown dialect does not error -- it silently emits `$1` placeholders
// and an admin schema, which is wrong everywhere except PostgreSQL and is the
// shape of the bug this pair already had once (cleat#865).
func TestTheResolverCoversEveryDialectTheWriterDoes(t *testing.T) {
	for _, dialect := range []string{DialectMySQL, DialectMSSQL} {
		writeStmt, _ := createAPIKeyStmt(dialect)
		readStmt := resolveAPIKeyStmt(dialect)

		// The PostgreSQL default is the giveaway: $1 placeholders.
		if strings.Contains(readStmt, "$1") {
			t.Errorf("resolveAPIKeyStmt(%q) fell through to the PostgreSQL default and emits "+
				"$1 placeholders: %s", dialect, readStmt)
		}
		if strings.Contains(writeStmt, "$1") {
			t.Errorf("createAPIKeyStmt(%q) fell through to the PostgreSQL default and emits "+
				"$1 placeholders: %s", dialect, writeStmt)
		}
	}
}
