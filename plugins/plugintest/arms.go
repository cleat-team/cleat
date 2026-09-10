// Package plugintest runs a plugin's dialect-specific SQL against real servers.
//
// cleat#1133. A plugin.Query names one statement per dialect, and nothing
// checked that any arm was valid for the dialect it names. Two rounds of fixing
// the jobqueue reaper (cleat#1134, cleat#1141) landed statements that no
// database would accept, because the only tests driving those paths used a fake
// driver that pattern-matches the query string -- which accepts anything.
//
// A lexical guard cannot close this. `enabled = true` is a BINDING error in
// T-SQL, not a syntax error, so `SET PARSEONLY ON` reports it clean; and the
// error a live server does return can name the wrong construct entirely (see
// RunEveryArm's doc). The only check that cannot be fooled is executing the
// statement on a real server of that dialect, against the schema the plugin's
// own migrations build.
//
// WHY THIS IS A SEPARATE PACKAGE. The natural home is engine/testutil, and it
// cannot go there: engine's and plugin's own tests import testutil, so testutil
// importing either is an import cycle in the test binary. `go build` does not
// notice; `go vet` does. plugins/* are leaves, so a helper here can import
// everything it needs.
package plugintest

import (
	"context"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// Arm is one named query to exercise, with arguments that bind on every
// dialect. The values do not matter -- the question is whether the STATEMENT is
// accepted, not what it returns.
type Arm struct {
	Name string
	Q    plugin.Query
	Args []any

	// WantCols, when non-zero, asserts the column count the caller scans. A
	// valid statement returning the wrong shape fails later, at Scan, and for a
	// background loop that means it fails silently.
	WantCols int

	// Exec runs the statement with Exec rather than Query. Set it for INSERT,
	// UPDATE and DELETE, where Query would succeed but return no rows and
	// prove less.
	Exec bool
}

// RunEveryArm runs each Arm against every configured backend, using the arm for
// that backend's dialect.
//
// THE ERROR A SERVER RETURNS MAY NAME THE WRONG CONSTRUCT, so read the whole
// message rather than the last clause. Measured on SQL Server 2022:
//
//	WHERE NOT processed                      -> Msg 4145, non-boolean type
//	WHERE NOT processed + OFFSET/FETCH       -> Msg 4145 AND Msg 153
//	WHERE processed = 0  + OFFSET/FETCH      -> succeeds
//
// SQL Server reports both errors and the Go driver surfaces only the last, so a
// statement whose fault is a boolean reports as a row-limit problem -- while
// the row-limit clause is correct T-SQL. This is why the failure message below
// prints the statement in full.
func RunEveryArm(t *testing.T, p plugin.Plugin, arms []Arm) {
	t.Helper()
	if len(arms) == 0 {
		t.Fatal("RunEveryArm called with no arms; this asserts nothing")
	}
	for _, be := range testutil.NewPluginTestBackends(t) {
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			ctx := context.Background()
			dialect := plugin.Dialect(be.Dialect)

			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("%s migrations on %s: %v", p.Info().Name, be.Name, err)
			}

			for _, a := range arms {
				t.Run(a.Name, func(t *testing.T) {
					stmt := plugin.Rebind(a.Q.For(dialect), dialect)
					if a.Exec {
						if _, err := be.DB.ExecContext(ctx, stmt, a.Args...); err != nil {
							t.Errorf("%s was rejected by a real %s server:\n  %v\n  %s",
								a.Name, be.Name, err, stmt)
						}
						return
					}
					rows, err := be.DB.QueryContext(ctx, stmt, a.Args...)
					if err != nil {
						t.Errorf("%s was rejected by a real %s server:\n  %v\n  %s",
							a.Name, be.Name, err, stmt)
						return
					}
					defer rows.Close()
					if a.WantCols > 0 {
						cols, err := rows.Columns()
						if err != nil {
							t.Fatalf("columns on %s: %v", be.Name, err)
						}
						if len(cols) != a.WantCols {
							t.Errorf("%s on %s returns %d columns, caller scans %d: %v",
								a.Name, be.Name, len(cols), a.WantCols, cols)
						}
					}
				})
			}
		})
	}
}
