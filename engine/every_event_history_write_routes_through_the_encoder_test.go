package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
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
			declSrc, key := declTextAndKey(fset, src, decl, base, containsEventHistoryInsert)
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

// eventHistoryWriteRE is containsEventHistoryInsert's real pattern, replacing
// the three exact substrings this guard shipped with (cleat#2335, a
// cleat-review follow-up on cleat#2333's own fix to this file). Three known
// gaps in the exact-substring version, each one a real shape rather than a
// hypothetical:
//
//   - A plain "INSERT INTO event_history" check -- the guard's first version
//     -- is blind to MySQL's "INSERT IGNORE INTO event_history", because
//     "IGNORE" sits between the two words it looks for. Not hypothetical:
//     it is what let mysql_store.go's own StartChildWorkflowAtomic carry the
//     same hand-rolled, unencoded child_workflow INSERT this guard exists to
//     catch, invisible to both the file-level grep that first swept this
//     package for cleat#2328 and to this guard's own file-level pre-filter,
//     until a second pass re-derived the site count with "INTO event_history"
//     instead of anchoring on "INSERT INTO".
//   - A bare "MERGE event_history" check -- what cleat#2333 added -- matches
//     only that exact spacing. mssql_events.go's own statement is "MERGE
//     event_history WITH (HOLDLOCK) AS target", so the substring happened to
//     still match, but a double space or a line break between MERGE and the
//     table name (routine in a hand-formatted multi-line SQL literal) would
//     not have, and the file-level pre-filter would drop the whole file
//     silently -- see that fix's own doc comment for what that class of
//     failure costs.
//   - Neither exact-substring form is case-insensitive or schema-prefix-aware
//     ("public.event_history", "dbo.event_history"), and neither recognises
//     COPY, Postgres's third bulk-write form (no current caller, but the
//     guard should not need a fourth patch the day one is added).
//
// The regex is whitespace-tolerant (\s+ matches a run of spaces, tabs or
// newlines identically), case-insensitive ((?i)), and treats a schema prefix
// as optional rather than required, covering all three dialects' INSERT
// spelling plus MERGE and COPY. It does NOT attempt to see through a table
// name assembled from a separate Go variable or constant (fmt.Sprintf("INSERT
// INTO %s", tbl), pq.CopyIn(tbl, ...) where tbl is not the literal
// "event_history") -- that is real interprocedural reachability, which this
// file's TestEveryEventHistoryWriteRoutesThroughTheEncoder doc comment
// already explains this guard deliberately does not attempt. A literal
// "INSERT INTO event_history" inside a Sprintf FORMAT STRING (the column list
// or placeholders templated, the table name literal) still matches, because
// the regex has no concept of Go syntax at all -- it scans raw declaration
// text, so proximity in the SOURCE is all it needs.
var eventHistoryWriteRE = regexp.MustCompile(
	`(?i)\binsert\s+(?:ignore\s+)?into\s+(?:\w+\.)?event_history\b` +
		`|\bmerge\s+(?:into\s+)?(?:\w+\.)?event_history\b` +
		`|\bcopy\s+(?:\w+\.)?event_history\b`,
)

// containsEventHistoryInsert reports whether s contains an event_history
// write in any of this codebase's dialect spellings, or one of the two
// forms this codebase has no current caller for (a schema-qualified table,
// COPY) but should not go blind to the day one is added. See
// eventHistoryWriteRE's doc comment for what each alternative covers and
// what it deliberately does not attempt.
func containsEventHistoryInsert(s string) bool {
	return eventHistoryWriteRE.MatchString(s)
}

