package auth

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// operatorStatements returns every per-dialect statement this package issues
// against the operator table, by the name a failure should report it under.
//
// A map rather than four call sites so that a statement added later is covered
// by the guards below the moment it is written, rather than when someone
// remembers to add it to each one.
func operatorStatements(dialect string) map[string]string {
	return map[string]string{
		"resolveOperatorStmt": resolveOperatorStmt(dialect),
		"createOperatorStmt":  createOperatorStmt(dialect),
		"listOperatorStmt":    listOperatorStmt(dialect),
		"revokeOperatorStmt":  revokeOperatorStmt(dialect),
	}
}

// TestEveryOperatorStatementCarriesItsDialect stops a dialect being added to one
// statement and not the others. All four fall through to a PostgreSQL default,
// so an unknown dialect does not error -- it silently emits `$1` placeholders
// and an admin schema, which is wrong everywhere except PostgreSQL.
//
// The same rule and the same discriminating evidence as
// TestTheResolverCoversEveryDialectTheWriterDoes, extended to the statements
// that guard does not reach. `$1` is the tell rather than the schema
// qualification alone because it is the half that fails at PARSE time: MySQL
// rejects `$1` outright, while a missing `admin.` prefix is only wrong at
// run time and only on a deployment that has no such schema.
func TestEveryOperatorStatementCarriesItsDialect(t *testing.T) {
	for _, dialect := range []string{DialectMySQL, DialectMSSQL} {
		for name, stmt := range operatorStatements(dialect) {
			if strings.Contains(stmt, "$1") {
				t.Errorf("%s(%q) fell through to the PostgreSQL default and emits $1: %s", name, dialect, stmt)
			}
		}
	}
	// The positive control. Without it, a typo that made every statement return
	// the empty string would satisfy the loop above.
	if stmt := resolveOperatorStmt(DialectPostgres); !strings.Contains(stmt, "$1") {
		t.Errorf("resolveOperatorStmt(%q) does not use $1, so the check above cannot tell a dialect arm from a fall-through: %s",
			DialectPostgres, stmt)
	}
}

var (
	createTableRE = regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z0-9_.]+)`)
	targetTableRE = regexp.MustCompile(`(?i)(?:INSERT\s+INTO|FROM|UPDATE)\s+([A-Za-z0-9_.]+)`)

	sqlStringRE       = regexp.MustCompile(`'(?:[^']|'')*'`)
	sqlBlockCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)
	sqlLineCommentRE  = regexp.MustCompile(`(?m)--.*$`)
)

// stripLiteralsAndComments removes string literals and SQL comments from a
// migration body before it is searched.
//
// This is not tidiness, and the guard below failed without it in a way worth
// recording. The MySQL migration's own header QUOTES a CREATE TABLE --
// "`CREATE TABLE IF NOT EXISTS tenants` here against `admin.tenants`" -- while
// explaining the bare-name convention, so an unstripped search found a table in
// a sentence about a table. Worse, that quotation WRAPS across two comment
// lines, so the token after IF NOT EXISTS is `-- tenants`: the optional clause
// could not be followed by a name, backtracked, and the regex captured "IF" as
// the table name. It reported four statements targeting the wrong table on a
// tree where every one of them was right.
//
// COMMENTS COME FIRST HERE, and that is a deliberate departure from CLAUDE.md's
// ordering (strings, then comments), which exists so a `--` inside a string
// literal cannot truncate a line and hide real code to its left. Applied to
// these files it does the opposite: SQL migration prose is dense with possessive
// apostrophes ("MySQL's application identity", "cleat's"), so a string-first
// pass pairs the apostrophe in a COMMENT with the next one anywhere below it and
// swallows everything between -- including the CREATE TABLE. Measured: the first
// version of this function removed the statement and reported the file as having
// zero of them.
//
// The residual risk of the other order is a literal containing a comment marker,
// which would truncate its line. It cannot hide the statement this guard looks
// for -- a CREATE TABLE does not appear inside a string -- and if it ever did,
// the count assertion below fails rather than passing quietly. The loud
// direction, chosen on purpose.
func stripLiteralsAndComments(s string) string {
	s = sqlBlockCommentRE.ReplaceAllString(s, " ")
	s = sqlLineCommentRE.ReplaceAllString(s, "")
	return sqlStringRE.ReplaceAllString(s, "''")
}

