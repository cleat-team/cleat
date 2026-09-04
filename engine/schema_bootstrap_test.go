package engine

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/lib/pq"
)

// The tests in this file assert that the schema we *ship* is the schema the
// engine actually needs.
//
// Every other database test in this repo builds its schema through
// engine/testutil, which is a test-only path. That left the shipped artifact
// completely unverified, and it had rotted badly: the documented bootstrap
// (a root schema.sql, named "the canonical schema" by
// docs/explanation/postgresql-schema.md and mounted into initdb.d by
// docker-compose.cluster.yml) had drifted into a strict subset of
// migrations/postgres/. A database built the documented way had:
//
//   - no finalize_workflow_status. store_lifecycle.go calls it on every
//     workflow completion and has no fallback, so no workflow could ever
//     finish.
//   - zero RLS policies and zero tables with rowsecurity set, so the entire
//     tenant isolation story was absent -- not bypassed, absent.
//   - no admin.tenants / admin.tenant_api_keys, so the Admin API could not
//     work either.
//
// schema.sql has been deleted and migrations/postgres/ is now the single
// source. These tests exist so that stays true: they build a scratch database
// from the shipped files alone and assert the engine's hard requirements.

// migrationsDir returns the repo's migrations/postgres directory.
func migrationsDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd() // .../engine
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Join(wd, "..", "migrations", "postgres")
}

// bootstrapScratchDB creates a fresh, empty database and applies every file in
// migrations/postgres/ to it in lexical order -- exactly what
// docker-entrypoint-initdb.d does with the mounted directory, and what
// docs/explanation/postgresql-schema.md tells an operator to do by hand.
//
// It deliberately does NOT use engine/testutil: the point is to exercise the
// shipped path, so borrowing the test harness's schema setup would defeat the
// test entirely.
func bootstrapScratchDB(t *testing.T, dbName ...string) *sql.DB {
	t.Helper()

	adminDSN := testutilPostgresDSN(t)
	admin, err := sql.Open("postgres", adminDSN)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()
	// This file deliberately does not call testutil.TestDB (see the package
	// doc comment above) so it has to reproduce, not inherit, the
	// configured-vs-default distinction TestDB makes centrally
	// (engine/testutil/schema.go). Before this fix, this Ping used a bare
	// t.Skipf regardless of why the connection failed -- the exact bug
	// TestDB was fixed for in c26c332, reintroduced locally. It was inert
	// only because this function used to call the since-deleted
	// requireBackendReachable first, which Fatal'd on a configured-but-
	// unreachable database one step earlier; deleting that helper without
	// this fix would have silently regressed every test in this file, plus
	// the postgres leg of TestPluginMigrations_AllDialects (which calls
	// bootstrapScratchDB via the exported BootstrapScratchDB wrapper), back
	// to "Multi-DB CI never reached PostgreSQL, green for months."
	if err := admin.Ping(); err != nil {
		if postgresConfiguredForTest() {
			t.Fatalf("configured postgres database at %s is unreachable: %v", redactPostgresDSN(adminDSN), err)
		}
		t.Skipf("no postgres available (default DSN, none configured): %v", err)
	}

	// Callers that need their own database (so that what they assert about a
	// fresh schema is not affected by another test's, or a live worker's,
	// migrations) pass a name.
	scratch := "cleat_schema_bootstrap_test"
	if len(dbName) > 0 && dbName[0] != "" {
		scratch = dbName[0]
	}
	// CREATE/DROP DATABASE cannot run inside a transaction, hence plain Exec.
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + scratch); err != nil {
		t.Fatalf("drop scratch database: %v", err)
	}
	if _, err := admin.Exec(`CREATE DATABASE ` + scratch); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("postgres", adminDSN)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(`DROP DATABASE IF EXISTS ` + scratch)
	})

	scratchDSN, err := swapDatabase(adminDSN, scratch)
	if err != nil {
		t.Fatalf("derive scratch DSN: %v", err)
	}
	db, err := sql.Open("postgres", scratchDSN)
	if err != nil {
		t.Fatalf("open scratch database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	dir := migrationsDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	if len(files) == 0 {
		t.Fatalf("no .sql files in %s -- has the migrations layout changed?", dir)
	}
	// Lexical order is the contract: it is what initdb.d uses and what the
	// numeric prefixes exist to encode.
	sort.Strings(files)

	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := db.Exec(string(data)); err != nil {
			t.Fatalf("applying shipped migration %s failed: %v\n\n"+
				"These files are what docker-compose.cluster.yml mounts into "+
				"initdb.d and what the docs tell operators to run. If this "+
				"fails, a fresh deployment cannot be created at all.", name, err)
		}
	}
	return db
}

