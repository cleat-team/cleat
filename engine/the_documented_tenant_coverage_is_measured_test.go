package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The per-dialect coverage table in docs/reference/multi-tenancy.md agrees with
// the migrations it describes. cleat#1777.
//
// # Why this exists
//
// That document has been wrong twice, in opposite directions, and both times a
// reader had no way to tell. In 2026-08-09 it said row-level security was
// PostgreSQL-only, which was wrong -- SQL Server has a real SECURITY POLICY. The
// correction then said SQL Server bound its predicate "on the same seven
// multi-tenant tables PostgreSQL forces RLS on", which was true when written and
// is now wrong in both halves: PostgreSQL forces RLS on 16 tables, SQL Server
// filters 13, and the sets are not the same size let alone the same set.
//
// A number in prose that nobody re-derives is a number that drifts. This
// re-derives them.
//
// # What it does NOT assert
//
// That the coverage is CORRECT -- that 16 and 13 are the right numbers, or that
// the four PostgreSQL tables without RLS should be without it. Those are design
// questions and this test would be the wrong place to relitigate them. It
// asserts only that the document says what the migrations do. If someone adds a
// policy, this fails and the document gets updated; that is the whole job.
//
// # Why the migrations and not a live database
//
// engine/mssql_policy_coverage_test.go already reads sys.security_predicates
// from a real SQL Server, and is the better check for "the database matches the
// files". This one runs with no database at all, because a guard that skips
// where none is configured reports success, and the numbers in a published
// document should be checked in every job rather than in the ones with a DSN.
//
// The two agree today: the catalogs on live PostgreSQL 16, SQL Server 2022 and
// MySQL 8.4 built from migrations/ to head return 16, 13 and 0, which is what
// the patterns below extract from the files.

var (
	// PostgreSQL: a table is covered when RLS is switched on for it.
	pgRLSRe = regexp.MustCompile(`(?i)ALTER TABLE\s+(?:ONLY\s+)?(?:"?(\w+)"?\.)?"?(\w+)"?\s+ENABLE ROW LEVEL SECURITY`)
	// SQL Server: the repo's own pattern, copied from
	// engine/mssql_policy_coverage_test.go so the two cannot disagree.
	msFilterRe = regexp.MustCompile(`ADD FILTER PREDICATE dbo\.fn_tenant_filter\(tenant_id\) ON dbo\.(\w+)`)
	msBlockRe  = regexp.MustCompile(`(?i)ADD BLOCK PREDICATE`)
	// The document's own table cells.
	docRowRe = regexp.MustCompile(`(?m)^\| RLS / filter predicates \| \*\*(\d+)\*\*[^|]*\| \*\*(\d+)\*\* \| \*\*(\d+)\*\* \|`)
	// A partition child, as migrations/postgres names them: <parent>_p<digits>.
	// Used to fold children into the table they are storage for -- see the
	// note beside pgTables, which explains why that folding is not cosmetic.
	partitionChildRe = regexp.MustCompile(`^(.*)_p\d+$`)
)

func repoRootForDoc(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("UNMEASURED: locating the repository root: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func readDialect(t *testing.T, root, dialect string) string {
	t.Helper()
	dir := filepath.Join(root, "migrations", dialect)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("UNMEASURED: reading %s: %v", dir, err)
	}
	var b strings.Builder
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("UNMEASURED: reading %s: %v", e.Name(), err)
		}
		// COMMENTS STRIPPED, because every scan below is a substring or regex
		// search over the whole file and cannot tell a construct from a
		// sentence about one. The failure that prompted it: this file's MySQL
		// arm counts occurrences of "ENABLE ROW LEVEL SECURITY", "CREATE POLICY"
		// and "ADD FILTER PREDICATE", and migrations/mysql/008_operator_api_keys.sql
		// documents that MySQL has NO row-level security by SAYING so ("zero
		// CREATE POLICY or ROW LEVEL SECURITY statements across
		// migrations/mysql/*.sql"). That sentence made the count 1 on a tree
		// where the answer is 0, and failed this test against a document that
		// was right. Prose documenting an ABSENCE is the prose most likely to
		// name the thing, so a comment-blind scan punishes the clearest writing
		// first.
		//
		// MEASURED before it was added, so it does not quietly move a number the
		// document asserts: migrations/postgres reads 81 constructs either way,
		// migrations/mssql 56 either way, and the only file whose count changes
		// is the one whose comment prompted this -- mysql 1 -> 0.
		//
		// stripSQLComments is mssql_uuid_projection_test.go's, shared rather
		// than duplicated: it blanks comments while PRESERVING byte offsets, so
		// a line number in any failure message stays true.
		b.WriteString(stripSQLComments(string(src)))
		b.WriteByte('\n')
		n++
	}
	// A dialect with no migration files would make every count below zero, and
	// zero is a legitimate documented value for MySQL -- so an empty read must
	// fail rather than agree.
	if n == 0 {
		t.Fatalf("UNMEASURED: no .sql files under migrations/%s, so its count would be "+
			"indistinguishable from a real zero.", dialect)
	}
	return b.String()
}

