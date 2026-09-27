package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// cleat#2311. An admin operation that appends an event to a history this worker
// cannot decrypt seals that event under the WRONG key and leaves the run
// unreadable to every single-key worker. The guard is one function,
// PostgresStore.assertHistoryReadable, called from PostgresStore.adminAppendAudit.
// This test keeps it structural, so a new admin verb cannot append around it.
//
// The first version of this test globbed "store_admin*.go", so a new admin file
// (admin_zz_review.go, say) carrying a PostgresStore method that calls
// appendEventsInTx stayed green -- exactly the blind spot the test exists to
// prevent, and a real gap cleat-review found (cleat#2337). It now scans every
// non-test engine source and classifies each appendEventsInTx call site against
// adminAppendSites, the same manifest shape as
// TestEveryEventHistoryWriteRoutesThroughTheEncoder (cleat#2328).
//
// Every non-test appendEventsInTx call site in engine/ must be on the manifest
// as either:
//
//   - "admin": one of the three dialects' adminAppendAudit methods, the only
//     admin path that may append. Postgres's is mechanically re-verified to
//     still call assertHistoryReadable.
//   - "exempt": a normal engine write path (continue-as-new, finalize, defer
//     phase, the public AppendEventHistoryBatch), each with a reason a reviewer
//     can check.
//
// A call site not on the list -- the shape a new admin verb takes, in whatever
// file it lands -- fails. MySQL and SQL Server encrypt nothing at rest (payload
// encryption is PostgreSQL-only), so their adminAppendAudit copies need no
// readability check.
func TestEveryAdminAppendGoesThroughTheReadabilityCheck(t *testing.T) {
	files := map[string]string{}
	for _, f := range engineGoFiles(t) {
		files[f] = readEngineFile(t, f)
	}
	for _, v := range checkAdminAppendSites(t, files, adminAppendSites) {
		t.Error(v)
	}
}

// adminAppendSiteStatus is a human classification of one declaration that calls
// appendEventsInTx.
type adminAppendSiteStatus string

const (
	// adminSiteAdmin means the declaration is an adminAppendAudit method, the only
	// admin path that may append; Postgres's is mechanically re-verified to call
	// assertHistoryReadable.
	adminSiteAdmin adminAppendSiteStatus = "admin"
	// adminSiteExempt means a human has judged this site a normal engine write path
	// (not an admin verb) for the reason given, and this test does not re-derive
	// that judgement.
	adminSiteExempt adminAppendSiteStatus = "exempt"
)

type adminAppendSite struct {
	status adminAppendSiteStatus
	reason string
}

// adminAppendSites is keyed by "<basefile>:<Type.Method>". Regenerate the key
// list with:
//
//	go test ./engine/ -run TestEveryAdminAppendGoesThroughTheReadabilityCheck -v
//
// which names any site it finds that is not on this list.
var adminAppendSites = map[string]adminAppendSite{
	"store_admin.go:PostgresStore.adminAppendAudit": {
		status: adminSiteAdmin,
		reason: "the admin append path; must call assertHistoryReadable before appending",
	},
	"store_admin.go:MySQLStore.adminAppendAudit": {
		status: adminSiteAdmin,
		reason: "MySQL encrypts nothing at rest, so there is no readability check",
	},
	"store_admin.go:MSSQLStore.adminAppendAudit": {
		status: adminSiteAdmin,
		reason: "SQL Server encrypts nothing at rest, so there is no readability check",
	},

	"store_lifecycle.go:PostgresStore.ContinueAsNew": {
		status: adminSiteExempt,
		reason: "engine continue-as-new write, not an admin verb",
	},
	"store_lifecycle.go:PostgresStore.finalizeWorkflowSegmentInner": {
		status: adminSiteExempt,
		reason: "engine finalize write, not an admin verb",
	},
	"store_event_write.go:PostgresStore.AppendEventHistoryBatch": {
		status: adminSiteExempt,
		reason: "the public append API, not an admin verb",
	},
	"store_defer_phase.go:PostgresStore.FinalizeDeferPhase": {
		status: adminSiteExempt,
		reason: "engine defer-phase finalize, not an admin verb",
	},

	"mssql_lifecycle.go:MSSQLStore.continueAsNewOnce": {
		status: adminSiteExempt,
		reason: "engine continue-as-new write, not an admin verb",
	},
	"mssql_lifecycle.go:MSSQLStore.finalizeWorkflowSegmentOnce": {
		status: adminSiteExempt,
		reason: "engine finalize write, not an admin verb",
	},
	"mssql_events.go:MSSQLStore.appendEventHistoryBatchOnce": {
		status: adminSiteExempt,
		reason: "engine batch append, not an admin verb",
	},
	"mssql_defer_phase.go:MSSQLStore.finalizeDeferPhaseOnce": {
		status: adminSiteExempt,
		reason: "engine defer-phase finalize, not an admin verb",
	},

	"mysql_events.go:MySQLStore.AppendEventHistoryBatch": {
		status: adminSiteExempt,
		reason: "the public append API, not an admin verb",
	},
	"mysql_lifecycle.go:MySQLStore.ContinueAsNew": {
		status: adminSiteExempt,
		reason: "engine continue-as-new write, not an admin verb",
	},
	"mysql_lifecycle.go:MySQLStore.finalizeWorkflowSegmentInner": {
		status: adminSiteExempt,
		reason: "engine finalize write, not an admin verb",
	},
	"mysql_defer_phase.go:MySQLStore.FinalizeDeferPhase": {
		status: adminSiteExempt,
		reason: "engine defer-phase finalize, not an admin verb",
	},
}