// BootstrapScratchDB exposes bootstrapScratchDB to the engine_test package,
// which cannot see unexported identifiers of package engine.
// TestPluginMigrations_AllDialects uses it to get a database no other test and
// no live worker has migrated; see the comment on the postgres backend there.
func BootstrapScratchDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	return bootstrapScratchDB(t, name)
}

// testutilPostgresDSN resolves the Postgres DSN with the same env-var
// precedence testutil.PostgresTestDSN uses. Duplicated rather than imported so
// that this file has no dependency on the test harness it is auditing.
func testutilPostgresDSN(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("CLEAT_TEST_POSTGRES"); v != "" {
		return v
	}
	if v := os.Getenv("CLEAT_TEST_DB"); v != "" {
		return v
	}
	return "postgres://localhost:5432/cleat?sslmode=disable"
}

// postgresConfiguredForTest reports whether a Postgres DSN was explicitly
// supplied for tests, as opposed to bootstrapScratchDB falling back to
// testutilPostgresDSN's hardcoded localhost default. Mirrors the
// `configured` check in testutil.TestDB (engine/testutil/schema.go) --
// duplicated rather than imported for the same reason testutilPostgresDSN
// is: this file exercises the shipped migrations independent of the test
// harness it is auditing.
func postgresConfiguredForTest() bool {
	return os.Getenv("CLEAT_TEST_POSTGRES") != "" || os.Getenv("CLEAT_TEST_DB") != ""
}

// redactPostgresDSN strips the password from a DSN so it can appear in test
// failure output. Duplicated from testutil's unexported redactDSN
// (engine/testutil/schema.go) for the same reason as testutilPostgresDSN.
func redactPostgresDSN(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		u.User = url.User(u.User.Username())
		return u.String()
	}
	if i := strings.LastIndex(dsn, "@"); i >= 0 {
		return "***@" + dsn[i+1:]
	}
	return dsn
}

// swapDatabase returns dsn with its database name replaced by name.
func swapDatabase(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

// TestShippedSchema_SupportsWorkflowCompletion asserts that a database built
// from the shipped migrations can finalize a workflow.
//
// finalize_workflow_status lives in 003_procedures.sql, and
// (*PostgresStore).FinalizeWorkflowSegment calls it directly with no fallback.
// An operator who applied only 001_schema.sql -- or who used the old root
// schema.sql, which never had it -- got a database where every single workflow
// completion failed with "function finalize_workflow_status does not exist".
func TestShippedSchema_SupportsWorkflowCompletion(t *testing.T) {
	db := bootstrapScratchDB(t)

	var exists bool
	err := db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM pg_proc p
			JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE p.proname = 'finalize_workflow_status'
			  AND n.nspname = 'public'
		)`).Scan(&exists)
	if err != nil {
		t.Fatalf("query pg_proc: %v", err)
	}
	if !exists {
		t.Fatalf("finalize_workflow_status is absent from a database built " +
			"from the shipped migrations.\n" +
			"engine/store_lifecycle.go calls it on every workflow completion " +
			"with no fallback, so this database cannot finish any workflow.")
	}

	// Existence is not enough: the signature has to match what the engine
	// sends. Call it exactly as FinalizeWorkflowSegment does, against a
	// workflow that does not exist -- it should report "fence not held"
	// (false) rather than fail to resolve.
	var fenceHeld bool
	err = db.QueryRow(`
		SELECT finalize_workflow_status($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, "no-such-workflow", "worker-1", int64(1), "done", `{}`, "", "", "{}",
		nil, "").Scan(&fenceHeld)
	if err != nil {
		t.Fatalf("finalize_workflow_status exists but is not callable with the "+
			"argument list engine/store_lifecycle.go uses: %v", err)
	}
	if fenceHeld {
		t.Errorf("finalize_workflow_status reported the fence held for a "+
			"workflow that does not exist; got %v, want false", fenceHeld)
	}
}

