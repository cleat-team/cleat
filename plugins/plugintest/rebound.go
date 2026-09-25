package plugintest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// ExecRebound and QueryRowRebound run a statement written in the portable
// $N form against a raw *sql.DB/*sql.Tx/*sql.Conn, translating it and its
// args for dialect first.
//
// WHY THESE EXIST (cleat#2259). plugin.Rebind is the identity for MySQL: the
// $N -> ? rewrite happens only inside plugin.RebindArgs, alongside the arg
// reorder MySQL's positional ? binding needs. A fixture that calls Rebind
// and hands the (unrewritten, for MySQL) result straight to a raw driver
// method sends literal "$1" text to the server -- a syntax error on every
// MySQL run, not a silent mis-bind. These wrap RebindArgs so a multi-backend
// test fixture gets the one-liner Rebind used to be, correctly, on all three
// dialects.
//
// WHY THIS PACKAGE, NOT engine/testutil. That is the natural home and it
// cannot go there, for the reason the package doc above already gives for
// RunEveryArm: engine's and plugin's own tests import engine/testutil, so
// engine/testutil importing plugin (for RebindArgs) is an import cycle in
// the test binary -- go build does not notice, go vet does. plugins/* are
// leaves, so a helper here can import plugin freely.
//
// PREFER ROUTING A FIXTURE THROUGH engine.SQLDBAdapter (or its transaction
// counterpart) OVER THESE, where the fixture does not need to keep a raw
// handle: that exercises the same translation path production traffic does,
// which these do not. These exist for the tests that must keep a raw
// handle -- typically to hold a lock or a transaction open across a
// goroutine boundary, or to read cross-tenant, which SQLDBAdapter's own
// tenant-scoped transaction would not permit.
func ExecRebound(t *testing.T, ctx context.Context, x interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}, dialect plugin.Dialect, query string, args ...any) (sql.Result, error) {
	t.Helper()
	stmt, reboundArgs := mustRebindArgs(t, dialect, query, args)
	return x.ExecContext(ctx, stmt, reboundArgs...)
}

// QueryRowRebound is ExecRebound's counterpart for a single-row read.
//
// No multi-row QueryRebound: none of this fix's 19 raw-handle call sites
// needed one (every multi-row read among them already went through
// plugin.PluginDB), and an unused exported wrapper is exactly the dead
// code scripts/check-dead-exports.sh exists to catch. Add one, following
// this same pattern, the day a fixture actually needs it.
func QueryRowRebound(t *testing.T, ctx context.Context, x interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, dialect plugin.Dialect, query string, args ...any) *sql.Row {
	t.Helper()
	stmt, reboundArgs := mustRebindArgs(t, dialect, query, args)
	return x.QueryRowContext(ctx, stmt, reboundArgs...)
}

func mustRebindArgs(t *testing.T, dialect plugin.Dialect, query string, args []any) (string, []any) {
	t.Helper()
	stmt, reboundArgs, err := plugin.RebindArgs(query, dialect, args)
	if err != nil {
		t.Fatalf("RebindArgs(%q, %s): %v", query, dialect, err)
	}
	return stmt, reboundArgs
}