// bareTable drops a schema qualifier, because MySQL cannot express one.
// cleat#1719 records the measurement: comparing qualified names across dialects
// reports twelve differences, the same six tables in both directions, every one
// spurious. The entity-contract guard compares bare names for the same reason.
func bareTable(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

// TestTheOperatorStatementsNameTheTableTheMigrationsCreate ties this package's
// SQL to the schema that defines the table.
//
// cleat#866 is the shape it prevents: a writer and a reader disagreeing about
// where a key lives, which does not look like a query bug at all -- the row is
// present, unrevoked and its hash matches, and every request still 401s.
// TestTheAPIKeyWriteAndReadNameTheSameTable covers that pair. This covers the
// half that guard cannot see, which is the side it does not compare against
// ANYTHING: an operator statement could be perfectly self-consistent and still
// name a table no migration creates.
//
// It reads the migration files rather than a list of names, so a table renamed
// in the schema and not in the code fails here. That is also why it globs for
// the file instead of hardcoding a number: a renumbered migration is not a
// reason for a guard to stop covering the table.
func TestTheOperatorStatementsNameTheTableTheMigrationsCreate(t *testing.T) {
	for dialect, dir := range map[string]string{
		DialectPostgres: "postgres",
		DialectMySQL:    "mysql",
		DialectMSSQL:    "mssql",
	} {
		t.Run(dialect, func(t *testing.T) {
			matches, err := filepath.Glob(filepath.Join("..", "migrations", dir, "*operator_api_keys.sql"))
			if err != nil {
				t.Fatalf("globbing the %s migrations: %v", dialect, err)
			}
			if len(matches) != 1 {
				t.Fatalf("found %d migration file(s) matching *operator_api_keys.sql in migrations/%s, want exactly 1: %v",
					len(matches), dir, matches)
			}
			body, err := os.ReadFile(matches[0])
			if err != nil {
				t.Fatalf("reading %s: %v", matches[0], err)
			}

			body = []byte(stripLiteralsAndComments(string(body)))

			// Exactly one, not simply the first. A file that creates two tables
			// would otherwise let this guard pass by reading whichever came
			// first, which is a denominator that says nothing about the table
			// the statements actually name.
			created := createTableRE.FindAllSubmatch(body, -1)
			if len(created) != 1 {
				t.Fatalf("%s has %d CREATE TABLE statement(s) after comments are stripped, want exactly 1, "+
					"so this guard cannot say which table it is comparing against", matches[0], len(created))
			}
			want := bareTable(string(created[0][1]))
			if want == "" {
				t.Fatalf("%s: the CREATE TABLE regex captured an empty name", matches[0])
			}

			for name, stmt := range operatorStatements(dialect) {
				found := targetTableRE.FindStringSubmatch(stmt)
				if found == nil {
					t.Errorf("%s(%q) names no table at all, so it cannot be checked against the migration: %s",
						name, dialect, stmt)
					continue
				}
				if got := bareTable(found[1]); got != want {
					t.Errorf("%s(%q) targets %q but %s creates %q.\n\nA credential written to one and read from the other "+
						"cannot authenticate anything, and the failure looks like a wrong key rather than a wrong table: "+
						"the row is present, unrevoked, and its hash matches. cleat#866.",
						name, dialect, got, filepath.Base(matches[0]), want)
				}
			}
		})
	}
}

// TestNewOperatorStoreForDialectRefusesAnUnknownDialect covers the constructor,
// with the control that the three real dialects are accepted -- otherwise the
// test would also pass against a constructor that refused everything.
func TestNewOperatorStoreForDialectRefusesAnUnknownDialect(t *testing.T) {
	for _, dialect := range []string{DialectPostgres, DialectMySQL, DialectMSSQL} {
		if _, err := NewOperatorStoreForDialect(nil, dialect); err != nil {
			t.Errorf("NewOperatorStoreForDialect(nil, %q) returned %v, want a store", dialect, err)
		}
	}

	// "postgresql" is the likeliest typo and the one that matters: it is not a
	// dialect this package knows, and a constructor that accepted it would emit
	// PostgreSQL's statements by way of the default arm while the rest of the
	// worker believed it was building PostgreSQL deliberately.
	for _, dialect := range []string{"postgresql", "", "sqlite", "POSTGRES"} {
		if _, err := NewOperatorStoreForDialect(nil, dialect); err == nil {
			t.Errorf("NewOperatorStoreForDialect(nil, %q) accepted an unknown dialect", dialect)
		}
	}
}

// The store must satisfy the interface the request path holds it to. A compile
// error here is the whole assertion, and it is the one that cannot rot: the
// middleware takes an OperatorResolver, so a signature drift is caught at build
// time rather than the first operator request.
var _ OperatorResolver = (*OperatorStore)(nil)
