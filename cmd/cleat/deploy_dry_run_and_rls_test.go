package main

import (
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#2065. Two independent defects in `cleat deploy`, both measured against
// a real installed binary and a real database before either was fixed:
//
//  1. With neither --db nor CLEAT_DATABASE_URL set, deploy printed a preview
//     and exited 0. A scripted `make deploy` (the fullstack template's, in
//     particular) that only checks the exit code believed it had deployed.
//  2. On the cleat_app role -- the one the worker's own error messages tell
//     an operator to use, since the worker refuses a superuser or BYPASSRLS
//     connection -- deploy failed with "cleat.tenant_id is not set", and
//     --tenant did nothing about it: the value was resolved but never told
//     to Postgres.
//
// writeFakeWasm below intentionally writes bytes that are not a valid WASM
// module. deploy's own metadata reader already has to tolerate that (it logs
// "no cleat.metadata section found" and falls back to flags-only
// configuration), and taking that path exercises the version-lookup SELECT
// as well as the INSERT -- both of which are RLS-scoped reads on
// workflow_defs -- rather than only the INSERT a real, versioned WASM binary
// would reach through its embedded version.

func writeFakeWasm(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("not a real wasm module"), 0o644); err != nil {
		t.Fatalf("write fake wasm: %v", err)
	}
	return p
}

func TestDeployWithoutDatabaseFailsRatherThanSilentlyDoingNothing(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	wasm := writeFakeWasm(t, t.TempDir(), "wf.wasm")

	cmd := exec.Command(cleatBinary, "deploy", "--name", "no-db-wf", wasm)
	cmd.Env = append(os.Environ(), "CLEAT_DATABASE_URL=")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("deploy with no database exited 0: %s", out)
	}
	if !strings.Contains(string(out), "--dry-run") {
		t.Errorf("refusal does not mention --dry-run as the way to preview: %s", out)
	}
}

func TestDeployDryRunNeedsNoDatabaseAndExitsZero(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	wasm := writeFakeWasm(t, t.TempDir(), "wf.wasm")

	cmd := exec.Command(cleatBinary, "deploy", "--dry-run", "--name", "dry-run-wf", wasm)
	cmd.Env = append(os.Environ(), "CLEAT_DATABASE_URL=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat deploy --dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Would deploy") {
		t.Errorf("--dry-run output does not describe what would happen: %s", out)
	}
}

// TestDeployUnderTheAppRoleSetsTenantContext is the RLS half of cleat#2065.
// PostgresRLSTestRole is neither a superuser nor a table owner, so -- unlike
// the DSN testutil.TestDB itself returns -- it is actually subject to
// workflow_defs's tenant_isolation_defs policy. A pass here that predates the
// fix would mean the policy was not being exercised, not that deploy worked;
// see engine/testutil/schema.go's doc comment on PostgresRLSTestRole.
func TestDeployUnderTheAppRoleSetsTenantContext(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupPostgresRLSRole(t, db)

	var dbName string
	if err := db.QueryRow(`SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatalf("reading current_database: %v", err)
	}
	superDSN := testutil.PostgresTestDSN()
	u, err := url.Parse(superDSN)
	if err != nil {
		t.Fatalf("parse %q: %v", superDSN, err)
	}
	u.Path = "/" + dbName
	rlsDSN, err := testutil.PostgresRLSDSN(u.String())
	if err != nil {
		t.Fatalf("derive RLS role DSN: %v", err)
	}

	const tenant = "3c711305-dcbc-4bd7-af7f-3c8081376e9a"
	const wf = "deploy-rls-tenant-wf"
	wasm := writeFakeWasm(t, t.TempDir(), "wf.wasm")

	cmd := exec.Command(cleatBinary, "--tenant", tenant, "deploy", "--db", rlsDSN, "--name", wf, wasm)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat deploy under the RLS test role: %v\n%s", err, out)
	}

	// THE ASSERTION THE OLD DEFECT WOULD HAVE FAILED: read the row back
	// through the superuser connection (which bypasses RLS, so it can see
	// rows in every tenant) and confirm it landed under the tenant --tenant
	// named, not the default the old, unset context would have produced --
	// except the old defect never got this far at all; it errored on the
	// version-lookup SELECT before any row existed.
	var gotTenant string
	if err := db.QueryRow(
		`SELECT tenant_id FROM workflow_defs WHERE name = $1`, wf,
	).Scan(&gotTenant); err != nil {
		t.Fatalf("no workflow_defs row for %q after deploy: %v", wf, err)
	}
	if gotTenant != tenant {
		t.Errorf("deployed row has tenant_id %q, want %q (the one --tenant named)", gotTenant, tenant)
	}
}
