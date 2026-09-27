package testutil

import (
	"database/sql"
	"testing"
)

// TestForeignSessionsAnswersUnderPostgresConnectionExhaustion pins the fix
// for the blind spot cleat#982's 09-20 CI incident found: ForeignSessions
// used to open a fresh connection on every call, including the one made at
// the moment of failure -- exactly the moment a suite's own connections are
// most likely to have spent the server's connection budget, so the probe's
// own attempt to ask failed the same way everything else was failing and it
// reported "could not tell" (ok=false) rather than an answer.
//
// reserveProbeConnection/probeConnection close that gap by claiming one
// connection before the suite can have exhausted anything and reusing it
// instead of opening a fresh one. This test manufactures a real exhaustion
// against this process's own sandbox database -- not a mock, not a stubbed
// driver -- and checks the fix survives it.
//
// Postgres only. It is the dialect the 09-20 incident actually hit, and it
// is the tractable one to exhaust deliberately: max_connections is a single
// server-wide setting, so exhausting it deterministically requires the whole
// SERVER to be this test's own -- a separate database on a shared server is
// not enough. cleat#2012: the merge queue's Cluster Integration Tests job
// used to give this test its own database on the same Postgres server as a
// live 4-worker cluster, and the cluster's own connections -- recycled in
// the background every 5 minutes by database/sql's ConnMaxLifetime,
// independent of load -- could free a slot in that shared, server-wide
// ceiling at exactly the moment this test relied on it staying occupied.
// That job now starts a dedicated, throwaway Postgres container for
// ./engine/... (see .github/workflows/ci.yml, "Start a dedicated Postgres
// for ./engine/..."), restoring the exclusivity this comment used to assert
// without CI actually providing it.
func TestForeignSessionsAnswersUnderPostgresConnectionExhaustion(t *testing.T) {
	// Establishes a reservation the same way a real suite does -- at the
	// first TestDB call -- rather than calling reserveProbeConnection
	// directly and testing a path a real run never takes.
	TestDB(t, DialectPostgres)

	// Exhaust the server's remaining connection budget by opening raw
	// connections until it refuses one. The boundary is DISCOVERED rather
	// than computed from max_connections/superuser_reserved_connections,
	// which differ by image and role and would make this test assert a
	// number instead of a behavior.
	var held []*sql.DB
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	dsn := tagPostgresDSN(PostgresTestDSN())
	const safetyCap = 500
	var exhaustionErr error
	for i := 0; i < safetyCap; i++ {
		c, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Fatalf("sql.Open (connection %d): %v", i, err)
		}
		c.SetMaxIdleConns(1)
		if pingErr := c.Ping(); pingErr != nil {
			c.Close()
			exhaustionErr = pingErr
			break
		}
		held = append(held, c)
	}
	if exhaustionErr == nil {
		t.Fatalf("opened %d connections without the server ever refusing one -- "+
			"this test's exhaustion technique did not do what it claims, so nothing "+
			"below is a measurement of anything", len(held))
	}
	t.Logf("connection budget exhausted after %d held connections: %v", len(held), exhaustionErr)

	// KNOWN-POSITIVE. Confirm the exhaustion this test manufactured actually
	// breaks a FRESH connection attempt the same way the 09-20 incident did --
	// without this, a bug in the exhaustion loop above could leave
	// openProbeConnection free to succeed and the measurement below would
	// pass whether or not the fix does anything.
	if _, err := openProbeConnection(DialectPostgres); err == nil {
		t.Fatalf("openProbeConnection succeeded despite holding %d connections against a "+
			"database that just refused one -- the exhaustion is not real", len(held))
	} else {
		t.Logf("known-positive OK: a fresh connection attempt fails under this exhaustion: %v", err)
	}

	// THE MEASUREMENT. ForeignSessions must still answer, using the
	// reservation TestDB made before the exhaustion above rather than
	// attempting a fresh connection.
	foreign, basis, ok := ForeignSessions(DialectPostgres)
	if !ok {
		t.Fatalf("ForeignSessions could not answer under connection exhaustion: %s\n\n"+
			"This is the cleat#982 09-20 blind spot the reservation exists to close -- "+
			"a fresh connection just failed above by design, and this must not have.", basis)
	}
	t.Logf("ForeignSessions answered under exhaustion: %d foreign session(s), basis: %s",
		len(foreign), basis)
}

// TestReserveProbeConnectionIsIdempotent checks that a second reservation for
// a dialect that already has one does not open (and leak) a second
// connection. Without this, reserveProbeConnection would slowly grow the
// number of connections it holds across every package in a suite that calls
// SampleForeignSessionsAtStart more than once per process -- which
// TestForeignSessionsAnswersUnderPostgresConnectionExhaustion's own reliance
// on TestDB does, indirectly, on every run.
func TestReserveProbeConnectionIsIdempotent(t *testing.T) {
	TestDB(t, DialectPostgres) // ensures a reservation exists

	v1, ok1 := reservedProbeConn.Load(DialectPostgres)
	if !ok1 {
		t.Fatalf("no reservation exists for postgres after TestDB -- reserveProbeConnection " +
			"is not being called where SampleForeignSessionsAtStart expects it to be")
	}

	reserveProbeConnection(DialectPostgres) // must be a no-op

	v2, ok2 := reservedProbeConn.Load(DialectPostgres)
	if !ok2 {
		t.Fatalf("reservation disappeared after a second call to reserveProbeConnection")
	}
	if v1 != v2 {
		t.Fatalf("a second reservation replaced the first *sql.DB rather than reusing it -- " +
			"the earlier connection was leaked (never closed) and a new one opened in its place")
	}
}