func TestTheDocumentedTenantCoverageIsMeasured(t *testing.T) {
	root := repoRootForDoc(t)

	pgTables := map[string]bool{}
	for _, m := range pgRLSRe.FindAllStringSubmatch(readDialect(t, root, "postgres"), -1) {
		pgTables[strings.ToLower(m[2])] = true
	}
	// A PARTITION IS NOT A SEPARATE TENANT-SCOPED TABLE. event_history is
	// hash-partitioned into 64 children (cleat#2059) and 001 enables RLS on
	// each of them, so counting raw names read 81 where the document says 17 --
	// and the document is the one that is right. The children are storage for
	// one table, not 64 further places a tenant's rows can live; reporting 81
	// would tell a reader there are 81 tenant-scoped tables to review.
	//
	// Folded only when the PARENT IS ITSELF COVERED, which is the falsifiable
	// half: the rule is "a partition of a covered table", not "a name ending in
	// _p<digits>". A real table called foo_p1 with no covered foo stays counted
	// as itself.
	//
	// This is the same distinction the rest of this file draws: the regex was
	// measuring a format it does not model -- physical relations, where the
	// document's claim is about logical tables -- and it moved in the direction
	// that inflated the denominator.
	unfolded := make([]string, 0, len(pgTables))
	for name := range pgTables {
		if parent := partitionChildRe.ReplaceAllString(name, "$1"); parent != name && pgTables[parent] {
			unfolded = append(unfolded, name)
		}
	}
	for _, name := range unfolded {
		delete(pgTables, name)
	}
	msSrc := readDialect(t, root, "mssql")
	msTables := map[string]bool{}
	for _, m := range msFilterRe.FindAllStringSubmatch(msSrc, -1) {
		msTables[strings.ToLower(m[1])] = true
	}
	msBlocks := len(msBlockRe.FindAllString(msSrc, -1))

	mySrc := readDialect(t, root, "mysql")
	myCovered := 0
	for _, pat := range []string{"ENABLE ROW LEVEL SECURITY", "CREATE POLICY", "ADD FILTER PREDICATE"} {
		myCovered += strings.Count(strings.ToUpper(mySrc), pat)
	}

	// The scanners must find something where something exists, or "the document
	// agrees with zero" is what a broken scanner also reports.
	if len(pgTables) == 0 || len(msTables) == 0 {
		t.Fatalf("UNMEASURED: the scanners found postgres=%d mssql=%d covered tables. "+
			"Both are known non-zero, so this is a failure of the check.",
			len(pgTables), len(msTables))
	}

	docPath := filepath.Join(root, "docs", "reference", "multi-tenancy.md")
	doc, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("UNMEASURED: reading %s: %v", docPath, err)
	}
	m := docRowRe.FindSubmatch(doc)
	if m == nil {
		t.Fatalf("UNMEASURED: could not find the coverage row in %s. The table was "+
			"reformatted, so this check is comparing nothing.", docPath)
	}
	docPG, _ := strconv.Atoi(string(m[1]))
	docMS, _ := strconv.Atoi(string(m[2]))
	docMY, _ := strconv.Atoi(string(m[3]))

	if docPG != len(pgTables) {
		t.Errorf("multi-tenancy.md says PostgreSQL covers %d tables; the migrations enable "+
			"RLS on %d. Update the document.", docPG, len(pgTables))
	}
	if docMS != len(msTables) {
		t.Errorf("multi-tenancy.md says SQL Server covers %d tables; the migrations bind "+
			"filter predicates to %d. Update the document.", docMS, len(msTables))
	}
	if docMY != myCovered {
		t.Errorf("multi-tenancy.md says MySQL covers %d tables; the migrations contain %d "+
			"row-level-security constructs. Update the document.", docMY, myCovered)
	}

	// The block-predicate claim is the sharpest sentence in that section, so it
	// is the one most worth pinning: if someone adds one, the prose saying there
	// are none must stop being true loudly.
	if msBlocks != 0 && strings.Contains(string(doc), "SQL Server has no `BLOCK` predicates — none.") {
		t.Errorf("multi-tenancy.md states SQL Server has no BLOCK predicates, but the "+
			"migrations now contain %d. The backstop is no longer read-side only.", msBlocks)
	}
}

