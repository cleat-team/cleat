package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// No PostgreSQL statement may reach a row-level-security table without the
// tenant set on its transaction.
//
// cleat#1177, censused in cleat#1178. `PostgresStore.successorOfRun` issued
//
//	SELECT id FROM workflow_instances WHERE continued_from = $1
//
// through s.db.QueryRowContext -- no transaction, so no
// `set_config('cleat.tenant_id', ...)`. workflow_instances is ENABLE + FORCE
// ROW LEVEL SECURITY with a fail-closed policy, `USING (tenant_id =
// cleat.assert_tenant_set())`, and that function RAISEs when the tenant is
// unset. Every continue-as-new chain with a successor errored.
//
// THE DECIDING VARIABLE IS A PROPERTY OF THE TRANSACTION, NOT OF THE SQL, and
// this is the whole reason the guard is shaped the way it is. The obvious
// criterion -- "the statement carries AND tenant_id = $n, so it is scoped" --
// is wrong. Measured on a live database as cleat_app (cleat#1178):
//
//	SET ROLE cleat_app;
//	SELECT id FROM workflow_instances WHERE continued_from='...' AND tenant_id='2222...';
//	ERROR:  cleat.tenant_id is not set -- tenant context required for RLS-scoped query
//
// A policy is applied IN ADDITION to the query's own predicates, never instead
// of them: `assert_tenant_set()` raises the moment a candidate row is examined,
// whatever the WHERE clause says. So a guard keyed on the SQL mentioning
// tenant_id passes both known faults, including the one it exists to prevent.
//
// WHY THIS IS STATIC AND NOT AN INTEGRATION TEST. A policy's USING is evaluated
// PER CANDIDATE ROW, so the same check against an empty table returns
// `(0 rows)` and no error under every role -- indistinguishable from working.
// Two sessions were caught by that on 2026-09-10. A database-backed version of
// this check needs a seeded row to mean anything; a static one has no such
// failure mode.
//
// And CI could not find these anyway: every job hands the Go suite a superuser
// DSN, and a superuser bypasses RLS unconditionally, FORCE included.
//
// WHAT THIS GUARD DOES NOT COVER, SAID HERE SO ITS SILENCE IS NOT READ AS
// COVERAGE. There are two ways a statement and a policy can disagree, and this
// checks one of them:
//
//	fail-CLOSED   the statement cannot run at all -- no tenant is set, so
//	              assert_tenant_set() raises. Loud, and what this guard finds.
//	fail-OPEN     the statement runs unscoped -- it carries no tenant predicate
//	              of its own and relies entirely on the policy to scope it. On a
//	              connection that bypasses RLS (a superuser, and every CI job
//	              uses one) it returns other tenants' rows and nothing errors.
//
// The second is cleat#1180, and it is NOT a defect the way the first is: a
// statement relying on the policy is the architecture working as intended.
// Which is exactly why it cannot be checked the way this file checks the other
// -- there is no rule of the form "a statement naming an RLS table must carry a
// tenant predicate" that is true, so a static guard would either flag the
// intended design or nothing at all. Read cleat#1180 for the counts rather than
// copying them here, where they would rot.
//
// So: a green run of this test means no statement is unable to run. It says
// nothing about what a statement returns on a connection that bypasses the
// policy.
func TestNoPostgresStatementReachesAnRLSTableWithoutTheTenantSet(t *testing.T) {
	rls := rlsTablesFromMigrations(t)
	if len(rls) < 5 {
		t.Fatalf("only %d RLS tables found in migrations/postgres; the migration scan is "+
			"broken and this guard asserts nothing", len(rls))
	}

	files := postgresStoreFiles(t)
	if len(files) < 20 {
		t.Fatalf("only %d PostgreSQL store files found; the file walk is broken", len(files))
	}

	consts := collectSQLConsts(t, files)
	if len(consts) == 0 {
		t.Fatal("resolved no package-level string constants; every query held in a const " +
			"would be reported unreadable and the allowlist below would be meaningless")
	}

	var found, unreadable []rlsFault
	cleared := 0
	for _, path := range files {
		f, fset := parseGo(t, path)
		if f == nil {
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			recv := receiverTypeName(fn)
			if receiversWithoutRLS[recv] || receiversWithNoProductionConstructor[recv] {
				continue
			}
			fs, un, n := rlsFaultsInFunc(fn, fset, rls, consts)
			found = append(found, fs...)
			unreadable = append(unreadable, un...)
			cleared += n
		}
	}

	// THE FLOOR COUNTS ONLY STATEMENTS THIS GUARD HAS AN OPINION ABOUT --
	// covered by a tenant, or with a query it could resolve. It used to count
	// unreadable statements too, so "the AST match is broken" could not be
	// distinguished from "the AST match works and the guard understood none of
	// it" -- cleat#1672, and the reason that issue exists.
	if cleared == 0 {
		t.Fatal("cleared no statements at all; the AST match is broken and a pass here " +
			"means nothing")
	}

	// Exemptions must stay LIVE. An allowlist that may only shrink is the
	// stated goal; one that cannot outlive its cause is the enforceable form of
	// it -- when the fix lands, the stale entry fails here rather than being
	// carried quietly.
	remaining := map[string]string{}
	for k, v := range knownRLSFaults {
		remaining[k] = v
	}
	byDesign := map[string]string{}
	for k, v := range statementsWithoutATenantByDesign {
		byDesign[k] = v
	}
	var unexpected []string
	for _, f := range found {
		key := shortPos(f.pos)
		if _, ok := remaining[key]; ok {
			delete(remaining, key)
			continue
		}
		if _, ok := byDesign[key]; ok {
			delete(byDesign, key)
			continue
		}
		unexpected = append(unexpected, key+"  ->  "+f.table+"  ("+f.why+")")
	}
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		t.Errorf("%d PostgreSQL statement(s) reach a row-level-security table with no tenant "+
			"set on the transaction. The policy raises on the first candidate row whatever "+
			"the WHERE clause says, so adding `AND tenant_id = $n` does NOT fix this -- open "+
			"the transaction with beginTxWithRLS, or call setRLSOnTx after BeginTx:\n  %s",
			len(unexpected), strings.Join(unexpected, "\n  "))
	}
	for key, reason := range remaining {
		t.Errorf("the exemption for %s no longer matches any statement (%s). It has been "+
			"fixed or moved -- delete the entry rather than leaving an allowance whose "+
			"cause is gone", key, reason)
	}

	for key, reason := range byDesign {
		t.Errorf("the by-design entry for %s no longer matches any statement (%s). The gate it "+
			"describes has moved or gone -- re-read it rather than leaving a standing "+
			"allowance for a line that may no longer have one", key, reason)
	}

	// An unreadable statement is NOT evidence of safety, so it fails unless
	// someone has written down why the guard cannot read it. Same liveness rule
	// as above: an entry matching nothing is a grant covering something that is
	// not there.
	stillUnreadable := map[string]string{}
	for k, v := range knownUnreadableStatements {
		stillUnreadable[k] = v
	}
	var unexplained []string
	for _, u := range unreadable {
		key := shortPos(u.pos)
		if _, ok := stillUnreadable[key]; ok {
			delete(stillUnreadable, key)
			continue
		}
		unexplained = append(unexplained, key)
	}
	sort.Strings(unexplained)
	if len(unexplained) > 0 {
		t.Errorf("%d statement(s) reach the database with a query this guard cannot resolve "+
			"from the source, and are not in knownUnreadableStatements:\n  %s\n\n"+
			"An unreadable statement is not a safe statement -- it is one nothing has "+
			"checked. Either give the query a form that can be read (a literal, a "+
			"concatenation of literals and package constants, or fmt.Sprintf over one), or "+
			"add an entry saying why it cannot be.",
			len(unexplained), strings.Join(unexplained, "\n  "))
	}
	for key, reason := range stillUnreadable {
		t.Errorf("the unreadable-statement entry for %s no longer matches anything (%s). The "+
			"query became readable or moved -- delete the entry rather than leaving a "+
			"standing excuse for a statement that no longer needs one", key, reason)
	}

	// BOTH NUMBERS, ALWAYS. A single figure here was correct on every run and
	// read as ordinary while the transaction arm was examining almost nothing
	// (cleat#1672: 17 against 168 on one tree). A reader who wants to know
	// whether this guard is looking needs the denominator beside the verdict.
	t.Logf("cleared %d statements and could not read %d, across %d files against %d RLS "+
		"tables; %d known faults, %d by design, %d unreadable statements exempted",
		cleared, len(unreadable), len(files), len(rls),
		len(knownRLSFaults), len(statementsWithoutATenantByDesign),
		len(knownUnreadableStatements))
}