// TestShippedSchema_HasTenantIsolation asserts that the tenant-scoped tables
// in a shipped database actually carry row-level security.
//
// This checks configuration, not enforcement -- Postgres exempts superusers
// from RLS unconditionally, so a superuser connection bypasses these policies
// no matter what this test says. Enforcement is covered by the tests that use
// testutil.OpenPostgresRLSTestDB. What this test catches is the failure the
// old schema.sql had: policies that were never created in the first place.
func TestShippedSchema_HasTenantIsolation(t *testing.T) {
	db := bootstrapScratchDB(t)

	// Tables the engine writes a tenant_id to and expects to be partitioned.
	tenantScoped := []string{
		"workflow_instances",
		"event_history",
		"workflow_signals",
		"workflow_schedules",
		"workflow_promises",
		"workflow_tags",
		"workflow_routing",
	}

	for _, table := range tenantScoped {
		t.Run(table, func(t *testing.T) {
			var rowSecurity, forceRowSecurity bool
			err := db.QueryRow(`
				SELECT c.relrowsecurity, c.relforcerowsecurity
				FROM pg_class c
				JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE c.relname = $1 AND n.nspname = 'public'
			`, table).Scan(&rowSecurity, &forceRowSecurity)
			if err == sql.ErrNoRows {
				t.Fatalf("table %s does not exist in a shipped database", table)
			}
			if err != nil {
				t.Fatalf("query pg_class for %s: %v", table, err)
			}
			if !rowSecurity {
				t.Errorf("%s does not have ROW LEVEL SECURITY enabled", table)
			}
			// Without FORCE, the table owner bypasses RLS silently -- and the
			// role that runs the migrations is normally the same role the
			// worker connects as.
			if !forceRowSecurity {
				t.Errorf("%s has RLS enabled but not FORCED, so the owning role "+
					"(usually the same role the worker connects as) bypasses "+
					"every policy on it", table)
			}

			var policies int
			if err := db.QueryRow(`
				SELECT count(*) FROM pg_policies
				WHERE schemaname = 'public' AND tablename = $1
			`, table).Scan(&policies); err != nil {
				t.Fatalf("query pg_policies for %s: %v", table, err)
			}
			if policies == 0 {
				t.Errorf("%s has no RLS policy, so RLS being enabled denies "+
					"everything rather than isolating tenants", table)
			}
		})
	}
}

