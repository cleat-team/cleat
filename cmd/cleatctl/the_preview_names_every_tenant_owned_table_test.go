package main

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// dropTenantTables is a hand-maintained list, and hand-maintained lists of
// tenant-owned tables have drifted three times in this repository:
// tenant_settings (missing for twenty migrations, per its own comment in
// droptenant.go), workflow_defs (cleat#1201), and workflow_memory_{stats,
// samples} (cleat#1644). This asserts the property rather than the names.
//
// WHAT A WRONG PREVIEW COSTS is different from what a wrong DELETE costs, and
// is the reason this is a test and not a comment. `drop-tenant` prints these
// counts twice -- once as the thing the operator confirms, and once afterwards
// as the audit record, because the command has no audit table and says so. A
// table missing from this list is therefore deleted silently and recorded as
// not having existed. droptenant.go's own comment already states the rule this
// enforces: "A preview that does not describe the action is worse than no
// preview."
//
// THE UNIVERSE IS READ FROM information_schema, not from anything in this
// repository, so it is a second derivation that can disagree with the list.
// It found admin.tenant_egress_allow on its first run -- a table that
// admin.drop_tenant removes correctly, by ON DELETE CASCADE from admin.tenants,
// and that the preview had never counted. Reading the function would not have
// found it; nothing in the function names it.
//
// Restricted to public and admin for the reason
// engine/a_dropped_tenants_rows_all_go_with_it_test.go gives at more length:
// per-tenant `tenant_<uuid>` schemas also hold tables with a tenant_id column,
// and those belong to other tenants.
func TestThePreviewNamesEveryTenantOwnedTable(t *testing.T) {
	db := dropTenantTestDB(t)
	ctx := context.Background()

	rows, err := db.QueryContext(ctx, `
		SELECT table_schema, table_name
		FROM information_schema.columns
		WHERE column_name = 'tenant_id'
		  AND table_schema IN ('public', 'admin')
		ORDER BY table_schema, table_name`)
	if err != nil {
		t.Fatalf("enumerate tenant-owned tables: %v", err)
	}
	defer rows.Close()

	var universe []string
	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			t.Fatalf("scan tenant-owned table: %v", err)
		}
		// The list writes public tables unqualified, matching the SQL it
		// carries, and admin tables qualified. Normalised to the list's own
		// spelling rather than the other way round, so a failure names
		// something that can be pasted straight into dropTenantTables.
		if schema == "public" {
			universe = append(universe, name)
		} else {
			universe = append(universe, schema+"."+name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("enumerate tenant-owned tables: %v", err)
	}
	if len(universe) == 0 {
		t.Fatal("no table in public or admin carries a tenant_id column -- this database is " +
			"not migrated, and an empty universe would make every assertion below vacuous")
	}

	listed := map[string]bool{}
	for _, tbl := range dropTenantTables {
		listed[tbl.label] = true
	}

	var missing []string
	for _, table := range universe {
		if !listed[table] {
			missing = append(missing, table)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("dropTenantTables does not count %v.\n\n"+
			"Each carries a tenant_id column, so dropping a tenant removes its rows there -- "+
			"by a DELETE in admin.drop_tenant or by ON DELETE CASCADE from admin.tenants, and "+
			"the operator does not distinguish. Uncounted means deleted without appearing in "+
			"the confirmation prompt or in the summary this command prints as its audit "+
			"record.", missing)
	}

	// The other direction: a label naming a table that no longer exists would
	// make countTenantRows fail at runtime with a confusing error, and the
	// preview would never be seen at all.
	inUniverse := map[string]bool{}
	for _, table := range universe {
		inUniverse[table] = true
	}
	var stale []string
	for _, tbl := range dropTenantTables {
		if !inUniverse[tbl.label] {
			stale = append(stale, tbl.label)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("dropTenantTables names %v, which carry no tenant_id column in public or "+
			"admin. A count against a table that is not there fails the whole command "+
			"before it prints anything", stale)
	}
}

// The labels have to be usable as the thing a failure above tells you to paste,
// and they are also what the operator reads. This catches the transposition
// that a set comparison cannot: a label that does not match the table its own
// query counts.
func TestEveryPreviewLabelNamesTheTableItsQueryCounts(t *testing.T) {
	for _, tbl := range dropTenantTables {
		if !strings.Contains(tbl.query, " "+tbl.label+" ") {
			t.Errorf("the %q row's query does not name %q: %s\n\n"+
				"The label is what the operator reads and what TestThePreviewNamesEvery"+
				"TenantOwnedTable compares against the catalogue, so a label that describes "+
				"a different table than its query counts makes both of those report about "+
				"the wrong table.", tbl.label, tbl.label, tbl.query)
		}
	}
	if len(dropTenantTables) == 0 {
		t.Fatal("dropTenantTables is empty, so the loop above asserted nothing")
	}
}