// tenantCoverageRatioRe extracts PostgreSQL's "N of M" tenant-bearing-table
// ratio from a sentence, anchored on the PHRASE rather than any one file's
// wording of it -- cleat#3285. The tree asserts this fact in at least three
// different phrasings ("forces RLS on N of M tenant-bearing tables",
// "forces it on N of M tenant-bearing tables", "'s N of M is deliberate"),
// and until this test only one of them -- the table cell
// TestTheDocumentedTenantCoverageIsMeasured already checks -- was ever
// verified. Bounded to [^.]{0,80} -- one sentence, not the whole document --
// so an unrelated number elsewhere near the word "PostgreSQL" is never
// mistaken for this claim.
var tenantCoverageRatioRe = regexp.MustCompile(`(?i)PostgreSQL\b[^.]{0,80}?(\d+)\s+of\s+(\d+)`)

// mssqlFilterCountRe extracts SQL Server's filter-predicate COUNT from a
// sentence, the same way and for the same reason as tenantCoverageRatioRe --
// cleat#3285. "binds read-only FILTER predicates to N" and "filters N" are
// the two phrasings in the tree today; a third phrasing needs a third
// alternative here, which is the cost phrase-anchoring accepts in exchange
// for never needing a fourth watched FILE the next time someone writes a
// fourth document.
var mssqlFilterCountRe = regexp.MustCompile(`(?i)SQL Server\b[^.]{0,80}?(?:filters?|FILTER predicates\s+to)\s+(\d+)\b`)

