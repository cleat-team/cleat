package engine

// cleat#2205. SQL Server's row-level security had FILTER predicates only --
// they restrict what SELECT/UPDATE/DELETE can SEE, and say nothing about
// what an INSERT writes or what an UPDATE leaves a row's tenant_id set to.
// migrations/mssql/103_a_filtered_write_is_a_blocked_write.sql adds BLOCK
// predicates (AFTER INSERT, AFTER UPDATE, BEFORE UPDATE) alongside every
// FILTER predicate. This file is the database-level proof: real stores,
// through the serving login's own tenant-scoped connector
// (openMSSQLTenantStore, the same one every other MSSQL test in this
// package uses) -- not sa, and not a raw admin pool. See
// engine/mssql_tenant_predicate_test.go for the complementary, Go-source
// half of cleat#2205's "audit the write paths" ask: it already asserts
// every MSSQL write statement in this codebase carries its own tenant
// predicate, so the class this file guards against is a write that reaches
// the database with the WRONG tenant_id despite that -- a bug in a
// predicate's value, not its absence, plus any future write path the
// Go-level guard has not yet been extended to cover.

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestMSSQLBlockPredicatesRejectCrossTenantWrites uses tenant_domains: it
// carries a FILTER/BLOCK-protected tenant_id column and its only FK
// (fk_tenant_domains_tenant, to admin.tenants) is NOT itself tenant-scoped,
// so -- unlike e.g. workflow_tags, whose FK to workflow_defs is COMPOSITE
// over tenant_id -- a cross-tenant write here is refused by the BLOCK
// predicate alone, not confounded by an FK that would refuse it anyway for
// an unrelated reason. Checked directly: measured 2026-09-24 that
// workflow_tags' FK error and this table's block-predicate error are
// distinguishable but the former was reached first when both apply, which
// is why this table and not that one.
func TestMSSQLBlockPredicatesRejectCrossTenantWrites(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}
	ctx := context.Background()
	const tenantA = "cccccccc-1111-4111-8111-111111111111"
	const tenantB = "cccccccc-2222-4222-8222-222222222222"
	storeA := openMSSQLTenantStore(t, tenantA)
	storeB := openMSSQLTenantStore(t, tenantB)

	for _, row := range []struct{ id, name string }{
		{tenantA, "block-predicate-test-a"},
		{tenantB, "block-predicate-test-b"},
	} {
		if _, err := storeA.db.Exec(
			`INSERT INTO admin.tenants (tenant_id, name) VALUES (@p1, @p2)`, row.id, row.name,
		); err != nil {
			t.Fatalf("seed admin.tenants(%s): %v", row.name, err)
		}
	}
	t.Cleanup(func() {
		_, _ = storeA.db.Exec(`DELETE FROM admin.tenants WHERE tenant_id IN (@p1, @p2)`, tenantA, tenantB)
	})

	const hostA = "block-predicate-test-a.example.com"

	// CROSS-TENANT INSERT IS REFUSED: storeA's connector holds tenant A's
	// session context (set once per connection by the tenant-scoped
	// connector every other MSSQL test in this package relies on), and this
	// INSERT names tenant B in the row it writes.
	_, err := storeA.db.ExecContext(ctx,
		`INSERT INTO dbo.tenant_domains (hostname, tenant_id, created_at, updated_at)
		 VALUES (@p1, @p2, SYSUTCDATETIME(), SYSUTCDATETIME())`, hostA, tenantB)
	if err == nil {
		t.Fatal("a cross-tenant INSERT into tenant_domains succeeded -- the AFTER INSERT block " +
			"predicate did not fire")
	}
	if !isMSSQLBlockPredicateError(err) {
		t.Fatalf("cross-tenant INSERT failed, but not with a block-predicate error: %v", err)
	}

	// LEGITIMATE SAME-TENANT WRITE STILL SUCCEEDS -- the predicate must
	// scope writes, not merely refuse all of them.
	if _, err := storeA.db.ExecContext(ctx,
		`INSERT INTO dbo.tenant_domains (hostname, tenant_id, created_at, updated_at)
		 VALUES (@p1, @p2, SYSUTCDATETIME(), SYSUTCDATETIME())`, hostA, tenantA); err != nil {
		t.Fatalf("a same-tenant INSERT into tenant_domains failed: %v", err)
	}
	t.Cleanup(func() { _, _ = storeA.db.Exec(`DELETE FROM dbo.tenant_domains WHERE hostname = @p1`, hostA) })

	// A TENANT-MOVING UPDATE IS REFUSED: storeA owns the row it just wrote;
	// this UPDATE, still under tenant A's session context, tries to move it
	// to tenant B.
	_, err = storeA.db.ExecContext(ctx,
		`UPDATE dbo.tenant_domains SET tenant_id = @p2 WHERE hostname = @p1`, hostA, tenantB)
	if err == nil {
		t.Fatal("an UPDATE moving tenant_domains.tenant_id to another tenant succeeded -- the " +
			"AFTER UPDATE block predicate did not fire")
	}
	if !isMSSQLBlockPredicateError(err) {
		t.Fatalf("tenant-moving UPDATE failed, but not with a block-predicate error: %v", err)
	}

	// The row must be exactly as it was: still tenant A's, unmoved.
	var gotTenant string
	if err := storeA.db.QueryRowContext(ctx,
		`SELECT CAST(tenant_id AS NVARCHAR(36)) FROM dbo.tenant_domains WHERE hostname = @p1`, hostA,
	).Scan(&gotTenant); err != nil {
		t.Fatalf("read back the row after the refused UPDATE: %v", err)
	}
	// SQL Server's CAST(UNIQUEIDENTIFIER AS NVARCHAR) renders upper-case hex.
	if !strings.EqualFold(gotTenant, tenantA) {
		t.Errorf("tenant_domains row's tenant_id is %q after a refused move, want %q (unchanged)",
			gotTenant, tenantA)
	}

	// LEGITIMATE SAME-TENANT UPDATE, NOT TOUCHING tenant_id, STILL SUCCEEDS.
	if _, err := storeA.db.ExecContext(ctx,
		`UPDATE dbo.tenant_domains SET updated_at = SYSUTCDATETIME() WHERE hostname = @p1`, hostA,
	); err != nil {
		t.Fatalf("a same-tenant UPDATE not touching tenant_id failed: %v", err)
	}

	// storeB, a genuinely different tenant's own connection, sees none of
	// this -- the FILTER half of the same policy, unaffected by any of the
	// above.
	var count int
	if err := storeB.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dbo.tenant_domains WHERE hostname = @p1`, hostA,
	).Scan(&count); err != nil {
		t.Fatalf("tenant B counting tenant A's row: %v", err)
	}
	if count != 0 {
		t.Errorf("tenant B's own connection can see tenant A's tenant_domains row (count=%d)", count)
	}
}

// isMSSQLBlockPredicateError reports whether err is SQL Server's error 33504
// ("has a block predicate that conflicts with this operation"), rather than
// some other failure a broken test could also produce (a typo'd column
// name, a closed connection). A refusal for the wrong reason is not a
// passing test -- see TestMSSQLBlockPredicatesRejectCrossTenantWrites'
// comment on why tenant_domains rather than a table whose FK would refuse
// the same INSERT first, for an unrelated reason, and read as this check
// passing regardless of whether the predicate exists.
func isMSSQLBlockPredicateError(err error) bool {
	return err != nil &&
		(strings.Contains(err.Error(), "block predicate") || strings.Contains(err.Error(), "33504"))
}

// TestEveryMSSQLFilterPredicateHasAMatchingBlockPredicate is the guard
// cleat#2205 asks for: it fails if a FILTER predicate on dbo.fn_tenant_filter
// exists with no BLOCK predicate for all three of AFTER INSERT, AFTER
// UPDATE and BEFORE UPDATE -- whether because 103 missed a table that
// existed when it was written, or because a migration added AFTER 103 binds
// a new table to fn_tenant_filter's FILTER without adding the matching
// BLOCK the way 103 itself would have (a plugin registering its own
// TenantFilter_* policy is exactly this case, and is the reason 103 derives
// its table set from sys.security_predicates rather than a literal list --
// see its own header comment).
//
// Queries live state rather than parsing migration files textually, for the
// same reason 075 and cross_tenant_claim.sql derive their own table sets
// live (see their comments): a table's CURRENT predicate set is the only
// thing that answers "is this table protected right now", and it is what a
// production database actually has, independent of which migration (or
// operator script) put it there.
func TestEveryMSSQLFilterPredicateHasAMatchingBlockPredicate(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}
	db := openMSSQLTenantStore(t, DefaultTenantUUID).db

	rows, err := db.Query(`
		SELECT o.name AS tbl, pred.predicate_type_desc, pred.operation_desc
		FROM sys.security_predicates pred
		JOIN sys.objects o ON o.object_id = pred.target_object_id
		WHERE pred.predicate_definition LIKE '%fn_tenant_filter%'
	`)
	if err != nil {
		t.Fatalf("query sys.security_predicates: %v", err)
	}
	defer rows.Close()

	const (
		filter = "FILTER"
		block  = "BLOCK"
	)
	wantOps := []string{"AFTER INSERT", "AFTER UPDATE", "BEFORE UPDATE"}

	haveFilter := map[string]bool{}
	haveBlock := map[string]map[string]bool{}
	for rows.Next() {
		var tbl, predType string
		var op *string
		if err := rows.Scan(&tbl, &predType, &op); err != nil {
			t.Fatalf("scan: %v", err)
		}
		switch predType {
		case filter:
			haveFilter[tbl] = true
		case block:
			if haveBlock[tbl] == nil {
				haveBlock[tbl] = map[string]bool{}
			}
			if op != nil {
				haveBlock[tbl][*op] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sys.security_predicates: %v", err)
	}

	// A zero here is a capture failure, not a small schema -- see 103's own
	// header comment for the identical reasoning on its capture query. 001
	// alone binds eight tables on a fresh install.
	if len(haveFilter) == 0 {
		t.Fatal("no table has a FILTER predicate referencing dbo.fn_tenant_filter -- the query " +
			"found nothing rather than the schema having nothing to protect")
	}

	for tbl := range haveFilter {
		for _, op := range wantOps {
			if !haveBlock[tbl][op] {
				t.Errorf("dbo.%s has a FILTER predicate on fn_tenant_filter but no BLOCK "+
					"predicate for %s -- a write reaching this table with the wrong tenant_id "+
					"would not be refused", tbl, op)
			}
		}
	}
}
