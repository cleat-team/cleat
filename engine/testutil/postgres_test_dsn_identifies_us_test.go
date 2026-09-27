package testutil

import (
	"database/sql"
	"net/url"
	"testing"
)

// TestPostgresTestDSNIdentifiesUsToTheGate pins the reason PostgresTestDSN
// tags the DSN it returns, rather than leaving that to its callers.
//
// The #982 gate recognises our own PostgreSQL sessions by application_name and
// by nothing else, because PostgreSQL -- alone among the three dialects -- does
// not tell the server the client's pid. So "the DSN this package hands out
// carries a tag" is not a property of the DSN, it is the gate's entire ability
// to tell us from a stranger. Thirteen files open connections from
// PostgresTestDSN; before cleat#1498 the tag was applied at three call sites
// inside this package and nowhere else, so the other ten produced connections
// that the gate correctly-by-its-own-lights called foreign. One of them, the
// scratch-database admin connection in plugins/kvstore, failed Test Go
// (plugins) on a PR that touched only migration/.
//
// The negative control is the half that gives this test its teeth: it strips
// the tag back off and requires ForeignSessions to report the resulting
// connection. Without it, this would pass just as happily against a probe that
// never reports anything -- which is precisely the cleat#982 failure the probe
// was built to end.
func TestPostgresTestDSNIdentifiesUsToTheGate(t *testing.T) {
	TestDB(t, DialectPostgres) // skips if PostgreSQL is unreachable

	dsn := PostgresTestDSN()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("PostgresTestDSN did not parse as a URL (%v).\n\n"+
			"tagPostgresDSN returns non-URL DSNs unchanged, so this is the one shape "+
			"where the tag cannot be applied and every caller silently looks foreign.", err)
	}
	if got := u.Query().Get("application_name"); got != testProcessTag() {
		t.Fatalf("PostgresTestDSN returned application_name=%q, want %q.\n\n"+
			"A connection opened from this DSN is a stranger to ForeignSessions, and "+
			"RefuseIfForeignSessionsAtStart will fail whichever suite opens it -- "+
			"usually a suite that changed nothing. See cleat#1498.", got, testProcessTag())
	}

	// The server has to agree. A DSN parameter PostgreSQL ignored would satisfy
	// the check above and still leave us unidentifiable.
	ours, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open from PostgresTestDSN: %v", err)
	}
	defer ours.Close()
	var echoed string
	if err := ours.QueryRow(
		`SELECT coalesce(application_name,'') FROM pg_stat_activity WHERE pid = pg_backend_pid()`,
	).Scan(&echoed); err != nil {
		t.Fatalf("reading our own application_name back from the server: %v", err)
	}
	if echoed != testProcessTag() {
		t.Fatalf("the server reports application_name=%q for our connection, want %q -- "+
			"the DSN carried the tag but PostgreSQL did not apply it", echoed, testProcessTag())
	}

	foreign, basis, ok := ForeignSessions(DialectPostgres)
	if !ok {
		t.Fatalf("the probe could not answer (%s); the control below means nothing until it can", basis)
	}
	if len(foreign) != 0 {
		t.Fatalf("a connection opened from PostgresTestDSN was reported as foreign: %v\n  basis: %s",
			foreign, basis)
	}

	// NEGATIVE CONTROL: strip the tag and require the probe to notice. If this
	// does not fire, the assertions above passed for a reason unrelated to the
	// tag and this test is measuring nothing.
	q := u.Query()
	q.Del("application_name")
	u.RawQuery = q.Encode()
	untagged, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatalf("open untagged: %v", err)
	}
	defer untagged.Close()
	if err := untagged.Ping(); err != nil {
		t.Fatalf("ping untagged: %v", err)
	}
	untagged.SetMaxIdleConns(1)

	after, basis, ok := ForeignSessions(DialectPostgres)
	if !ok {
		t.Fatalf("the probe stopped answering during the negative control: %s", basis)
	}
	if len(after) == 0 {
		t.Fatalf("an UNTAGGED connection to this database was not reported as foreign.\n"+
			"  basis: %s\n\n"+
			"That is the exact connection shape that failed cleat#1498, so the probe "+
			"agreeing with us here means it has stopped discriminating -- not that the "+
			"tag is working.", basis)
	}
	t.Logf("negative control OK: untagged connection reported as foreign (%d): %v", len(after), after)
}
