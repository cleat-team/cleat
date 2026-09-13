package testutil

import (
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
)

// TestForeignSessionsSeesAnotherProcess is the positive control for cleat#982's
// missing datum, and it is the half that matters: an empty foreign-session list
// is exactly what a query against the wrong view, the wrong column or a
// disabled performance_schema also returns. The negative case is checked in the
// same function so a probe that reports EVERYTHING cannot pass either.
//
// The two dialect groups need different setups because their discriminators
// differ, and the difference is real rather than a convenience:
//
//   - MySQL and SQL Server tell the server our actual OS pid without being
//     asked, so the only way to stage a second process in-process is to claim
//     to be a different one. selfPID is a variable for exactly this.
//   - PostgreSQL has no client pid at all; the discriminator is an
//     application_name this package puts in the DSN. A connection carrying a
//     different application_name IS a different process as far as the
//     instrument is concerned, so the control opens one and needs no injection.
func TestForeignSessionsSeesAnotherProcess(t *testing.T) {
	for _, dialect := range []Dialect{DialectPostgres, DialectMySQL, DialectMSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			TestDB(t, dialect) // applies the schema and the PostgreSQL DSN tag

			// NEGATIVE CONTROL, first and on the real identity: this process's
			// own connections must NOT be reported. A probe that lists every
			// session would "pass" the positive case below for the wrong
			// reason, and this is what separates them.
			extra := openSecondConnection(t, dialect, "")
			defer extra.Close()

			before, basis, ok := ForeignSessions(dialect)
			if !ok {
				t.Fatalf("the probe could not answer at all: %s\n\n"+
					"Neither control below means anything until it can.", basis)
			}
			if len(before) != 0 {
				t.Fatalf("with only this process attached, the probe reported %d foreign "+
					"session(s), so it cannot distinguish us from anyone else:\n  basis: %s\n  %s\n\n"+
					"If another session really is using this database, that is the cleat#982 "+
					"hazard and this test cannot run against it -- use a database of your own.",
					len(before), basis, strings.Join(before, "\n  "))
			}
			t.Logf("negative control OK: own connections not reported. basis: %s", basis)

			// POSITIVE CONTROL: stage a session that belongs to someone else
			// and require the probe to name it.
			var foreign []string
			if dialect == DialectPostgres {
				stranger := openSecondConnection(t, dialect, "cleat-test-999999")
				defer stranger.Close()
				foreign, basis, ok = ForeignSessions(dialect)
			} else {
				restore := selfPID
				selfPID = func() int { return restore() + 1 } // we are now "someone else"
				defer func() { selfPID = restore }()
				foreign, basis, ok = ForeignSessions(dialect)
			}

			if !ok {
				t.Fatalf("the probe stopped being able to answer between the two controls: %s", basis)
			}
			if len(foreign) == 0 {
				t.Fatalf("a session belonging to another process was attached and the probe "+
					"reported none.\n  basis: %s\n\n"+
					"This is the failure mode the probe exists to avoid: cleat#982's earlier "+
					"instrument fired zero times in eight runs and nobody could tell a quiet "+
					"period from a broken query. An empty list has to MEAN something.", basis)
			}
			t.Logf("positive control OK: %d foreign session(s) reported. basis: %s\n  %s",
				len(foreign), basis, strings.Join(foreign, "\n  "))
		})
	}
}

// openSecondConnection opens one extra connection to the same test database.
// appName applies to PostgreSQL only, where it is what identifies a process;
// empty means "tagged as us, like any other connection this process makes".
func openSecondConnection(t *testing.T, dialect Dialect, appName string) *sql.DB {
	t.Helper()
	var driver, dsn string
	switch dialect {
	case DialectPostgres:
		driver = "postgres"
		dsn = tagPostgresDSN(PostgresTestDSN())
		if appName != "" {
			u, err := url.Parse(dsn)
			if err != nil {
				t.Skipf("PostgreSQL DSN is not a URL, so this control cannot stage a stranger")
			}
			q := u.Query()
			q.Set("application_name", appName)
			u.RawQuery = q.Encode()
			dsn = u.String()
		}
	case DialectMySQL:
		driver, dsn = "mysql", os.Getenv("CLEAT_TEST_MYSQL")
	case DialectMSSQL:
		driver, dsn = "sqlserver", os.Getenv("CLEAT_TEST_MSSQL")
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("opening a second connection: %v", err)
	}
	// database/sql is lazy; force the connection to exist so the server can
	// see it, and hold it open so it is still there when the probe looks.
	if err := db.Ping(); err != nil {
		t.Fatalf("pinging the second connection: %v", err)
	}
	db.SetMaxIdleConns(1)
	return db
}