type rlsFault struct{ pos, table, why string }

// knownRLSFaults are the two instances censused in cleat#1178. Each must still
// match a real statement -- see the staleness check above.
// The entry for terminal_run.go:132 (cleat#1177, successorOfRun) was deleted
// when cleat#1179 landed -- and it was this guard's own liveness check that
// said so, minutes after that merge, rather than anyone remembering:
//
//	the exemption for terminal_run.go:132 no longer matches any statement.
//	It has been fixed or moved -- delete the entry rather than leaving an
//	allowance whose cause is gone
//
// That is the whole argument for requiring exemptions to stay live. A stale
// allowance is not inert: terminal_run.go:132 is now a perfectly ordinary line,
// and an exemption still naming it would silently cover whatever statement
// arrives there next.
// Empty, and that is the finding rather than a gap: cleat#1178's census found
// exactly two, cleat#1177 fixed the live one, and StreamEventHistory -- the
// latent one this entry covered -- now opens a transaction per page through
// beginTxWithRLS. Its exemption was removed by this guard's own liveness check
// rather than by anyone remembering, the same way terminal_run.go:132's was.
//
// An empty map is not a reason to delete the mechanism. The next statement
// written on s.db against an RLS table is the case it exists for, and it will
// be reported rather than exempted.
var knownRLSFaults = map[string]string{
	// Empty again as of cleat#1677. The adaptive_flush.go:253 entry that stood
	// here was written by cleat#1672 -- which found the fault, could not choose
	// its fix, and listed it so that this guard's liveness check would force the
	// entry out the moment somebody did. That is what happened: the fix opens a
	// transaction and calls setRLSOnFlushTx, the exemption stopped matching any
	// statement, and the guard said so in CI before any human looked.
}

// statementsWithoutATenantByDesign reach an RLS table with no tenant AND ARE
// CORRECT, because they only execute in a configuration where there is no
// tenant to set.
//
// SEPARATE FROM knownRLSFaults ON PURPOSE. That map carries real, unfixed
// defects and its stated goal is to reach zero; an entry that is correct as
// written can never leave it, so mixing the two would quietly retire the "may
// only shrink" property that makes it worth having. Two maps, two invariants:
// one shrinks, this one does not.
//
// Each entry still has to name the gate, and the gate has to be checkable by
// reading one `if`. "It is fine" is not a reason.
var statementsWithoutATenantByDesign = map[string]string{
	"flush.go:453": "the UNTENANTED path of Engine.flushEvent, guarded by `if e.tenantID != \"\"` " +
		"immediately above it -- the tenanted branch opens a transaction, calls " +
		"setRLSOnFlushTx and returns, so this line runs only when there is no tenant to set. " +
		"Not reached by a worker: cmd/cleat-worker/setup.go:2260 always passes " +
		"engine.WithTenantID(wf.TenantID), and workflow_instances.tenant_id is NOT NULL with " +
		"a default, so wf.TenantID is never empty there. It serves the embedded and test " +
		"engines, against databases where no policy is installed",
}