// declTextAndKey returns a top-level declaration's own source text and its
// manifest key ("<basefile>:<name>", "<basefile>:<Type.Method>", or ""
// for a declaration kind this guard does not classify). detect decides
// whether a const/var block is the SQL string this scan cares about --
// parameterised (cleat#2335) so the UPDATE-side scan below can share this
// function with the INSERT-side one instead of a second copy that could
// drift from it; the two callers pass containsEventHistoryInsert and
// containsEventHistoryUpdate respectively.
func declTextAndKey(fset *token.FileSet, src string, decl ast.Decl, base string, detect func(string) bool) (string, string) {
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
		if !detect(text) {
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

// TestEventHistoryWriteREMatchesEveryKnownShape is the known-positive for
// eventHistoryWriteRE itself (cleat#2335): every shape the issue named as a
// gap in the exact-substring version this replaced, each checked
// individually so a future change that breaks one alternative fails on the
// specific case rather than a single aggregate pass/fail. Per CLAUDE.md's
// known-positive discipline, a pattern that has only ever been run against
// the real tree (which it currently matches cleanly) has not been shown
// capable of catching anything -- these cases would all have been invisible
// to the exact-substring check this replaced.
func TestEventHistoryWriteREMatchesEveryKnownShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"double space", `db.Exec("INSERT  INTO event_history (workflow_id) VALUES ($1)")`},
		{"newline between keywords", "db.Exec(`INSERT\nINTO event_history (workflow_id) VALUES ($1)`)"},
		{"lowercase", `db.Exec("insert into event_history (workflow_id) values ($1)")`},
		{"mixed case", `db.Exec("Insert Into event_history (workflow_id) VALUES ($1)")`},
		{"schema-qualified, public", `db.Exec("INSERT INTO public.event_history (workflow_id) VALUES ($1)")`},
		{"schema-qualified, dbo", `db.Exec("INSERT INTO dbo.event_history (workflow_id) VALUES ($1)")`},
		{"Sprintf format string, columns templated", `db.Exec(fmt.Sprintf("INSERT INTO event_history (%s) VALUES (%s)", cols, placeholders))`},
		{"MERGE with INTO", `db.Exec("MERGE INTO event_history AS target USING src ON ...")`},
		{"MERGE, double space", `db.Exec("MERGE  event_history WITH (HOLDLOCK) AS target USING src ON ...")`},
		{"MERGE, lowercase and schema-qualified", `db.Exec("merge into dbo.event_history as target using src on ...")`},
		{"COPY", `db.Exec("COPY event_history (workflow_id) FROM STDIN")`},
		{"COPY, schema-qualified", `db.Exec("COPY public.event_history FROM STDIN")`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !containsEventHistoryInsert(tc.src) {
				t.Errorf("containsEventHistoryInsert did not match: %s", tc.src)
			}
		})
	}

	// Negative controls: a table name that merely contains "event_history" as
	// a substring, rather than being it, must not match -- \b on both sides
	// of the literal is what this asserts, and it is easy to lose while
	// widening a pattern (loosening the boundary is the natural-looking fix
	// for a real gap elsewhere).
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"suffixed table name", `db.Exec("INSERT INTO event_history_v2 (workflow_id) VALUES ($1)")`},
		{"prefixed table name", `db.Exec("INSERT INTO archived_event_history (workflow_id) VALUES ($1)")`},
		{"unrelated table", `db.Exec("INSERT INTO workflow_instances (id) VALUES ($1)")`},
	} {
		t.Run("negative: "+tc.name, func(t *testing.T) {
			if containsEventHistoryInsert(tc.src) {
				t.Errorf("containsEventHistoryInsert matched a table it should not have: %s", tc.src)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// UPDATE side (cleat#2335)
// ---------------------------------------------------------------------------
//
// The INSERT-side manifest above answers "does this write ever store an
// encrypted column plaintext". It cannot see an UPDATE: CompleteCallIntent
// and ResolveCallIntent (engine/store_intent.go) both persist response,
// error and payload -- three EncryptedEventColumns members -- through a bare
// `UPDATE event_history SET ...`, which contains neither of the words
// "INSERT" or "MERGE" that eventHistoryWriteRE looks for. A hand-rolled,
// unencoded UPDATE of one of these columns would be exactly cleat#2328's bug
// again, on the completion path instead of the creation path, and nothing
// above would notice.
//
// Not every UPDATE of event_history needs a manifest entry, though --
// repairChainAfterResolve (all three dialects) only ever sets checksum,
// which is not in EncryptedEventColumns, so there is nothing for an encoder
// to do and requiring a classification for it would be noise. That is why
// this scan does not just look for "UPDATE ... event_history"; it also asks
// whether the SET clause assigns one of the encrypted columns, and only
// requires classification when it does.

// eventHistorySetClauseRE matches an `UPDATE [schema.]event_history SET
// <clause> WHERE` statement and captures <clause> -- case-insensitive and
// whitespace-tolerant like eventHistoryWriteRE, for the same reason (a
// hand-formatted multi-line SQL literal routinely puts SET on its own line,
// as store_intent.go's CompleteCallIntent/ResolveCallIntent do, or on the
// same line as UPDATE, as repairChainAfterResolve does). It stops the
// capture at the first WHERE so a nested `EXISTS (SELECT 1 FROM
// workflow_instances WHERE id = ... AND assigned_to = ...)` inside the outer
// WHERE clause is never read as part of the SET list -- every real site here
// has one, and "assigned_to"/"generation" are not encrypted columns, but
// there is no reason to depend on that.
var eventHistorySetClauseRE = regexp.MustCompile(
	`(?is)\bupdate\s+(?:\w+\.)?event_history\b\s*set\b(.*?)(?:\bwhere\b|$)`,
)

// setColumnAssignmentRE extracts each column name assigned in a SET clause
// already isolated by eventHistorySetClauseRE -- an identifier immediately
// followed by "=". Applied only to that isolated clause, never to a whole
// declaration, so it cannot mistake a WHERE-clause comparison (`step = $2`)
// for an assignment.
var setColumnAssignmentRE = regexp.MustCompile(`(?i)([A-Za-z_][A-Za-z0-9_]*)\s*=`)

// touchesEncryptedColumn reports whether s contains an event_history UPDATE
// whose SET clause assigns at least one EncryptedEventColumns member.
func touchesEncryptedColumn(s string) bool {
	for _, clause := range eventHistorySetClauseRE.FindAllStringSubmatch(s, -1) {
		for _, col := range setColumnAssignmentRE.FindAllStringSubmatch(clause[1], -1) {
			if isEncryptedEventColumn(col[1]) {
				return true
			}
		}
	}
	return false
}

func isEncryptedEventColumn(name string) bool {
	name = strings.ToLower(name)
	for _, c := range EncryptedEventColumns {
		if c == name {
			return true
		}
	}
	return false
}

// containsEventHistoryUpdate reports whether s contains an event_history
// UPDATE that assigns an EncryptedEventColumns member -- the UPDATE-side
// equivalent of containsEventHistoryInsert, and declTextAndKey's other
// caller (see that function's doc comment). An UPDATE of event_history that
// touches no encrypted column (repairChainAfterResolve's checksum-only SET)
// correctly reports false: there is nothing here for an encoder to do, so it
// needs no manifest entry at all.
func containsEventHistoryUpdate(s string) bool {
	return touchesEncryptedColumn(s)
}

// eventHistoryUpdateSites is eventHistoryInsertSites' UPDATE-side twin, same
// key scheme, same two statuses. store_intent.go has nine real UPDATE
// statements against event_history: CompleteCallIntent and ResolveCallIntent
// on each of the three dialects set response/error/payload (all
// EncryptedEventColumns members) and are listed here; repairChainAfterResolve
// on each dialect sets only checksum and is deliberately absent -- see
// touchesEncryptedColumn.
//
// Regenerate the key list with:
//
//	go test ./engine/ -run TestEveryEventHistoryUpdateOfAnEncryptedColumnRoutesThroughTheEncoder -v
var eventHistoryUpdateSites = map[string]eventHistorySite{
	"store_intent.go:PostgresStore.CompleteCallIntent": {
		status: siteEncoder,
		reason: "calls encodeEventForStorage directly before the UPDATE, same as WriteCallIntent",
	},
	"store_intent.go:PostgresStore.ResolveCallIntent": {
		status: siteEncoder,
		reason: "calls encodeEventForStorage directly before the UPDATE, same as WriteCallIntent",
	},
	"store_intent.go:MySQLStore.CompleteCallIntent": {
		status: siteExempt,
		reason: "MySQL: encryption at rest is not supported on this dialect",
	},
	"store_intent.go:MySQLStore.ResolveCallIntent": {
		status: siteExempt,
		reason: "MySQL: encryption at rest is not supported on this dialect",
	},
	"store_intent.go:MSSQLStore.CompleteCallIntent": {
		status: siteExempt,
		reason: "MSSQL: encryption at rest is not supported on this dialect",
	},
	"store_intent.go:MSSQLStore.ResolveCallIntent": {
		status: siteExempt,
		reason: "MSSQL: encryption at rest is not supported on this dialect",
	},
}

// checkEventHistoryUpdateSites is checkEventHistoryInsertSites' UPDATE-side
// twin. It shares declTextAndKey with the INSERT-side scan (parameterised by
// detect, see that function's doc comment) but keeps its own loop rather
// than merging the two into one generic function, so this addition cannot
// change what the already-proven INSERT-side scan does.
func checkEventHistoryUpdateSites(t *testing.T, files map[string]string, manifest map[string]eventHistorySite) []string {
	t.Helper()
	fset := token.NewFileSet()
	found := map[string]bool{}
	var violations []string

	for name, src := range files {
		if !containsEventHistoryUpdate(src) {
			continue
		}
		f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
		if err != nil {
			violations = append(violations, name+": does not parse as Go: "+err.Error())
			continue
		}
		base := filepath.Base(name)

		for _, decl := range f.Decls {
			declSrc, key := declTextAndKey(fset, src, decl, base, containsEventHistoryUpdate)
			if key == "" || !containsEventHistoryUpdate(declSrc) {
				continue
			}
			found[key] = true
			site, ok := manifest[key]
			if !ok {
				violations = append(violations, key+
					": a new UPDATE of event_history that assigns an EncryptedEventColumns "+
					"member is not on eventHistoryUpdateSites -- classify it as \"encoder\" "+
					"(and confirm the declaration calls encodeEventForStorage) or \"exempt\" "+
					"(with a reason)")
				continue
			}
			if site.status == siteEncoder && !strings.Contains(declSrc, "encodeEventForStorage(") {
				violations = append(violations, key+
					": listed as \"encoder\" in eventHistoryUpdateSites but its "+
					"declaration no longer calls encodeEventForStorage")
			}
			if site.reason == "" {
				violations = append(violations, key+": eventHistoryUpdateSites entry has no reason")
			}
		}
	}

	for key := range manifest {
		if !found[key] {
			violations = append(violations, key+
				": listed in eventHistoryUpdateSites but no longer found in the tree -- "+
				"remove the stale entry (the declaration was renamed, moved, or no longer "+
				"assigns an EncryptedEventColumns member of event_history)")
		}
	}
	return violations
}

// TestEveryEventHistoryUpdateOfAnEncryptedColumnRoutesThroughTheEncoder is
// cleat#2335's UPDATE-side check, alongside
// TestEveryEventHistoryWriteRoutesThroughTheEncoder's INSERT-side one: every
// `UPDATE ... event_history SET ...` in engine/ that assigns an
// EncryptedEventColumns member sits inside a declaration named on
// eventHistoryUpdateSites, classified the same way as the INSERT-side
// manifest.
func TestEveryEventHistoryUpdateOfAnEncryptedColumnRoutesThroughTheEncoder(t *testing.T) {
	files := map[string]string{}
	for _, f := range engineGoFiles(t) {
		files[f] = readEngineFile(t, f)
	}
	for _, v := range checkEventHistoryUpdateSites(t, files, eventHistoryUpdateSites) {
		t.Error(v)
	}
}

// TestContainsEventHistoryUpdateMatchesEveryKnownShape is the known-positive
// for containsEventHistoryUpdate/touchesEncryptedColumn: every real
// placeholder style store_intent.go uses (Postgres $N, MySQL ?, MSSQL @pN),
// case and schema-prefix variance matching eventHistoryWriteRE's own cases,
// plus the negative cases specific to this side -- a checksum-only or
// intent_at-only SET, which must NOT require classification even though it
// is a genuine event_history UPDATE.
func TestContainsEventHistoryUpdateMatchesEveryKnownShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"single column, Postgres placeholders", `UPDATE event_history SET response = $1 WHERE workflow_id = $2`},
		{"multiple columns, SET on next line", "UPDATE event_history\nSET response = $3, error = $4, payload = $5, checksum = $6, intent_at = NULL\nWHERE workflow_id = $1"},
		{"MySQL ? placeholders", `UPDATE event_history SET response = ?, error = ?, payload = ?, checksum = ? WHERE workflow_id = ?`},
		{"MSSQL @pN placeholders", `UPDATE event_history SET response = @p3, error = @p4, payload = @p5, checksum = @p6 WHERE workflow_id = @p1`},
		{"lowercase", `update event_history set response = $1 where workflow_id = $2`},
		{"schema-qualified", `UPDATE dbo.event_history SET response = $1 WHERE workflow_id = $2`},
		{"an encrypted column other than response", `UPDATE event_history SET plugin_output = $1 WHERE workflow_id = $2`},
		{"nested EXISTS in the outer WHERE, encrypted column in SET", `UPDATE event_history SET response = $1 WHERE workflow_id = $2 AND ($3 = '' OR EXISTS (SELECT 1 FROM workflow_instances WHERE id = $2 AND assigned_to = $3))`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !containsEventHistoryUpdate(tc.src) {
				t.Errorf("containsEventHistoryUpdate did not match: %s", tc.src)
			}
		})
	}

	for _, tc := range []struct {
		name string
		src  string
	}{
		{"checksum only", `UPDATE event_history SET checksum = $1 WHERE workflow_id = $2`},
		{"intent_at only", `UPDATE event_history SET intent_at = NULL WHERE workflow_id = $2`},
		{"suffixed table name", `UPDATE event_history_v2 SET response = $1 WHERE workflow_id = $2`},
		{"unrelated table", `UPDATE workflow_instances SET status = $1 WHERE id = $2`},
	} {
		t.Run("negative: "+tc.name, func(t *testing.T) {
			if containsEventHistoryUpdate(tc.src) {
				t.Errorf("containsEventHistoryUpdate matched a statement it should not have: %s", tc.src)
			}
		})
	}
}

