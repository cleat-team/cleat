package plugin_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A ledger of every cross-tenant bypass in the tree. cleat#1623.
//
// WHY THIS EXISTS, AND WHY IT IS NOT ABOUT MALICIOUS CODE. Plugins are trusted:
// in-process, holding a *sql.DB, able to issue any statement they like. A
// policy is not a wall against a plugin, it is a backstop against a plugin's
// MISTAKES -- cleat#1277's "a requirement authors meet most of the time, whose
// miss rate does not improve on its own". Workflow code is the untrusted
// surface and never reaches this: it is sandboxed WASM with no database handle,
// and gets a tenant bridged in at the host-call boundary.
//
// So the risk is not that AcrossAllTenants can be ABUSED. It is that it is
// CONVENIENT. It is one line, and it is the obvious reach when a fail-closed
// policy makes a sweep return nothing -- which is exactly what a correct policy
// looks like from the inside when you forgot to thread the tenant through.
// AcrossAllTenants already refuses an empty reason, so nobody does it silently;
// but a reason is free to write, and "this spans every tenant by definition" is
// true of a real sweep and equally easy to type over a bug.
//
// This guard does not judge whether a reason is TRUE. It forces the claim to be
// written down and TYPED, so that a new one is a diff a human sees.
//
// WHAT IT DOES NOT COVER, said out loud so a green is not read as more than it
// is: it does not check that a declared bypass WORKS. cleat#1528 is exactly
// that failure -- blobstore's TTL sweep declares AcrossAllTenants and its first
// phase reads workflow_instances, whose policy no bypass admits -- and this
// guard passes it, because the declaration is present and correct.
//
// CACHING. This test reads the tree through the repository root, which is
// outside its own package directory, and `go test` only invalidates its cache
// on files opened INSIDE that directory. A local re-run after adding a call
// site elsewhere can therefore print `ok (cached)` without having looked. CI
// passes -count=1 to every Go job, so CI is sound; locally, use -count=1.

// bypassKind is a CLOSED set, and the point of the type is that its members are
// not interchangeable.
//
// engine/mssql_tenant_predicate_test.go learned this over five audits and
// records it: "THE ALLOWLIST SAYS WHY, AND THE REASONS ARE NOT INTERCHANGEABLE
// ... collapsing them is how this surface got into the state it was in." The
// same applies here, and kindTenantIsTheLookup is why.
type bypassKind int

const (
	// kindGlobalSweep: the predicate is a cutoff that belongs to no tenant --
	// an expiry, a retention age, a window boundary. Deleting by age is the
	// canonical shape.
	kindGlobalSweep bypassKind = iota + 1

	// kindClaimAcrossTenants: one transaction claims and advances every
	// tenant's due rows. Distinct from a sweep because it WRITES the rows it
	// selects, so a wrong tenant here is a wrong actor rather than a wrong
	// reader.
	kindClaimAcrossTenants

	// kindDiscovery: a configuration table read across tenants -- either to
	// find which tenants have something configured, or to load every tenant's
	// config into memory -- after which the per-tenant work is scoped
	// normally. The bypass covers the config read alone.
	kindDiscovery

	// kindTenantIsTheLookup: NOT A SWEEP. A lookup whose RESULT determines the
	// tenant -- a session token hash, a webhook source id, a delivery row that
	// names its config.
	//
	// THIS IS THE ONE THAT NEEDS ITS OWN NAME. "I do not know the tenant yet"
	// is what an author says when it is genuinely undiscoverable AND when they
	// simply did not thread it through, and those look identical in prose. A
	// site claiming this kind is asserting that the tenant is the value being
	// looked up, which is checkable in review in a way that "spans every
	// tenant" is not.
	kindTenantIsTheLookup

	// kindDeploymentQuestion: asked of the deployment rather than of any
	// tenant, generally before any request exists -- "does this deployment
	// hold any secrets", "is any config enabled". A startup or health check.
	kindDeploymentQuestion
)

func (k bypassKind) String() string {
	switch k {
	case kindGlobalSweep:
		return "global-sweep"
	case kindClaimAcrossTenants:
		return "claim-across-tenants"
	case kindDiscovery:
		return "discovery"
	case kindTenantIsTheLookup:
		return "tenant-is-the-lookup"
	case kindDeploymentQuestion:
		return "deployment-question"
	}
	return fmt.Sprintf("bypassKind(%d)", int(k))
}