// knownUnreadableStatements are the statements whose query this guard cannot
// resolve from the source, each with the reason it cannot.
//
// WHY THIS IS A LIST AND NOT A COUNTER. Before cleat#1672 an unreadable
// statement incremented the same counter as one the guard had read, so 19 of
// the 168 it reported as "examined" were statements it had no opinion about,
// and nothing in the output said which. A count cannot be reviewed; a list can,
// and the liveness check below means an entry cannot outlive its cause.
//
// TWO CLASSES, AND ONLY ONE OF THEM CAN SHRINK:
//
//	built at runtime   the query is assembled from user input, sort orders or
//	                   filter clauses. No static pass can read these, and that
//	                   is a property of the code rather than of this guard.
//	                   Permanent.
//	everything else    a form this guard has not been taught. Should not
//	                   appear -- concatenations, package constants, fmt.Sprintf
//	                   and leading SQL comments are all resolved now. A new
//	                   entry here is a prompt to teach sqlTextOf, not to write
//	                   a reason.
//
// Keep the two labelled, so the second class is visibly zero rather than
// buried among the first.
var knownUnreadableStatements = map[string]string{
	"db.go:1984": "built at runtime: `CREATE SCHEMA IF NOT EXISTS ` + pq.QuoteIdentifier(" +
		"f.schemaName), where the schema name is a field. The literal half is DDL naming no " +
		"table, but the guard reports the statement rather than the half it can read -- a " +
		"partial resolution could be dropping a FROM clause, which is the failure this whole " +
		"file exists to prevent, reintroduced as a convenience",
}

// receiversWithoutRLS are store types whose backends have no row-level
// security, so a statement of theirs naming one of these tables is not a
// fault. successorOfRun has all three implementations in ONE file
// (terminal_run.go), so filtering by filename cannot separate them -- the
// receiver type is the only thing that can.
var receiversWithoutRLS = map[string]bool{
	"MySQLStore": true, "MSSQLStore": true,
}

// receiversWithNoProductionConstructor were examined and excluded, with the
// command that re-derives the exclusion. Both reach RLS tables outside a
// transaction and neither can execute in a running worker: their only
// constructor calls are in engine/unit_test.go, every one passing a nil db.
//
//	grep -rn 'NewWorkflowLoader(\|NewFaultInjector(' --include='*.go' . | grep -v 'func New'
//
// Guarding them would add nine exemptions for no safety, and a guard that
// reports non-faults gets switched off. If either is ever wired into a worker,
// the statements are already written and this exclusion is what to revisit.
var receiversWithNoProductionConstructor = map[string]bool{
	"WorkflowLoader": true, "FaultInjector": true,
}

func receiverTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	switch t := fn.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.Ident:
		return t.Name
	}
	return ""
}

func shortPos(p string) string {
	// /abs/path/engine/foo.go:12:34 -> engine/foo.go:12
	parts := strings.Split(p, ":")
	if len(parts) < 2 {
		return p
	}
	file := parts[0]
	if i := strings.LastIndex(file, "/engine/"); i >= 0 {
		file = file[i+1:]
	}
	return file + ":" + parts[1]
}

var reEnableRLS = regexp.MustCompile(`(?i)ALTER\s+TABLE\s+([A-Za-z_][A-Za-z0-9_.]*)\s+ENABLE\s+ROW\s+LEVEL\s+SECURITY`)
var reLineComment = regexp.MustCompile(`--[^\n]*`)
var reBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

// rlsTablesFromMigrations reads the table list from the migrations rather than
// a literal here. A literal silently stops covering the twelfth table the day
// someone adds one, and a guard that quietly narrows is worse than none.
func rlsTablesFromMigrations(t *testing.T) map[string]bool {
	t.Helper()
	dir := filepath.Join("..", "migrations", "postgres")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		// Strip comments first, or a header quoting an ALTER counts as one --
		// a text search cannot tell a thing from a sentence about the thing.
		s := reBlockComment.ReplaceAllString(string(b), " ")
		s = reLineComment.ReplaceAllString(s, " ")
		for _, m := range reEnableRLS.FindAllStringSubmatch(s, -1) {
			name := m[1]
			if i := strings.LastIndex(name, "."); i >= 0 {
				name = name[i+1:]
			}
			out[strings.ToLower(name)] = true
		}
	}
	return out
}

func postgresStoreFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read engine dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		if strings.HasPrefix(n, "mysql_") || strings.HasPrefix(n, "mssql_") {
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func parseGo(t *testing.T, path string) (*ast.File, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, nil
	}
	return f, fset
}

// dbStatementKind reports a call of the form s.db.<Kind>(...).
func dbStatementKind(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	if !ok || inner.Sel.Name != "db" {
		return "", false
	}
	switch sel.Sel.Name {
	case "QueryRowContext", "QueryContext", "ExecContext", "BeginTx":
		return sel.Sel.Name, true
	}
	return "", false
}

