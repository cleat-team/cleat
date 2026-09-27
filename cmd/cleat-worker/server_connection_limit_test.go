package main

import (
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// The usable ceiling is not max_connections. cleat#1487.
//
// A stock PostgreSQL 16 container advertises 100 and lets an ordinary role open
// 97: superuser_reserved_connections takes 3, and PostgreSQL 16 added a separate
// reserved_connections on top. Measured against a running container, not read
// from documentation.
func TestUsableIsNotMaxConnections(t *testing.T) {
	stock := serverConnectionLimit{Max: 100, Reserved: 3}
	if got := stock.Usable(); got != 97 {
		t.Errorf("Usable() = %d, want 97.\n\n"+
			"Reporting the advertised 100 tells a worker needing exactly 100 that it fits, "+
			"which is a diagnosis wrong by the reserved slots -- the one number the operator "+
			"cannot see from the outside.", got)
	}
	// Never negative: a misconfigured server with more reserved than max should
	// report "none available", not a negative capacity that arithmetic below
	// would treat as enormous.
	if got := (serverConnectionLimit{Max: 2, Reserved: 5}).Usable(); got != 0 {
		t.Errorf("Usable() with more reserved than max = %d, want 0", got)
	}
}

// The three verdicts, at their boundaries.
func TestAssessConnectionFit(t *testing.T) {
	// A stock PostgreSQL: 100 advertised, 97 usable.
	pg := serverConnectionLimit{Max: 100, Reserved: 3, Detail: "max_connections=100 superuser_reserved=3 reserved=0"}

	for _, tc := range []struct {
		name     string
		fixed    int
		severity string
		mentions string
	}{
		// A default worker needs 75 of 97. It fits; two would need 150.
		{"default worker, second will not fit", 75, "warn", "a SECOND worker will not fit"},
		// Exactly half is the boundary: two of these fit precisely.
		{"two fit exactly", 48, "", ""},
		{"one over half", 49, "warn", "SECOND"},
		// A worker that cannot reach full load on its own is the serious case.
		{"does not fit alone", 120, "error", "cannot reach full load"},
		// THE BOUNDARY, asserted rather than assumed: a worker needing exactly
		// the usable total FITS (97 is not more than 97) and excludes a second.
		// So this is the warning, not the error -- and getting it backwards
		// would refuse to start the only worker a single-node deployment has.
		{"fills the server exactly", 97, "warn", "a SECOND worker will not fit"},
	} {
		sev, msg := assessConnectionFit(tc.fixed, pg)
		if sev != tc.severity {
			t.Errorf("%s: severity = %q, want %q\n  msg: %s", tc.name, sev, tc.severity, msg)
		}
		if tc.mentions != "" && !strings.Contains(msg, tc.mentions) {
			t.Errorf("%s: message does not mention %q:\n  %s", tc.name, tc.mentions, msg)
		}
	}
}

// The message must carry the arithmetic and the remedies, not a verdict.
//
// "Connection limit exceeded" sends an operator to guess. The numbers and the
// setting names let them check the server themselves.
func TestTheWarningCarriesItsArithmetic(t *testing.T) {
	pg := serverConnectionLimit{Max: 100, Reserved: 3, Detail: "max_connections=100 superuser_reserved=3 reserved=0"}
	_, msg := assessConnectionFit(75, pg)
	for _, want := range []string{"75", "97", "max_connections=100", "superuser_reserved=3", "pooler"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the warning omits %q:\n  %s", want, msg)
		}
	}
	// AND IT NAMES THE LIMIT OF THE REMEDY. A pooler consolidates the fixed
	// pools and does nothing for per-tenant pools, which connect as distinct
	// roles -- so "add PgBouncer" is a half-answer, and saying so here is
	// cheaper than the reader discovering it in production.
	if !strings.Contains(msg, "tenant-isolation=role") {
		t.Errorf("the warning recommends a pooler without noting it does not consolidate "+
			"per-tenant pools:\n  %s", msg)
	}
}

// A dialect where the question does not arise says so, rather than being silent.
//
// Silence reads as "checked and fine", which is the failure this whole issue is
// about: a limit nothing looks at.
func TestAnUncheckedDialectSaysSo(t *testing.T) {
	_, ok, reason := queryServerConnectionLimit(t.Context(), nil, "sqlserver")
	if ok {
		t.Error("SQL Server reported a connection limit; its ceiling is 32767 and no cleat " +
			"deployment approaches it, so there is nothing to check")
	}
	if reason == "" {
		t.Error("SQL Server returned no reason. A silent skip is indistinguishable from a " +
			"check that passed.")
	}
}

// The query works against a real server, and reports the reserved slots.
//
// The arithmetic above is pure and could be right while the SQL is wrong -- a
// misspelled setting name, a missing row, a type that will not scan. This asks
// a live PostgreSQL, which is the only thing that can tell those apart.
func TestTheServerLimitIsReadFromARealServer(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	t.Cleanup(func() { db.Close() })

	limit, ok, reason := queryServerConnectionLimit(t.Context(), db, "postgres")
	if !ok {
		t.Fatalf("could not read the limit from a live server: %s", reason)
	}
	if limit.Max <= 0 {
		t.Errorf("max_connections read as %d; a server always has one", limit.Max)
	}
	// RESERVED MUST BE READ, NOT ASSUMED ZERO. A stock container reserves 3 for
	// superusers, and reporting the advertised maximum would tell a worker
	// needing exactly that much that it fits.
	if limit.Reserved <= 0 {
		t.Errorf("reserved slots read as %d. PostgreSQL reserves "+
			"superuser_reserved_connections (3 by default) before admitting an ordinary "+
			"role, so a zero here means the setting was not read and Usable() is "+
			"overstated by exactly the number an operator cannot see.", limit.Reserved)
	}
	if limit.Usable() != limit.Max-limit.Reserved {
		t.Errorf("Usable() = %d, want %d", limit.Usable(), limit.Max-limit.Reserved)
	}
	// The detail string is what a reader checks the server against, so it has
	// to name the settings rather than just restate the numbers.
	for _, want := range []string{"max_connections=", "superuser_reserved=", "reserved="} {
		if !strings.Contains(limit.Detail, want) {
			t.Errorf("Detail omits %q: %s", want, limit.Detail)
		}
	}
	t.Logf("live server: %s -> usable %d", limit.Detail, limit.Usable())
}
