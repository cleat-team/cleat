package engine

// cleat#2911 G2. See callIntentClock's doc comment (store_intent.go) for the
// mechanism: MySQL converts a TIMESTAMP column to and from the SESSION's
// time_zone on the wire, and the Go driver parses whatever comes back using
// its own loc parameter (UTC by default), with no idea what session zone
// produced it. A session whose time_zone is not UTC therefore makes
// EventRecord.CreatedAt silently wrong when compared against this process's
// own clock -- which is exactly what the old retention check did.

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestIdempotencyKeyRetentionIsSafeOnANonUTCMySQLSession mirrors
// plugins/auditlog/chain_db_test.go's
// TestTheChainDoesNotDependOnTheMySQLSessionTimeZone: a session time_zone is
// appended to the DSN with the same net/url.QueryEscape convention
// go-sql-driver/mysql uses to set it on connect, rather than going through
// MySQLBackend.Setup (forEachBackend), which has no hook for this.
//
// +02:00, not an arbitrary zone: AHEAD of UTC is the direction that makes a
// biased CreatedAt read LATER than the truth, which is the direction that
// makes a stale row look FRESHER than it is -- the unsafe direction
// cleat-review measured (CreatedAt read ~2h into the future at this same
// offset). A retention check that fails in the other direction (treating a
// fresh row as stale) would reject a legitimate replay, which is a
// correctness bug but not the unsafe-resend hazard this guards against.
func TestIdempotencyKeyRetentionIsSafeOnANonUTCMySQLSession(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MYSQL") == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
	}
	baseDSN := os.Getenv("CLEAT_TEST_MYSQL")

	dsn := baseDSN + "&time_zone=" + url.QueryEscape("'+02:00'")
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open MySQL test DB with a non-UTC session: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping MySQL test DB with a non-UTC session: %v", err)
	}
	testutil.SetupMySQLFullSchema(t, db)
	applyMySQLProcedures(t, db)
	testutil.CleanupMySQLTestData(t, db)
	t.Cleanup(func() { testutil.CleanupMySQLTestData(t, db) })
	store := NewMySQLStore(db)

	ctx := context.Background()
	wfID := newIntentWorkflow(t, ctx, store, "keyreplay-tz")
	writePendingCall(t, ctx, store, wfID)

	// A 1ns retention, smaller than any real clock skew between this
	// process and the DB server, isolates the session-zone bias from
	// ordinary test timing noise: the row must read as stale under ANY
	// correct age computation, so a replay attempt here can only mean the
	// age computation itself is wrong.
	replayer := &fakeKeyReplayer{response: `{"charged":true}`, outcomes: []IdempotencyReplayOutcome{IdempotencyReplayResolved}}
	ops := map[string]bool{intentService + "." + intentOperation: true}
	s, _ := replaySessionFor(t, ctx, store, wfID,
		WithIdempotencyKeyReplayer(replayer), WithIdempotencyKeyOps(ops), WithIdempotencyKeyRetention(1*time.Nanosecond))

	result := s.DurableCall(ctx, nil, intentService, intentOperation, `{"amount":100}`, 0, 0)
	if errCodeOf(result) == 0 {
		t.Fatal("a stale pending row was resolved anyway, under a +02:00 MySQL session -- " +
			"cleat#2911 G2: CreatedAt read back ahead of the truth made the row look " +
			"fresher than it was")
	}
	if replayer.calls != 0 {
		t.Errorf("the replayer was consulted %d times under a +02:00 session; a row past "+
			"its retention bound must not be replayed at all, not even once", replayer.calls)
	}
}
