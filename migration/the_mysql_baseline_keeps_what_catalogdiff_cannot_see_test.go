package migration_test

// The MySQL baseline keeps the things a structural differential cannot see.
//
// WHY THIS EXISTS AS A TEST AND NOT AS A PARAGRAPH. cleat#2433 compacted MySQL
// from 70 files to three, and equivalence was established by comparing a
// database built from the chain against one built from the baseline with
// migration/catalogdiff. That comparison is the strongest evidence available
// and it is STRUCTURALLY BLIND to three of the four things below:
//
//   * catalogdiff read no MySQL triggers until cleat#2882 closed that gap
//     (originally found: acceptance 2026-09-26, section 0). Until then, the
//     baseline applying "cleanly" with the trigger absent -- which is exactly
//     what the generator's first version did, because mysqldump wraps it in
//     /*!50003 ... */ and the runner's splitSQL discards that whole statement
//     -- produced an EMPTY diff. No signal existed at the time this test was
//     written, which is why it asserts the shipped artifact directly rather
//     than depending on the differential ever being re-run: that is a second,
//     independent line of defense now, not the only one.
//   * catalogdiff reads no rows, so a missing seed is invisible. Both seeds
//     were missing on the first pass: the default org because its INSERT lived
//     in a migration the compaction deleted, and the default tenant because
//     INSERT IGNORE downgraded a foreign-key failure to a warning.
//   * catalogdiff compared no collation on any dialect until cleat#3121
//     (cleat#2882) added exactly that comparison. This test stays independent
//     of it regardless: catalogdiff only detects DRIFT between two catalogs,
//     so two builds sharing the same wrong collation would still match each
//     other. This test asserts a specific EXPECTED value instead (below).
//
// Every one of those was found by a person looking, not by a check. This is
// the check.
//
// WHAT IT DOES NOT DO, stated here rather than left to be assumed: it does not
// re-prove equivalence to the pre-compaction chain. That chain no longer exists
// in the tree, so it cannot be re-derived -- the equivalence was established
// once, at cleat#2433, against a chain reconstructed from git history. What
// this test can do post-merge is assert that the SHIPPED baseline still has the
// properties the differential could not see, which is the half that can rot
// silently.
//
// The database is built from ../migrations as it ships, through the real
// migration.Runner, so what is asserted is the artifact a deployment gets.

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/migration"
)

// The collation the shipped baseline names on every table.
//
// This is an EXPECTED VALUE, deliberately written here rather than read back
// from the baseline. Comparing the database against the file it was built from
// would compare the generator to itself and pass whatever the generator emitted
// -- which is the vacuity this whole file exists to avoid. The value is
// utf8mb4's default on MySQL 8, and it is asserted against the SERVER's
// reported collation for each column.
const mysqlBaselineCollation = "utf8mb4_0900_ai_ci"

// Columns that are deliberately NOT the schema default, each with its reason.
//
// THIS LIST IS THE POINT OF THE CHECK, NOT AN ESCAPE FROM IT. The first version
// of this test asserted uniformity and failed on slack_workspace.team_id, which
// is utf8mb4_bin -- and the failure was the test's assumption, not the schema's.
// A Slack team id is an opaque identifier that must compare CASE-SENSITIVELY,
// so a case-insensitive collation would make two distinct workspaces collide on
// the primary key. Measured: the pre-compaction CHAIN reports the same
// utf8mb4_bin, so the baseline reproduces it faithfully.
//
// Nothing in this repo compared collation before this test, so nobody could
// have said whether that column was binary on purpose or by accident. Now it is
// written down, and a column arriving here without an entry fails.
var mysqlBaselineCollationExceptions = map[string]string{
	// Opaque external identifier: case-insensitive comparison would let two
	// distinct Slack workspaces collide on the primary key.
	"slack_workspace.team_id": "utf8mb4_bin",
}