// checkAdminAppendSites is the testable core of
// TestEveryAdminAppendGoesThroughTheReadabilityCheck: given an in-memory fileset
// and a manifest, it returns one violation string per problem found. Taking the
// fileset as a parameter, rather than reading the real tree directly, is what
// lets TestAdminAppendGuardCatchesAKnownBreak below feed it a synthetic broken
// tree without touching any real file.
func checkAdminAppendSites(t *testing.T, files map[string]string, manifest map[string]adminAppendSite) []string {
	t.Helper()
	fset := token.NewFileSet()
	found := map[string]string{}
	var violations []string

	for name, src := range files {
		if !strings.Contains(src, "appendEventsInTx") {
			continue
		}
		f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
		if err != nil {
			// A file that does not parse is a failure of the check, not a finding
			// about the tree -- see CLAUDE.md's UNMEASURED discipline. It cannot
			// happen for a real engine source (go vet would refuse it), so this
			// only fires against a malformed synthetic fixture in the self-test.
			violations = append(violations, name+": does not parse as Go: "+err.Error())
			continue
		}
		base := filepath.Base(name)

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			recv := ""
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				recv = recvTypeName(fn.Recv.List[0].Type)
			}
			callsAppend, callsReadable := false, false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "appendEventsInTx":
					callsAppend = true
				case "assertHistoryReadable":
					callsReadable = true
				}
				return true
			})
			if !callsAppend {
				continue
			}
			key := base + ":" + recv + "." + fn.Name.Name
			found[key] = declText(fset, src, fn.Pos(), fn.End())

			site, ok := manifest[key]
			if !ok {
				violations = append(violations, key+
					": a new appendEventsInTx call site is not on adminAppendSites -- "+
					"classify it as \"admin\" (an adminAppendAudit) or \"exempt\" (a normal engine write path)")
				continue
			}
			if site.status == adminSiteAdmin && recv == "PostgresStore" && !callsReadable {
				violations = append(violations, key+
					": PostgresStore.adminAppendAudit no longer calls assertHistoryReadable")
			}
			if site.reason == "" {
				violations = append(violations, key+": adminAppendSites entry has no reason")
			}
		}
	}

	for key := range manifest {
		if _, ok := found[key]; !ok {
			violations = append(violations, key+
				": listed in adminAppendSites but no longer found in the tree -- "+
				"remove the stale entry (the declaration was renamed, moved, or no longer calls appendEventsInTx)")
		}
	}
	return violations
}

// TestAdminAppendGuardCatchesAKnownBreak is the guard's own known-positive --
// CLAUDE.md's "before trusting a new guard, run its parse once against a case
// already known to be broken". Two synthetic trees, each broken a way the
// cleat#2311 gap actually opens: a new admin verb appending on its own (in a
// file the old store_admin*.go glob would have missed), and a Postgres
// adminAppendAudit that stopped calling assertHistoryReadable. A guard with no
// such test is a claim, not a check.
func TestAdminAppendGuardCatchesAKnownBreak(t *testing.T) {
	const newAdminVerb = `package engine

func (s *PostgresStore) adminForceSomething() error {
	return s.appendEventsInTx()
}
`
	if v := checkAdminAppendSites(t, map[string]string{"admin_zz_review.go": newAdminVerb}, adminAppendSites); len(v) == 0 {
		t.Error("a new admin verb appending on its own was not reported")
	}

	const adminWithoutReadable = `package engine

func (s *PostgresStore) adminAppendAudit() error {
	return s.appendEventsInTx()
}
`
	fakeManifest := map[string]adminAppendSite{
		"admin_zz_review.go:PostgresStore.adminAppendAudit": {status: adminSiteAdmin, reason: "fixture"},
	}
	if v := checkAdminAppendSites(t, map[string]string{"admin_zz_review.go": adminWithoutReadable}, fakeManifest); len(v) == 0 {
		t.Error("an adminAppendAudit that stopped calling assertHistoryReadable was not reported")
	}

	// Negative control: a correctly classified adminAppendAudit reports nothing.
	const adminWithReadable = `package engine

func (s *PostgresStore) adminAppendAudit() error {
	s.assertHistoryReadable()
	return s.appendEventsInTx()
}
`
	if v := checkAdminAppendSites(t, map[string]string{"admin_zz_review.go": adminWithReadable}, fakeManifest); len(v) != 0 {
		t.Errorf("a correctly classified adminAppendAudit was reported as broken: %v", v)
	}
}