// rlsFaultsInFunc reports every statement in fn that reaches an RLS table with
// no tenant established on the transaction it runs on, and how many statements
// it examined. Extracted from the test body so the ordering and
// transaction-matching rules below can be exercised against synthetic source,
// which a scan wired only to the real tree cannot be once the real tree is
// clean.
func rlsFaultsInFunc(fn *ast.FuncDecl, fset *token.FileSet, rls map[string]bool,
	consts map[string]string) (faults, unreadable []rlsFault, cleared int) {

	ests := tenantEstablishments(fn)
	opensTx := functionOpensTx(fn)
	consts = withLocalConsts(fn, consts)

	note := func(call *ast.CallExpr, table, why string) {
		faults = append(faults, rlsFault{
			pos: fset.Position(call.Pos()).String(), table: table, why: why,
		})
	}

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		// Classify the statement: on the pool, on a transaction, or neither.
		var why string
		var isStmt bool
		if kind, isDB := dbStatementKind(call); isDB {
			if kind == "BeginTx" {
				return true // reached through its tx statements instead
			}
			isStmt = true
			why = "s.db." + kind + " runs outside any transaction, so the tenant can never be set for it"
		} else if opensTx {
			if txVar, isTx := txStatementVar(call); isTx {
				isStmt = true
				if establishedBefore(ests, txVar, call.Pos()) {
					// COVERED, AND COUNTED. The tenant is established on this
					// transaction before this point, so the answer is "not a
					// fault" whatever the SQL says -- there is no need to
					// resolve it, and demanding a readability entry for it
					// would be noise. It is still a statement this guard has an
					// opinion about, so it counts toward the floor.
					cleared++
					return true
				}
				why = "runs on " + txVar + " in " + fn.Name.Name + ", which has no setRLSOnTx or " +
					"beginTxWithRLS for " + txVar + " ahead of this point"
			}
		}
		if !isStmt {
			return true
		}

		sql, kind := queryArgOf(call, consts)
		switch kind {
		case argUnreadable:
			// NOT counted as examined. cleat#1672: this used to increment the
			// same counter as a statement the guard had actually read, so the
			// floor assertion below was satisfied by statements it could say
			// nothing about.
			unreadable = append(unreadable, rlsFault{
				pos: fset.Position(call.Pos()).String(), table: "?",
				why: "the query is not resolvable from the source, so this guard has no opinion about it",
			})
			return true
		case argNonSQL:
			// SAVEPOINT, ROLLBACK TO SAVEPOINT, CREATE SCHEMA. Resolved, reads
			// no row of any table, and therefore not a fault -- but it IS read,
			// which is why it is counted rather than filed as unreadable.
			cleared++
			return true
		}

		cleared++
		if setsTenantInSQL(sql) {
			return true
		}
		if hit := namesRLSTable(sql, rls); hit != "" {
			note(call, hit, why)
		}
		return true
	})
	return faults, unreadable, cleared
}

// tenantEstablishment records WHERE the tenant was set and on WHICH transaction
// variable. The guard had neither, and cleat#1534 is what makes both load
// bearing.
//
// ORDER. This was a boolean over the whole function body, on the stated grounds
// that StartNewRun does its idempotency_keys work first and calls setRLSOnTx
// after. That was sound only while idempotency_keys carried no policy. Give it
// one and the very statement that comment points at becomes the fault -- and a
// whole-body boolean scores it covered, so the guard would pass the tree this
// change exists to fix.
//
// VARIABLE. A call establishes the tenant on the transaction it is HANDED.
// setRLSOnTx(tx1) says nothing about tx2. Name alone cannot separate them in
// startNewRun, which has two transactions in sibling scopes BOTH CALLED tx;
// position can, and does.
type tenantEstablishment struct {
	name string // the transaction variable it was applied to; "" means all
	pos  token.Pos
}

func tenantEstablishments(fn *ast.FuncDecl) []tenantEstablishment {
	var out []tenantEstablishment
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		// tx, err := s.beginTxWithRLS(ctx) -- the variable is on the left, and
		// the transaction is established the moment it exists.
		if as, ok := n.(*ast.AssignStmt); ok {
			if !assignsFrom(as, "beginTxWithRLS") {
				return true
			}
			for _, lhs := range as.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name != "_" && id.Name != "err" {
					out = append(out, tenantEstablishment{name: id.Name, pos: as.Pos()})
				}
			}
			return true
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch calleeName(call) {
		case "setRLSOnTx", "setRLSOnFlushTx":
			// The transaction is an argument. Record every ident argument
			// rather than a fixed position: the two have it at different
			// indices, a spurious name matches no statement, and a missed one
			// would hide a fault.
			for _, a := range call.Args {
				if id, ok := a.(*ast.Ident); ok {
					out = append(out, tenantEstablishment{name: id.Name, pos: call.Pos()})
				}
			}
		case "withRLSTx":
			// A closure form: the transaction is created inside and never
			// named at the call site, so there is no variable to key on.
			// Recorded as covering everything after it, which is what the
			// boolean did for all four names. There is no such function in the
			// tree today -- grep says so -- and this entry is inherited rather
			// than measured. If one is written, check it against the ordering
			// rule above before trusting this line.
			out = append(out, tenantEstablishment{name: "", pos: call.Pos()})
		}
		return true
	})
	return out
}

// establishedBefore reports whether the tenant was set on txVar EARLIER IN THE
// SOURCE than pos.
//
// Source order is not execution order in general: two sibling branches are laid
// out in an order neither of them runs in. Requiring the variable to match as
// well is what makes that safe here -- a statement on tx2 is not covered by an
// establishment on tx1 however the two are arranged, and two transactions
// sharing a name in sibling scopes are separated by position, which is exactly
// startNewRun's shape.
func establishedBefore(ests []tenantEstablishment, txVar string, pos token.Pos) bool {
	for _, e := range ests {
		if e.name != "" && e.name != txVar {
			continue
		}
		if e.pos < pos {
			return true
		}
	}
	return false
}

// txStatementVar reports a statement run on a transaction, and the variable it
// runs on. Every transaction in the PostgreSQL store is called tx today
//
//	grep -nE '\b[A-Za-z_][A-Za-z0-9_]*, err :?= s\.(db\.BeginTx|beginTxWithRLS)\(' engine/*.go
//
// so the prefix admits a second one -- tx2 -- rather than silently not checking
// it. Over-matching is the safe direction: a non-transaction identifier with an
// ExecContext method and a tx-shaped name would be reported, not missed.
func txStatementVar(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || !strings.HasPrefix(id.Name, "tx") {
		return "", false
	}
	switch sel.Sel.Name {
	case "QueryRowContext", "QueryContext", "ExecContext", "Exec", "Query", "QueryRow":
		return id.Name, true
	}
	return "", false
}

