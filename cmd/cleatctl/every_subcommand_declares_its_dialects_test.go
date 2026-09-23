package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// unrestrictedSubcommands names the subcommands that deliberately run on every
// dialect, and says why for each.
//
// The counterpart of portedOn, and it lives here rather than beside it because
// it is an ASSERTION about those commands, not something cleatctl consults at
// runtime.
//
// Together the two maps close the hole cleat#1956 came through. Absence from
// portedOn means "runs on every dialect", so the default for a new subcommand
// is the least conservative behaviour available and it is reached by not typing
// anything. `queue` was added on 2026-09-20, never listed, and wrote to the
// wrong physical database on MySQL for two days -- silently, because the write
// succeeded and `queue list` read it straight back from the same wrong place.
// Two of the three subcommands added since portedOn was written were
// undeclared: that one, and suspend-tenant, which dies on MySQL with
// "Unknown database 'admin'".
//
// The reason string is not decoration. It is what a reviewer checks when the
// next subcommand arrives, and "it seemed to work" is visibly not one.
var unrestrictedSubcommands = map[string]string{
	// Issues no SQL at all: runCost receives neither a store nor a *sql.DB.
	// It models cost from flags and prints.
	"cost": "issues no SQL: runCost takes neither a store nor a db",

	// All three, through engine.QueueStore -- and, since cleat#1958, through
	// tenantScopedDB, so MySQL's per-tenant database is the one reached.
	// TestQueueCommandWorksOnEveryDialect is the test this absence is supposed
	// to have behind it; before #1958 that test passed while the command was
	// broken on MySQL, because it read its own write back from the same wrong
	// database. It now verifies against the tenant-scoped one.
	"queue": "ported to all three; TestQueueCommandWorksOnEveryDialect, which since cleat#1958 verifies against the tenant-scoped database",

	// All three. tenant_secrets is written here on the --db connection and
	// READ by cleat-worker on the base connection it opens at startup --
	// cmd/cleat-worker/main.go builds engine.NewSecretStore(db, ...) and hands
	// that same store to the runtime -- so the write and the read agree on
	// MySQL too. Recorded because the first pass of cleat#1956 called this
	// per-tenant from migration 069's header and was wrong: which database a
	// table is authoritative in is a property of the READER, not of the
	// migration that created it.
	"set-secret": "ported to all three; cleat-worker reads tenant_secrets from the same base connection cleatctl writes it on, measured in cleat#1956",

	// All three, same table as set-secret, same reasoning: tenant_secrets is
	// plain and per-tenant everywhere, not admin.*-schema-gated like
	// tenant_api_keys (which is why revoke-api-key is postgres-only in
	// portedOn instead). retireSecretStmt/secretMetaStmt in
	// engine/tenant_secrets.go carry all three dialect arms.
	"retire-secret": "ported to all three; tenant_secrets is a plain per-tenant table on every dialect, same as set-secret (cleat#1989)",
}