// crossTenantLedger is keyed by "<repo-relative file>:<enclosing function>".
//
// Keyed by FUNCTION rather than by line, deliberately: a line number changes
// every time anything above it moves, which would make this file conflict on
// every unrelated edit and train everyone to re-run --update without reading.
// A function name changes when someone renames the function, which is a moment
// worth stopping at.
var crossTenantLedger = map[string]bypassKind{
	// Deployment-level questions, asked before any request exists.
	"plugins/pagerdutyalert/plugin.go:(*Plugin).Health": kindDeploymentQuestion,

	// Cutoffs that belong to no tenant.
	"plugins/auditlog/background.go:(*Plugin).cleanupRetention":     kindGlobalSweep,
	"plugins/blobstore/background.go:(*Plugin).Run":                 kindGlobalSweep,
	"plugins/eventstore/background.go:(*Plugin).Run":                kindGlobalSweep,
	"plugins/ratelimiter/background.go:(*Plugin).pruneRateCounters": kindGlobalSweep,

	// One transaction claims and advances every tenant's due rows.
	"plugins/eventtriggers/background.go:(*Plugin).Run": kindClaimAcrossTenants,
	"plugins/jobqueue/background.go:(*Plugin).Run":      kindClaimAcrossTenants,
	// cleat#2125: the non-MSSQL arm moved out of Run and into its own
	// function so SQL Server's per-tenant loop (sweepAbandonedJobsPerTenant)
	// could sit beside it without sharing a marked ctx -- same statement,
	// same reasoning as the Run entry above, just no longer inlined there.
	"plugins/jobqueue/background.go:(*Plugin).sweepAbandonedJobs": kindClaimAcrossTenants,
	"plugins/webhookingest/background.go:(*Plugin).Run":           kindClaimAcrossTenants,
	"plugins/scheduler/background.go:(*Plugin).runDueSchedules":   kindClaimAcrossTenants,
	// TWO CLAIMS IN ONE REASON, noted rather than tidied: this site's reason
	// says "one transaction claims and advances every tenant's due configs,
	// AND the orphan sweep belongs to no tenant" -- a claim and a global
	// sweep. They are both true and both covered by the one bypass today. If
	// the two are ever split into separate transactions they need separate
	// entries, because they are separate kinds and this ledger's whole premise
	// is that the kinds are not interchangeable.
	"plugins/scheduledbackup/background.go:(*Plugin).runDueBackups": kindClaimAcrossTenants,

	// Config read across tenants; the per-tenant work that follows is scoped.
	"plugins/datadogexport/background.go:(*Plugin).exportMetrics": kindDiscovery,
	"plugins/kafkaconnect/background.go:(*Plugin).pollConfigs":    kindDiscovery,
	"plugins/ratelimiter/background.go:(*Plugin).reload":          kindDiscovery,

	// The tenant is the value being looked up. Not sweeps.
	"plugins/notifications/background.go:(*Plugin).Run":             kindTenantIsTheLookup,
	"plugins/oauthprovider/middleware.go:(*Plugin).Middleware":      kindTenantIsTheLookup,
	"plugins/oauthprovider/routes.go:(*Plugin).extractSession":      kindTenantIsTheLookup,
	"plugins/oauthprovider/routes.go:(*Plugin).handleCallback":      kindTenantIsTheLookup,
	"plugins/webhookingest/routes.go:(*Plugin).handleIngestWebhook": kindTenantIsTheLookup,
	// run_id is a workflow run id, unique across every tenant, and the WHERE
	// clause is keyed on it alone -- the write reaches at most one row, the
	// one that run named. Not a sweep: cleat#1715.
	"plugins/jobqueue/finalize_observer.go:(*Plugin).ObserveFinalize": kindTenantIsTheLookup,
}

// crossTenantSite is one AcrossAllTenants call, located by go/ast.
type crossTenantSite struct {
	File   string // repo-relative
	Func   string // enclosing function, "(*Recv).Name" for a method
	Line   int
	Reason string
	// LiteralReason is false when the second argument is anything other than a
	// string literal -- a variable, a call, a concatenation with one.
	LiteralReason bool
}

