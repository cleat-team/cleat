package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/migration"
)

// --migration-lock-timeout exists AND is passed to every migration runner.
// cleat#1775.
//
// # Why this test and not a flag-existence check
//
// cleat#1790 added migration.Runner.WithLockTimeout with a doc comment saying
// "an operator who has decided that should not have to fight the runner". There
// was no flag. The override existed only in the Go API, so the sentence
// described a capability no deployment could reach, while reading as though it
// shipped -- a mechanism wired to nothing, in the comment claiming it was wired.
//
// WS-2's check-test-only-exports guard found it within hours of the merge, by
// noticing WithLockTimeout had three test callers and no production one. That is
// the general detector; this is the specific one, and it asserts the half the
// general detector cannot: not merely that something calls WithLockTimeout, but
// that EVERY migration runner the worker constructs is given the flag.
//
// # Why both call sites, by construction
//
// There are two: the instance migrator and the per-tenant one. Passing the flag
// to one and not the other is the shape this repository keeps finding -- the
// fix applied at its call site rather than to its class (cleat#1769). So this
// counts NewRunner constructions and requires each to be followed by
// WithLockTimeout, rather than checking that the string appears somewhere.

var flagDeclRe = regexp.MustCompile(`flag\.Duration\("migration-lock-timeout",\s*(\S+),`)

// runnerSites returns, for each migration.NewRunner(...) construction in src,
// the set of methods chained onto it.
//
// AST, NOT REGEX, and the reason is worth keeping. Two regex attempts failed
// here: `[^)]*` stopped at the first nested paren, and one level of alternation
// still could not match, because the argument list contains
// migration.Dialect(factory.Dialect()) -- two levels deep. A regular expression
// cannot model balanced parentheses at all, so the third attempt would have
// failed too. Both failures reported UNMEASURED rather than "every site
// complies", which is the only reason this was not a guard that passed over an
// empty set.
func runnerSites(t *testing.T, src string) []map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", src, 0)
	if err != nil {
		t.Fatalf("UNMEASURED: parsing main.go: %v", err)
	}
	var out []map[string]bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewRunner" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "migration" {
			return true
		}
		// Walk back up: the chain hangs off this call as nested SelectorExprs.
		chain := map[string]bool{}
		ast.Inspect(f, func(m ast.Node) bool {
			c, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			s, ok := c.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// s.X is the receiver expression; if the NewRunner call is anywhere
			// inside it, this method is chained onto that construction.
			found := false
			ast.Inspect(s.X, func(k ast.Node) bool {
				if k == call {
					found = true
				}
				return !found
			})
			if found {
				chain[s.Sel.Name] = true
			}
			return true
		})
		out = append(out, chain)
		return true
	})
	return out
}

func readWorkerFile(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("UNMEASURED: locating the repository root: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "cmd", "cleat-worker", name))
	if err != nil {
		t.Fatalf("UNMEASURED: reading %s: %v", name, err)
	}
	return string(b)
}

func TestTheMigrationLockTimeoutFlagReachesEveryRunner(t *testing.T) {
	cfg := readWorkerFile(t, "config.go")

	m := flagDeclRe.FindStringSubmatch(cfg)
	if m == nil {
		t.Fatal("no --migration-lock-timeout flag in config.go. migration.Runner's " +
			"WithLockTimeout doc comment promises an operator can change this; without a " +
			"flag that promise is unreachable from a deployment. cleat#1775.")
	}
	// The default must be the package's, not a second literal that can drift
	// from it -- the same fault the plugin/core lock_timeout pair already needed
	// a guard for.
	if def := m[1]; def != "migration.DefaultLockTimeout" {
		t.Errorf("the flag defaults to %s rather than migration.DefaultLockTimeout. "+
			"Two literals for one bound is how they drift apart.", def)
	}

	sites := runnerSites(t, readWorkerFile(t, "main.go"))
	// If the scan finds nothing, "every site passes the flag" is vacuous.
	if len(sites) == 0 {
		t.Fatal("UNMEASURED: found no migration.NewRunner construction in main.go, so " +
			"'every runner gets the flag' would be true of nothing. This is a failure of " +
			"the check, not a finding about the tree.")
	}
	for i, chain := range sites {
		if !chain["WithLockTimeout"] {
			names := make([]string, 0, len(chain))
			for k := range chain {
				names = append(names, k)
			}
			sort.Strings(names)
			t.Errorf("migration.NewRunner construction %d in main.go is chained to %v but not "+
				"WithLockTimeout. Every runner the worker builds must take the flag, or the "+
				"bound is applied at one call site and not to the class. cleat#1769, cleat#1775.",
				i+1, names)
		}
	}
	t.Logf("%d migration.NewRunner sites, all passing the flag; default is %s (%v)",
		len(sites), m[1], migration.DefaultLockTimeout)
}