// functionOpensTx reports whether the function opens a transaction of its own,
// by either route.
func functionOpensTx(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if k, ok := dbStatementKind(call); ok && k == "BeginTx" {
			found = true
		}
		if calleeName(call) == "beginTxWithRLS" {
			found = true
		}
		return true
	})
	return found
}

func calleeName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.SelectorExpr:
		return f.Sel.Name
	case *ast.Ident:
		return f.Name
	}
	return ""
}

func assignsFrom(as *ast.AssignStmt, name string) bool {
	for _, rhs := range as.Rhs {
		if call, ok := rhs.(*ast.CallExpr); ok && calleeName(call) == name {
			return true
		}
	}
	return false
}

var reSQLString = regexp.MustCompile(`(?is)^\s*(SELECT|INSERT|UPDATE|DELETE|WITH)\b`)

// reLeadingSQLComment strips comment lines from the FRONT of a statement only.
//
// Not a general SQL comment stripper, deliberately: `--` inside a quoted
// literal is not a comment, and this runs before namesRLSTable has stripped
// the quotes. Anchored at the start, it can only ever consume text that
// precedes the verb, which is the one thing it is for -- db.go:1028 is a
// complete UPDATE whose first line is
//
//	-- No AND tenant_id, deliberately: RLS bounds this.
//
// and `^\s*(SELECT|...)` does not match it. The statement was invisible to
// this guard for that reason alone, with the answer sitting in the source.
var reLeadingSQLComment = regexp.MustCompile(`(?m)\A(?:[ \t\r\n]*--[^\n]*\n)+`)

// argKind is the three-way outcome of looking at a statement's query argument.
// TWO outcOMES ARE NOT ENOUGH, and that is the whole of cleat#1672: "not SQL
// this guard should check" and "SQL this guard cannot read" were both reported
// as the empty string, so an unreadable statement was indistinguishable from a
// SAVEPOINT.
type argKind int

const (
	argSQL        argKind = iota // resolved, and it reads or writes rows
	argNonSQL                    // resolved, and it does not -- SAVEPOINT, CREATE SCHEMA
	argUnreadable                // NOT resolved; the guard has no opinion and must say so
)

// queryArgOf returns the statement's query text and how much of it is known.
//
// It takes the query by POSITION rather than hunting for the first string
// argument that looks like SQL. The position is fixed by the database/sql
// method being called, and keying on it removes a guessing step that could
// silently pick a different argument.
func queryArgOf(call *ast.CallExpr, consts map[string]string) (string, argKind) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", argUnreadable
	}
	idx := 0
	if strings.HasSuffix(sel.Sel.Name, "Context") {
		idx = 1 // (ctx, query, args...)
	}
	if len(call.Args) <= idx {
		return "", argUnreadable
	}
	text, ok := sqlTextOf(call.Args[idx], consts)
	if !ok {
		return "", argUnreadable
	}
	if !reSQLString.MatchString(reLeadingSQLComment.ReplaceAllString(text, "")) {
		return text, argNonSQL
	}
	return text, argSQL
}

// sqlTextOf resolves an expression to the SQL it produces, or reports that it
// cannot.
//
// FAILING IS THE SAFE DIRECTION AND IS TAKEN DELIBERATELY. A concatenation
// with one unresolvable part could be dropping the FROM clause, so a partial
// resolution would let a statement name an RLS table invisibly -- the exact
// failure this guard exists to prevent, reintroduced by a convenience. Any
// unresolvable part makes the whole expression unreadable, and unreadable is
// reported rather than skipped.
func sqlTextOf(e ast.Expr, consts map[string]string) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		if err != nil {
			return "", false
		}
		return s, true

	case *ast.Ident:
		// A package-level string constant, which is where several of these
		// statements keep their query. Moving a literal into a const is an
		// unambiguous improvement in review and used to remove the statement
		// from this guard's coverage with nothing failing.
		s, ok := consts[x.Name]
		return s, ok

	case *ast.ParenExpr:
		return sqlTextOf(x.X, consts)

	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		l, lok := sqlTextOf(x.X, consts)
		if !lok {
			return "", false
		}
		r, rok := sqlTextOf(x.Y, consts)
		if !rok {
			return "", false
		}
		return l + r, true

	case *ast.CallExpr:
		// fmt.Sprintf's FORMAT string carries the table names; the verbs it
		// interpolates carry values and predicates. adaptive_flush.go:253 is
		// this shape, and cleat#1677 is the live fault it was hiding.
		if f, ok := x.Fun.(*ast.SelectorExpr); ok {
			if pkg, ok := f.X.(*ast.Ident); ok && pkg.Name == "fmt" && f.Sel.Name == "Sprintf" && len(x.Args) > 0 {
				return sqlTextOf(x.Args[0], consts)
			}
		}
		return "", false
	}
	return "", false
}

// withLocalConsts layers a function's own `const q = ...` declarations over the
// package-level ones.
//
// A function-local const is where CheckCrossTenantCapability keeps its query,
// and scoping is the reason it needs its own pass rather than being swept into
// the package map: two functions may both declare `q`, and merging them into
// one namespace would let one function's query answer for another's. Layered
// per function, the local declaration shadows, which is what Go does.
//
// Returns the original map unchanged when there is nothing local, so the common
// case allocates nothing.
func withLocalConsts(fn *ast.FuncDecl, pkg map[string]string) map[string]string {
	var local map[string]string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ds, ok := n.(*ast.DeclStmt)
		if !ok {
			return true
		}
		gd, ok := ds.Decl.(*ast.GenDecl)
		if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
			return true
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != len(vs.Values) {
				continue
			}
			for i, name := range vs.Names {
				if text, ok := sqlTextOf(vs.Values[i], pkg); ok {
					if local == nil {
						local = map[string]string{}
					}
					local[name.Name] = text
				}
			}
		}
		return true
	})
	if local == nil {
		return pkg
	}
	merged := make(map[string]string, len(pkg)+len(local))
	for k, v := range pkg {
		merged[k] = v
	}
	for k, v := range local {
		merged[k] = v
	}
	return merged
}

