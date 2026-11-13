package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/auth"
)

// cleat#3772, owner decision D82. A worker whose connection is not subject to row-level security
// (a superuser, or BYPASSRLS) must not serve more than one tenant, whatever --require-auth and
// -rls-check say. cleat#3740 is what two tenants on such a connection did: a run executed the other
// tenant's module. Until D82 the only thing between that and an operator was --require-auth, which
// defaults to true, so `--require-auth=false` (documented in the tutorial for a single-tenant laptop)
// or `-rls-check=off` turned the refusal into a warning, or into nothing.
//
// These run the REAL BINARY, because every property here is what the process does at startup. They are
// PostgreSQL only: SQL Server's policy applies to every principal (measured for cleat#3740) and MySQL is
// tenant-per-database and single-tenant, and neither runs this check.
//
// The line is ONE TENANT. The tutorial's `--migrate-on-start --require-auth=false` on the superuser DSN
// must keep starting, and that is the first subtest, with the same flags.

const secondTenant = "f3772000-0000-4000-8000-0000000000aa"

// refusal returns the decoded message of the first ERROR line the worker logged whose message starts with
// prefix, or "". The worker logs JSON on stderr, so the message is read out of the record rather than out
// of an escaped string.
func refusal(out, prefix string) string {
	for _, line := range strings.Split(out, "\n") {
		var rec struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Level == "ERROR" && strings.HasPrefix(rec.Msg, prefix) {
			return rec.Msg
		}
	}
	return ""
}

