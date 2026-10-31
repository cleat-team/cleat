package migration_test

// package migration_test (external), as mssql_admin_role_test.go: engine/testutil depends on this package.
//
// cleat#3640 (owner D60-b). Migration 012 makes MySQL compare workflow-definition names exactly, as PostgreSQL and
// (since 011) SQL Server do. Run the way an operator meets it: on a database already built by the migrations BEFORE
// it, holding rows. MySQL differs from SQL Server in the one way that shapes this file: DDL is not transactional, so
// the migration cannot roll back, and "what a failure leaves behind" is a thing to MEASURE, not to assert in prose.
//
//  1. A child that matches its parent only by case OR ONLY BY ACCENT is refused, naming the table and the first
//     offender, within the 128 characters SIGNAL allows, and nothing is changed. The operator's query for listing
//     every offender (the three keyed kinds and the schedules) is the one in CHANGELOG.md, and this test runs that text.
//  2. A run that fails part-way leaves exactly the state the migration header documents, and running it again
//     converges. The failure is a lock timeout on the last table.
//  3. Afterwards every name column is utf8mb4_0900_bin, the keys are back, and the catalog differs only in those
//     ten collations.

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

	mysqldriver "github.com/go-sql-driver/mysql"

	"github.com/cleat-team/cleat/migration"
	"github.com/cleat-team/cleat/migration/catalogdiff"
)

const (
	mysqlNamesTestDB  = "cleat_migration_names_exact_mysql_test"
	mysqlNamesVersion = 12
	mysqlNamesFile    = "012_workflow_names_are_compared_exactly.sql"
)

var mysqlNamesColumns = []string{
	"workflow_defs.name", "workflow_instances.def_name", "workflow_routing.workflow_name", "workflow_tags.workflow_name",
	"workflow_memory_samples.def_name", "workflow_memory_stats.def_name", "workflow_schedules.def_name",
	"idempotency_keys.def_name", "event_history.child_name", "plugin_dep_overrides.definition_name",
}

var mysqlNamesForeignKeys = []string{"fk_instances_def", "fk_workflow_routing_def", "fk_workflow_tags_def"}

// mysqlOrphanQuery is the text CHANGELOG.md gives an operator. It is a constant here so the test runs the same bytes.
const mysqlOrphanQuery = "SELECT CAST('workflow_instances' AS CHAR) COLLATE utf8mb4_0900_bin AS child, c.def_name COLLATE utf8mb4_0900_bin AS name, CAST(c.def_version AS CHAR) COLLATE utf8mb4_0900_bin AS detail FROM workflow_instances c WHERE NOT EXISTS (SELECT 1 FROM workflow_defs p WHERE p.tenant_id = c.tenant_id AND p.version = c.def_version AND p.name COLLATE utf8mb4_0900_bin = c.def_name COLLATE utf8mb4_0900_bin) " +
	"UNION ALL SELECT CAST('workflow_routing' AS CHAR) COLLATE utf8mb4_0900_bin, c.workflow_name COLLATE utf8mb4_0900_bin, CAST(c.target_version AS CHAR) COLLATE utf8mb4_0900_bin FROM workflow_routing c WHERE c.tenant_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM workflow_defs p WHERE p.tenant_id = c.tenant_id AND p.version = c.target_version AND p.name COLLATE utf8mb4_0900_bin = c.workflow_name COLLATE utf8mb4_0900_bin) " +
	"UNION ALL SELECT CAST('workflow_tags' AS CHAR) COLLATE utf8mb4_0900_bin, c.workflow_name COLLATE utf8mb4_0900_bin, CAST(c.version AS CHAR) COLLATE utf8mb4_0900_bin FROM workflow_tags c WHERE NOT EXISTS (SELECT 1 FROM workflow_defs p WHERE p.tenant_id = c.tenant_id AND p.version = c.version AND p.name COLLATE utf8mb4_0900_bin = c.workflow_name COLLATE utf8mb4_0900_bin) " +
	"UNION ALL SELECT CAST('workflow_schedules' AS CHAR) COLLATE utf8mb4_0900_bin, c.def_name COLLATE utf8mb4_0900_bin, c.name COLLATE utf8mb4_0900_bin FROM workflow_schedules c WHERE EXISTS (SELECT 1 FROM workflow_defs p WHERE p.tenant_id = c.tenant_id AND p.name = c.def_name) AND NOT EXISTS (SELECT 1 FROM workflow_defs p WHERE p.tenant_id = c.tenant_id AND p.name COLLATE utf8mb4_0900_bin = c.def_name COLLATE utf8mb4_0900_bin)"

