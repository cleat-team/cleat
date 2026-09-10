package plugin_test

import (
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// Rebind must not rewrite anything inside a quoted region or a comment.
//
// cleat#1133. The previous implementation ran each regex over the whole
// statement, so it corrupted data that merely LOOKED like something it
// rewrites. That was latent -- no shipped plugin had a $N inside a string --
// and it stops being latent for two reasons at once: the rewrite moves to the
// adapter, where it reaches every plugin statement rather than the 166 that opt
// in, and TRUE/FALSE joins the substitution list, which is a token that turns up
// in ordinary English prose stored in ordinary columns.
func TestRebindLeavesQuotedRegionsAlone(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		mssql string
	}{
		{
			// The case that motivated the rewrite.
			name:  "$N inside a string literal",
			in:    `SELECT * FROM t WHERE label = 'costs $100' AND id = $1`,
			mssql: `SELECT * FROM t WHERE label = 'costs $100' AND id = @p1`,
		},
		{
			name:  "the word true inside a string literal",
			in:    `UPDATE t SET note = 'set this to true' WHERE enabled = true`,
			mssql: `UPDATE t SET note = 'set this to true' WHERE enabled = 1`,
		},
		{
			name:  "now() inside a string literal",
			in:    `INSERT INTO t (msg, at) VALUES ('run now() again', now())`,
			mssql: `INSERT INTO t (msg, at) VALUES ('run now() again', SYSUTCDATETIME())`,
		},
		{
			name:  "doubled quote escapes and does not end the literal",
			in:    `SELECT * FROM t WHERE s = 'it''s $5 and true' AND id = $1`,
			mssql: `SELECT * FROM t WHERE s = 'it''s $5 and true' AND id = @p1`,
		},
		{
			name:  "bracket-quoted identifier (T-SQL)",
			in:    `SELECT [true] FROM t WHERE id = $1`,
			mssql: `SELECT [true] FROM t WHERE id = @p1`,
		},
		{
			name:  "double-quoted identifier",
			in:    `SELECT "true" FROM t WHERE id = $1`,
			mssql: `SELECT "true" FROM t WHERE id = @p1`,
		},
		{
			name:  "line comment",
			in:    "SELECT a FROM t -- was $9 and true\nWHERE id = $1",
			mssql: "SELECT a FROM t -- was $9 and true\nWHERE id = @p1",
		},
		{
			name:  "block comment",
			in:    `SELECT a /* $9 and true and now() */ FROM t WHERE id = $1`,
			mssql: `SELECT a /* $9 and true and now() */ FROM t WHERE id = @p1`,
		},
		{
			// Both shapes the plugin corpus actually uses, measured 2026-09-10:
			// 28 of `= true`, 10 of a bare literal in a VALUES list. A regex
			// keyed on `=\s*true` sees the first and is blind to the second.
			name:  "bare boolean in a VALUES list",
			in:    `INSERT INTO t (a, b, c) VALUES ($1, true, false)`,
			mssql: `INSERT INTO t (a, b, c) VALUES (@p1, 1, 0)`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := plugin.Rebind(c.in, plugin.DialectMSSQL); got != c.mssql {
				t.Errorf("mssql\n  in:   %s\n  got:  %s\n  want: %s", c.in, got, c.mssql)
			}
			// PostgreSQL is the source dialect: Rebind is the identity.
			if got := plugin.Rebind(c.in, plugin.DialectPostgres); got != c.in {
				t.Errorf("postgres must be identity\n  in:  %s\n  got: %s", c.in, got)
			}
		})
	}
}

// A control for the test above: it must be able to tell a working scanner from
// a broken one. Without this, every assertion is satisfied by a Rebind that
// rewrites nothing at all.
//
// This repository's recurring defect is a check that could not have disagreed,
// so the positive direction is asserted explicitly rather than inferred from
// the cases above passing.
func TestRebindStillRewritesOutsideQuotedRegions(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`SELECT * FROM t WHERE id = $1 AND b = $22`, `SELECT * FROM t WHERE id = @p1 AND b = @p22`},
		{`SELECT * FROM t WHERE enabled = true`, `SELECT * FROM t WHERE enabled = 1`},
		{`SELECT * FROM t WHERE enabled = FALSE`, `SELECT * FROM t WHERE enabled = 0`},
		{`UPDATE t SET at = now()`, `UPDATE t SET at = SYSUTCDATETIME()`},
	} {
		if got := plugin.Rebind(c.in, plugin.DialectMSSQL); got != c.want {
			t.Errorf("in:   %s\n got:  %s\n want: %s", c.in, got, c.want)
		}
	}
	if got := plugin.Rebind(`SELECT * FROM t WHERE id = $1`, plugin.DialectMySQL); got != `SELECT * FROM t WHERE id = ?` {
		t.Errorf("mysql placeholder: got %s", got)
	}
	// MySQL documents TRUE/FALSE as aliases for 1/0, so it needs no boolean
	// rewrite -- and must not get one, since rewriting is not free of risk.
	if got := plugin.Rebind(`SELECT * FROM t WHERE e = true`, plugin.DialectMySQL); !strings.Contains(got, "true") {
		t.Errorf("mysql must leave TRUE alone: got %s", got)
	}
}

// The adapter applies Rebind to statements whose call site may already have
// applied it, so a second pass must be a no-op. This is asserted rather than
// argued because the whole central-rewrite design rests on it.
func TestRebindIsIdempotent(t *testing.T) {
	cases := []string{
		`SELECT a FROM t WHERE id = $1 AND b = $2 AND c <= now()`,
		`UPDATE t SET x = now(), y = true WHERE id = $10`,
		`SELECT * FROM t WHERE name = 'costs $100 and true' AND id = $1`,
		`INSERT INTO t (a, b) VALUES ($1, false)`,
		`SELECT * FROM t WHERE id = @p1 AND e = 1`,
		`INSERT INTO t (a) VALUES (?)`,
		"SELECT a -- $1 true\nFROM t WHERE id = $1",
	}
	for _, d := range []plugin.Dialect{plugin.DialectPostgres, plugin.DialectMySQL, plugin.DialectMSSQL} {
		for _, q := range cases {
			one := plugin.Rebind(q, d)
			if two := plugin.Rebind(one, d); one != two {
				t.Errorf("dialect %v not idempotent\n  in:    %s\n  once:  %s\n  twice: %s", d, q, one, two)
			}
		}
	}
}