func (s crossTenantSite) key() string { return s.File + ":" + s.Func }

// trackedGoFiles lists the repository's own Go sources.
//
// git ls-files rather than a filepath.Walk, and this is not a style choice: a
// walk descends into .claude/worktrees/, which is an entire second copy of the
// repository, and attributes call sites to files that exist only in somebody's
// scratch checkout. CLAUDE.md records that as a scope bug that makes a guard
// MORE likely to pass as the working tree gets messier.
//
// --others --exclude-standard so that a call site added in an untracked file is
// still seen. A guard that only reads committed files is blind to exactly the
// change being reviewed.
func trackedGoFiles(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files",
		"--cached", "--others", "--exclude-standard", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, rel := range strings.Fields(string(out)) {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		// testdata holds this guard's own known-positive fixture, which is
		// undeclared ON PURPOSE. The scanner takes a file list precisely so
		// that the two tests below differ only in what they are given.
		if strings.Contains(rel, "/testdata/") || strings.HasPrefix(rel, "testdata/") {
			continue
		}
		files = append(files, rel)
	}
	return files
}

// scanCrossTenantSites parses each file and returns every AcrossAllTenants call.
//
// go/ast rather than a regex, and there is a fresh demonstration of why. Writing
// the census for cleat#1623 I used a regex to find call sites whose reason is
// not a literal; it reported twelve, and ALL TWELVE were false -- the same
// literal sites, re-matched because the pattern modelled neither line-continued
// arguments nor the function's own declaration. It also counted a call inside
// plugin/crosstenant.go's DOC COMMENT as real code, which is the "a text search
// cannot tell a thing from a sentence about the thing" trap.
//
// A parser has none of those problems for free: a comment is not a CallExpr and
// a declaration is not a call.
func scanCrossTenantSites(t *testing.T, root string, files []string) []crossTenantSite {
	t.Helper()
	fset := token.NewFileSet()
	var sites []crossTenantSite

	for _, rel := range files {
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			name := funcName(fn)
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isAcrossAllTenants(call.Fun) {
					return true
				}
				site := crossTenantSite{
					File: rel,
					Func: name,
					Line: fset.Position(call.Pos()).Line,
				}
				if len(call.Args) >= 2 {
					if lit, ok := call.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if s, err := strconv.Unquote(lit.Value); err == nil {
							site.Reason, site.LiteralReason = s, true
						}
					}
				}
				sites = append(sites, site)
				return true
			})
		}
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].File != sites[j].File {
			return sites[i].File < sites[j].File
		}
		return sites[i].Line < sites[j].Line
	})
	return sites
}

// isAcrossAllTenants matches both spellings: plugin.AcrossAllTenants from
// outside the package, and the bare name from within it.
func isAcrossAllTenants(fun ast.Expr) bool {
	switch e := fun.(type) {
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		return ok && pkg.Name == "plugin" && e.Sel.Name == "AcrossAllTenants"
	case *ast.Ident:
		return e.Name == "AcrossAllTenants"
	}
	return false
}

func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	var b strings.Builder
	b.WriteString("(")
	if star, ok := fn.Recv.List[0].Type.(*ast.StarExpr); ok {
		b.WriteString("*")
		if id, ok := star.X.(*ast.Ident); ok {
			b.WriteString(id.Name)
		}
	} else if id, ok := fn.Recv.List[0].Type.(*ast.Ident); ok {
		b.WriteString(id.Name)
	}
	b.WriteString(").")
	b.WriteString(fn.Name.Name)
	return b.String()
}