// Every subcommand main.go dispatches declares the dialects it runs on.
//
// THE GUARD THIS REPLACES COULD NOT SEE AN ABSENCE, which is the only kind of
// defect it needed to catch. TestUnportedCommandsRefuseBeforeTheyMutateAnything
// opens by saying "no subcommand is silently in between", and then iterates
// five names written into the test itself:
//
//	postIncident := []string{"replay", "debug", "check-db"}
//	{"drop-tenant", ...}, {"revoke-api-key", ...}
//
// A subcommand added to the dispatch switch and to neither map is invisible to
// it by construction -- there is no list it fails to appear on. So the check
// was strongest exactly where nothing could go wrong (the commands someone had
// already thought about) and silent where it did (cleat#1956).
//
// This reads the switch instead. A new subcommand fails this test on the commit
// that adds it, and the only way to pass is to say which dialects it runs on
// and why.
//
// IT FAILS IN BOTH DIRECTIONS, for the reason the pinned list in
// TestEveryInlineStatementParsesOnPostgres gives: a declaration for a command
// that no longer exists stops describing anything, and a stale entry that is
// merely ignored lands on the permissive side.
func TestEverySubcommandDeclaresItsDialects(t *testing.T) {
	dispatched := dispatchedSubcommands(t)

	// A floor, not an equality: the population grows, and this fires when the
	// EXTRACTION breaks -- the failure that would leave this test passing
	// while checking nothing, which is precisely the failure it exists to
	// replace.
	if len(dispatched) < 12 {
		t.Fatalf("found only %d subcommands in main.go's dispatch switch; there were 15 on "+
			"2026-09-22.\n\nThis test reads the switch with go/ast, so too few names means the "+
			"switch moved or changed shape and the check has quietly stopped covering it.",
			len(dispatched))
	}

	for _, cmd := range dispatched {
		_, ported := portedOn[cmd]
		reason, unrestricted := unrestrictedSubcommands[cmd]
		switch {
		case ported && unrestricted:
			t.Errorf("%s is in BOTH portedOn and unrestrictedSubcommands. Those mean opposite "+
				"things -- a restricted list of dialects, and none -- so one of them is wrong "+
				"and requirePortedFor will honour portedOn while the other reads as documentation",
				cmd)
		case !ported && !unrestricted:
			t.Errorf("%s is dispatched by main.go and declared in neither portedOn nor "+
				"unrestrictedSubcommands.\n\n"+
				"Absence from portedOn is not neutral: it means this command runs on every "+
				"dialect, which is a claim about SQL nobody has checked. That default is what "+
				"cleat#1956 was -- `queue` ran on MySQL and wrote to the wrong physical "+
				"database.\n\n"+
				"Add it to portedOn with the dialects it is written for, or to "+
				"unrestrictedSubcommands with the reason it genuinely runs on all three.",
				cmd)
		case unrestricted && strings.TrimSpace(reason) == "":
			t.Errorf("%s is in unrestrictedSubcommands with an empty reason. The reason is the "+
				"part a reviewer checks; without it the entry only records that someone wanted "+
				"the test to pass", cmd)
		}
	}

	// The other direction. Both maps are keyed by subcommand, so an entry
	// naming something main.go does not dispatch is describing a command that
	// no longer exists -- and it would go on reading as coverage.
	inDispatch := make(map[string]bool, len(dispatched))
	for _, c := range dispatched {
		inDispatch[c] = true
	}
	for cmd := range portedOn {
		if !inDispatch[cmd] {
			t.Errorf("portedOn declares %q, which main.go does not dispatch. If the subcommand "+
				"was removed, remove its entry in the same commit", cmd)
		}
	}
	for cmd := range unrestrictedSubcommands {
		if !inDispatch[cmd] {
			t.Errorf("unrestrictedSubcommands declares %q, which main.go does not dispatch. "+
				"If the subcommand was removed, remove its entry in the same commit", cmd)
		}
	}
}

// dispatchedSubcommands returns the case values of main.go's `switch cmd`.
//
// Read from the source rather than from a list kept here, because a list kept
// here is the thing that was wrong: it cannot go stale against the switch if it
// IS the switch.
//
// Matched on the switch's tag being the identifier `cmd` rather than on
// position, so that reordering or adding another switch to main.go does not
// silently select the wrong one -- and if the dispatch is ever renamed, the
// floor assertion above fires rather than this returning an empty set that
// passes everything.
func dispatchedSubcommands(t *testing.T) []string {
	t.Helper()

	root := cleatctlRepoRoot(t)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, "cmd", "cleatctl", "main.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse cmd/cleatctl/main.go: %v", err)
	}

	var cmds []string
	ast.Inspect(f, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		tag, ok := sw.Tag.(*ast.Ident)
		if !ok || tag.Name != "cmd" {
			return true
		}
		for _, stmt := range sw.Body.List {
			clause, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue // the default arm has no values
			}
			for _, expr := range clause.List {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				name, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				cmds = append(cmds, name)
			}
		}
		return true
	})

	sort.Strings(cmds)
	return cmds
}