// mysqlChainBefore copies every MySQL migration older than version into a temporary directory.
func mysqlChainBefore(t *testing.T, version int) string {
	t.Helper()
	src := filepath.Join(migrationsRoot(t), "mysql")
	dst := filepath.Join(t.TempDir(), "mysql")
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
		t.Fatalf("UNMEASURED: no MySQL migration older than %03d in %s, so there is no 'before' to build", version, src)
	}
	return filepath.Dir(dst)
}

func mysqlCollations(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, c := range mysqlNamesColumns {
		parts := strings.SplitN(c, ".", 2)
		var coll sql.NullString
		if err := db.QueryRow(`SELECT collation_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`, parts[0], parts[1]).Scan(&coll); err != nil {
			t.Fatalf("UNMEASURED: read the collation of %s: %v", c, err)
		}
		out[c] = coll.String
	}
	return out
}

func mysqlFKCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	n := 0
	for _, fk := range mysqlNamesForeignKeys {
		var c int
		if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.table_constraints WHERE constraint_schema = DATABASE() AND constraint_name = ? AND constraint_type = 'FOREIGN KEY'`, fk).Scan(&c); err != nil {
			t.Fatal(err)
		}
		n += c
	}
	return n
}

// mysqlUnchanged is what "a refusal changes nothing" means: no column is converted, all three keys exist, and the
// migration is not recorded.
func mysqlUnchanged(t *testing.T, db *sql.DB, what string) {
	t.Helper()
	for c, coll := range mysqlCollations(t, db) {
		if coll == "utf8mb4_0900_bin" {
			t.Errorf("%s: %s is already utf8mb4_0900_bin, so the refusal was not atomic", what, c)
		}
	}
	if n := mysqlFKCount(t, db); n != 3 {
		t.Errorf("%s: %d of 3 foreign keys remain", what, n)
	}
	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, mysqlNamesFile).Scan(&applied); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if applied != 0 {
		t.Errorf("%s: migration 012 is recorded as applied", what)
	}
}

func mysqlExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%.90s: %v", strings.Join(strings.Fields(q), " "), err)
	}
}

func mysqlCount(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestMySQLWorkflowNamesBecomeExact(t *testing.T) {
	db := newMySQLScratchDB(t, mysqlNamesTestDB)
	ctx := context.Background()

	// BEFORE: the chain as it stood without 012.
	if err := runMigrations(t, ctx, migration.NewRunner(db, migration.DialectMySQL, mysqlChainBefore(t, mysqlNamesVersion)), migration.DialectMySQL); err != nil {
		t.Fatalf("apply the migrations before 012: %v", err)
	}
	for c, coll := range mysqlCollations(t, db) {
		if coll == "utf8mb4_0900_bin" {
			t.Fatalf("UNMEASURED: %s is already utf8mb4_0900_bin before 012, so this test cannot show the change", c)
		}
	}
	beforeCatalog, err := catalogdiff.Snapshot(ctx, db, migration.DialectMySQL)
	if err != nil {
		t.Fatalf("snapshot the catalog before 012: %v", err)
	}

	// Rows: a healthy chain, a name that exists only with an accent, and three orphans the old collation's keys
	// accepted: a case-only child, an accent-only child, and a case-only tag.
	for _, n := range []string{"Billing.Pay", "Checkout", "Résumé"} {
		mysqlExec(t, db, `INSERT INTO workflow_defs (name, version, wasm_bytes) VALUES (?, 1, 0x0061736d)`, n)
	}
	mysqlExec(t, db, `INSERT INTO workflow_instances (id, def_name, def_version) VALUES ('run-ok', 'Billing.Pay', 1), ('run-case-orphan', 'checkout', 1)`)
	mysqlExec(t, db, `INSERT INTO workflow_routing (id, workflow_name, target_version, tenant_id) VALUES (UUID(), 'Billing.Pay', 1, '00000000-0000-0000-0000-000000000000'), (UUID(), 'Resume', 1, '00000000-0000-0000-0000-000000000000')`) // 'Resume' is the accent-only orphan
	// A routing row with NO tenant (the one nullable key column): its foreign key is never enforced, so it is not an
	// orphan of it. A case-different name here must be neither listed nor refused.
	mysqlExec(t, db, `INSERT INTO workflow_routing (id, workflow_name, target_version) VALUES (UUID(), 'BILLING.PAY', 1)`)
	mysqlExec(t, db, `INSERT INTO workflow_tags (workflow_name, version, tag) VALUES ('Billing.Pay', 1, 'stable'), ('billing.pay', 1, 'case-orphan')`)
	// Schedules have NO foreign key: handleCreateSchedule stores def_name as typed and never looks the definition up,
	// yet a due schedule starts its workflow by that name (cleat-review-69 measured the gap on SQL Server, cleat#3642).
	mysqlExec(t, db, `INSERT INTO workflow_schedules (name, def_name, cron_expression) VALUES
		('nightly', 'checkout', '* * * * *'),          -- reaches 'Checkout' ONLY by case: must be refused
		('weekly', 'Billing.Pay', '* * * * *'),        -- exact: must not be
		('ghost', 'NoSuchWorkflow', '* * * * *')       -- names no definition in any case: already broken, not refused`)
	const schedJoin = `SELECT COUNT(*) FROM workflow_schedules s JOIN workflow_defs d ON d.tenant_id = s.tenant_id AND d.name = s.def_name WHERE s.name = '%s'`
	for name, want := range map[string]int{"nightly": 1, "weekly": 1, "ghost": 0} {
		if n := mysqlCount(t, db, fmt.Sprintf(schedJoin, name)); n != want {
			t.Fatalf("UNMEASURED: before 012 the schedule %q joins %d definition(s), want %d", name, n, want)
		}
	}

	// The lookups 012 changes, measured BEFORE so that "0 afterwards" is a change and not a miss.
	const lower = `SELECT COUNT(*) FROM workflow_defs WHERE name = 'billing.pay'`
	const stripped = `SELECT COUNT(*) FROM workflow_defs WHERE name = 'Resume'`
	if n := mysqlCount(t, db, lower); n != 1 {
		t.Fatalf("UNMEASURED: before 012 the lowercase probe finds %d of 'Billing.Pay', want 1 (the column should fold case)", n)
	}
	if n := mysqlCount(t, db, stripped); n != 1 {
		t.Fatalf("UNMEASURED: before 012 the accent-stripped probe finds %d of 'Résumé', want 1 (the column should fold accents)", n)
	}

	// The operator's query, the text CHANGELOG.md gives, lists all three orphans while they exist.
	rows, err := db.Query(mysqlOrphanQuery)
	if err != nil {
		t.Fatalf("the documented orphan query: %v", err)
	}
	var listed []string
	for rows.Next() {
		var child, name, detail string
		if err := rows.Scan(&child, &name, &detail); err != nil {
			t.Fatal(err)
		}
		listed = append(listed, fmt.Sprintf("%s/%s@%s", child, name, detail))
	}
	rows.Close()
	if got := strings.Join(listed, ","); !(len(listed) == 4 && strings.Contains(got, "workflow_instances/checkout@1") &&
		strings.Contains(got, "workflow_routing/Resume@1") && strings.Contains(got, "workflow_tags/billing.pay@1") &&
		strings.Contains(got, "workflow_schedules/checkout@nightly")) {
		t.Errorf("the documented orphan query lists %v, want the four seeded orphans (three keyed kinds and the case-only schedule; not the exact schedule, the ghost, or the NULL-tenant routing row)", listed)
	}
	changelog, err := os.ReadFile(filepath.Join(migrationsRoot(t), "..", "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(changelog), mysqlOrphanQuery) {
		t.Error("CHANGELOG.md does not contain the orphan query this test runs, byte for byte; the release note and the test have drifted")
	}

	real := migration.NewRunner(db, migration.DialectMySQL, migrationsRoot(t))

	// 1. REFUSED, one orphan at a time, each naming its own table, within 128 characters.
	for _, step := range []struct {
		says   string
		want   string
		repair string
	}{
		{"no exact parent", "workflow_instances.def_name, first checkout@1", `UPDATE workflow_instances SET def_name = 'Checkout' WHERE id = 'run-case-orphan'`},
		{"no exact parent", "workflow_routing.workflow_name, first Resume@1", `UPDATE workflow_routing SET workflow_name = 'Résumé' WHERE workflow_name = 'Resume'`},
		{"no exact parent", "workflow_tags.workflow_name, first billing.pay@1", `UPDATE workflow_tags SET workflow_name = 'Billing.Pay', tag = 'repaired' WHERE tag = 'case-orphan'`},
		{"case/accent", "workflow_schedules (name=def_name), first nightly=checkout", `UPDATE workflow_schedules SET def_name = 'Checkout' WHERE name = 'nightly'`},
	} {
		err := runMigrations(t, ctx, real, migration.DialectMySQL)
		if err == nil {
			t.Fatalf("migration 012 applied over a mismatch it must refuse (expected %q)", step.want)
		}
		for _, w := range []string{"cleat#3640 refused", "nothing changed", step.says, step.want} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("the refusal does not say %q:\n%v", w, err)
			}
		}
		for _, notWant := range []string{"weekly", "ghost", "NoSuchWorkflow"} {
			if strings.Contains(err.Error(), notWant) {
				t.Errorf("the refusal names %q, which is not a case-only match:\n%v", notWant, err)
			}
		}
		if strings.Contains(err.Error(), "Data too long") || strings.Contains(err.Error(), "1648") {
			t.Errorf("the refusal text exceeded SIGNAL's 128 characters and was replaced by error 1648:\n%v", err)
		}
		mysqlUnchanged(t, db, "after the refusal for "+step.want)
		mysqlExec(t, db, step.repair)
	}
	if rows, err := db.Query(mysqlOrphanQuery); err != nil {
		t.Fatal(err)
	} else {
		if rows.Next() {
			t.Error("the documented orphan query still lists a row after every orphan was repaired")
		}
		rows.Close()
	}

	// 2. A FAILURE PART-WAY. Another session holds a write lock on idempotency_keys, the ninth of ten tables; the
	// migrating session waits for the metadata lock only 2 seconds. MySQL cannot roll back, so this measures the
	// state the migration header documents. (event_history is NOT the table to lock: it holds a foreign key to
	// workflow_instances, so ALTERing workflow_instances needs a metadata lock on it, and locking it fails the run
	// at its FIRST statement, before anything is converted -- a benign state, but not the one being tested.)
	cfg, err := mysqldriver.ParseDSN(os.Getenv("CLEAT_TEST_MYSQL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = mysqlNamesTestDB
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["lock_wait_timeout"] = "2"
	short, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer short.Close()

	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.ExecContext(ctx, `LOCK TABLES idempotency_keys WRITE`); err != nil {
		t.Fatalf("UNMEASURED: could not hold a write lock on idempotency_keys: %v", err)
	}
	err = runMigrations(t, ctx, migration.NewRunner(short, migration.DialectMySQL, migrationsRoot(t)), migration.DialectMySQL)
	if err == nil {
		holder.ExecContext(ctx, `UNLOCK TABLES`)
		holder.Close()
		t.Fatal("UNMEASURED: the migration succeeded while idempotency_keys was write-locked, so the partial-failure state was not produced")
	}
	t.Logf("the injected failure: %v", err)
	if _, err := holder.ExecContext(ctx, `UNLOCK TABLES`); err != nil {
		t.Fatal(err)
	}
	holder.Close()

	coll := mysqlCollations(t, db)
	converted := 0
	for c, v := range coll {
		if v == "utf8mb4_0900_bin" {
			converted++
		} else if c != "idempotency_keys.def_name" && c != "event_history.child_name" {
			t.Errorf("after the part-way failure %s is %s, want utf8mb4_0900_bin (only the locked table and the one after it should be left)", c, v)
		}
	}
	if converted != len(mysqlNamesColumns)-2 {
		t.Errorf("after the part-way failure %d columns are converted, want %d", converted, len(mysqlNamesColumns)-2)
	}
	if n := mysqlFKCount(t, db); n != 0 {
		t.Errorf("after the part-way failure %d foreign keys remain; the header says all three are dropped until the migration completes", n)
	}
	// The header's claim about the mixed state, measured: comparing a converted name column with an unconverted one
	// does NOT fail (an earlier draft of the header said error 1267, and was wrong); the binary side wins, so a
	// case-different pair silently does not match. The unconverted column still folds case against itself.
	mysqlExec(t, db, `INSERT INTO event_history (workflow_id, step, event_type, thread_id, local_step, global_seq, child_name) VALUES ('run-ok', 1, 'child_started', 'main', 1, 1, 'billing.pay'), ('run-ok', 2, 'child_started', 'main', 1, 2, 'BILLING.PAY')`)
	if n := mysqlCount(t, db, `SELECT COUNT(*) FROM workflow_defs d JOIN event_history h ON h.child_name = d.name`); n != 0 {
		t.Errorf("a converted-vs-unconverted comparison matched %d case-different rows; the binary side should have won and matched none", n)
	}
	if n := mysqlCount(t, db, `SELECT COUNT(*) FROM event_history a JOIN event_history b ON a.child_name = b.child_name AND a.step < b.step`); n != 1 {
		t.Errorf("two UNCONVERTED columns matched %d case-different pairs, want 1: they should still fold case among themselves", n)
	}

	// 2b. THE WINDOW IS REAL: with the keys gone, a row naming a definition that does not exist in ANY spelling is
	// accepted. Re-running while it is still there must be refused, with nothing changed (still 8 of 10, still no keys);
	// removing it and re-running converges. This is the header's claim about what a failed run leaves behind.
	mysqlExec(t, db, `INSERT INTO workflow_instances (id, def_name, def_version) VALUES ('run-window-orphan', 'No.Such.Definition', 1)`)
	err = runMigrations(t, ctx, real, migration.DialectMySQL)
	if err == nil {
		t.Fatal("migration 012 applied over an orphan written while the keys were missing; it must refuse")
	}
	for _, w := range []string{"cleat#3640 refused", "nothing changed", "no exact parent", "workflow_instances.def_name, first No.Such.Definition@1"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the refusal of a window orphan does not say %q:\n%v", w, err)
		}
	}
	after := 0
	for _, v := range mysqlCollations(t, db) {
		if v == "utf8mb4_0900_bin" {
			after++
		}
	}
	if after != converted || mysqlFKCount(t, db) != 0 {
		t.Errorf("the refusal of a window orphan changed state: %d of %d columns converted (was %d), %d keys (was 0)", after, len(mysqlNamesColumns), converted, mysqlFKCount(t, db))
	}
	mysqlExec(t, db, `DELETE FROM workflow_instances WHERE id = 'run-window-orphan'`)

	// 3. Run it again: that is the documented recovery, and it converges.
	if err := runMigrations(t, ctx, real, migration.DialectMySQL); err != nil {
		t.Fatalf("re-running 012 after the part-way failure: %v", err)
	}
	for c, v := range mysqlCollations(t, db) {
		if v != "utf8mb4_0900_bin" {
			t.Errorf("%s is %s after 012, want utf8mb4_0900_bin", c, v)
		}
	}
	if n := mysqlFKCount(t, db); n != 3 {
		t.Errorf("%d of 3 foreign keys exist after 012", n)
	}

	// 012 changes COLLATIONS and nothing else: every catalog difference is one column whose only change is its collation.
	afterCatalog, err := catalogdiff.Snapshot(ctx, db, migration.DialectMySQL)
	if err != nil {
		t.Fatalf("snapshot the catalog after 012: %v", err)
	}
	norm := regexp.MustCompile(`collation=\S+`)
	removed, added := map[string]int{}, map[string]int{}
	for _, d := range catalogdiff.Diff(beforeCatalog, afterCatalog) {
		body := norm.ReplaceAllString(d[2:], "collation=*")
		if !strings.Contains(body, " COLUMN ") {
			t.Errorf("012 changed something other than a column's collation: %s", d)
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
	if len(removed) != len(mysqlNamesColumns) || len(added) != len(mysqlNamesColumns) {
		t.Errorf("012 changed the collation of %d column(s), want %d", len(removed), len(mysqlNamesColumns))
	}

	// What a user sees: neither probe finds the name any more; the exact spelling does; both can be registered.
	if n := mysqlCount(t, db, lower); n != 0 {
		t.Errorf("after 012 a lowercase probe still finds %d of 'Billing.Pay'", n)
	}
	if n := mysqlCount(t, db, stripped); n != 0 {
		t.Errorf("after 012 an accent-stripped probe still finds %d of 'Résumé'", n)
	}
	if n := mysqlCount(t, db, `SELECT COUNT(*) FROM workflow_defs WHERE name = 'Résumé'`); n != 1 {
		t.Errorf("after 012 the exact accented name finds %d rows, want 1: the conversion lost the row", n)
	}
	mysqlExec(t, db, `INSERT INTO workflow_defs (name, version, wasm_bytes) VALUES ('billing.pay', 1, 0x00), ('Resume', 1, 0x00)`)
	if n := mysqlCount(t, db, `SELECT COUNT(*) FROM workflow_defs WHERE name IN ('Billing.Pay', 'billing.pay', 'Résumé', 'Resume')`); n != 4 {
		t.Errorf("after 012 all four spellings should exist as separate definitions, found %d", n)
	}

	// And it is safe to run AGAIN over a converted database (a missing tracking row, as --migrate-only repairs).
	mysqlExec(t, db, `DELETE FROM schema_migrations WHERE name = ?`, mysqlNamesFile)
	if err := runMigrations(t, ctx, real, migration.DialectMySQL); err != nil {
		t.Errorf("012 re-run over an already-converted database: %v", err)
	}
}

// TestEveryMySQLStringColumnNamedForAWorkflowDefinitionIsExactAfter012 asks the catalog, not the migration file, which
// string columns look like definition names, so a column a LATER migration adds under the same naming and leaves
// case-insensitive is found here and not in a customer's data.
func TestEveryMySQLStringColumnNamedForAWorkflowDefinitionIsExactAfter012(t *testing.T) {
	db := newMySQLScratchDB(t, mysqlNamesTestDB+"_catalog")
	if err := runMigrations(t, context.Background(), migration.NewRunner(db, migration.DialectMySQL, migrationsRoot(t)), migration.DialectMySQL); err != nil {
		t.Fatalf("apply the shipped MySQL migrations: %v", err)
	}
	rows, err := db.Query(`SELECT table_name, column_name, collation_name FROM information_schema.columns
		WHERE table_schema = DATABASE() AND collation_name IS NOT NULL
		  AND (column_name IN ('def_name', 'workflow_name', 'definition_name', 'child_name')
		       OR (table_name = 'workflow_defs' AND column_name = 'name'))`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var tbl, col, coll string
		if err := rows.Scan(&tbl, &col, &coll); err != nil {
			t.Fatal(err)
		}
		seen++
		if coll != "utf8mb4_0900_bin" {
			t.Errorf("%s.%s is %s: a definition-name column that 012 left folding case and accent (add it to the migration, or explain it here)", tbl, col, coll)
		}
	}
	if seen != len(mysqlNamesColumns) {
		t.Fatalf("UNMEASURED: the catalog query found %d name-like columns, want %d; the probe is not looking at the schema it was written for", seen, len(mysqlNamesColumns))
	}
}

// TestMySQLMigrationFinishesByAssertingEveryColumnAndKey: the half-converted state is SILENT (a comparison between a
// converted and an unconverted column is quietly exact, no error), so 012 ends by reading every target column and every
// key back and failing loudly, naming the first one left behind. Each case gives the migration a COPY of itself with one
// step removed, shows the assertion names exactly what was left, and then shows the real file converges over the
// leftovers: the file is resumable.
func TestMySQLMigrationFinishesByAssertingEveryColumnAndKey(t *testing.T) {
	realFile, err := os.ReadFile(filepath.Join(migrationsRoot(t), "mysql", mysqlNamesFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, remove, want string
	}{
		{"a column left behind",
			"ALTER TABLE plugin_dep_overrides MODIFY definition_name VARCHAR(255) COLLATE utf8mb4_0900_bin NOT NULL, ALGORITHM=COPY, LOCK=SHARED;",
			"cleat#3640 INCOMPLETE: 1 name column(s) not utf8mb4_0900_bin, first plugin_dep_overrides.definition_name"},
		{"a key not put back",
			"ALTER TABLE workflow_tags ADD CONSTRAINT fk_workflow_tags_def FOREIGN KEY (tenant_id, workflow_name, version) REFERENCES workflow_defs (tenant_id, name, version);",
			"cleat#3640 INCOMPLETE: 1 of the 3 definition-name foreign keys are missing"},
	} {
		c := c
		t.Run(strings.ReplaceAll(c.name, " ", "_"), func(t *testing.T) {
			db := newMySQLScratchDB(t, mysqlNamesTestDB+"_assert_"+strconv.Itoa(len(c.want)))
			ctx := context.Background()
			if err := runMigrations(t, ctx, migration.NewRunner(db, migration.DialectMySQL, mysqlChainBefore(t, mysqlNamesVersion)), migration.DialectMySQL); err != nil {
				t.Fatalf("apply the migrations before 012: %v", err)
			}
			if strings.Count(string(realFile), c.remove) != 1 {
				t.Fatalf("UNMEASURED: the step to remove appears %d times in 012, so this test no longer removes what it says", strings.Count(string(realFile), c.remove))
			}
			mutant := strings.Replace(string(realFile), c.remove, "DO 0;", 1)
			dir := filepath.Join(t.TempDir(), "mysql")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			entries, _ := os.ReadDir(filepath.Join(mysqlChainBefore(t, mysqlNamesVersion), "mysql"))
			for _, e := range entries {
				b, _ := os.ReadFile(filepath.Join(mysqlChainBefore(t, mysqlNamesVersion), "mysql", e.Name()))
				os.WriteFile(filepath.Join(dir, e.Name()), b, 0o644)
			}
			if err := os.WriteFile(filepath.Join(dir, mysqlNamesFile), []byte(mutant), 0o644); err != nil {
				t.Fatal(err)
			}

			// The mutant file finishes every other step and then the assertion refuses, naming what is left.
			err := runMigrations(t, ctx, migration.NewRunner(db, migration.DialectMySQL, filepath.Dir(dir)), migration.DialectMySQL)
			if err == nil {
				t.Fatal("the migration reported success with a step removed; its final assertion did not fire")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the assertion did not say %q:\n%v", c.want, err)
			}
			if len(err.Error()) > 400 || strings.Contains(err.Error(), "1648") {
				t.Errorf("the assertion text was cut or replaced by error 1648:\n%v", err)
			}
			var applied int
			if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, mysqlNamesFile).Scan(&applied); err != nil || applied != 0 {
				t.Errorf("a migration that failed its own assertion is recorded as applied (count %d, err %v)", applied, err)
			}

			// The real file resumes over the leftovers and converges.
			if err := runMigrations(t, ctx, migration.NewRunner(db, migration.DialectMySQL, migrationsRoot(t)), migration.DialectMySQL); err != nil {
				t.Fatalf("the real migration over the leftovers of the interrupted one: %v", err)
			}
			for col, coll := range mysqlCollations(t, db) {
				if coll != "utf8mb4_0900_bin" {
					t.Errorf("%s is %s after resuming", col, coll)
				}
			}
			if n := mysqlFKCount(t, db); n != 3 {
				t.Errorf("%d of 3 foreign keys exist after resuming", n)
			}
		})
	}
}
