package testutil

// cleat#3597: a connection authenticated as the library PUBLISHER, and one authenticated as
// a login that is NOT, for tests of the publish path.
//
// Publishing is gated by a database role (cleat_library_publisher, migrations/*/005), not by
// code, so a test that publishes through the superuser connection proves nothing about it:
// a superuser can write anywhere, and would also hide a statement that needs a privilege the
// publisher lacks (UPDATE, or a row lock on PostgreSQL). These helpers connect as real,
// non-owning logins, one per dialect, so the grants are what is measured.

import (
	"database/sql"
	"net/url"
	"os"
	"testing"
)

const (
	// LibraryPublisherLogin is a login that is a member of cleat_library_publisher.
	LibraryPublisherLogin = "cleat_publish_test_login"
	// LibraryReaderLogin is a login that is NOT: it can sign in and holds nothing on the
	// library schema, which is the negative control for the publisher's grants.
	LibraryReaderLogin = "cleat_nopublish_test_login"
	// A password both engines accept: SQL Server enforces complexity.
	libraryLoginPassword = "Cleat-library-test-1!" //nolint:gosec // G101: a fixed login for a role that only exists in ephemeral test databases
)

// OpenLibraryLoginDB opens a separate connection to the database db is on, authenticated as
// LibraryPublisherLogin (member true) or LibraryReaderLogin (member false). PostgreSQL and
// SQL Server only. db must be an owner connection with the schema already applied.
func OpenLibraryLoginDB(t *testing.T, db *sql.DB, d Dialect, member bool) *sql.DB {
	t.Helper()
	driver, dsn := LibraryLoginDSN(t, db, d, member)
	login := LibraryReaderLogin
	if member {
		login = LibraryPublisherLogin
	}
	conn, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("open the %s connection: %v", login, err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping as %s: %v", login, err)
	}
	return conn
}

// LibraryLoginDSN provisions the login (see OpenLibraryLoginDB) and returns its driver name and
// DSN, for a test that runs a real binary as that login instead of opening a connection itself.
func LibraryLoginDSN(t *testing.T, db *sql.DB, d Dialect, member bool) (driver, dsn string) {
	t.Helper()
	login := LibraryReaderLogin
	if member {
		login = LibraryPublisherLogin
	}
	switch d {
	case DialectPostgres:
		return "postgres", provisionPostgresLibraryLogin(t, db, login, member)
	case DialectMSSQL:
		return "sqlserver", provisionMSSQLLibraryLogin(t, db, login, member)
	}
	t.Fatalf("no library publisher login on dialect %s", d)
	return "", ""
}

func provisionPostgresLibraryLogin(t *testing.T, db *sql.DB, login string, member bool) string {
	t.Helper()
	// CREATE first and catch the collision, not IF NOT EXISTS: a role is cluster-wide and
	// two packages creating it at once both see "not exists" (the same race
	// SetupPostgresRLSRole documents).
	stmts := []string{
		`DO $$ BEGIN
			CREATE ROLE ` + login + ` LOGIN PASSWORD '` + libraryLoginPassword + `' NOSUPERUSER NOCREATEDB NOCREATEROLE;
		EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END $$;`,
		`GRANT CONNECT ON DATABASE ` + currentPostgresDatabase(t, db) + ` TO ` + login,
	}
	if member {
		stmts = append(stmts, `GRANT cleat_library_publisher TO `+login)
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("provision %s: %v\n%s", login, err, s)
		}
	}
	base, err := swapDatabaseName(PostgresTestDSN(), currentPostgresDatabase(t, db))
	if err != nil {
		t.Fatalf("build the DSN: %v", err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse the DSN: %v", err)
	}
	u.User = url.UserPassword(login, libraryLoginPassword)
	return u.String()
}

func currentPostgresDatabase(t *testing.T, db *sql.DB) string {
	t.Helper()
	var name string
	if err := db.QueryRow(`SELECT current_database()`).Scan(&name); err != nil {
		t.Fatalf("read the current database: %v", err)
	}
	return name
}

func provisionMSSQLLibraryLogin(t *testing.T, db *sql.DB, login string, member bool) string {
	t.Helper()
	var dbName string
	if err := db.QueryRow(`SELECT DB_NAME()`).Scan(&dbName); err != nil {
		t.Fatalf("read the current database: %v", err)
	}
	// A login is server-wide, a user and a role membership are per database.
	stmts := []string{
		`IF SUSER_ID(N'` + login + `') IS NULL CREATE LOGIN [` + login + `] WITH PASSWORD = N'` + libraryLoginPassword + `', CHECK_POLICY = OFF`,
		`IF DATABASE_PRINCIPAL_ID(N'` + login + `') IS NULL CREATE USER [` + login + `] FOR LOGIN [` + login + `]`,
	}
	if member {
		stmts = append(stmts, `IF IS_ROLEMEMBER(N'cleat_library_publisher', N'`+login+`') IS NULL OR IS_ROLEMEMBER(N'cleat_library_publisher', N'`+login+`') = 0
			ALTER ROLE [cleat_library_publisher] ADD MEMBER [`+login+`]`)
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("provision %s: %v\n%s", login, err, s)
		}
	}
	base := os.Getenv("CLEAT_TEST_MSSQL")
	if base == "" {
		base = "sqlserver://sa:CleatTest123!@localhost:1433?database=cleat"
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse the DSN: %v", err)
	}
	q := u.Query()
	q.Set("database", dbName)
	u.RawQuery = q.Encode()
	u.User = url.UserPassword(login, libraryLoginPassword)
	return u.String()
}
