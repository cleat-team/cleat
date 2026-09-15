package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestAnRLSWarningFitsItsDialect is cleat#1646.
//
// warnIfTenantScoped ran unconditionally and went through
// engine.CheckRLSEnforced, which reads pg_roles. On the other two dialects
// every cleatctl invocation therefore opened with:
//
//	warning: could not determine whether this connection is subject to
//	row-level security: check RLS: read role attributes:
//	mssql: Invalid object name 'pg_roles'.
//
// Two defects, and the second is the one that matters. The noise is a warning
// an operator learns to scroll past. The advice, had the check run, would have
// been WRONG on SQL Server: it names "a superuser or BYPASSRLS", and neither
// exists there -- a security policy applies to sysadmin, db_owner and dbo
// alike. Measured on a migrated database: sa reads
// IS_ROLEMEMBER('cleat_admin') = 0 while IS_SRVROLEMEMBER('sysadmin') = 1, so
// an operator following that advice connects as the most privileged principal
// on the server and changes nothing.
func TestAnRLSWarningFitsItsDialect(t *testing.T) {
	origFn := rlsPostureFn
	t.Cleanup(func() { rlsPostureFn = origFn })

	t.Run("mysql asks nothing and says nothing", func(t *testing.T) {
		called := false
		rlsPostureFn = func(ctx context.Context, db *sql.DB, d string) (rlsPosture, []engine.RLSBypassReason, error) {
			called = true
			return rlsPostureOf(ctx, db, d)
		}
		// A nil *sql.DB is safe here BECAUSE the mysql arm must not touch it.
		// If that ever changes this panics rather than passing quietly, which
		// is the assertion: MySQL has no row-level security, so there is
		// nothing to interrogate.
		out := captureStderr(t, func() { warnIfTenantScoped(context.Background(), nil, "mysql") })
		if !called {
			t.Fatal("the posture function was not reached; this test asserted nothing")
		}
		if out != "" {
			t.Errorf("mysql produced output, want silence:\n%s", out)
		}
	})

	t.Run("mssql subject gets the mssql remedy, not the postgres one", func(t *testing.T) {
		rlsPostureFn = stubPosture(rlsSubject, nil)
		out := captureStderr(t, func() { warnIfTenantScoped(context.Background(), nil, "mssql") })

		if !strings.Contains(out, "cleat_admin") {
			t.Errorf("the SQL Server warning does not name the remedy that exists:\n%s", out)
		}
		// The load-bearing half. Naming BYPASSRLS here sends an operator after
		// a privilege SQL Server does not have.
		if strings.Contains(out, "BYPASSRLS") || strings.Contains(out, "superuser that") {
			t.Errorf("the SQL Server warning offers a PostgreSQL remedy:\n%s", out)
		}
		if !strings.Contains(out, "no superuser exemption") {
			t.Errorf("the warning does not say the superuser route is absent, which is the "+
				"thing an operator will otherwise try first:\n%s", out)
		}
	})

	t.Run("postgres is unchanged", func(t *testing.T) {
		rlsPostureFn = stubPosture(rlsSubject, nil)
		out := captureStderr(t, func() { warnIfTenantScoped(context.Background(), nil, "postgres") })
		if !strings.Contains(out, "BYPASSRLS") {
			t.Errorf("the PostgreSQL warning lost its remedy:\n%s", out)
		}
		if strings.Contains(out, "cleat_admin") {
			t.Errorf("the PostgreSQL warning gained a SQL Server remedy:\n%s", out)
		}
	})

	t.Run("an exempt connection says nothing on either dialect", func(t *testing.T) {
		// The control. Without it "warns on mssql" is satisfied by a function
		// that warns unconditionally, which is the defect this replaces.
		for _, d := range []string{"postgres", "mssql"} {
			rlsPostureFn = stubPosture(rlsExempt, nil)
			out := captureStderr(t, func() { warnIfTenantScoped(context.Background(), nil, d) })
			if out != "" {
				t.Errorf("%s: an exempt connection was warned:\n%s", d, out)
			}
		}
	})
}
