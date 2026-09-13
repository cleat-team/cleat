package migration

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"os"
)

// cleat#1366: a second pool migrating into its own schema in the SAME database
// could not apply 033.
//
// An extension is per-DATABASE. The first pool creates pg_trgm wherever its
// search_path pointed; the second gets `IF NOT EXISTS` -> NOTICE -> no-op, and
// then cannot resolve a bare `gin_trgm_ops` because the opclass lives in the
// first pool's schema.
//
// The assertion is on the SECOND pool applying cleanly, not on any count: the
// first pool succeeded before this fix too, so a test that only ran one pool
// passes against the bug.
func TestAnExtensionIsPerDatabaseNotPerSchema(t *testing.T) {
	// BOTH variables, and the fallback is the load-bearing half: the only CI
	// job that runs ./migration/... is test-go/support, and it provides its
	// PostgreSQL service as CLEAT_TEST_DB. Reading only CLEAT_TEST_POSTGRES
	// would make this skip in the one job that runs it, and a test that always
	// skips is strictly worse than no test.
	//
	// Inlined rather than calling engine/testutil.PostgresTestDSN, which does
	// exactly this: engine/testutil imports migration, so a test in package
	// migration importing it is an import cycle.
	dsn := os.Getenv("CLEAT_TEST_POSTGRES")
	if dsn == "" {
		dsn = os.Getenv("CLEAT_TEST_DB")
	}
	if dsn == "" {
		t.Skip("no PostgreSQL DSN (CLEAT_TEST_POSTGRES or CLEAT_TEST_DB)")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	for _, s := range []string{"pool_a_1366", "pool_b_1366"} {
		if _, err := db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", s)); err != nil {
			t.Fatalf("reset %s: %v", s, err)
		}
	}

	// EITHER pool can be the one that fails, and which one depends on state
	// this test does not own. On a database where pg_trgm does not yet exist,
	// pool A creates it in pool_a_1366 and pool B is the casualty. On a
	// database where some earlier run already put it in public -- the common
	// case on a shared test instance -- pool A is already the "second pool"
	// relative to that, and it fails first.
	//
	// So both are checked the same way. An earlier version asserted only on
	// pool B and reported a pool A failure as an unrelated error, which would
	// have sent the next reader looking in the wrong place.
	runPool := func(schema string) {
		t.Helper()
		err := NewRunner(db, DialectPostgres, "../migrations").
			WithSchema(schema).Run(context.Background())
		if err == nil {
			return
		}
		if strings.Contains(err.Error(), "gin_trgm_ops") {
			t.Fatalf("cleat#1366 (%s): the opclass is being looked up on this pool's "+
				"search_path rather than where pg_trgm actually lives. An extension is "+
				"per-database, so IF NOT EXISTS is a no-op for every pool after the "+
				"first and the bare opclass does not resolve: %v", schema, err)
		}
		t.Fatalf("%s: %v", schema, err)
	}

	runPool("pool_a_1366")
	runPool("pool_b_1366")
}