// collectSQLConsts reads every package-level string const and var in the files
// given, so an identifier used as a query can be resolved to its text.
//
// Two passes, because a const may be built from other consts. Two rather than
// a fixed point: one is demonstrably not enough in this package and a loop
// would need a cycle guard for no benefit that exists here. A name that is
// still unresolved after the second pass stays unresolved, and its statement
// is reported as unreadable rather than quietly skipped -- which is the whole
// point of this change, so the limit fails in the direction that tells you.
func collectSQLConsts(t *testing.T, files []string) map[string]string {
	t.Helper()
	type pending struct {
		name string
		expr ast.Expr
	}
	var todo []pending
	out := map[string]string{}
	for _, path := range files {
		f, _ := parseGo(t, path)
		if f == nil {
			continue
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != len(vs.Values) {
					continue
				}
				for i, name := range vs.Names {
					todo = append(todo, pending{name.Name, vs.Values[i]})
				}
			}
		}
	}
	for pass := 0; pass < 2; pass++ {
		for _, p := range todo {
			if _, done := out[p.name]; done {
				continue
			}
			if s, ok := sqlTextOf(p.expr, out); ok {
				out[p.name] = s
			}
		}
	}
	return out
}

var reSQLLiteral = regexp.MustCompile(`'[^']*'`)

// namesRLSTable returns the first RLS table the statement REFERENCES, or "".
//
// Quoted string literals are stripped first. A table name inside a string is
// not a reference to the table -- `pg_total_relation_size('event_history')` is
// a catalog function taking a name, reading no rows, so no policy is ever
// evaluated. Keyed on the shape rather than on that one call site, so the same
// shape elsewhere is also not flagged: a guard that reports a non-fault gets
// switched off.
func namesRLSTable(sql string, rls map[string]bool) string {
	stripped := reSQLLiteral.ReplaceAllString(sql, "''")
	lower := strings.ToLower(stripped)
	var names []string
	for tbl := range rls {
		names = append(names, tbl)
	}
	sort.Strings(names)
	for _, tbl := range names {
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(tbl) + `\b`)
		if re.MatchString(lower) {
			return tbl
		}
	}
	return ""
}

var reSetConfigInSQL = regexp.MustCompile(`(?i)set_config\s*\(\s*'cleat\.tenant_id'`)

// setsTenantInSQL reports whether the statement establishes the tenant itself.
//
// THIS IS A THIRD FORM AND THE GUARD WAS BLIND TO IT ON ITS FIRST RUN. The
// adaptive flusher carries
//
//	WITH cfg AS (SELECT set_config('cleat.tenant_id', ($1::jsonb->0->>'tenant_id'), true))
//	INSERT INTO event_history (...)
//
// in the statement rather than calling setRLSOnTx around it -- flush.go:102
// explains why that CTE form is needed there. A guard looking only for Go-level
// calls scores both flusher statements a fault, and they are correct.
func setsTenantInSQL(sql string) bool {
	return reSetConfigInSQL.MatchString(sql)
}

