package migration_test

// package migration_test (external), as mssql_admin_role_test.go: engine/testutil depends on this package.
//
// cleat#3635 (owner D60). Migration 011 makes SQL Server compare workflow-definition names exactly, as
// PostgreSQL does. This test runs it the way an operator meets it: on a database already built by the
// migrations BEFORE it, holding rows.
//
//  1. A child that matches its parent only by case is refused, with a message that names the row and the
//     tenant, and the database is left exactly as it was (one transaction, transactional DDL).
//  2. The same refusal comes from the foreign keys themselves, for a tenant the preflight loop cannot see.
//  3. Once the rows are repaired the migration applies, every name column is binary, every key is trusted,
//     and a lookup that matched across case before now does not.
//
// WHY THE PREFLIGHT NEEDS A TENANT CONTEXT, measured 2026-10-30: the tenant filter on SQL Server is
// `tenant_id = SESSION_CONTEXT('tenant_id')` with no exemption for a privileged login, so `sa` (db_owner)
// reads ZERO rows of workflow_defs without one. A preflight that only counted would pass on every database.
// Case 1 below fails if the loop is removed, because the orphan is invisible to a plain session.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/migration"
	"github.com/cleat-team/cleat/migration/catalogdiff"
)

const (
	namesTestDB      = "cleat_migration_names_exact_test"
	namesTenantKnown = "33333333-3333-3333-3333-333333333333" // registered in admin.tenants: the preflight reads it
	namesTenantHid   = "44444444-4444-4444-4444-444444444444" // NOT registered: only the foreign keys can see it
	namesVersion     = 11
)

// namesColumns is every column migration 011 converts. Derived from the migration file by hand, so the
// final check below ALSO asks the catalog for any other name-like column the file left alone.
var namesColumns = []string{
	"dbo.workflow_defs.name", "dbo.workflow_defs.library",
	"dbo.workflow_instances.def_name", "dbo.workflow_routing.workflow_name", "dbo.workflow_tags.workflow_name",
	"dbo.tenant_library_installs.library", "dbo.workflow_memory_samples.def_name", "dbo.workflow_memory_stats.def_name",
	"library.members.library", "library.members.name",
	"library.bundle_members.library", "library.bundle_members.name", "library.bundles.library",
	"dbo.workflow_schedules.def_name", "dbo.idempotency_keys.def_name", "dbo.event_history.child_name",
	"admin.plugin_dep_overrides.definition_name",
}

var namesForeignKeys = []string{
	"fk_instances_def", "fk_workflow_routing_def", "fk_workflow_tags_def",
	"fk_workflow_defs_library", "fk_bundle_members_member", "fk_bundle_members_bundle",
}

// chainBefore copies every SQL Server migration older than 011 into a temporary directory, so that the
// shipped runner can build the database exactly as it stood before this change.
func chainBefore(t *testing.T, version int) string {
	t.Helper()
	src := filepath.Join(migrationsRoot(t), "mssql")
	dst := filepath.Join(t.TempDir(), "mssql")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	copied := 0
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		n, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil || n >= version {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
		copied++
	}
	if copied == 0 {
		t.Fatalf("UNMEASURED: no migration older than %03d was found in %s, so there is no 'before' to build", version, src)
	}
	return filepath.Dir(dst) // the runner appends the dialect directory itself
}

// inTenant runs fn on ONE pinned connection with the tenant stamped into its session, which is where the
// row-level security predicate reads it (see mssql_admin_role_test.go for why one connection).
func inTenant(t *testing.T, db *sql.DB, tenant string, fn func(c *sql.Conn)) {
	t.Helper()
	ctx := context.Background()
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, `EXEC sp_set_session_context @key=N'tenant_id', @value=@p1`, tenant); err != nil {
		t.Fatalf("set the tenant context %s: %v", tenant, err)
	}
	fn(c)
}

func mustExec(t *testing.T, c *sql.Conn, q string, args ...any) {
	t.Helper()
	if _, err := c.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("%.80s: %v", strings.Join(strings.Fields(q), " "), err)
	}
}

