package migration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/migration"
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
	// ITS OWN DATABASE, NOT ITS OWN SCHEMA IN A SHARED ONE. cleat#1479.
	//
	// This test deliberately migrates two pools, and a pool migration rebinds
	// the SHARED admin functions: 001 creates them `SET search_path FROM
	// CURRENT`, which freezes the migrating pool's schema onto them. admin is
	// not moved by --schema -- there is one copy per DATABASE -- so the last
	// pool to migrate owns it for everyone else in that database.
	//
	// Run against the engine suite's database, as this test used to be, the
	// effect is that admin.claim_workflows starts looking for
	// workflow_instances in pool_b_1366. It finds none and returns an empty
	// list WITH NO ERROR, so eight cross-tenant and RLS tests in ./engine/ fail
	// as "the rows were not there". Measured: pristine database, the eight
	// pass, one `go test ./migration/`, the eight fail; two ALTER FUNCTION
	// statements and they pass again.
	//
	// A scratch database keeps the premise exactly -- two pools still share one
	// database, which is the thing being tested -- and confines the rebinding
	// to a database nothing else uses.
	db := newScratchDB(t, "cleat_ext_per_db_1366")

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
		err := runMigrations(t, context.Background(),
			migration.NewRunner(db, migration.DialectPostgres, "../migrations").WithSchema(schema),
			migration.DialectPostgres)
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