// The guard's parts must each be able to disagree.
//
// The scan above passes on a clean tree, and so would every broken version of
// it. These are synthetic, so they keep working once the tree is clean and a
// real-site mutation no longer exists to perform.
//
// Verified additionally against real sites while writing this, which the
// synthetic cases cannot replace:
//
//	unexempt terminal_run.go:132       -> reports exactly it
//	delete `AND tenant_id = $3` from a  -> STAYS SILENT; a tenant predicate is
//	  statement inside an RLS tx           not what makes a statement safe
//	move a tx statement onto s.db      -> reports store_lifecycle.go:404
//
// The middle one is the one that matters: it is the difference between a guard
// keyed on the deciding variable and one keyed on something that correlates
// with it.
func TestTheRLSGuardsPartsCanDisagree(t *testing.T) {
	rls := map[string]bool{"workflow_instances": true, "event_history": true}

	t.Run("a table named only inside a string literal is not a reference", func(t *testing.T) {
		// pg_total_relation_size takes the table NAME and reads no rows, so no
		// policy is ever evaluated. Keyed on the shape, not on one call site.
		if hit := namesRLSTable(`SELECT pg_total_relation_size('event_history')`, rls); hit != "" {
			t.Errorf("flagged a catalog function naming a table as a read of it: %s", hit)
		}
		if hit := namesRLSTable(`SELECT id FROM event_history WHERE x = $1`, rls); hit == "" {
			t.Error("did not flag a real reference to event_history")
		}
	})

	t.Run("a tenant predicate does not make a statement safe", func(t *testing.T) {
		// The criterion that would have passed both known faults.
		with := `SELECT id FROM workflow_instances WHERE continued_from = $1 AND tenant_id = $2`
		if hit := namesRLSTable(with, rls); hit == "" {
			t.Error("a tenant predicate suppressed the finding -- the guard is keyed on " +
				"the SQL text rather than on the transaction, which is the mistake " +
				"cleat#1178 measured")
		}
	})

	t.Run("set_config in the statement itself does establish the tenant", func(t *testing.T) {
		flusher := `WITH cfg AS (SELECT set_config('cleat.tenant_id', ($1::jsonb->0->>'tenant_id'), true))
			INSERT INTO event_history (workflow_id) VALUES ($2)`
		if !setsTenantInSQL(flusher) {
			t.Error("did not recognise the flusher's CTE form; the guard would score two " +
				"correct statements a fault, and a guard that reports non-faults gets " +
				"switched off")
		}
		if setsTenantInSQL(`SELECT id FROM workflow_instances WHERE continued_from = $1`) {
			t.Error("claimed a plain SELECT establishes the tenant")
		}
	})

	// ORDER AND TRANSACTION IDENTITY, the two things the guard could not see
	// until cleat#1534. These are synthetic on purpose: the real site that
	// proved them is startNewRun, and this PR fixes it, so a check wired to the
	// real tree would have nothing left to report the day it landed.
	//
	// Measured against the real tree before the fix, with idempotency_keys
	// added to the RLS set the migration in this PR gives it:
	//
	//	guard before this change   2 faults -- store_lifecycle.go:980, :1026
	//	guard after this change    3 faults -- and :1013, the INSERT on a
	//	                           transaction whose setRLSOnTx comes later
	//
	// :1013 is the statement the old comment on functionEstablishesTenant cited
	// as its reason for scanning the whole body. It was right that the scan
	// must not use a line window, and wrong that a boolean is the alternative.

	t.Run("a statement before setRLSOnTx is reported", func(t *testing.T) {
		faults, n := faultsInSource(t, `
			func (s *PostgresStore) f(ctx context.Context) error {
				tx, _ := s.db.BeginTx(ctx, nil)
				_, _ = tx.ExecContext(ctx, `+"`"+`INSERT INTO workflow_instances (id) VALUES ($1)`+"`"+`, 1)
				_ = s.setRLSOnTx(tx)
				return nil
			}`)
		if n == 0 {
			t.Fatal("examined no statements; the synthetic parse is broken and this " +
				"subtest asserts nothing")
		}
		if len(faults) != 1 {
			t.Fatalf("want exactly the statement ahead of setRLSOnTx, got %d: %v", len(faults), faults)
		}
	})

	t.Run("a statement after setRLSOnTx is not reported", func(t *testing.T) {
		// The negative control for the subtest above: same function, one line
		// moved. Without it, a guard that reports every tx statement passes the
		// known-positive and is useless.
		faults, n := faultsInSource(t, `
			func (s *PostgresStore) f(ctx context.Context) error {
				tx, _ := s.db.BeginTx(ctx, nil)
				_ = s.setRLSOnTx(tx)
				_, _ = tx.ExecContext(ctx, `+"`"+`INSERT INTO workflow_instances (id) VALUES ($1)`+"`"+`, 1)
				return nil
			}`)
		if n == 0 {
			t.Fatal("examined no statements; the synthetic parse is broken")
		}
		if len(faults) != 0 {
			t.Fatalf("reported a statement that runs after setRLSOnTx: %v", faults)
		}
	})

	t.Run("setRLSOnTx on one transaction does not cover another", func(t *testing.T) {
		faults, _ := faultsInSource(t, `
			func (s *PostgresStore) f(ctx context.Context) error {
				tx, _ := s.db.BeginTx(ctx, nil)
				_ = s.setRLSOnTx(tx)
				tx2, _ := s.db.BeginTx(ctx, nil)
				_, _ = tx2.ExecContext(ctx, `+"`"+`INSERT INTO workflow_instances (id) VALUES ($1)`+"`"+`, 1)
				return nil
			}`)
		if len(faults) != 1 {
			t.Fatalf("a set_config on tx was read as covering tx2; want 1 fault, got %d: %v",
				len(faults), faults)
		}
	})

	t.Run("a function handed a transaction is the caller's to establish", func(t *testing.T) {
		// The other direction, and the reason the tx arm is gated on opening a
		// transaction at all. Reporting these would bury the real findings.
		faults, _ := faultsInSource(t, `
			func (s *PostgresStore) f(ctx context.Context, tx *sql.Tx) error {
				_, _ = tx.ExecContext(ctx, `+"`"+`INSERT INTO workflow_instances (id) VALUES ($1)`+"`"+`, 1)
				return nil
			}`)
		if len(faults) != 0 {
			t.Fatalf("reported a statement on a caller-supplied transaction: %v", faults)
		}
	})

	// WHAT THE GUARD CAN READ, and what it must refuse to guess at. cleat#1672:
	// 19 of the 168 statements it reported as examined were ones it had not
	// read, and one of those was a live fault (cleat#1677).

	t.Run("a table named in a concatenated package constant is found", func(t *testing.T) {
		faults, unreadable, _ := scanSource(t, "const src = ` FROM workflow_instances WHERE id = $1`\n"+
			"func (s *PostgresStore) f(ctx context.Context) error {\n"+
			"\ttx, _ := s.db.BeginTx(ctx, nil)\n"+
			"\t_, _ = tx.ExecContext(ctx, `SELECT id`+src, 1)\n"+
			"\treturn nil\n}")
		if len(unreadable) != 0 {
			t.Fatalf("a concatenation of literals and a package const should be readable: %v", unreadable)
		}
		if len(faults) != 1 {
			t.Fatalf("the table lives in the CONSTANT half; want 1 fault, got %d", len(faults))
		}
	})

	t.Run("a table named inside fmt.Sprintf's format string is found", func(t *testing.T) {
		faults, unreadable, _ := scanSource(t, "func (s *PostgresStore) f(ctx context.Context) error {\n"+
			"\t_, _ = s.db.QueryContext(ctx, fmt.Sprintf(`UPDATE workflow_instances SET x = %s`, v))\n"+
			"\treturn nil\n}")
		if len(unreadable) != 0 {
			t.Fatalf("fmt.Sprintf's format string should be readable: %v", unreadable)
		}
		if len(faults) != 1 {
			t.Fatalf("want the Sprintf'd statement reported; got %d faults. This is the exact "+
				"shape of adaptive_flush.go:253, which was invisible until cleat#1672", len(faults))
		}
	})

	t.Run("a statement whose first line is a SQL comment is still SQL", func(t *testing.T) {
		faults, unreadable, _ := scanSource(t, "func (s *PostgresStore) f(ctx context.Context) error {\n"+
			"\t_, _ = s.db.ExecContext(ctx, `\n\t-- why this carries no tenant predicate\n"+
			"\tUPDATE workflow_instances SET x = 1\n`)\n\treturn nil\n}")
		if len(unreadable) != 0 {
			t.Fatalf("a leading SQL comment should not make a literal unreadable: %v", unreadable)
		}
		if len(faults) != 1 {
			t.Fatalf("want 1 fault, got %d -- `^\\s*(SELECT|...)` does not match a leading "+
				"`--`, which is how db.go:1028 stayed invisible", len(faults))
		}
	})

	t.Run("a query built at runtime is reported, not cleared", func(t *testing.T) {
		faults, unreadable, cleared := scanSource(t, "func (s *PostgresStore) f(ctx context.Context, q string) error {\n"+
			"\t_, _ = s.db.QueryContext(ctx, q)\n\treturn nil\n}")
		if len(unreadable) != 1 {
			t.Fatalf("want the unresolvable query reported as unreadable, got %d", len(unreadable))
		}
		if cleared != 0 {
			t.Errorf("an unreadable statement was counted as cleared (%d). That is the whole "+
				"of cleat#1672: the floor assertion is then satisfied by statements the "+
				"guard has no opinion about", cleared)
		}
		if len(faults) != 0 {
			t.Errorf("an unreadable statement is not a fault either; got %v", faults)
		}
	})

	t.Run("a PARTLY resolvable concatenation is unreadable, not partly read", func(t *testing.T) {
		// The safe direction, asserted rather than assumed. The readable half
		// names no table, so a partial resolution would CLEAR this statement
		// while the runtime half could carry `FROM workflow_instances`.
		faults, unreadable, cleared := scanSource(t, "func (s *PostgresStore) f(ctx context.Context, rest string) error {\n"+
			"\t_, _ = s.db.QueryContext(ctx, `SELECT id `+rest)\n\treturn nil\n}")
		if len(unreadable) != 1 {
			t.Fatalf("a concatenation with an unresolvable part must be unreadable, got %d "+
				"unreadable and %d faults", len(unreadable), len(faults))
		}
		if cleared != 0 {
			t.Errorf("the readable half cleared the statement; the other half could name any " +
				"table at all")
		}
	})

	t.Run("a resolved statement that reads no rows is neither fault nor unreadable", func(t *testing.T) {
		faults, unreadable, cleared := scanSource(t, "func (s *PostgresStore) f(ctx context.Context) error {\n"+
			"\ttx, _ := s.db.BeginTx(ctx, nil)\n"+
			"\t_, _ = tx.ExecContext(ctx, `SAVEPOINT before_thing`)\n\treturn nil\n}")
		if len(faults) != 0 || len(unreadable) != 0 {
			t.Fatalf("SAVEPOINT names no table and is fully readable; got %d faults, %d unreadable",
				len(faults), len(unreadable))
		}
		if cleared != 1 {
			t.Errorf("want it counted as cleared, got %d -- otherwise the guard reports a "+
				"non-fault, and a guard that reports non-faults gets switched off", cleared)
		}
	})

	t.Run("a function-local const does not answer for another function's", func(t *testing.T) {
		// Two functions, each with its own `q`. If the local scope leaked into
		// a shared namespace, the second would be read using the first's SQL
		// and scored a fault.
		faults, unreadable, _ := scanSource(t, "func (s *PostgresStore) a(ctx context.Context) error {\n"+
			"\tconst q = `SELECT id FROM workflow_instances`\n"+
			"\t_, _ = s.db.QueryContext(ctx, q)\n\treturn nil\n}\n"+
			"func (s *PostgresStore) b(ctx context.Context) error {\n"+
			"\tconst q = `SELECT 1`\n"+
			"\t_, _ = s.db.QueryContext(ctx, q)\n\treturn nil\n}")
		if len(unreadable) != 0 {
			t.Fatalf("both queries are function-local consts and should be readable: %v", unreadable)
		}
		if len(faults) != 1 {
			t.Fatalf("want exactly a's statement reported, got %d -- b's `q` is `SELECT 1` and "+
				"names no table", len(faults))
		}
	})

	t.Run("the migration scan finds the tables and not the prose about them", func(t *testing.T) {
		got := rlsTablesFromMigrations(t)
		for _, want := range []string{"workflow_instances", "event_history"} {
			if !got[want] {
				t.Errorf("the migration scan missed %s", want)
			}
		}
		// A count would rot as tables are added; the predicate does not.
		if len(got) < 5 {
			t.Errorf("only %d RLS tables parsed out of the migrations", len(got))
		}
	})
}