func countIn(t *testing.T, db *sql.DB, tenant, q string, args ...any) int {
	t.Helper()
	n := -1
	inTenant(t, db, tenant, func(c *sql.Conn) {
		if err := c.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	})
	return n
}

// collations returns schema.table.column -> collation for every column in names.
func collations(t *testing.T, db *sql.DB, names []string) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT SCHEMA_NAME(t.schema_id) + '.' + t.name + '.' + c.name, c.collation_name
		FROM sys.columns c JOIN sys.tables t ON t.object_id = c.object_id WHERE c.collation_name IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	all := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		all[k] = v
	}
	out := map[string]string{}
	for _, n := range names {
		v, ok := all[n]
		if !ok {
			t.Fatalf("UNMEASURED: the catalog has no string column %s, so the list in this test is stale", n)
		}
		out[n] = v
	}
	return out
}

func unchanged(t *testing.T, db *sql.DB, what string) {
	t.Helper()
	for col, coll := range collations(t, db, namesColumns) {
		if coll == "Latin1_General_100_BIN2" {
			t.Errorf("%s: %s is already binary, so the refusal was not atomic", what, col)
		}
	}
	for _, fk := range namesForeignKeys {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sys.foreign_keys WHERE name = @p1`, fk).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s: foreign key %s is missing after a refused migration (n=%d, err=%v)", what, fk, n, err)
		}
	}
	var applied int
	// version is stored as text without padding ('8', not '008'); see schema_migrations.
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = N'11'`).Scan(&applied); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if applied != 0 {
		t.Errorf("%s: migration 011 is recorded as applied", what)
	}
}

