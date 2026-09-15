package engine

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#1541. MSSQLStore.CheckCrossTenantCapability had NO test -- the existing
// TestCheckCrossTenantCapability_* cases in cross_tenant_capability_test.go all
// build a PostgresStore. That is why it could answer from IS_ROLEMEMBER alone
// for as long as it did: the property it got wrong was never asserted on this
// dialect.
//
// Three arms, because the third is the one the design turns on:
//
//	admin predicate + member   -> available
//	plain predicate + member   -> NOT available, and the reason names the PREDICATE
//	marker unreadable          -> UNKNOWN, and the reason must NOT say "grant membership"
//
// The second arm is the regression: before 074 a member read IS_ROLEMEMBER = 1
// and the check said available, while the connection saw zero rows.

func TestMSSQLCapabilityFollowsTheInstalledPredicate(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL integration test in short mode")
	}

	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)
	store := NewMSSQLStore(db)
	ctx := context.Background()

	// This test sets its OWN starting state rather than assuming the harness
	// leaves the shipped default in place, and the reason is worth recording:
	// testutil.MSSQLAdminDB deliberately applies the cross-tenant opt-in,
	// because CleanupMSSQLTestData deletes across tenants and cannot work
	// without it. So by the time any test runs, the database may be opted IN.
	//
	// The first version of this test asserted "the shipped schema records
	// plain" and failed for exactly that reason -- a correct failure about a
	// premise that is true of a deployment and false of this harness.
	applyMigrationFileForTest(t, db, "074_the_admin_bypass_is_opt_in.sql")

	var form string
	if err := db.QueryRow(`SELECT form FROM admin.rls_predicate_form`).Scan(&form); err != nil {
		t.Fatalf("the marker table is missing after applying 074 (%v). It is what creates "+
			"it, and without it the capability check cannot answer.", err)
	}
	if form != rlsPredicatePlain {
		t.Fatalf("after applying 074 the marker records form=%q, want %q", form, rlsPredicatePlain)
	}

	// ARM 1: plain predicate. The predicate admits nobody, so the capability
	// must be false and the reason must point at the predicate rather than at a
	// grant.
	//
	// THE REASON IS THE DISCRIMINATOR HERE, NOT THE BOOLEAN, and that is worth
	// stating because it is not obvious. This connection is `sa`, which 012
	// records is NOT a member of cleat_admin (db_owner does not confer it, and
	// dbo cannot be added). So a membership-only check ALSO returns false here,
	// for the wrong reason, and an assertion on Claim alone would pass against
	// the regression. Verified: reverting CheckCrossTenantCapability to the
	// membership-only form fails the three message assertions below and none of
	// the boolean ones.
	//
	// Making the boolean discriminate would need a login created and added to
	// cleat_admin inside this test. That is worth doing if the messages ever
	// stop being asserted; while they are, it would be a second way to catch
	// the same regression rather than a new one.
	cap1 := store.CheckCrossTenantCapability(ctx)
	if cap1.Claim || cap1.Schedules {
		t.Errorf("reported cross-tenant capability under the PLAIN predicate "+
			"(claim=%v schedules=%v). This is the regression 074 introduces if the check "+
			"still asks IS_ROLEMEMBER alone: a member reads 1 and sees zero rows.",
			cap1.Claim, cap1.Schedules)
	}
	if !strings.Contains(cap1.ClaimReason, "plain predicate") {
		t.Errorf("the refusal does not name the predicate: %q. An operator reading this "+
			"needs to be sent to the opt-in migration, not to a role grant they may "+
			"already have.", cap1.ClaimReason)
	}
	if !strings.Contains(cap1.ClaimReason, "cross_tenant_claim.sql") {
		t.Errorf("the refusal does not name the file that fixes it: %q", cap1.ClaimReason)
	}

	// ARM 2: opt in, and the answer must change. Without this the test above
	// would pass against a check that reports false unconditionally.
	applyMigrationFileForTest(t, db, filepath.Join("optional", "cross_tenant_claim.sql"))

	if err := db.QueryRow(`SELECT form FROM admin.rls_predicate_form`).Scan(&form); err != nil {
		t.Fatalf("marker unreadable after the opt-in: %v", err)
	}
	if form != rlsPredicateAdmin {
		t.Fatalf("the opt-in migration left form=%q, want %q", form, rlsPredicateAdmin)
	}

	cap2 := store.CheckCrossTenantCapability(ctx)
	// This connection is sa, which is NOT a member of cleat_admin -- 012
	// documents that db_owner does not confer it. So the expected answer is
	// still false, but for a DIFFERENT reason, and the reason is the assertion.
	if strings.Contains(cap2.ClaimReason, "plain predicate") {
		t.Errorf("after opting in, the refusal still blames the predicate: %q. The marker "+
			"says admin, so the check is not reading it.", cap2.ClaimReason)
	}
	if cap2.Claim != cap2.Schedules {
		t.Errorf("claim=%v but schedules=%v; one predicate governs both on SQL Server",
			cap2.Claim, cap2.Schedules)
	}

	// ARM 3: the marker unreadable. Must be UNKNOWN, not a denial -- "not
	// available" would send an operator to grant a membership they may already
	// have, and the whole point of the marker is to distinguish the two.
	if _, err := db.Exec(`DROP TABLE admin.rls_predicate_form`); err != nil {
		t.Fatalf("dropping the marker: %v", err)
	}
	cap3 := store.CheckCrossTenantCapability(ctx)
	if cap3.Claim || cap3.Schedules {
		t.Errorf("reported capability with the marker table absent (claim=%v)", cap3.Claim)
	}
	if !strings.Contains(cap3.ClaimReason, "rls_predicate_form") {
		t.Errorf("an unreadable marker does not say so: %q", cap3.ClaimReason)
	}
	if strings.Contains(cap3.ClaimReason, "plain predicate") {
		t.Errorf("an unreadable marker is reported as if the predicate were known to be "+
			"plain: %q. Unknown and denied are different answers and want different "+
			"actions from an operator.", cap3.ClaimReason)
	}
}