// TestEventHistoryUpdateSiteGuardCatchesAKnownBreak is
// checkEventHistoryUpdateSites' own known-positive, mirroring
// TestEventHistoryInsertSiteGuardCatchesAKnownBreak for the UPDATE side --
// CLAUDE.md's "before trusting a new guard, run its parse once against a
// case already known to be broken".
func TestEventHistoryUpdateSiteGuardCatchesAKnownBreak(t *testing.T) {
	const unclassified = `package engine

func (s *PostgresStore) hypotheticalNewUpdater() error {
	_, err := s.db.Exec("UPDATE event_history SET response = $1 WHERE workflow_id = $2", "x", "y")
	return err
}
`
	if v := checkEventHistoryUpdateSites(t, map[string]string{"x.go": unclassified}, eventHistoryUpdateSites); len(v) == 0 {
		t.Error("an unclassified event_history UPDATE of an encrypted column was not reported")
	}

	const claimsEncoderButDoesNot = `package engine

func (s *PostgresStore) hypotheticalNewUpdater() error {
	_, err := s.db.Exec("UPDATE event_history SET response = $1 WHERE workflow_id = $2", "x", "y")
	return err
}
`
	fakeManifest := map[string]eventHistorySite{
		"x.go:PostgresStore.hypotheticalNewUpdater": {status: siteEncoder, reason: "test fixture"},
	}
	if v := checkEventHistoryUpdateSites(t, map[string]string{"x.go": claimsEncoderButDoesNot}, fakeManifest); len(v) == 0 {
		t.Error("an \"encoder\" UPDATE site whose function does not call encodeEventForStorage was not reported")
	}

	const empty = `package engine
`
	if v := checkEventHistoryUpdateSites(t, map[string]string{"x.go": empty}, fakeManifest); len(v) == 0 {
		t.Error("a stale manifest entry naming an UPDATE site absent from the tree was not reported")
	}

	// Negative control: a correctly classified encoder site reports nothing.
	const correct = `package engine

func (s *PostgresStore) hypotheticalNewUpdater() error {
	stored, err := encodeEventForStorage(EventRecord{}, s.encryption, s.encryptSensitivePayloads, "t")
	if err != nil {
		return err
	}
	_, err = s.db.Exec("UPDATE event_history SET response = $1 WHERE workflow_id = $2", stored.Response, "y")
	return err
}
`
	if v := checkEventHistoryUpdateSites(t, map[string]string{"x.go": correct}, fakeManifest); len(v) != 0 {
		t.Errorf("a correctly classified encoder UPDATE site was reported as broken: %v", v)
	}

	// Negative control unique to the UPDATE side: an UPDATE of event_history
	// that only assigns checksum -- not an EncryptedEventColumns member --
	// needs no classification at all, the same as the three real
	// repairChainAfterResolve declarations. Confirm it is not flagged as
	// unclassified even though it is a genuine event_history UPDATE.
	const checksumOnly = `package engine

func (s *PostgresStore) hypotheticalChecksumRepair() error {
	_, err := s.db.Exec("UPDATE event_history SET checksum = $1 WHERE workflow_id = $2", "c", "y")
	return err
}
`
	if v := checkEventHistoryUpdateSites(t, map[string]string{"x.go": checksumOnly}, map[string]eventHistorySite{}); len(v) != 0 {
		t.Errorf("an UPDATE that only sets checksum (not an EncryptedEventColumns member) was flagged even though it needs no classification: %v", v)
	}
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