func TestTheMySQLBaselineKeepsWhatCatalogdiffCannotSee(t *testing.T) {
	const scratchDB = "cleat_migration_mysql_baseline_test"
	db := newMySQLScratchDB(t, scratchDB)
	ctx := context.Background()

	// "../migrations", not "..": the runner joins the dialect onto the directory
	// it is given, so the base is the migrations ROOT and the runner appends
	// /mysql. Passing ".." asks for <repo>/mysql, which does not exist.
	// Through runMigrations, not r.Run(ctx) directly: a migration run that
	// bypasses it is outside the cluster-wide lock (cleat#1666), and
	// migration/every_migration_run_takes_the_cluster_lock_test.go fails on
	// exactly this line -- caught by that guard, not by review.
	if err := runMigrations(t, ctx,
		migration.NewRunner(db, migration.DialectMySQL, "../migrations"),
		migration.DialectMySQL); err != nil {
		// Not a skip and not a pass. The build failing is a failure of the
		// baseline, and calling it UNMEASURED would send the reader to the
		// check rather than to the thing under test.
		t.Fatalf("applying the shipped MySQL baseline to an empty database failed: %v", err)
	}

	// ---------------------------------------------------------------------
	// 1. The trigger. catalogdiff reads it too as of cleat#2882, but this
	//    test is independent of whether that differential is ever actually
	//    re-run against this baseline -- it asserts the shipped artifact
	//    directly.
	// ---------------------------------------------------------------------
	t.Run("the trigger survives", func(t *testing.T) {
		var n int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM information_schema.triggers
			 WHERE trigger_schema = DATABASE()`).Scan(&n); err != nil {
			t.Fatalf("reading information_schema.triggers: %v", err)
		}
		// NON-VACUITY: zero is the failure, but so is "the query found the
		// table and nothing was ever there". The name is asserted so a
		// different trigger cannot satisfy the count.
		var name string
		if err := db.QueryRowContext(ctx, `
			SELECT MIN(trigger_name) FROM information_schema.triggers
			 WHERE trigger_schema = DATABASE()`).Scan(&name); err != nil {
			t.Fatalf("reading the trigger name: %v", err)
		}
		if n != 1 || name != "tenants_org_id_immutable" {
			t.Fatalf("the baseline installed %d trigger(s), newest named %q; want exactly "+
				"tenants_org_id_immutable.\n\nThis is a check on the SHIPPED artifact, "+
				"independent of catalogdiff -- a baseline missing the trigger applied "+
				"cleanly and diffed EMPTY before cleat#2882 taught catalogdiff to read "+
				"MySQL triggers, which is how the first generator shipped it.",
				n, name)
		}
	})

	// ---------------------------------------------------------------------
	// 2. The seed rows, which a structural diff cannot see at all.
	//
	// BOTH, not one. The default org alone passes on a baseline where the
	// tenant is missing, and the tenant is the row every other table's
	// tenant_id eventually refers to -- a check that passes on the broken case
	// is worse than no check, because it gets quoted as evidence.
	// ---------------------------------------------------------------------
	t.Run("both seed rows are present", func(t *testing.T) {
		for _, table := range []string{"orgs", "tenants"} {
			var n int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
				t.Fatalf("counting %s: %v", table, err)
			}
			if n == 0 {
				t.Fatalf("%s is EMPTY after applying the shipped baseline.\n\n"+
					"002_defaults.sql is HAND-ASSEMBLED -- a catalog dump carries no rows, "+
					"which is why the generator refuses to overwrite it -- so nothing else "+
					"in this repo can report its contents going missing. A row the chain "+
					"produced was lost to exactly this in cleat#2433, and the structural "+
					"differential was empty throughout.", table)
			}
		}
	})

	// ---------------------------------------------------------------------
	// 3. The ORDER the seeds depend on, because on this dialect the failure is
	//    SILENT.
	//
	// tenants.org_id carries tenants_org_id_fk to orgs(org_id), so the org must
	// be inserted first. Out of order, MySQL raises 1452 and INSERT IGNORE
	// downgrades it to a WARNING: the statement reports SUCCESS, the row does
	// not land, and the count check above reports it only as "empty" -- which
	// reads as "the seed was forgotten" rather than "the seed ran and was
	// refused". This asserts the effect directly.
	//
	// MSSQL's seeds cannot fail this way, so its equivalent check does not
	// carry this assertion and does not need it. Copying its adequacy here
	// would have been the mistake: an effect-assertion is only as good as its
	// dialect's silent failure modes.
	// ---------------------------------------------------------------------
	t.Run("the tenant's org resolves", func(t *testing.T) {
		var orphans int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM tenants t
			 LEFT JOIN orgs o ON o.org_id = t.org_id
			 WHERE t.org_id IS NOT NULL AND o.org_id IS NULL`).Scan(&orphans); err != nil {
			t.Fatalf("checking tenants.org_id against orgs: %v", err)
		}
		if orphans != 0 {
			t.Fatalf("%d tenant row(s) name an org that does not exist.\n\n"+
				"The seeds in 002_defaults.sql are ORDER-DEPENDENT: org first, then "+
				"tenant, because tenants_org_id_fk points at orgs. Reversed, MySQL "+
				"raises 1452 and INSERT IGNORE turns it into a warning -- the statement "+
				"reports success and the row silently does not land. Measured during "+
				"cleat#2433, not hypothesised.", orphans)
		}
		// NON-VACUITY: the join above is satisfied trivially by ZERO tenants, so
		// a baseline with no tenant rows at all would pass it. tenant_id is
		// NOT NULL, so the count is the direct question.
		var withOrg int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tenants WHERE org_id IS NOT NULL`).Scan(&withOrg); err != nil {
			t.Fatalf("counting tenants with an org: %v", err)
		}
		if withOrg == 0 {
			t.Fatalf("no tenant row carries an org_id, so the join above proved nothing. " +
				"The default tenant's org_id defaults to the zero UUID and must resolve " +
				"to the default org; if this is zero the seed did not land at all.")
		}
	})

	// ---------------------------------------------------------------------
	// 4. Collation. cleat#3121 (cleat#2882) has since given catalogdiff its own
	//    collation comparison, closing cleat-review's GAP 1 on cleat#2435 --
	//    but that is DRIFT between two catalogs, not an assertion of a
	//    specific value, so it cannot tell a shared wrong collation from a
	//    correct one. This check stays independent for that reason: it
	//    asserts against the EXPECTED value below, not against another build.
	//
	// It asserts against the CONSTANT above rather than against the baseline,
	// because comparing the database to the file it came from would compare
	// the generator to itself.
	// ---------------------------------------------------------------------
	t.Run("every column's collation is the default or a declared exception", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, `
			SELECT table_name, column_name, collation_name
			  FROM information_schema.columns
			 WHERE table_schema = DATABASE() AND collation_name IS NOT NULL
			 ORDER BY table_name, column_name`)
		if err != nil {
			t.Fatalf("reading column collations: %v", err)
		}
		defer rows.Close()

		checked := 0
		seen := map[string]bool{}
		var wrong []string
		for rows.Next() {
			var table, column, collation string
			if err := rows.Scan(&table, &column, &collation); err != nil {
				t.Fatalf("scanning a column's collation: %v", err)
			}
			checked++
			key := table + "." + column
			// The FIRST wrong one is reported, not the first row overall. The
			// first version of this message printed MIN(CONCAT(...)) over every
			// column, so a failure named a single column and then asserted that
			// some column was not the default -- a sentence that contradicts
			// itself, because the column it named was the correct one. A
			// diagnostic that misdirects is half a check.
			//
			// The original failure text is described rather than quoted: written
			// out it is a `name -> value` pair, and gitleaks' generic-api-key
			// rule reads a high-entropy value after an arrow as a credential.
			// That is a false positive, and the fix is not to widen
			// .gitleaks.toml for a comment -- the file's own rule is to
			// allowlist exact placeholder VALUES, never to blunt a rule.
			want := mysqlBaselineCollation
			if exc, ok := mysqlBaselineCollationExceptions[key]; ok {
				want = exc
			}
			seen[key] = true
			if collation != want {
				wrong = append(wrong, key+" -> "+collation+" (want "+want+")")
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterating columns: %v", err)
		}

		// NON-VACUITY. Zero checked means the query selected nothing and the
		// failure list below is empty for that reason alone -- the strongest
		// possible pass, from a check that looked at nothing.
		if checked == 0 {
			t.Fatalf("no column in the built schema reports a collation, so this check " +
				"compared nothing and cannot report anything about collation.")
		}
		if len(wrong) > 0 {
			t.Fatalf("%d of %d collated column(s) do not carry their intended collation:\n  %s\n\n"+
				"Collation decides case-sensitivity on this dialect. catalogdiff compares it "+
				"too (cleat#3121), but only as drift between two catalogs -- it would not "+
				"catch this alone if both sides shared the wrong value.\n\n"+
				"If the SERVER default moved, the baseline is version-pinned and the fix is "+
				"to regenerate on the pinned server. If a column is genuinely binary, add it "+
				"to mysqlBaselineCollationExceptions WITH its reason, because an exception "+
				"nobody had to justify is how a deliberate one and an accident look alike.",
				len(wrong), checked, strings.Join(wrong, "\n  "))
		}

		// The other direction: an exemption that no longer matches a real column
		// is a claim about a schema that has moved on, and it would silently
		// cover whatever arrives at that name next.
		for key := range mysqlBaselineCollationExceptions {
			if !seen[key] {
				t.Fatalf("mysqlBaselineCollationExceptions names %q, which is not a collated "+
					"column in the built schema. Remove the entry, or fix the name -- an "+
					"exemption that outlives its subject silently covers whatever arrives "+
					"at that name next.", key)
			}
		}
	})

	// ---------------------------------------------------------------------
	// 5. The descending index, and the auto_increment columns.
	//
	// THESE TWO ARE THE CLASS cleat#2446 FOUND, AND THEY ARE THE REASON THIS
	// FILE IS THE STRONGER HALF OF THE PAIR. catalogdiff could not see either
	// -- it selected no STATISTICS.COLLATION and no COLUMNS.EXTRA -- and that
	// is now fixed (cleat#2448). But the comparator only helps WHEN THE
	// DIFFERENTIAL RUNS, and these two are the modes where it does not: a
	// hand-edit to 001, a bad merge, a regeneration on a different server.
	//
	// The MSSQL side has been protected by exactly this all along --
	// supplementary.go reads is_descending_key -- which is why its analogue of
	// this gap was latent while MySQL's was live. This is half a pair that was
	// missing, not a follow-up to a fix.
	// ---------------------------------------------------------------------
	t.Run("the descending index is still descending", func(t *testing.T) {
		var descending int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM information_schema.statistics
			 WHERE table_schema = DATABASE() AND collation = 'D'`).Scan(&descending); err != nil {
			t.Fatalf("reading index directions: %v", err)
		}
		// NON-VACUITY: an index table that is empty, or a query that selected
		// nothing, reports zero descending keys for the same reason a correct
		// schema without one does.
		var indexed int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM information_schema.statistics
			 WHERE table_schema = DATABASE()`).Scan(&indexed); err != nil {
			t.Fatalf("counting index columns: %v", err)
		}
		if indexed == 0 {
			t.Fatalf("no index columns at all, so this check compared nothing")
		}
		if descending == 0 {
			t.Fatalf("no index column is DESCENDING, and one must be: " +
				"workflow_memory_samples.idx_memory_samples_tenant_def is declared " +
				"`(tenant_id, def_name, recorded_at DESC)` in both the pre-compaction " +
				"chain and the compacted baseline.\n\n" +
				"A DESC key silently becoming ASC changes what the index is for and is " +
				"invisible to every behavioural test -- the rows are the same either " +
				"way. catalogdiff could not see it until cleat#2448, and that only helps " +
				"when the differential runs, which is not every mode.")
		}
	})

	t.Run("the auto_increment columns are still auto_increment", func(t *testing.T) {
		// NAMED, not counted, and for the reason the seed rows above are named:
		// a COUNT would pass while one of the two had lost the attribute. The
		// first version of this check asserted `count == 0` and could not fire
		// when the battery removed one of the two -- the check was weaker than
		// the failure it existed to catch.
		want := []string{"workflow_memory_samples.id", "workflow_signals.id"}
		for _, key := range want {
			var extra string
			if err := db.QueryRowContext(ctx, `
				SELECT extra FROM information_schema.columns
				 WHERE table_schema = DATABASE()
				   AND CONCAT(table_name, '.', column_name) = ?`, key).Scan(&extra); err != nil {
				t.Fatalf("reading the extra for %s: %v\n\nIt is missing from the built "+
					"schema entirely, or has been renamed.", key, err)
			}
			if !strings.Contains(strings.ToLower(extra), "auto_increment") {
				t.Fatalf("%s is not AUTO_INCREMENT (extra=%q).\n\n"+
					"`auto_increment` exists nowhere else this test can read it: "+
					"COLUMN_TYPE reports `bigint` and COLUMN_DEFAULT reports NULL either "+
					"way, so a column losing it inserts identically for well-formed data "+
					"and no behavioural test distinguishes the two -- cleat#2445's class, "+
					"on this dialect (cleat#2446).", key, extra)
			}
		}
		// NON-VACUITY: the loop above is satisfied trivially by a schema with no
		// columns at all, because then the read would have errored rather than
		// passed -- but this counts what it actually looked at, so a rename that
		// happened to keep the CONCAT matching still shows up.
		var cols int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM information_schema.columns
			 WHERE table_schema = DATABASE()`).Scan(&cols); err != nil {
			t.Fatalf("counting columns: %v", err)
		}
		if cols == 0 {
			t.Fatalf("the built schema has no columns, so this check compared nothing")
		}
	})
}