// applyMigrationFileForTest applies one file from migrations/mssql/ by name.
//
// Used to put the database into a known predicate state rather than inheriting
// whatever the harness left, which is the difference between a test that
// asserts a property and one that asserts an ordering.
func applyMigrationFileForTest(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "migrations", "mssql", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	// ONE connection for every batch, not the pool.
	//
	// 074 captures the policy set into a #temp table in one batch and replays
	// it in another; a #temp table lives for the SESSION, so batches issued
	// through a pool can land on different connections and the second one sees
	// "Invalid object name '#cleat_bound_policies'". migration.Runner does not
	// have this problem because it opens one connection per file -- so a helper
	// that applies a migration has to do the same or it is not applying it the
	// way the runner does.
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire a single connection to apply %s: %v", name, err)
	}
	defer conn.Close()

	for _, batch := range splitMSSQLBatchesForTest(string(raw)) {
		if strings.TrimSpace(batch) == "" {
			continue
		}
		if _, err := conn.ExecContext(ctx, batch); err != nil {
			t.Fatalf("applying %s: %v", name, err)
		}
	}
}

// splitMSSQLBatchesForTest splits on GO, which is a client directive rather than
// T-SQL: database/sql sends one statement at a time and rejects a batch
// separator. Same job as migration.Runner's splitter, duplicated here rather
// than exported, because a test reaching into the runner's internals to apply
// one file would couple them for no benefit.
func splitMSSQLBatchesForTest(sqlText string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(sqlText, "\n") {
		if strings.EqualFold(strings.TrimSpace(line), "GO") {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteString(line)
		cur.WriteString("\n")
	}
	out = append(out, cur.String())
	return out
}
