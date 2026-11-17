package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3994: `cleatctl deploy` was declared PostgreSQL-only as ONE key covering TWO commands, and
// they are not equally portable. `deploy workflow` and `versions` go entirely through the store and
// engine.PluginNameRegistry, both of which take a dialect; `deploy plugin` goes to
// engine.PluginLoader, whose `ON CONFLICT` is PostgreSQL-shaped and which fails on MySQL with
// Error 1064. Measured 2026-11-17 against real MySQL 8.4 and SQL Server 2022 databases.
//
// These tests hold both halves of that split: the workflow half runs on every dialect a DSN is
// provided for, and the plugin half refuses with a message that names what does work.

// TestDeployPluginRefusesOutsidePostgresForADialectReason pins the REFUSAL, which is the half that
// sends a MySQL user somewhere useful. The message must name `deploy workflow`; without that a
// reader concludes the whole command is unavailable and reaches for a lower-level tool.
func TestDeployPluginRefusesOutsidePostgresForADialectReason(t *testing.T) {
	for _, tc := range []struct {
		dialect engine.Dialect
		refused bool
	}{
		{engine.DialectPostgres, false},
		{engine.DialectMySQL, true},
		{engine.DialectMSSQL, true},
	} {
		t.Run(string(tc.dialect), func(t *testing.T) {
			// Asserted on the MESSAGE rather than on an exit code, because the message is the whole
			// contract here: an exit a user cannot read sends them nowhere. withExitPanicOutput turns
			// osExit into a panic it recovers, so a refusal returns its text like any other run.
			_, stderr := withExitPanicOutput(t, func() { requireDeployPluginPorted(tc.dialect) })

			if !tc.refused {
				if strings.Contains(stderr, "has not been ported") {
					t.Fatalf("deploy plugin refused on %s, which it is written for; stderr:\n%s", tc.dialect, stderr)
				}
				return
			}
			if !strings.Contains(stderr, "has not been ported") {
				t.Fatalf("deploy plugin did not refuse on %s. The plugin loader's SQL is PostgreSQL-only: "+
					"reaching it there fails at `ON CONFLICT (name, version)` with Error 1064", tc.dialect)
			}
			for _, want := range []string{"deploy plugin", "deploy workflow", string(tc.dialect)} {
				if !strings.Contains(stderr, want) {
					t.Errorf("the refusal does not name %q.\n\nstderr:\n%s\n\n"+
						"It must say that `deploy workflow` does work there, or a reader concludes the whole "+
						"command is unavailable. cleat#3994.", want, stderr)
				}
			}
		})
	}
}

// TestDeployWorkflowAndVersionsOnEveryDialect is the port's evidence, on each dialect a DSN is
// configured for. It drives the SAME store path main builds -- the factory, which is what provisions
// a tenant's own database on MySQL -- rather than a bare constructor, because a bare constructor
// would skip the step this test exists to cross.
func TestDeployWorkflowAndVersionsOnEveryDialect(t *testing.T) {
	for _, tc := range []struct {
		dialect testutil.Dialect
		ctl     dialect
	}{
		{testutil.DialectPostgres, dialectPostgres},
		{testutil.DialectMySQL, dialectMySQL},
		{testutil.DialectMSSQL, dialectMSSQL},
	} {
		t.Run(string(tc.dialect), func(t *testing.T) {
			db := testutil.TestDB(t, tc.dialect)
			dsn := dsnFor(t, db, tc.dialect)
			factory, err := tc.ctl.openStoreFactory(db, dsn, "public")
			if err != nil {
				t.Fatalf("openStoreFactory: %v", err)
			}
			store, closer, err := factory.OpenStore(context.Background(), defaultTenantID)
			if err != nil {
				t.Fatalf("OpenStore (this is the step a bare constructor would have skipped): %v", err)
			}
			defer func() { _ = closer.Close() }()

			// Unique per run: on MySQL a tenant's tables live in a database of their own, which outlives
			// this process's scratch database, so a fixed name would hit the dedup guard on the SECOND run
			// of this test -- a failure of the fixture, not of the port.
			name := fmt.Sprintf("cleatctl-3994-%s-%d", tc.dialect, time.Now().UnixNano())
			dir := t.TempDir()
			path := writeWASM(t, dir, stampedArtifact(t, name, 1, "body"))

			stdout, stderr := withExitPanicOutput(t, func() {
				deployWorkflow(context.Background(), store, db, []string{name, path})
			})
			if !strings.Contains(stdout, "Deployed "+name+" v1") {
				t.Fatalf("deploy workflow did not create v1 on %s.\nstdout: %s\nstderr: %s",
					tc.dialect, stdout, stderr)
			}

			// The same artifact again: the dedup guard reads through LoadWASM and ListWorkflowDefs,
			// so this is the read half of the port, not a repeat of the write half.
			stdout, stderr = withExitPanicOutput(t, func() {
				deployWorkflow(context.Background(), store, db, []string{name, path})
			})
			if !strings.Contains(stdout, "WASM unchanged") {
				t.Fatalf("redeploying the same artifact on %s did not hit the dedup guard, so the read "+
					"path is not returning its bytes.\nstdout: %s\nstderr: %s", tc.dialect, stdout, stderr)
			}

			// versions list renders from ListWorkflowDefs. The table it prints does not repeat the
			// workflow name, so the assertion is on the query `versions` runs plus the absence of the
			// "No versions found" branch -- asserting on the rendered table alone would pass on an
			// implementation that listed nothing.
			stdout, stderr = withExitPanicOutput(t, func() {
				listVersions(context.Background(), store, []string{name})
			})
			if strings.Contains(stdout, "No versions found") {
				t.Fatalf("versions list reported nothing for %s on %s.\nstdout: %s\nstderr: %s",
					name, tc.dialect, stdout, stderr)
			}
			defs, err := store.ListWorkflowDefs(context.Background(), name)
			if err != nil {
				t.Fatalf("ListWorkflowDefs on %s: %v", tc.dialect, err)
			}
			if len(defs) != 1 || defs[0].Name != name {
				t.Fatalf("ListWorkflowDefs returned %d definitions on %s, want exactly the one just "+
					"deployed (%q)", len(defs), tc.dialect, name)
			}
		})
	}
}

