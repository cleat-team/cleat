package engine

import (
	"context"
	"os"
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

	// The schema as shipped: 074 has run, so the marker says plain.
	var form string
	if err := db.QueryRow(`SELECT form FROM admin.rls_predicate_form`).Scan(&form); err != nil {
		t.Fatalf("the marker table is missing after applying the shipped migrations (%v). "+
			"074 is what creates it, and without it the capability check cannot answer.", err)
	}
	if form != rlsPredicatePlain {
		t.Fatalf("the shipped schema records form=%q, want %q -- the default is supposed to be "+
			"the plain predicate", form, rlsPredicatePlain)
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
	optIn, err := os.ReadFile("../migrations/mssql/optional/cross_tenant_claim.sql")
	if err != nil {
		t.Fatalf("read the opt-in migration: %v", err)
	}
	for _, batch := range splitMSSQLBatchesForTest(string(optIn)) {
		if strings.TrimSpace(batch) == "" {
			continue
		}
		if _, err := db.Exec(batch); err != nil {
			t.Fatalf("applying the opt-in migration failed: %v", err)
		}
	}

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
