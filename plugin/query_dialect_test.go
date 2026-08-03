package plugin

import "testing"

func TestQuoteIdent(t *testing.T) {
	cases := []struct {
		dialect Dialect
		name    string
		want    string
	}{
		{DialectPostgres, "key", `"key"`},
		{DialectMySQL, "key", "`key`"},
		{DialectMSSQL, "key", "[key]"},
		// Unknown dialect falls back to standard SQL rather than to nothing:
		// emitting a bare identifier is what broke MySQL and SQL Server in the
		// first place.
		{Dialect(""), "key", `"key"`},
		// Embedded quote characters are escaped, not passed through.
		{DialectMySQL, "we`ird", "`we``ird`"},
		{DialectMSSQL, "we]ird", "[we]]ird]"},
		{DialectPostgres, `we"ird`, `"we""ird"`},
	}
	for _, c := range cases {
		if got := QuoteIdent(c.name, c.dialect); got != c.want {
			t.Errorf("QuoteIdent(%q, %q) = %q, want %q", c.name, c.dialect, got, c.want)
		}
	}
}

func TestLimitClause(t *testing.T) {
	cases := []struct {
		dialect Dialect
		want    string
	}{
		{DialectPostgres, "LIMIT $1"},
		{DialectMySQL, "LIMIT $1"},
		// SQL Server has no LIMIT.
		{DialectMSSQL, "OFFSET 0 ROWS FETCH NEXT $1 ROWS ONLY"},
		{Dialect(""), "LIMIT $1"},
	}
	for _, c := range cases {
		if got := LimitClause("$1", c.dialect); got != c.want {
			t.Errorf("LimitClause($1, %q) = %q, want %q", c.dialect, got, c.want)
		}
	}
}

// TestRebind_UnknownDialectPassesThrough documents the behaviour that hid a
// whole class of failures: Rebind leaves a query untouched for a dialect it
// does not recognise, so a caller that forgets to set its dialect sends
// PostgreSQL placeholders to MySQL and SQL Server and gets "Unknown column
// '$1'". That is intended (PostgreSQL is the default form), but it means the
// dialect field is load-bearing and must never be left at its zero value --
// see the multi-backend tests in plugins/kvstore and plugins/featureflags.
func TestRebind_UnknownDialectPassesThrough(t *testing.T) {
	const q = `SELECT 1 FROM t WHERE a = $1 AND b = $2`
	if got := Rebind(q, Dialect("")); got != q {
		t.Errorf("Rebind with zero dialect = %q, want it unchanged", got)
	}
	if got := Rebind(q, DialectMySQL); got != `SELECT 1 FROM t WHERE a = ? AND b = ?` {
		t.Errorf("MySQL rebind = %q", got)
	}
	if got := Rebind(q, DialectMSSQL); got != `SELECT 1 FROM t WHERE a = @p1 AND b = @p2` {
		t.Errorf("MSSQL rebind = %q", got)
	}
}