// dsnFor reconstructs a DSN for the SCRATCH database testutil.TestDB handed us, reading its name back
// from the connection rather than parsing it out of the environment: TestDB creates a new database
// per run and the env DSN still names the shared one.
func dsnFor(t *testing.T, db *sql.DB, d testutil.Dialect) string {
	t.Helper()
	var name string
	var query string
	switch d {
	case testutil.DialectPostgres:
		if testutil.PostgresTestDSN() == "" {
			// NOT a skip: testutil.TestDB already skipped when no DSN was configured, so reaching
			// here with an empty one means the two disagree about where the DSN comes from -- a
			// defect in this test, and a skip would hide it (scripts/check-skips.sh's case (c)).
			t.Fatalf("testutil.TestDB returned a PostgreSQL database but PostgresTestDSN() is empty")
		}
		if err := db.QueryRow("SELECT current_database()").Scan(&name); err != nil {
			t.Fatalf("current_database: %v", err)
		}
	case testutil.DialectMySQL:
		if os.Getenv("CLEAT_TEST_MYSQL") == "" {
			t.Fatalf("testutil.TestDB returned a MySQL database but CLEAT_TEST_MYSQL is empty")
		}
		if err := db.QueryRow("SELECT DATABASE()").Scan(&name); err != nil {
			t.Fatalf("SELECT DATABASE(): %v", err)
		}
	case testutil.DialectMSSQL:
		if os.Getenv("CLEAT_TEST_MSSQL") == "" {
			t.Fatalf("testutil.TestDB returned a SQL Server database but CLEAT_TEST_MSSQL is empty")
		}
		query = "SELECT DB_NAME()"
		if err := db.QueryRow(query).Scan(&name); err != nil {
			t.Fatalf("DB_NAME(): %v", err)
		}
	}

	var base string
	switch d {
	case testutil.DialectPostgres:
		base = testutil.PostgresTestDSN()
	case testutil.DialectMySQL:
		base = os.Getenv("CLEAT_TEST_MYSQL")
	case testutil.DialectMSSQL:
		base = os.Getenv("CLEAT_TEST_MSSQL")
	}
	return swapDSNDatabase(t, base, name)
}

// swapDSNDatabase replaces the database a DSN names, in the shape each one uses: a path segment for
// the URL-style PostgreSQL and MySQL DSNs, and the `database` query parameter for SQL Server.
func swapDSNDatabase(t *testing.T, dsn, name string) string {
	t.Helper()
	if strings.HasPrefix(dsn, "sqlserver://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parse mssql DSN: %v", err)
		}
		q := u.Query()
		q.Set("database", name)
		u.RawQuery = q.Encode()
		return u.String()
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// TestDeployAndVersionsAreNotDeclaredPostgresOnly pins the DECLARATION, which is the part of
// cleat#3994 that changes what a user is allowed to run. The dialect test above proves the code path
// works; this proves nobody has to be refused to reach it.
//
// Without this, reverting portedOn to {"postgres"} would leave every other test green: they call the
// functions directly, and requirePortedFor is what main calls first. That is the shape this file
// exists to avoid -- a green suite over a command nobody can run.
func TestDeployAndVersionsAreNotDeclaredPostgresOnly(t *testing.T) {
	for _, cmd := range []string{"deploy", "versions"} {
		for _, d := range []dialect{dialectPostgres, dialectMySQL, dialectMSSQL} {
			t.Run(cmd+"/"+d.name, func(t *testing.T) {
				_, stderr := withExitPanicOutput(t, func() { requirePortedFor(cmd, d) })
				if strings.Contains(stderr, "has not been ported") {
					t.Fatalf("`cleatctl %s` still refuses on %s.\n\nstderr:\n%s\n\n"+
						"deploy workflow and versions were measured working on MySQL 8.4 and SQL Server "+
						"2022 (cleat#3994); their SQL is the store's and engine.PluginNameRegistry's, both "+
						"dialect-aware. Only `deploy plugin` remains PostgreSQL-only, and it is restricted "+
						"separately.", cmd, d.name, stderr)
				}
			})
		}
	}
}
