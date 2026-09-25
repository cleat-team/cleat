package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryEventHistoryWriteRoutesThroughTheEncoder asserts that every
// non-test `INSERT INTO event_history` in engine/ sits inside a top-level
// declaration -- a func, or a const SQL string consumed by a named func --
// that is named on eventHistoryInsertSites below, with a human judgement of
// why it is safe: "encoder" (mechanically re-verified: the declaration's own
// source must still contain a call to encodeEventForStorage) or "exempt"
// (a reason a reviewer can check, not re-verified by this test).
//
// WHY A MANIFEST RATHER THAN CALL-GRAPH REACHABILITY. Proving "encryption is
// applied before this INSERT runs" in general needs real interprocedural
// analysis. adaptive_flush.go's flushAndNotify writes entry.params, which
// prepareEntry built by calling encodeEventForStorage several calls and one
// struct field away; store_event_write.go's appendEventsInTx only PREPAREs a
// statement that a SEPARATE function, execEventStmt, executes per row. A
// generic "does this function reach that one" checker would have to be right
// about both, and being subtly wrong about either is exactly the "condition
// that never decides anything" failure CLAUDE.md's *Is this result real?*
// section warns about -- a guard that always reports the reviewed-by-hand
// answer looks identical to one that actually re-derives it.
//
// So this checks the narrower thing that IS mechanical: every declaration
// containing an event_history INSERT is on the list, and every "encoder"
// entry's declaration still textually contains the call. What that catches
// on every run: a NEW event_history INSERT nobody has classified, and an
// "encoder" entry whose declaration stopped calling the encoder. What it
// cannot catch: a declaration that calls the encoder for the wrong record,
// or an exemption reason that becomes wrong later -- both need a human
// reading the list, which is why the reason sits next to the site rather
// than being summarised away.
//
// This is cleat#2328's guard. engine/store_children.go's
// StartChildWorkflowAtomic had its own hand-rolled event_history INSERT that
// never called encodeEventForStorage, so a child_workflow event's
// child_input and payload were plaintext under --encrypt-sensitive-payloads
// despite being listed in EncryptedEventColumns -- and
// TestEncryptedEventColumnsIsComplete could not see it, because that test
// only ever calls encodeEventForStorage directly, never a real write path.
func TestEveryEventHistoryWriteRoutesThroughTheEncoder(t *testing.T) {
	files := map[string]string{}
	for _, f := range engineGoFiles(t) {
		files[f] = readEngineFile(t, f)
	}
	for _, v := range checkEventHistoryInsertSites(t, files, eventHistoryInsertSites) {
		t.Error(v)
	}
}

// eventHistorySiteStatus is a human classification of one declaration that
// writes to event_history.
type eventHistorySiteStatus string

const (
	// siteEncoder means the declaration's own source must contain a call to
	// encodeEventForStorage -- mechanically re-verified every run.
	siteEncoder eventHistorySiteStatus = "encoder"
	// siteExempt means a human has judged this site safe for the reason
	// given, and this test does not re-derive that judgement -- see the
	// entries below for what each one actually checked.
	siteExempt eventHistorySiteStatus = "exempt"
)

type eventHistorySite struct {
	status eventHistorySiteStatus
	reason string
}