// stripBlockquotes blanks every line beginning with '>' (after leading
// whitespace), preserving line count and byte layout otherwise -- cleat#3285.
// multi-tenancy.md's own "Corrected 2026-09-17" blockquote is a dated
// HISTORICAL record of what was once true (PostgreSQL forced RLS on 16
// tables when that correction was written) and must not be read as a live
// claim: rewriting it to the current number would misdate a past correction
// rather than fix a stale one. A doc-wide scan for this phrase has to carry
// this exemption, or it "fixes" a sentence that is already correct for what
// it is dated to say.
func stripBlockquotes(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), ">") {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

// TestTheTenantCoverageRatioIsConsistentEverywhereItIsCited guards cleat#3285:
// TestTheDocumentedTenantCoverageIsMeasured above checks exactly one file's
// table cells. The same PostgreSQL "N of M" ratio and SQL Server filter
// count are separately asserted in prose in SECURITY.md,
// docs/contributor/plugins/plugin-contract.md, and multi-tenancy.md's OWN
// prose (distinct from its table, which the test above already checks) --
// and nothing checked any of the three until this test. Migration 016
// (cleat#3245 Phase 3 step 1) moved PostgreSQL's ratio and only the table
// was updated automatically; the other three were fixed by hand in #3280
// because a human happened to grep for the old numbers while reviewing it,
// which is the exact failure mode this test exists to remove.
//
// What this does NOT check: the "M" denominator (total tenant-bearing
// tables) has no independent live derivation anywhere in this repo today --
// only the "N" (RLS/filter COVERAGE) side does. This test compares each
// citation's N against the same live-derived pgTables/msTables counts
// TestTheDocumentedTenantCoverageIsMeasured computes, and leaves M as a
// value trusted from multi-tenancy.md's own (already-checked) table cell,
// not as a third independently-derived fact. Verifying M too would need its
// own definition of "tenant-bearing" read out of the migrations and is a
// separate, larger piece of work than this issue asked for.
func TestTheTenantCoverageRatioIsConsistentEverywhereItIsCited(t *testing.T) {
	root := repoRootForDoc(t)

	// Live-derived truth, computed the same way and for the same reason as
	// TestTheDocumentedTenantCoverageIsMeasured above -- re-derived here
	// rather than shared via a package-level variable, so this test still
	// fails on its own if that one is ever deleted or renamed.
	pgTables := map[string]bool{}
	for _, m := range pgRLSRe.FindAllStringSubmatch(readDialect(t, root, "postgres"), -1) {
		pgTables[strings.ToLower(m[2])] = true
	}
	unfolded := make([]string, 0, len(pgTables))
	for name := range pgTables {
		if parent := partitionChildRe.ReplaceAllString(name, "$1"); parent != name && pgTables[parent] {
			unfolded = append(unfolded, name)
		}
	}
	for _, name := range unfolded {
		delete(pgTables, name)
	}
	msSrc := readDialect(t, root, "mssql")
	msTables := map[string]bool{}
	for _, m := range msFilterRe.FindAllStringSubmatch(msSrc, -1) {
		msTables[strings.ToLower(m[1])] = true
	}
	if len(pgTables) == 0 || len(msTables) == 0 {
		t.Fatalf("UNMEASURED: the scanners found postgres=%d mssql=%d covered tables. "+
			"Both are known non-zero, so this is a failure of the check.",
			len(pgTables), len(msTables))
	}

	type citationDoc struct {
		path            string
		stripBlockquote bool
	}
	docs := []citationDoc{
		{filepath.Join(root, "SECURITY.md"), false},
		{filepath.Join(root, "docs", "contributor", "plugins", "plugin-contract.md"), false},
		{filepath.Join(root, "docs", "reference", "multi-tenancy.md"), true},
	}

	totalPG, totalMS := 0, 0
	for _, d := range docs {
		raw, err := os.ReadFile(d.path)
		if err != nil {
			t.Fatalf("UNMEASURED: reading %s: %v", d.path, err)
		}
		text := string(raw)
		if d.stripBlockquote {
			text = stripBlockquotes(text)
		}

		for _, m := range tenantCoverageRatioRe.FindAllStringSubmatch(text, -1) {
			n, _ := strconv.Atoi(m[1])
			totalPG++
			if n != len(pgTables) {
				t.Errorf("%s cites PostgreSQL covering %d tables; the migrations enable RLS "+
					"on %d. Update the citation.", d.path, n, len(pgTables))
			}
		}
		for _, m := range mssqlFilterCountRe.FindAllStringSubmatch(text, -1) {
			n, _ := strconv.Atoi(m[1])
			totalMS++
			if n != len(msTables) {
				t.Errorf("%s cites SQL Server filtering %d tables; the migrations bind filter "+
					"predicates to %d. Update the citation.", d.path, n, len(msTables))
			}
		}
	}

	// A scan that silently stops matching finds zero citations -- which
	// reads exactly like "nothing to check" and exactly like success. All
	// three documents are known, as of this test's own writing, to carry at
	// least one of each citation (that is the whole premise of cleat#3285),
	// so finding none is this test's own failure, not a clean tree.
	if totalPG == 0 {
		t.Fatalf("UNMEASURED: found zero PostgreSQL \"N of M\" citations across %d documents. "+
			"cleat#3285 was filed because these exist; a scan that cannot find any of them "+
			"has stopped matching, not run out of things to find.", len(docs))
	}
	if totalMS == 0 {
		t.Fatalf("UNMEASURED: found zero SQL Server filter-count citations across %d documents.",
			len(docs))
	}
}
