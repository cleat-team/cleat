package main

import (
	"os"
	"regexp"
	"testing"
)

// cleat#2377: main() built authResolver twice, from identical arguments, the
// second shadowing the first inside `if *requireAuth {`. It compiled cleanly
// -- a shadowing redeclaration in a nested block scope, not a `:=` "no new
// variables" error -- and no behavioural test could have caught it: both
// builds return an equal *auth.TenantStore (auth.NewTenantStoreForDialect has
// no state and no side effects), so a duplicate build is indistinguishable
// from a single one to anything that only observes what authResolver does.
//
// This asserts on the SOURCE instead, the same move
// TestOnlyRegisterRoutesRegistersRoutes (route_table_test.go) makes for a
// different one-table property: comments stripped first (stripGoComments,
// route_table_test.go), because a future comment mentioning the exact
// declaration shape in prose must not trip this.
//
// The pattern is anchored on the DECLARATION -- `authResolver, ... :=` --
// not on a bare call count of auth.NewTenantStoreForDialect(. That function
// has other legitimate callers in this same file: three `store, tsErr :=`
// bindings for CLI subcommands and one `ts, tsErr :=` binding for the
// auto-generated-startup-key path (all building their own store for a
// narrower purpose). A count of the bare call would need to track that
// number as it changes for unrelated reasons; the declaration form does not,
// because "authResolver" is only ever the name of the one resolver
// pluginEnv, the host-binding check and the auth middleware all share.
//
// The second bound variable is `\w+`, not the literal `arErr`: an earlier
// version of this pattern keyed on `arErr` specifically, which a duplicate
// spelled `authResolver, err := ...` -- a different but equally valid error
// name -- would pass straight through (cleat-review, reviewing #2694).
var authResolverDeclaration = regexp.MustCompile(`\bauthResolver\s*(?:,\s*\w+\s*)?:=`)

func TestAuthResolverIsBuiltExactlyOnce(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := stripGoComments(string(src))
	matches := authResolverDeclaration.FindAllStringIndex(body, -1)
	if len(matches) != 1 {
		t.Fatalf("found %d declarations of authResolver via auth.NewTenantStoreForDialect, want 1 (cleat#2377): "+
			"a second one is a shadowing duplicate in a nested block, not a compile error, and every consumer "+
			"downstream of whichever one is innermost silently stops being the one pluginEnv was given",
			len(matches))
	}
}

// TestTheGuardCatchesADuplicateWithADifferentErrorVariableName is the
// negative control for the widening above: M2 in cleat-review's mutation
// survey found that the `arErr`-only pattern missed a duplicate spelled
// `authResolver, err := ...` -- a different but equally valid error-variable
// name that a fresh author writing a second build would plausibly choose. A
// test that only exercises the exact historical duplicate (arErr) cannot
// tell a name-specific pattern from a general one; this exercises the
// deliberately different spelling instead.
func TestTheGuardCatchesADuplicateWithADifferentErrorVariableName(t *testing.T) {
	src := "authResolver, arErr := auth.NewTenantStoreForDialect(db, *driver)\n" +
		"authResolver, err := auth.NewTenantStoreForDialect(db, *driver)\n"
	body := stripGoComments(src)
	matches := authResolverDeclaration.FindAllStringIndex(body, -1)
	if len(matches) != 2 {
		t.Fatalf("found %d declarations, want 2: a duplicate spelled with a different "+
			"error-variable name must still be caught, not just the historical `arErr` spelling",
			len(matches))
	}
}

// TestTheAuthResolverScannerIgnoresComments keeps the comment-stripping above
// honest rather than decorative -- the same check route_table_test.go makes
// for muxRegistration, and for the same reason: a scanner that reads a
// sentence ABOUT the declaration as the declaration itself would pass a tree
// that reintroduced the duplicate for real, so long as the duplicate sat
// beside a comment repeating the pattern.
func TestTheAuthResolverScannerIgnoresComments(t *testing.T) {
	src := "// authResolver, arErr := auth.NewTenantStoreForDialect(db, *driver)\n" +
		"authResolver, arErr := auth.NewTenantStoreForDialect(db, *driver)\n"
	body := stripGoComments(src)
	matches := authResolverDeclaration.FindAllStringIndex(body, -1)
	if len(matches) != 1 {
		t.Fatalf("comment stripping is not filtering out commented-out declarations: found %d, want 1", len(matches))
	}
}