func TestSQLServerWorkflowNamesBecomeExact(t *testing.T) {
	db := newMSSQLScratchDB(t, namesTestDB)
	ctx := context.Background()

	// BEFORE: the chain as it stood without 011.
	if err := runMigrations(t, ctx, migration.NewRunner(db, migration.DialectMSSQL, chainBefore(t, namesVersion)), migration.DialectMSSQL); err != nil {
		t.Fatalf("apply the migrations before 011: %v", err)
	}

	// Precondition, the known-positive for everything below: before 011 the columns FOLD case. If they did
	// not, the refusal cases would be refusing nothing.
	for col, coll := range collations(t, db, []string{"dbo.workflow_defs.name", "dbo.workflow_instances.def_name"}) {
		if coll == "Latin1_General_100_BIN2" {
			t.Fatalf("UNMEASURED: %s is already binary before 011, so this test cannot show the change", col)
		}
	}

	beforeCatalog, err := catalogdiff.Snapshot(ctx, db, migration.DialectMSSQL)
	if err != nil {
		t.Fatalf("snapshot the catalog before 011: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO admin.tenants (tenant_id, name) VALUES (@p1, N'names-known')`, namesTenantKnown); err != nil {
		t.Fatalf("register the tenant the preflight reads: %v", err)
	}
	// The AFTER INSERT block predicate checks the row's own tenant_id against the session, so it is named.
	defs := func(c *sql.Conn, tenant, name string) {
		t.Helper()
		mustExec(t, c, `INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points, task_queue, tenant_id) VALUES (@p1, 1, 0x00, '[]', 'default', @p2)`, name, tenant)
	}
	inst := func(c *sql.Conn, tenant, id, defName string) {
		t.Helper()
		mustExec(t, c, `INSERT INTO workflow_instances (id, def_name, def_version, tenant_id) VALUES (@p1, @p2, 1, @p3)`, id, defName, tenant)
	}

	// A healthy tenant's rows, and one case-only orphan in the registered tenant.
	inTenant(t, db, namesTenantKnown, func(c *sql.Conn) {
		defs(c, namesTenantKnown, "Billing.Pay")
		inst(c, namesTenantKnown, "run-ok", "Billing.Pay")
		mustExec(t, c, `INSERT INTO workflow_tags (workflow_name, version, tag, tenant_id) VALUES (N'Billing.Pay', 1, N'stable', @p1)`, namesTenantKnown)
		mustExec(t, c, `INSERT INTO workflow_routing (workflow_name, target_version, tenant_id) VALUES (N'Billing.Pay', 1, @p1)`, namesTenantKnown)
		defs(c, namesTenantKnown, "Checkout")
		inst(c, namesTenantKnown, "run-case-orphan", "checkout") // the FK accepts this today: that is the hazard
		// Schedules have NO foreign key: handleCreateSchedule stores def_name as typed and never looks the
		// definition up, yet a due schedule starts its workflow by that name. cleat-review-69 measured the gap.
		sched := func(name, def string) {
			t.Helper()
			mustExec(t, c, `INSERT INTO workflow_schedules (name, def_name, entry_point, cron_expression, input, next_run_at, tenant_id) VALUES (@p1, @p2, N'main', N'* * * * *', N'{}', SYSDATETIMEOFFSET(), @p3)`, name, def, namesTenantKnown)
		}
		sched("nightly", "checkout")     // reaches 'Checkout' ONLY by case: must be refused
		sched("weekly", "Billing.Pay")   // exact: must not be
		sched("ghost", "NoSuchWorkflow") // names no definition in any case: already broken, not this migration's to refuse
	})

	// The lookup 011 changes, measured BEFORE so that "0 afterwards" is a change and not a miss.
	const folded = `SELECT COUNT(*) FROM workflow_defs WHERE name = N'billing.pay'`
	if n := countIn(t, db, namesTenantKnown, folded); n != 1 {
		t.Fatalf("UNMEASURED: before 011 the lowercase probe finds %d of 'Billing.Pay', want 1 (the column should fold case)", n)
	}
	// The schedule-to-definition join the reviewer measured: it finds 'nightly' BEFORE 011 (so "0 afterwards" would
	// be a change and not a miss), finds the exact schedule, and finds nothing for the ghost.
	const schedJoin = `SELECT COUNT(*) FROM workflow_schedules s JOIN workflow_defs d ON d.tenant_id = s.tenant_id AND d.name = s.def_name WHERE s.name = N'%s'`
	for name, want := range map[string]int{"nightly": 1, "weekly": 1, "ghost": 0} {
		if n := countIn(t, db, namesTenantKnown, fmt.Sprintf(schedJoin, name)); n != want {
			t.Fatalf("UNMEASURED: before 011 the schedule %q joins %d definition(s), want %d", name, n, want)
		}
	}
	// And the row is invisible without the tenant context, which is why the preflight iterates tenants.
	var plain int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_defs`).Scan(&plain); err != nil || plain != 0 {
		t.Fatalf("UNMEASURED: a plain session sees %d workflow_defs rows (err=%v); the premise that row-level security hides them from the migrating login does not hold here", plain, err)
	}

	real := migration.NewRunner(db, migration.DialectMSSQL, migrationsRoot(t))

	// 0. FAILS CLOSED on an object it does not know. The preflight's catalog check had only ever PASSED until
	// these two cases: an operator-added index on a name column, and a schema-bound view over one. Each must be
	// named and refused before anything is touched, even though the case-only orphan above is also present
	// (the object check runs first). Dropping the object lets the run go on.
	for _, c := range []struct{ what, create, drop, want string }{
		{"an operator-added index", `CREATE NONCLUSTERED INDEX zz_operator_index ON dbo.workflow_defs (name, version)`, `DROP INDEX zz_operator_index ON dbo.workflow_defs`, "zz_operator_index"},
		{"a schema-bound view", `CREATE VIEW dbo.zz_operator_view WITH SCHEMABINDING AS SELECT tenant_id, name FROM dbo.workflow_defs`, `DROP VIEW dbo.zz_operator_view`, "zz_operator_view"},
	} {
		if _, err := db.Exec(c.create); err != nil {
			t.Fatalf("UNMEASURED: could not create %s to test the fail-closed check: %v", c.what, err)
		}
		err := runMigrations(t, ctx, real, migration.DialectMSSQL)
		if err == nil {
			t.Fatalf("migration 011 applied over %s that it does not know how to rebuild", c.what)
		}
		for _, want := range []string{"cleat#3635", c.want, "not part of the shipped schema"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal over %s does not say %q:\n%v", c.what, want, err)
			}
		}
		unchanged(t, db, "after the unknown-object refusal over "+c.what)
		if _, err := db.Exec(c.drop); err != nil {
			t.Fatalf("drop %s: %v", c.what, err)
		}
	}

	// 1. REFUSED, readably, for the registered tenant.
	err = runMigrations(t, ctx, real, migration.DialectMSSQL)
	if err == nil {
		t.Fatal("migration 011 applied over a child that matches its parent only by case; it must refuse")
	}
	for _, want := range []string{"cleat#3635", "workflow_instances.def_name", namesTenantKnown, "'checkout'@1", "Nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	unchanged(t, db, "after the preflight refusal")

	// 1b. The first repair is done; the SCHEDULE that reaches its definition only by case is next. It must be refused
	// naming the schedule, and the control rows (the exact schedule, and the ghost that names no definition at all)
	// must not be what it refuses: the run below applies with both still present.
	inTenant(t, db, namesTenantKnown, func(c *sql.Conn) {
		mustExec(t, c, `UPDATE workflow_instances SET def_name = N'Checkout' WHERE id = N'run-case-orphan'`)
	})
	err = runMigrations(t, ctx, real, migration.DialectMSSQL)
	if err == nil {
		t.Fatal("migration 011 applied over a schedule that reaches its definition only by case; it must refuse (the schedule would stop firing)")
	}
	for _, want := range []string{"cleat#3635", "workflow_schedules.def_name", namesTenantKnown, "'checkout' (schedule nightly)", "Nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the schedule refusal does not say %q:\n%v", want, err)
		}
	}
	for _, notWant := range []string{"weekly", "ghost", "NoSuchWorkflow"} {
		if strings.Contains(err.Error(), notWant) {
			t.Errorf("the schedule refusal names %q, which is not a case-only match", notWant)
		}
	}
	unchanged(t, db, "after the schedule refusal")
	inTenant(t, db, namesTenantKnown, func(c *sql.Conn) {
		mustExec(t, c, `UPDATE workflow_schedules SET def_name = N'Checkout' WHERE name = N'nightly'`)
	})

	// 2. The same defect in a tenant that is NOT in admin.tenants is invisible to the preflight loop; the foreign
	// keys, recreated WITH CHECK, are the backstop and must refuse it too.
	inTenant(t, db, namesTenantHid, func(c *sql.Conn) {
		defs(c, namesTenantHid, "Hidden.Flow")
		inst(c, namesTenantHid, "run-hidden-orphan", "hidden.flow")
	})
	err = runMigrations(t, ctx, real, migration.DialectMSSQL)
	if err == nil {
		t.Fatal("migration 011 applied over a case-only orphan in a tenant the preflight does not list; the foreign keys must refuse it")
	}
	if !strings.Contains(err.Error(), "fk_instances_def") && !strings.Contains(strings.ToUpper(err.Error()), "FOREIGN KEY") {
		t.Errorf("the second refusal is not the foreign key's:\n%v", err)
	}
	unchanged(t, db, "after the foreign-key refusal")

	// 3. Repair it, and the migration applies. ONE KNOWN LIMITATION IS PINNED HERE, not hidden: a case-only SCHEDULE in
	// the tenant the preflight does not list has no foreign key to refuse it and no loop iteration to see it, so the
	// migration applies over it and the schedule stops reaching its definition. 011's header and the CHANGELOG say so;
	// if that is ever closed this assertion flips on purpose.
	inTenant(t, db, namesTenantHid, func(c *sql.Conn) {
		mustExec(t, c, `UPDATE workflow_instances SET def_name = N'Hidden.Flow' WHERE id = N'run-hidden-orphan'`)
		mustExec(t, c, `INSERT INTO workflow_schedules (name, def_name, entry_point, cron_expression, input, next_run_at, tenant_id) VALUES (N'hidden-sched', N'hidden.flow', N'main', N'* * * * *', N'{}', SYSDATETIMEOFFSET(), @p1)`, namesTenantHid)
	})
	const hiddenSchedJoin = `SELECT COUNT(*) FROM workflow_schedules s JOIN workflow_defs d ON d.tenant_id = s.tenant_id AND d.name = s.def_name WHERE s.name = N'hidden-sched'`
	if n := countIn(t, db, namesTenantHid, hiddenSchedJoin); n != 1 {
		t.Fatalf("UNMEASURED: before 011 the unlisted tenant's case-only schedule joins %d definition(s), want 1", n)
	}
	if err := runMigrations(t, ctx, real, migration.DialectMSSQL); err != nil {
		t.Fatalf("migration 011 refused a database with no case-only orphans: %v", err)
	}

	for col, coll := range collations(t, db, namesColumns) {
		if coll != "Latin1_General_100_BIN2" {
			t.Errorf("%s is %s after 011, want Latin1_General_100_BIN2", col, coll)
		}
	}
	for _, fk := range namesForeignKeys {
		var trusted sql.NullInt64
		if err := db.QueryRow(`SELECT CAST(1 - is_not_trusted AS int) FROM sys.foreign_keys WHERE name = @p1`, fk).Scan(&trusted); err != nil || trusted.Int64 != 1 {
			t.Errorf("foreign key %s is not present and trusted after 011 (trusted=%v, err=%v)", fk, trusted, err)
		}
	}

	// 011 changes COLLATIONS and nothing else. The keys, indexes, check constraint and foreign keys were dropped
	// and recreated by hand, and this is what says they came back as they were: diff the catalog before and
	// after, and every difference must be one column whose only change is its collation.
	afterCatalog, err := catalogdiff.Snapshot(ctx, db, migration.DialectMSSQL)
	if err != nil {
		t.Fatalf("snapshot the catalog after 011: %v", err)
	}
	diff := catalogdiff.Diff(beforeCatalog, afterCatalog)
	norm := regexp.MustCompile(`collation=\S+`)
	removed, added := map[string]int{}, map[string]int{}
	for _, d := range diff {
		body := norm.ReplaceAllString(d[2:], "collation=*")
		if !strings.Contains(body, " COLUMN ") {
			t.Errorf("011 changed something other than a column's collation: %s", d)
			continue
		}
		if d[0] == '-' {
			removed[body]++
		} else {
			added[body]++
		}
	}
	for body, n := range removed {
		if added[body] != n {
			t.Errorf("a column changed in more than its collation: %s", body)
		}
	}
	if len(removed) != len(namesColumns) || len(added) != len(namesColumns) {
		t.Errorf("011 changed the collation of %d column(s), want %d (the list at the top of this file)", len(removed), len(namesColumns))
	}

	// The pinned limitation: the unlisted tenant's case-only schedule was NOT refused, and no longer reaches its definition.
	if n := countIn(t, db, namesTenantHid, hiddenSchedJoin); n != 0 {
		t.Errorf("after 011 the unlisted tenant's case-only schedule still joins %d definition(s); the documented limitation has changed, so the header and CHANGELOG must too", n)
	}

	// The change a user sees: the lowercase probe no longer finds the mixed-case name; the exact one does,
	// and both spellings can now be registered side by side, as on PostgreSQL.
	if n := countIn(t, db, namesTenantKnown, folded); n != 0 {
		t.Errorf("after 011 a lowercase probe still finds %d of 'Billing.Pay'", n)
	}
	if n := countIn(t, db, namesTenantKnown, `SELECT COUNT(*) FROM workflow_defs WHERE name = N'Billing.Pay'`); n != 1 {
		t.Errorf("after 011 the exact name finds %d rows, want 1: the conversion lost the row", n)
	}
	if n := countIn(t, db, namesTenantKnown, `SELECT COUNT(*) FROM workflow_instances i JOIN workflow_defs d ON d.name = i.def_name AND d.version = i.def_version`); n != 2 {
		t.Errorf("after 011 the instance-to-definition join finds %d rows, want 2 (run-ok and run-case-orphan, repaired)", n)
	}
	inTenant(t, db, namesTenantKnown, func(c *sql.Conn) {
		defs(c, namesTenantKnown, "billing.pay") // would be a duplicate primary key before 011
	})
	if n := countIn(t, db, namesTenantKnown, `SELECT COUNT(*) FROM workflow_defs WHERE name IN (N'Billing.Pay', N'billing.pay')`); n != 2 {
		t.Errorf("after 011 both spellings should exist as two definitions, found %d", n)
	}

	// A second run finds nothing to do.
	if err := runMigrations(t, ctx, real, migration.DialectMSSQL); err != nil {
		t.Errorf("running the migrations again after 011: %v", err)
	}

	// And 011 is safe to run AGAIN over a database it has already converted, which is what `--migrate-only`
	// does when a tracking row is missing (cmd/cleat-worker's TestMigrationIsADeployStepOnEveryDialect). The
	// preflight builds its refusal text from columns that are now binary beside columns that are not
	// (library vs bundle_version), and SQL Server cannot resolve two different implicit collations in one
	// expression: this failed with error 4191 until the text was built with an explicit collation.
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = N'11'`); err != nil {
		t.Fatalf("remove the 011 tracking row: %v", err)
	}
	if err := runMigrations(t, ctx, real, migration.DialectMSSQL); err != nil {
		t.Errorf("011 re-run over an already-converted database: %v", err)
	}
	for col, coll := range collations(t, db, namesColumns) {
		if coll != "Latin1_General_100_BIN2" {
			t.Errorf("%s is %s after the re-run", col, coll)
		}
	}
}