func TestAWorkerOnAConnectionThatBypassesRLSRefusesToServeMoreThanOneTenant(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the worker binary")
	}
	c := deployDialect{"postgres", "CLEAT_TEST_POSTGRES", "postgres"}
	if c.admin() == "" {
		t.Skip("CLEAT_TEST_POSTGRES/CLEAT_TEST_DB not set, skipping")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "cleat-worker")
	build := exec.Command("go", "build", "-o", bin, "./cmd/cleat-worker")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/cleat-worker: %v\n%s", err, out)
	}

	// migrated returns a fresh, migrated scratch database and the owner (superuser) DSN for it.
	migrated := func(t *testing.T) (dsn string, owner *sql.DB) {
		t.Helper()
		dsn, owner = deployScratch(t, c)
		if code, out := runWorker(t, bin, nil, "--driver=postgres", "--db="+dsn, "--migrate-only"); code != 0 {
			t.Fatalf("--migrate-only exited %d:\n%s", code, out)
		}
		return dsn, owner
	}
	// tenants is what the guard counts, read by the test itself.
	tenants := func(t *testing.T, owner *sql.DB) int {
		t.Helper()
		var n int
		if err := owner.QueryRow(`SELECT count(*) FROM admin.tenants`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	addTenant := func(t *testing.T, owner *sql.DB, suspended bool) {
		t.Helper()
		if _, err := owner.Exec(`INSERT INTO admin.tenants (tenant_id, name, suspended, org_id) VALUES ($1, 'cleat-3772-b', $2, '00000000-0000-0000-0000-000000000000')`, secondTenant, suspended); err != nil {
			t.Fatalf("UNMEASURED: adding a second tenant: %v", err)
		}
	}
	// PRECONDITION, asserted rather than assumed: the owner connection really bypasses RLS. If it did not,
	// "two tenants and it refused" would say nothing about the bypass.
	bypasses := func(t *testing.T, owner *sql.DB) {
		t.Helper()
		var b bool
		if err := owner.QueryRow(`SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&b); err != nil || !b {
			t.Fatalf("UNMEASURED: the owner connection does not bypass row-level security (err=%v)", err)
		}
	}
	// wantRefusal runs the worker to completion and asserts it refused for the right reason, with the message
	// an operator needs: the role, the tenant count, the fix, and that the two escape flags do not lift it.
	wantRefusal := func(t *testing.T, dsn string, role string, count string, extra ...string) {
		t.Helper()
		args := append([]string{"--driver=postgres", "--db=" + dsn, "--api-addr=127.0.0.1:0", "--require-auth=false"}, extra...)
		code, out := runWorker(t, bin, nil, args...)
		if code == 0 {
			t.Fatalf("the worker exited 0 on a bypassing connection with %s tenants; it must refuse.\n%s", count, out)
		}
		msg := refusal(out, "refusing to start: this worker connects as")
		if msg == "" {
			t.Fatalf("the worker exited %d but not with the multi-tenant refusal:\n%s", code, out)
		}
		for _, want := range []string{`"` + role + `"`, "the database holds " + count + " tenants", "superuser", "--require-auth=false", "--rls-check=off", "cleat_app", "--migrate-db"} {
			if !strings.Contains(msg, want) {
				t.Errorf("the refusal is missing %q:\n%s", want, msg)
			}
		}
	}

	t.Run("one tenant on the superuser connection with --require-auth=false still starts (the tutorial's configuration)", func(t *testing.T) {
		dsn, owner := deployScratch(t, c)
		bypasses(t, owner)
		// The tutorial's one-command shortcut, flag for flag: --migrate-on-start on the superuser DSN.
		ok, out := startsHealthy(t, bin, "--driver=postgres", "--db="+dsn, "--migrate-on-start")
		if !ok {
			t.Fatalf("the single-tenant laptop configuration did not start:\n%s", out)
		}
		if n := tenants(t, owner); n != 1 {
			t.Fatalf("UNMEASURED: expected the default tenant alone, found %d", n)
		}
	})

	t.Run("two tenants on the superuser connection with --require-auth=false refuse", func(t *testing.T) {
		dsn, owner := migrated(t)
		bypasses(t, owner)
		addTenant(t, owner, false)
		wantRefusal(t, dsn, "postgres", "2")
	})

	t.Run("-rls-check=off does not lift it", func(t *testing.T) {
		dsn, owner := migrated(t)
		bypasses(t, owner)
		addTenant(t, owner, false)
		wantRefusal(t, dsn, "postgres", "2", "--rls-check=off")
	})

	t.Run("a suspended tenant counts: a resume puts it back in service with no restart", func(t *testing.T) {
		dsn, owner := migrated(t)
		bypasses(t, owner)
		addTenant(t, owner, true)
		wantRefusal(t, dsn, "postgres", "2")
	})

	t.Run("two tenants on a role that is subject to RLS start, with --require-auth=false", func(t *testing.T) {
		dsn, owner := migrated(t)
		addTenant(t, owner, false)
		app := pgAppRoleDSN(t, owner, dsn)
		if ok, out := startsHealthy(t, bin, "--driver=postgres", "--db="+app); !ok {
			t.Fatalf("a worker on cleat_app with two tenants did not start:\n%s", out)
		}
	})

	t.Run("creating a tenant is refused on a bypassing connection, and nothing is created", func(t *testing.T) {
		dsn, owner := migrated(t)
		bypasses(t, owner)
		code, out := runWorker(t, bin, nil, "--driver=postgres", "--db="+dsn,
			"--create-tenant=second", "--org=00000000-0000-0000-0000-000000000000")
		if code == 0 {
			t.Fatalf("--create-tenant on a bypassing connection exited 0; it must refuse.\n%s", out)
		}
		if msg := refusal(out, "refusing to create a tenant:"); msg == "" || !strings.Contains(msg, "would make 2 tenants") {
			t.Fatalf("the refusal is missing or does not say how many tenants it would make:\n%s", out)
		}
		if n := tenants(t, owner); n != 1 {
			t.Errorf("a refused --create-tenant left %d tenants, want 1", n)
		}
	})

	t.Run("the same --create-tenant on a role that is subject to RLS creates it (the control: the path works)", func(t *testing.T) {
		dsn, owner := migrated(t)
		app := pgAppRoleDSN(t, owner, dsn)
		code, out := runWorker(t, bin, nil, "--driver=postgres", "--db="+app,
			"--create-tenant=second", "--org=00000000-0000-0000-0000-000000000000")
		if code != 0 {
			t.Fatalf("--create-tenant as cleat_app exited %d:\n%s", code, out)
		}
		if n := tenants(t, owner); n != 2 {
			t.Errorf("--create-tenant as cleat_app left %d tenants, want 2", n)
		}
	})

	// POST /api/admin/tenants, the other supported way to create one. It needs an OPERATOR credential (a
	// worker with --require-auth=false has none, so the route answers 403 there), which means the worker
	// that can reach it on a bypassing connection is one with authentication ON and -rls-check=off: the
	// combination whose startup refusal D82 leaves in place while the database holds a single tenant.
	provision := func(t *testing.T, owner *sql.DB, dsn string) (int, string, bool) {
		t.Helper()
		rawKey := auth.GenerateOperatorKey()
		store, err := auth.NewOperatorStoreForDialect(owner, auth.DialectPostgres)
		if err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		if _, err := store.CreateOperatorKey(context.Background(), rawKey, auth.OperatorKeySpec{
			Description: "cleat#3772", Scopes: []string{auth.ScopeProvision}, Principal: "d82-test",
		}); err != nil {
			t.Fatalf("UNMEASURED: minting an operator key: %v", err)
		}
		cmd := exec.Command(bin, "--driver=postgres", "--db="+dsn, "--api-addr=127.0.0.1:0", "--enable-admin-api", "--rls-check=off")
		var out syncBuffer
		cmd.Stdout, cmd.Stderr = &out, &out
		cmd.Env = os.Environ()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan struct{})
		go func() { _ = cmd.Wait(); close(exited) }()
		defer func() { _ = cmd.Process.Kill(); <-exited }()
		client := &http.Client{Timeout: 5 * time.Second}
		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-exited:
				return 0, out.String(), false
			default:
			}
			addr := boundAPIAddr(out.String())
			if addr == "" {
				time.Sleep(300 * time.Millisecond)
				continue
			}
			resp, err := client.Get("http://" + addr + "/healthz")
			if err != nil || resp.StatusCode != http.StatusOK {
				if resp != nil {
					resp.Body.Close()
				}
				time.Sleep(300 * time.Millisecond)
				continue
			}
			resp.Body.Close()
			req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/api/admin/tenants",
				bytes.NewBufferString(`{"name":"second","org_name":"d82-org"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+rawKey)
			resp, err = client.Do(req)
			if err != nil {
				t.Fatalf("POST /api/admin/tenants: %v", err)
			}
			defer resp.Body.Close()
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(resp.Body)
			return resp.StatusCode, buf.String(), true
		}
		return 0, out.String(), false
	}

	t.Run("POST /api/admin/tenants is refused with 409 on a bypassing connection, and nothing is created", func(t *testing.T) {
		dsn, owner := migrated(t)
		bypasses(t, owner)
		status, body, ok := provision(t, owner, dsn)
		if !ok {
			t.Fatalf("UNMEASURED: the worker did not come up to be asked:\n%s", body)
		}
		if status != http.StatusConflict {
			t.Fatalf("POST /api/admin/tenants on a bypassing connection answered %d, want 409:\n%s", status, body)
		}
		if !strings.Contains(body, "refusing to create a tenant") || !strings.Contains(body, "would make 2 tenants") {
			t.Errorf("the 409 does not carry the refusal message:\n%s", body)
		}
		if n := tenants(t, owner); n != 1 {
			t.Errorf("a refused provision left %d tenants, want 1", n)
		}
	})

	t.Run("the same POST on a role that is subject to RLS creates it (the control: the route works)", func(t *testing.T) {
		dsn, owner := migrated(t)
		app := pgAppRoleDSN(t, owner, dsn)
		status, body, ok := provision(t, owner, app)
		if !ok {
			t.Fatalf("UNMEASURED: the worker did not come up to be asked:\n%s", body)
		}
		if status != http.StatusCreated {
			t.Fatalf("POST /api/admin/tenants as cleat_app answered %d, want 201:\n%s", status, body)
		}
		if n := tenants(t, owner); n != 2 {
			t.Errorf("a provision as cleat_app left %d tenants, want 2", n)
		}
	})
}
