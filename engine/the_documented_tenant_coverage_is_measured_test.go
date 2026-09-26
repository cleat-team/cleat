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
		b.Write(src)
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
