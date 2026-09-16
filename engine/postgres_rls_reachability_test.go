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

	var found []rlsFault
	stmts := 0
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
			fs, n := rlsFaultsInFunc(fn, fset, rls)
			found = append(found, fs...)
			stmts += n
		}
	}

	if stmts == 0 {
		t.Fatal("examined no statements at all; the AST match is broken and a pass here " +
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
	var unexpected []string
	for _, f := range found {
		key := shortPos(f.pos)
		if _, ok := remaining[key]; ok {
			delete(remaining, key)
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
	t.Logf("examined %d statements across %d files against %d RLS tables; %d known faults "+
		"exempted", stmts, len(files), len(rls), len(knownRLSFaults))
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
var knownRLSFaults = map[string]string{}

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
func rlsFaultsInFunc(fn *ast.FuncDecl, fset *token.FileSet, rls map[string]bool) ([]rlsFault, int) {
	var found []rlsFault
	stmts := 0
	ests := tenantEstablishments(fn)
	opensTx := functionOpensTx(fn)

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		// A statement on the pool. There is no transaction for a set_config to
		// live on, so nothing the function does elsewhere can cover it.
		if kind, isDBStmt := dbStatementKind(call); isDBStmt {
			if kind == "BeginTx" {
				return true // reached through its tx statements instead
			}
			stmts++
			sql := sqlArgOf(call)
			if sql == "" || setsTenantInSQL(sql) {
				return true
			}
			if hit := namesRLSTable(sql, rls); hit != "" {
				found = append(found, rlsFault{
					pos: fset.Position(call.Pos()).String(), table: hit,
					why: "s.db." + kind + " runs outside any transaction, so the tenant can never be set for it",
				})
			}
			return true
		}
		// A statement on a transaction, checked only where the function opened
		// one itself. A function handed a *sql.Tx is the caller's to establish,
		// and reporting it would be a non-fault -- a guard that reports
		// non-faults gets switched off.
		if !opensTx {
			return true
		}
		txVar, isTx := txStatementVar(call)
		if !isTx {
			return true
		}
		stmts++
		sql := sqlArgOf(call)
		if sql == "" || setsTenantInSQL(sql) {
			return true
		}
		hit := namesRLSTable(sql, rls)
		if hit == "" {
			return true
		}
		if establishedBefore(ests, txVar, call.Pos()) {
			return true
		}
		found = append(found, rlsFault{
			pos: fset.Position(call.Pos()).String(), table: hit,
			why: "runs on " + txVar + " in " + fn.Name.Name + ", which has no setRLSOnTx or " +
				"beginTxWithRLS for " + txVar + " ahead of this point",
		})
		return true
	})
	return found, stmts
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

func sqlArgOf(call *ast.CallExpr) string {
	for _, a := range call.Args {
		b, ok := a.(*ast.BasicLit)
		if !ok || b.Kind != token.STRING {
			continue
		}
		s, err := strconv.Unquote(b.Value)
		if err != nil || !reSQLString.MatchString(s) {
			continue
		}
		return s
	}
	return ""
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
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", "package engine\n"+fn, 0)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	rls := map[string]bool{"workflow_instances": true}
	var out []rlsFault
	stmts := 0
	for _, decl := range f.Decls {
		d, ok := decl.(*ast.FuncDecl)
		if !ok || d.Body == nil {
			continue
		}
		fs, n := rlsFaultsInFunc(d, fset, rls)
		out = append(out, fs...)
		stmts += n
	}
	return out, stmts
}