// faultsInSource runs the guard's scan over a single synthetic function.
func faultsInSource(t *testing.T, fn string) ([]rlsFault, int) {
	t.Helper()
	faults, _, cleared := scanSource(t, fn)
	return faults, cleared
}

// scanSource runs the whole scan over synthetic source, package-level consts
// included, and returns all three outcomes.
func scanSource(t *testing.T, src string) (faults, unreadable []rlsFault, cleared int) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", "package engine\n"+src, 0)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	rls := map[string]bool{"workflow_instances": true, "event_history": true}

	// The same two-pass package-const collection the real scan does, over this
	// one synthetic file.
	consts := map[string]string{}
	for pass := 0; pass < 2; pass++ {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != len(vs.Values) {
					continue
				}
				for i, name := range vs.Names {
					if text, ok := sqlTextOf(vs.Values[i], consts); ok {
						consts[name.Name] = text
					}
				}
			}
		}
	}

	for _, decl := range f.Decls {
		d, ok := decl.(*ast.FuncDecl)
		if !ok || d.Body == nil {
			continue
		}
		fs, un, n := rlsFaultsInFunc(d, fset, rls, consts)
		faults = append(faults, fs...)
		unreadable = append(unreadable, un...)
		cleared += n
	}
	return faults, unreadable, cleared
}