// TestEveryCrossTenantBypassIsDeclared is the ratchet.
//
// Both directions are checked, because they fail for different reasons and only
// one of them is the one people expect. An UNDECLARED site is a new bypass
// nobody reviewed. A STALE entry is a grant covering something that is not
// there -- scripts/skip-ledger.tsv's rule, and what catches a rename.
func TestEveryCrossTenantBypassIsDeclared(t *testing.T) {
	root := repoRoot(t)
	sites := scanCrossTenantSites(t, root, trackedGoFiles(t, root))

	found := map[string]crossTenantSite{}
	for _, s := range sites {
		found[s.key()] = s
	}

	for key, site := range found {
		if _, ok := crossTenantLedger[key]; !ok {
			t.Errorf("undeclared cross-tenant bypass at %s:%d\n"+
				"  reason given: %q\n"+
				"  Add it to crossTenantLedger with the KIND that fits. If none of the five\n"+
				"  fits, that is worth saying in review rather than picking the nearest one.\n"+
				"  If the tenant is available on this path, thread it through instead: a\n"+
				"  bypass that did not need to exist is the failure this ledger is for.",
				site.File, site.Line, site.Reason)
		}
	}
	for key := range crossTenantLedger {
		if _, ok := found[key]; !ok {
			t.Errorf("stale ledger entry %q: no AcrossAllTenants call there.\n"+
				"  The call was removed or its function was renamed. A ledger line that\n"+
				"  matches nothing is a grant covering something that is not there -- it\n"+
				"  will silently cover the NEXT thing to take that name.", key)
		}
	}
	if len(found) == 0 {
		t.Fatal("the scanner found no cross-tenant bypasses at all, which cannot be right " +
			"while the ledger is non-empty -- the file list or the matcher is broken, and " +
			"a guard that looks at nothing reports everything as fine")
	}
}

// TestTheCrossTenantScannerReportsAnUndeclaredSite is the known-positive.
//
// The test above answers "does it pass when the tree is fine?", which every
// broken version of it also answers yes to. This one answers the harder
// question: does it REPORT a case that is genuinely wrong? The fixture under
// testdata carries one undeclared site and one whose reason is assembled at
// runtime, so both checks have something they must catch.
func TestTheCrossTenantScannerReportsAnUndeclaredSite(t *testing.T) {
	root := repoRoot(t)
	fixture := filepath.Join("plugin", "testdata", "crosstenant", "undeclared_sweep.go")

	sites := scanCrossTenantSites(t, root, []string{fixture})
	if len(sites) != 2 {
		t.Fatalf("scanner found %d site(s) in the fixture, want 2 -- it can no longer see "+
			"what it is looking for, and would report a real undeclared bypass as absent", len(sites))
	}

	for _, s := range sites {
		if _, declared := crossTenantLedger[s.key()]; declared {
			t.Fatalf("the fixture site %s is in the ledger; it must stay undeclared, "+
				"or this test proves nothing", s.key())
		}
	}

	var literal, assembled int
	for _, s := range sites {
		if s.LiteralReason {
			literal++
		} else {
			assembled++
		}
	}
	if literal != 1 || assembled != 1 {
		t.Fatalf("fixture reasons: %d literal, %d assembled; want 1 and 1 -- the "+
			"literal-reason check can no longer tell them apart", literal, assembled)
	}
}

// TestEveryCrossTenantReasonIsAStringLiteral keeps the reasons greppable.
//
// A reason assembled at runtime cannot be read in review, and review is the
// whole mechanism here -- this guard deliberately does not judge whether a
// reason is true. No site violates this today, so the ratchet starts clean.
func TestEveryCrossTenantReasonIsAStringLiteral(t *testing.T) {
	root := repoRoot(t)
	for _, s := range scanCrossTenantSites(t, root, trackedGoFiles(t, root)) {
		if !s.LiteralReason {
			t.Errorf("%s:%d (%s): the cross-tenant reason is not a string literal.\n"+
				"  It is recorded in the transaction so a stuck sweep can be identified at\n"+
				"  three in the morning, and it is read in review. Both want a constant.",
				s.File, s.Line, s.Func)
		}
	}
}

// TestEveryLedgerKindIsInTheClosedSet stops an entry carrying a kind the type
// does not name -- which String() would render as "bypassKind(9)" and a reader
// would skim past.
func TestEveryLedgerKindIsInTheClosedSet(t *testing.T) {
	for key, kind := range crossTenantLedger {
		switch kind {
		case kindGlobalSweep, kindClaimAcrossTenants, kindDiscovery,
			kindTenantIsTheLookup, kindDeploymentQuestion:
		default:
			t.Errorf("ledger entry %q carries kind %s, which is not one of the five", key, kind)
		}
	}
}