// TestEveryStringColumnNamedForAWorkflowDefinitionIsExactAfter011 asks the catalog, not the migration file,
// which string columns look like definition names, so that a column a LATER migration adds under the same
// naming and leaves case-insensitive is found here instead of in a customer's data. It is deliberately
// loose: a name it flags that is not a definition name goes in `notDefinitionNames`, with a reason.
func TestEveryStringColumnNamedForAWorkflowDefinitionIsExactAfter011(t *testing.T) {
	db := newMSSQLScratchDB(t, namesTestDB+"_catalog")
	if err := runMigrations(t, context.Background(), migration.NewRunner(db, migration.DialectMSSQL, migrationsRoot(t)), migration.DialectMSSQL); err != nil {
		t.Fatalf("apply the shipped SQL Server migrations: %v", err)
	}
	// Definition-name columns that plugins create at worker boot (the scheduler, eventtriggers, jobqueue) are
	// not in this database and stay case-insensitive until their own migrations convert them: cleat#3641.
	rows, err := db.Query(`SELECT SCHEMA_NAME(t.schema_id) + '.' + t.name + '.' + c.name, c.collation_name
		FROM sys.columns c JOIN sys.tables t ON t.object_id = c.object_id
		WHERE c.collation_name IS NOT NULL
		  AND (c.name IN (N'def_name', N'workflow_name', N'definition_name', N'child_name')
		       OR (t.name IN (N'workflow_defs', N'members', N'bundle_members', N'bundles') AND c.name IN (N'name', N'library')))`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var col, coll string
		if err := rows.Scan(&col, &coll); err != nil {
			t.Fatal(err)
		}
		seen++
		if coll != "Latin1_General_100_BIN2" {
			t.Errorf("%s is %s: a definition-name column that 011 left case-insensitive (add it to the migration, or explain it here)", col, coll)
		}
	}
	if seen < len(namesColumns)-1 { // this query cannot see tenant_library_installs.library, whose name is not name-like
		t.Fatalf("UNMEASURED: the catalog query found only %d name-like columns; the probe is not looking at the schema it was written for", seen)
	}
}