// eventHistoryInsertSites is keyed by "<basefile>:<declaration>", where
// <declaration> is "Type.Method" for a method, the bare name for a
// free function, or the const/var identifier for a SQL string template.
// Regenerate the key list with:
//
//	go test ./engine/ -run TestEveryEventHistoryWriteRoutesThroughTheEncoder -v
//
// which names any site it finds that is not on this list.
var eventHistoryInsertSites = map[string]eventHistorySite{
	"store_children.go:PostgresStore.StartChildWorkflowAtomic": {
		status: siteEncoder,
		reason: "cleat#2328: routed through encodeEventForStorage",
	},
	"store_event_write.go:PostgresStore.appendOneEvent": {
		status: siteEncoder,
		reason: "calls encodeEventForStorage directly",
	},

	"adaptive_flush.go:AdaptiveFlusher.flushAndNotify": {
		status: siteExempt,
		reason: "writes entry.params, already encoded per-event by prepareEntry " +
			"(same file), which calls encodeEventForStorage before batching",
	},
	"adaptive_flush.go:retryBatchFlush": {
		status: siteExempt,
		reason: "retries the same eventsJSON flushAndNotify already built from " +
			"encoded batch entries; encodes nothing new",
	},
	"flush.go:insertEventSQL": {
		status: siteExempt,
		reason: "SQL text only, no Go call site of its own; executed as a " +
			"prepared statement from store_event_write.go's execEventStmt, " +
			"which calls encodeEventForStorage per row before executing it",
	},
	"store_event_write.go:PostgresStore.appendEventsInTx": {
		status: siteExempt,
		reason: "PREPAREs the multi-row statement only; each row is executed " +
			"by execEventStmt (same file), which calls encodeEventForStorage",
	},
	"store_intent.go:writeCallIntentSQLPostgres": {
		status: siteExempt,
		reason: "SQL text only, no Go call site of its own; executed by " +
			"PostgresStore.WriteCallIntent (same file), which calls " +
			"encodeEventForStorage before executing it",
	},

	// MySQL and SQL Server: --encrypt-sensitive-payloads is refused at
	// worker startup unless --driver=postgres (cmd/cleat-worker/main.go), so
	// encryptSensitivePayloads is always false on these stores -- there is
	// nothing for these INSERTs to encrypt. See MSSQLStore.WithEncryption's
	// doc comment for the same statement on that field.
	//
	// appendEventsInTxOpts and WriteCallIntent below are still exempt: they
	// carry their own long-standing plaintext encoding and routing either
	// through encodeEventForStorage needs their read paths fixed first (see
	// encodeEventForStorage's SCOPE paragraph). Both StartChildWorkflowAtomic
	// entries are different -- cleat#2328 gave them the same encoder call as
	// the Postgres twin, functionally a no-op while encryptSensitivePayloads
	// is false, so there is one place that builds this event's
	// payload/payload_encoding on every dialect rather than a second
	// hand-rolled copy per store.
	"mssql_events.go:MSSQLStore.appendEventsInTxOpts": {
		status: siteExempt,
		reason: "MSSQL: encryption at rest is not supported on this dialect",
	},
	"mssql_signals_promises.go:MSSQLStore.StartChildWorkflowAtomic": {
		status: siteEncoder,
		reason: "cleat#2328: routed through encodeEventForStorage for parity " +
			"with the Postgres twin; a no-op today since MSSQL never sets " +
			"encryptSensitivePayloads",
	},
	// mysql_events.go and mysql_store.go use "INSERT IGNORE INTO", not
	// "INSERT INTO" -- containsEventHistoryInsert's doc comment explains why
	// that spelling let this pair evade the guard's first version entirely,
	// found only by re-deriving the site count with a wider pattern.
	"mysql_events.go:MySQLStore.appendEventsInTxOpts": {
		status: siteExempt,
		reason: "MySQL: encryption at rest is not supported on this dialect",
	},
	"mysql_store.go:MySQLStore.StartChildWorkflowAtomic": {
		status: siteEncoder,
		reason: "cleat#2328: routed through encodeEventForStorage for parity " +
			"with the Postgres twin; a no-op today since MySQL never sets " +
			"encryptSensitivePayloads",
	},
	"store_intent.go:MySQLStore.WriteCallIntent": {
		status: siteExempt,
		reason: "MySQL: encryption at rest is not supported on this dialect",
	},
	"store_intent.go:MSSQLStore.WriteCallIntent": {
		status: siteExempt,
		reason: "MSSQL: encryption at rest is not supported on this dialect",
	},
}

// checkEventHistoryInsertSites is the testable core of
// TestEveryEventHistoryWriteRoutesThroughTheEncoder: given an in-memory
// fileset and a manifest, it returns one violation string per problem found.
// Taking the fileset as a parameter, rather than reading the real tree
// directly, is what lets TestEventHistoryInsertSiteGuardCatchesAKnownBreak
// below feed it a synthetic broken tree without touching any real file.
func checkEventHistoryInsertSites(t *testing.T, files map[string]string, manifest map[string]eventHistorySite) []string {
	t.Helper()
	fset := token.NewFileSet()
	found := map[string]bool{}
	var violations []string

	for name, src := range files {
		if !containsEventHistoryInsert(src) {
			continue
		}
		f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
		if err != nil {
			// A file that does not parse is a failure of the check, not a
			// finding about the tree -- see CLAUDE.md's UNMEASURED discipline.
			// It cannot happen for a real engine source (go vet would already
			// refuse it), so this only fires against a malformed synthetic
			// fixture in the self-test below.
			violations = append(violations, name+": does not parse as Go: "+err.Error())
			continue
		}
		base := filepath.Base(name)

		for _, decl := range f.Decls {
			declSrc, key := declTextAndKey(fset, src, decl, base)
			if key == "" || !containsEventHistoryInsert(declSrc) {
				continue
			}
			found[key] = true
			site, ok := manifest[key]
			if !ok {
				violations = append(violations, key+
					": a new INSERT INTO event_history is not on eventHistoryInsertSites -- "+
					"classify it as \"encoder\" (and confirm the declaration calls "+
					"encodeEventForStorage) or \"exempt\" (with a reason)")
				continue
			}
			if site.status == siteEncoder && !strings.Contains(declSrc, "encodeEventForStorage(") {
				violations = append(violations, key+
					": listed as \"encoder\" in eventHistoryInsertSites but its "+
					"declaration no longer calls encodeEventForStorage")
			}
			if site.reason == "" {
				violations = append(violations, key+": eventHistoryInsertSites entry has no reason")
			}
		}
	}

	for key := range manifest {
		if !found[key] {
			violations = append(violations, key+
				": listed in eventHistoryInsertSites but no longer found in the tree -- "+
				"remove the stale entry (the declaration was renamed, moved, or no "+
				"longer writes to event_history)")
		}
	}
	return violations
}

