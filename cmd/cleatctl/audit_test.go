package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/auditlog"
	"github.com/google/uuid"
)

// runAuditCapturing runs `cleatctl audit <args>` and returns its exit status and both
// output streams. The status is 0 when osExit is never reached.
func runAuditCapturing(t *testing.T, db *sql.DB, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	stdout, stderr = withExitPanicOutput(t, func() {
		orig := osExit
		defer func() { osExit = orig }()
		osExit = func(c int) { code = c; panic("EXIT") }
		runAudit(context.Background(), db, dialectPostgres, args)
	})
	return code, stdout, stderr
}

// The three exit statuses are the point of this command's interface: 1 sends the
// operator to the log, 2 sends them to the check, and a check that measured nothing
// must not agree with a log that is fine.
func TestAuditVerifyExitStatusesAreDistinct(t *testing.T) {
	adminDSN := os.Getenv("CLEAT_TEST_POSTGRES")
	if adminDSN == "" {
		adminDSN = os.Getenv("CLEAT_TEST_DB")
	}
	if adminDSN == "" {
		t.Skip("neither CLEAT_TEST_POSTGRES nor CLEAT_TEST_DB is set, skipping")
	}
	ctx := context.Background()
	admin, err := sql.Open("postgres", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("configured PostgreSQL is unreachable: %v", err)
	}
	scratch := "cleat_ctl2047_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+scratch); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	defer func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS " + scratch + " WITH (FORCE)") }()
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + scratch
	db, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	// 1. THE AUDIT TABLES ARE ABSENT: the check cannot look, and must say so with
	// status 2 -- not 0, which would read as a clean log.
	code, _, stderr := runAuditCapturing(t, db, "verify", "--all-tenants")
	if code != 2 || !strings.Contains(stderr, "UNMEASURED") {
		t.Errorf("with no audit tables: exit %d, stderr %q; want exit 2 and UNMEASURED", code, stderr)
	}

	// The plugin's own migrations, then real rows written by its own recorder.
	pl := auditlog.New()
	if err := plugin.RunMigrations(ctx, db, plugin.DialectPostgres, nil,
		[]*plugin.LoadedPlugin{{Plugin: pl, Healthy: true}}, plugin.WithSchema("public")); err != nil {
		t.Fatalf("the plugin's migrations: %v", err)
	}
	pdb := &engine.SQLDBAdapter{DB: db, Dialect: plugin.DialectPostgres}
	if err := pl.Init(ctx, &plugin.Environment{DB: pdb, Dialect: plugin.DialectPostgres,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pl.(interface{ Run(context.Context) error }).Run(runCtx)
	}()
	defer func() { stop(); <-done }()
	mw := pl.(interface {
		Middleware(http.Handler) http.Handler
	}).Middleware(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	good, bad := uuid.New(), uuid.New()
	const n = 6
	for _, tenant := range []uuid.UUID{good, bad} {
		for i := 0; i < n; i++ {
			req := httptest.NewRequest("GET", fmt.Sprintf("/api/workflows/%d", i), nil)
			req = req.WithContext(auth.WithSubject(auth.WithTenantID(req.Context(), tenant), "someone"))
			mw.ServeHTTP(httptest.NewRecorder(), req)
		}
	}
	// Wait for the recorder to have written every row: a chain checked while it is
	// still being appended would be a measurement of the wait, not of the command.
	deadline := time.Now().Add(20 * time.Second)
	for {
		reps := 0
		for _, tenant := range []uuid.UUID{good, bad} {
			if r, err := auditlog.VerifyChain(ctx, pdb, plugin.DialectPostgres, tenant, auditlog.VerifyOptions{}); err == nil && r.Checked == n {
				reps++
			}
		}
		if reps == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("UNMEASURED: the recorder did not write both tenants' rows within 20s")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// 2. AN UNTOUCHED CHAIN: exit 0, and the output says what was checked.
	code, stdout, stderr := runAuditCapturing(t, db, "verify", "--tenant", good.String())
	if code != 0 || !strings.Contains(stdout, "OK") || !strings.Contains(stdout, fmt.Sprintf("%d row(s) verified", n)) {
		t.Errorf("an untouched chain: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// 3. THE KNOWN POSITIVE: edit one row, and the command must say exit 1 and name it.
	if n, err := pdb.Exec(plugin.ForTenant(ctx, bad),
		`UPDATE audit_events SET path = '/edited' WHERE tenant_id = $1 AND seq = 3`, bad); err != nil || n != 1 {
		t.Fatalf("tampering changed %d rows (err %v); it must change exactly one, or the test tampers with nothing", n, err)
	}
	code, stdout, stderr = runAuditCapturing(t, db, "verify", "--tenant", bad.String())
	if code != 1 || !strings.Contains(stdout, "BROKEN") || !strings.Contains(stdout, "edited at seq 3") {
		t.Errorf("an edited row: exit %d\nstdout: %s\nstderr: %s\nwant exit 1 naming the break", code, stdout, stderr)
	}

	// 4. ALL TENANTS: one clean and one broken. The broken one decides the status; the
	// clean one is still printed; the denominator is stated.
	code, stdout, stderr = runAuditCapturing(t, db, "verify", "--all-tenants")
	if code != 1 || !strings.Contains(stdout, "OK      tenant "+good.String()) ||
		!strings.Contains(stdout, "BROKEN  tenant "+bad.String()) ||
		!strings.Contains(stderr, "verified 2 of 2 tenant chain(s): 1 ok, 1 broken, 0 unmeasured") {
		t.Errorf("all tenants: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// 4b. A TENANT THE RUN CANNOT READ makes the whole answer incomplete: status 2 even
	// though another tenant is broken and a third is fine -- and the ones it did read
	// are still printed, so the operator loses nothing but the certainty.
	realVerify := auditVerifyChain
	defer func() { auditVerifyChain = realVerify }()
	auditVerifyChain = func(ctx context.Context, db plugin.PluginDB, d plugin.Dialect, tenant uuid.UUID, o auditlog.VerifyOptions) (auditlog.ChainReport, error) {
		if tenant == good {
			return auditlog.ChainReport{}, fmt.Errorf("simulated read failure")
		}
		return realVerify(ctx, db, d, tenant, o)
	}
	code, stdout, stderr = runAuditCapturing(t, db, "verify", "--all-tenants")
	if code != 2 || !strings.Contains(stderr, "UNMEASURED tenant "+good.String()) ||
		!strings.Contains(stdout, "BROKEN  tenant "+bad.String()) ||
		!strings.Contains(stderr, "verified 1 of 2 tenant chain(s): 0 ok, 1 broken, 1 unmeasured") {
		t.Errorf("one tenant unreadable: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	auditVerifyChain = realVerify

	// 5. JSON is a parseable array carrying the same finding.
	code, stdout, _ = runAuditCapturing(t, db, "verify", "--tenant", bad.String(), "--json")
	if code != 1 || !strings.Contains(stdout, `"kind": "edited"`) {
		t.Errorf("--json: exit %d, stdout %s", code, stdout)
	}

	// 5c. A FLOOR. Move it over the first two of `good`'s rows, recording their true
	// timestamp (minutes old). Without --retention-days that verifies, and says out loud that
	// the floor was not checked; with it, the floor over unexpired rows is a finding.
	var h2 string
	var ts2 int64
	if err := pdb.QueryRow(plugin.ForTenant(ctx, good), `SELECT row_hash, (EXTRACT(EPOCH FROM timestamp) * 1000000)::bigint FROM audit_events WHERE tenant_id = $1 AND seq = 2`, good).Scan(&h2, &ts2); err != nil {
		t.Fatal(err)
	}
	if n, err := pdb.Exec(plugin.ForTenant(ctx, good), `DELETE FROM audit_events WHERE tenant_id = $1 AND seq <= 2`, good); err != nil || n != 2 {
		t.Fatalf("removed %d rows (err %v), want 2", n, err)
	}
	if n, err := pdb.Exec(plugin.ForTenant(ctx, good), `UPDATE audit_chain_heads SET floor_seq = 2, floor_hash = $1, floor_ts = $2 WHERE tenant_id = $3`, strings.TrimSpace(h2), ts2, good); err != nil || n != 1 {
		t.Fatalf("moved the floor on %d rows (err %v)", n, err)
	}
	code, stdout, stderr = runAuditCapturing(t, db, "verify", "--tenant", good.String())
	if code != 0 || !strings.Contains(stdout, "OK") || !strings.Contains(stderr, "NOTE tenant "+good.String()) || !strings.Contains(stderr, "was not checked") {
		t.Errorf("a floor with no --retention-days: exit %d\nstdout: %s\nstderr: %s\nwant OK and a NOTE that the floor was not checked", code, stdout, stderr)
	}
	code, stdout, stderr = runAuditCapturing(t, db, "verify", "--tenant", good.String(), "--retention-days", "90")
	if code != 1 || !strings.Contains(stdout, "floor_unexpired") || strings.Contains(stderr, "NOTE") {
		t.Errorf("a floor over unexpired rows with --retention-days 90: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// 6. USAGE is status 2, never 0: no flags, both flags, a non-UUID, another verb.
	for _, args := range [][]string{
		{"verify"},
		{"verify", "--tenant", good.String(), "--all-tenants"},
		{"verify", "--tenant", "not-a-uuid"},
		{"export"},
		{},
	} {
		if code, _, _ := runAuditCapturing(t, db, args...); code != 2 {
			t.Errorf("audit %v: exit %d, want 2", args, code)
		}
	}
}