// TestShippedSchema_HasAdminTables asserts the admin/tenant registry exists.
//
// The Admin API (cmd/cleat-worker, --enable-admin-api) reads and writes these.
// The old root schema.sql had none of them, so the Admin API was unusable on a
// database built the documented way.
func TestShippedSchema_HasAdminTables(t *testing.T) {
	db := bootstrapScratchDB(t)

	for _, table := range []string{"tenants", "tenant_api_keys", "tenant_roles"} {
		var exists bool
		err := db.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = 'admin' AND table_name = $1
			)`, table).Scan(&exists)
		if err != nil {
			t.Fatalf("query information_schema for admin.%s: %v", table, err)
		}
		if !exists {
			t.Errorf("admin.%s is absent from a shipped database; the Admin API "+
				"depends on it", table)
		}
	}
}

// TestShippedSchema_IsIdempotent asserts the migrations can be applied twice.
//
// docs/explanation/postgresql-schema.md tells operators that re-running them is
// safe, and an operator upgrading an existing deployment will do exactly that.
// This is also the property that broke in 004: it used CREATE OR REPLACE on a
// function whose return type had changed, which Postgres rejects with 42P13,
// so any database that had already run 003 could not take the fix.
func TestShippedSchema_IsIdempotent(t *testing.T) {
	db := bootstrapScratchDB(t)

	dir := migrationsDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := db.Exec(string(data)); err != nil {
			t.Errorf("re-applying %s failed: %v\n\n%s", name, err,
				fmt.Sprintf("The docs promise these files are idempotent, and an "+
					"operator upgrading an existing database will re-run them. "+
					"%s cannot be applied to a database that already has it.", name))
		}
	}

	// Re-applying must also leave the *right* definition in place. 003 creates
	// finalize_workflow_status RETURNS VOID and 004 replaces it with the
	// fence-guarded RETURNS BOOLEAN version; the DROP that makes 003
	// re-appliable also means a re-run briefly reinstates the unguarded VOID
	// version. That is only safe because 004 sorts after 003 and runs again
	// too. If the ordering or the numbering ever changes, a re-applied
	// database would silently lose the zombie-writer fence -- the guard would
	// be gone while every test that mocks the store still passed.
	var returnType string
	err = db.QueryRow(`
		SELECT pg_catalog.pg_get_function_result(p.oid)
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE p.proname = 'finalize_workflow_status' AND n.nspname = 'public'
	`).Scan(&returnType)
	if err != nil {
		t.Fatalf("look up finalize_workflow_status return type after re-apply: %v", err)
	}
	if !strings.EqualFold(returnType, "boolean") {
		t.Errorf("after re-applying the migrations, finalize_workflow_status "+
			"returns %q, want boolean.\n"+
			"The BOOLEAN version (004) carries the zombie-writer fence guard; "+
			"the VOID version (003) does not. A re-applied database has lost "+
			"the fence.", returnType)
	}

	// The same pairing, one function later: 023 creates admin.claim_workflows
	// and 040 replaces it with a version that accepts 'terminating' and returns
	// pending_terminal_status. 023 carries the DROP that makes it re-appliable,
	// which means a re-run briefly reinstates the 14-column version that cannot
	// claim a defer phase. Safe only because 040 sorts after 023 and runs again.
	//
	// Asserted on the column count rather than on the predicate because the
	// count is what the Go scan depends on: a re-applied database left at 023's
	// shape fails every cross-tenant claim with "expected 15 destination
	// arguments in Scan, not 14", which is a loud failure, while the missing
	// 'terminating' alone would be a silent one -- defer phases never claimed,
	// every terminate waiting out its deadline.
	var claimCols int
	err = db.QueryRow(`
		SELECT COALESCE(array_length(p.proallargtypes, 1), 0)
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE p.proname = 'claim_workflows' AND n.nspname = 'admin'
	`).Scan(&claimCols)
	if err != nil {
		t.Fatalf("look up admin.claim_workflows shape after re-apply: %v", err)
	}
	// proallargtypes counts IN plus OUT (the RETURNS TABLE columns): 3 in,
	// 15 out.
	if claimCols != 18 {
		t.Errorf("after re-applying the migrations, admin.claim_workflows has %d "+
			"arguments and result columns, want 18 (3 in + 15 out).\n"+
			"A re-applied database has been left at 023's 14-column version, "+
			"which cannot claim a defer phase.", claimCols)
	}
}

// TestShippedSchema_CreatesObjectsInPublic asserts that the migrations build the
// schema in `public` regardless of what the connecting role is called.
//
// PostgreSQL's default search_path is "$user", public. 001_schema.sql creates a
// schema named "cleat", and docker-compose.cluster.yml connects as
// POSTGRES_USER=cleat -- so "$user" resolved to that freshly-created schema and
// every unqualified CREATE landed inside it. Measured on PostgreSQL 16 before
// the fix, against a database built exactly as the shipped compose builds one:
//
//	 nspname | tables
//	---------+--------
//	 admin   |      4
//	 cleat   |     14      <- should have been public
//
// finalize_workflow_status went the same way. Nothing looked wrong from psql,
// because the same "$user" entry that misplaced the objects also found them
// again; but anything naming public.* broke, including create_tenant_role's
// GRANTs on public.workflow_defs, and the engine could not see its own tables.
//
// The migrations now pin `SET search_path = public`. This test only has teeth
// when the connecting role happens to share a name with a schema -- which is
// precisely the shipped configuration, and what the cluster CI job uses.
func TestShippedSchema_CreatesObjectsInPublic(t *testing.T) {
	db := bootstrapScratchDB(t)

	rows, err := db.Query(`
		SELECT n.nspname, count(*)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'r'
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		GROUP BY n.nspname
	`)
	if err != nil {
		t.Fatalf("query table namespaces: %v", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var ns string
		var n int
		if err := rows.Scan(&ns, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		counts[ns] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if counts["public"] == 0 {
		t.Errorf("no tables in the public schema; the migrations built the schema "+
			"somewhere else entirely. Table counts by schema: %v", counts)
	}
	for ns, n := range counts {
		if ns != "public" && ns != "admin" {
			t.Errorf("%d table(s) created in schema %q; the migrations must create "+
				"application tables in public regardless of the connecting role's "+
				"name (search_path is \"$user\", public). Counts by schema: %v",
				n, ns, counts)
		}
	}
}