// containsEventHistoryInsert reports whether s contains an event_history
// INSERT in any of this codebase's three dialect spellings.
//
// A plain "INSERT INTO event_history" substring check -- what this guard
// shipped with -- is blind to MySQL's "INSERT IGNORE INTO event_history",
// because "IGNORE" sits between the two words it looks for. That blindness
// is not hypothetical: it is what let mysql_store.go's own
// StartChildWorkflowAtomic carry the same hand-rolled, unencoded
// child_workflow INSERT this guard exists to catch, invisible to both the
// file-level grep that first swept this package for cleat#2328 and to this
// guard's own file-level pre-filter, until a second pass re-derived the site
// count with "INTO event_history" instead of anchoring on "INSERT INTO".
func containsEventHistoryInsert(s string) bool {
	return strings.Contains(s, "INSERT INTO event_history") ||
		strings.Contains(s, "INSERT IGNORE INTO event_history")
}

// declTextAndKey returns a top-level declaration's own source text and its
// manifest key ("<basefile>:<name>", "<basefile>:<Type.Method>", or ""
// for a declaration kind this guard does not classify).
func declTextAndKey(fset *token.FileSet, src string, decl ast.Decl, base string) (string, string) {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		text := declText(fset, src, d.Pos(), d.End())
		name := d.Name.Name
		if d.Recv != nil && len(d.Recv.List) > 0 {
			if t := recvTypeName(d.Recv.List[0].Type); t != "" {
				name = t + "." + name
			}
		}
		return text, base + ":" + name
	case *ast.GenDecl:
		if d.Tok != token.CONST && d.Tok != token.VAR {
			return "", ""
		}
		text := declText(fset, src, d.Pos(), d.End())
		if !containsEventHistoryInsert(text) {
			return text, ""
		}
		// A const/var block declaring the SQL string this test cares about is
		// written as a single spec in this codebase (see insertEventSQL,
		// writeCallIntentSQLPostgres) -- keyed on its first name.
		for _, spec := range d.Specs {
			if vs, ok := spec.(*ast.ValueSpec); ok && len(vs.Names) > 0 {
				return text, base + ":" + vs.Names[0].Name
			}
		}
		return text, ""
	default:
		return "", ""
	}
}

func declText(fset *token.FileSet, src string, pos, end token.Pos) string {
	p := fset.Position(pos).Offset
	e := fset.Position(end).Offset
	if p < 0 || e > len(src) || p > e {
		return ""
	}
	return src[p:e]
}

func recvTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	default:
		return ""
	}
}

// TestEventHistoryInsertSiteGuardCatchesAKnownBreak is the guard's own
// known-positive -- CLAUDE.md's "before trusting a new guard, run its parse
// once against a case already known to be broken". Three synthetic trees,
// each broken a different way store_children.go actually was or could be:
// an unclassified new site, a manifest entry whose function stopped calling
// the encoder, and a stale manifest entry naming a site that no longer
// exists. A guard with no such test is a claim, not a check.
func TestEventHistoryInsertSiteGuardCatchesAKnownBreak(t *testing.T) {
	const unclassified = `package engine

func (s *PostgresStore) hypotheticalNewWriter() error {
	_, err := s.db.Exec("INSERT INTO event_history (workflow_id) VALUES ($1)", "x")
	return err
}
`
	if v := checkEventHistoryInsertSites(t, map[string]string{"x.go": unclassified}, eventHistoryInsertSites); len(v) == 0 {
		t.Error("an unclassified event_history INSERT site was not reported")
	}

	const claimsEncoderButDoesNot = `package engine

func (s *PostgresStore) hypotheticalNewWriter() error {
	_, err := s.db.Exec("INSERT INTO event_history (workflow_id) VALUES ($1)", "x")
	return err
}
`
	fakeManifest := map[string]eventHistorySite{
		"x.go:PostgresStore.hypotheticalNewWriter": {status: siteEncoder, reason: "test fixture"},
	}
	if v := checkEventHistoryInsertSites(t, map[string]string{"x.go": claimsEncoderButDoesNot}, fakeManifest); len(v) == 0 {
		t.Error("an \"encoder\" site whose function does not call encodeEventForStorage was not reported")
	}

	const empty = `package engine
`
	if v := checkEventHistoryInsertSites(t, map[string]string{"x.go": empty}, fakeManifest); len(v) == 0 {
		t.Error("a stale manifest entry naming a site absent from the tree was not reported")
	}

	// Negative control: a correctly classified encoder site reports nothing.
	const correct = `package engine

func (s *PostgresStore) hypotheticalNewWriter() error {
	stored, err := encodeEventForStorage(EventRecord{}, s.encryption, s.encryptSensitivePayloads, "t")
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO event_history (workflow_id) VALUES ($1)", stored.Request)
	return err
}
`
	if v := checkEventHistoryInsertSites(t, map[string]string{"x.go": correct}, fakeManifest); len(v) != 0 {
		t.Errorf("a correctly classified encoder site was reported as broken: %v", v)
	}
}
