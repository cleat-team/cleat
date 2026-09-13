package main

import (
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// TestTheDSNSelectsTheDriver is the acceptance criterion cleat#1316 asks for:
// "a test that fails when cleatctl is run against a MySQL or SQL Server DSN".
//
// Before this change every DSN reached sql.Open("postgres", ...), so a SQL
// Server DSN did not fail cleanly -- lib/pq attempted to parse it as a
// PostgreSQL connection string, and what an operator saw after a production
// incident was a parse error about their own DSN.
//
// The DSN forms below are the ones people paste, which is the population the
// heuristic is for. It is NOT a validating parser: a string it maps to postgres
// is not thereby proven to be valid PostgreSQL, which stays the driver's job.
func TestTheDSNSelectsTheDriver(t *testing.T) {
	for _, tc := range []struct {
		dsn        string
		wantName   string
		wantDriver string
	}{
		// The mismatch that makes driver a field rather than a derived string.
		{"sqlserver://sa:pw@127.0.0.1:1433?database=cleat", "mssql", "sqlserver"},
		{"mssql://sa:pw@host:1433?database=cleat", "mssql", "sqlserver"},
		{"jdbc:sqlserver://host:1433;databaseName=cleat", "mssql", "sqlserver"},

		// The Go MySQL driver's form has no scheme at all, so @tcp( is the
		// only thing to key on.
		{"root:cleat@tcp(127.0.0.1:3306)/cleat?parseTime=true", "mysql", "mysql"},
		{"mysql://root:cleat@127.0.0.1:3306/cleat", "mysql", "mysql"},

		// PostgreSQL is the default, including for anything unrecognised --
		// every DSN cleatctl accepted before this existed was a PostgreSQL one,
		// and a tool that starts rejecting input it used to take is a worse
		// outcome than one that guesses the way it always did.
		{"postgres://cleat:pw@localhost:5432/cleat?sslmode=disable", "postgres", "postgres"},
		{"postgresql://localhost/cleat", "postgres", "postgres"},
		{"host=localhost user=cleat dbname=cleat", "postgres", "postgres"},
		{"", "postgres", "postgres"},

		// Case and surrounding space are not a different database.
		{"  SQLSERVER://sa:pw@host:1433  ", "mssql", "sqlserver"},
	} {
		got := detectDialect(tc.dsn)
		if got.name != tc.wantName || got.driver != tc.wantDriver {
			t.Errorf("detectDialect(%q) = %s/%s, want %s/%s",
				tc.dsn, got.name, got.driver, tc.wantName, tc.wantDriver)
		}
	}
}

// TestTheDriverFlagOverridesTheDSN covers the escape hatch for a DSN shape the
// heuristic does not know. Without it, an unrecognised form is silently treated
// as PostgreSQL and there is no way to say otherwise.
func TestTheDriverFlagOverridesTheDSN(t *testing.T) {
	for _, tc := range []struct {
		flag    string
		want    string
		wantErr bool
	}{
		{"postgres", "postgres", false},
		{"postgresql", "postgres", false},
		{"mysql", "mysql", false},
		{"mssql", "mssql", false},
		{"sqlserver", "mssql", false},
		{"  MySQL  ", "mysql", false},
		{"oracle", "", true},
		{"", "", true},
	} {
		got, err := dialectByName(tc.flag)
		if tc.wantErr {
			if err == nil {
				t.Errorf("dialectByName(%q) = %v, want an error", tc.flag, got.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("dialectByName(%q): %v", tc.flag, err)
		} else if got.name != tc.want {
			t.Errorf("dialectByName(%q) = %s, want %s", tc.flag, got.name, tc.want)
		}
	}
}

// TestUnportedCommandsRefuseBeforeTheyMutateAnything is the half that matters
// more than the driver selection.
//
// Selecting a driver makes every subcommand CONNECT on all three dialects. It
// does not make them work: drop-tenant issues 17 statements against `admin.*`,
// and MySQL has no admin schema. A drop-tenant that connects and then fails on
// statement 9 of 17 has half-deleted a tenant, which is strictly worse than the
// refusal it replaced.
//
// So this asserts the POLICY, not a message: every subcommand is either ported
// for a dialect or refuses on it, and no subcommand is silently in between.
func TestUnportedCommandsRefuseBeforeTheyMutateAnything(t *testing.T) {
	postIncident := []string{"replay", "debug", "check-db"}
	for _, cmd := range postIncident {
		for _, d := range []dialect{dialectPostgres, dialectMySQL, dialectMSSQL} {
			if !isPortedFor(cmd, d) {
				t.Errorf("%s is not ported for %s, but it is one of the post-incident "+
					"tools cleat#1316 is about", cmd, d.name)
			}
		}
	}

	// The destructive ones must NOT claim dialects they have not been written
	// for. This is the assertion that goes red if someone widens portedOn
	// without porting the SQL underneath it.
	for _, cmd := range []string{"drop-tenant", "revoke-api-key"} {
		if isPortedFor(cmd, dialectMySQL) || isPortedFor(cmd, dialectMSSQL) {
			t.Errorf("%s claims a non-PostgreSQL dialect. Its SQL is unqualified `admin.*` "+
				"with $N placeholders; if it has genuinely been ported, this test should "+
				"be updated in the same commit as the port and not before", cmd)
		}
		if !isPortedFor(cmd, dialectPostgres) {
			t.Errorf("%s should still work on postgres", cmd)
		}
	}
}

// isPortedFor is requirePortedFor without the exit, so the policy can be
// asserted rather than the message.
func isPortedFor(cmd string, d dialect) bool {
	supported, restricted := portedOn[cmd]
	if !restricted {
		return true
	}
	for _, s := range supported {
		if s == d.name {
			return true
		}
	}
	return false
}

// TestEveryDialectArmBindsItsOwnPlaceholders checks the statements that carry
// explicit per-dialect arms actually differ where they must, and agree where
// they must.
//
// The failure this guards against is the quiet one: an arm added by copying the
// Default and editing nothing, which compiles, runs, and is wrong only on the
// dialect nobody tests locally.
func TestEveryDialectArmBindsItsOwnPlaceholders(t *testing.T) {
	t.Run("the instance load casts per dialect", func(t *testing.T) {
		pg := loadWorkflowInstanceSQL().For(plugin.DialectPostgres)
		my := loadWorkflowInstanceSQL().For(plugin.DialectMySQL)
		ms := loadWorkflowInstanceSQL().For(plugin.DialectMSSQL)

		// `::text` is PostgreSQL-only syntax. An arm still carrying it is an
		// arm that was copied and not edited.
		for name, q := range map[string]string{"mysql": my, "mssql": ms} {
			if strings.Contains(q, "::") {
				t.Errorf("the %s arm still carries a PostgreSQL :: cast", name)
			}
		}
		if !strings.Contains(pg, "result::text") {
			t.Error("the postgres arm lost its result::text cast; result is JSONB there")
		}
		if !strings.Contains(ms, "CONVERT(NVARCHAR(36), tenant_id)") {
			t.Error("the mssql arm must CONVERT tenant_id: it is UNIQUEIDENTIFIER there")
		}

		// All three select the same columns in the same order, because one
		// Scan reads all three.
		if got := map[string]int{"pg": strings.Count(pg, ","), "my": strings.Count(my, ","), "ms": strings.Count(ms, ",")}; got["pg"] != got["my"] {
			t.Errorf("postgres and mysql arms disagree on column count: %v", got)
		}
	})

	t.Run("the migration read limits rows per dialect", func(t *testing.T) {
		pg := latestMigrationSQL.For(plugin.DialectPostgres)
		ms := latestMigrationSQL.For(plugin.DialectMSSQL)
		if !strings.Contains(pg, "LIMIT 1") {
			t.Error("the postgres arm lost its LIMIT")
		}
		if strings.Contains(ms, "LIMIT") {
			t.Error("the mssql arm carries LIMIT, which SQL Server does not accept")
		}
		if !strings.Contains(ms, "TOP 1") {
			t.Error("the mssql arm must use TOP 1")
		}
		// MySQL takes LIMIT, so it deliberately has no arm and falls back to
		// Default. Asserted so that adding a redundant arm is a visible choice.
		if latestMigrationSQL.For(plugin.DialectMySQL) != pg {
			t.Error("mysql should fall back to the Default arm: it accepts LIMIT")
		}
	})
}

// TestRebindRewritesEveryPlaceholderInThisPackage.
//
// The statements here are written in the PostgreSQL $N form and rewritten at
// the call site. This asserts the rewrite reaches all three forms, so that a
// statement routed through d.rebind is genuinely portable rather than portable
// only where $N happens to be accepted.
func TestRebindRewritesEveryPlaceholderInThisPackage(t *testing.T) {
	const q = "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2"
	for _, tc := range []struct {
		d       dialect
		want    string
		absent  string
		comment string
	}{
		{dialectPostgres, "$1", "", "postgres keeps its own form"},
		{dialectMySQL, "?", "$1", "mysql takes positional ?"},
		{dialectMSSQL, "@p1", "$1", "sqlserver takes @pN"},
	} {
		got := tc.d.rebind(q)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: rebind produced %q, expected it to contain %q (%s)", tc.d.name, got, tc.want, tc.comment)
		}
		if tc.absent != "" && strings.Contains(got, tc.absent) {
			t.Errorf("%s: rebind left %q in %q", tc.d.name, tc.absent, got)
		}
	}
}
