package main

// `--require-host-match` must boot as the role a deployment is told to run as, and serve (cleat#2258).
//
// At startup the worker asks "is ANY hostname registered?" (checkHostBindingConfigured ->
// TenantStore.CountTenantDomains), and every authenticated request then asks "does this tenant own this
// Host?" (TenantForHost). Both read tenant_domains, which is scoped by a policy that reads the tenant the
// CONNECTION carries. Both used to read on the bare pool. As cleat_app on PostgreSQL that raised
// `cleat.tenant_id is not set (P0001)` and the worker exited; on SQL Server the security policy filtered
// every row, so the count was 0 and the worker refused with "tenant_domains is empty" while a domain was
// registered. No test booted the worker as cleat_app, which is presumably how it shipped, and a superuser
// (what every other test connects as) bypasses the policy, so a test against one would have passed.
//
// This runs the real binary, because the property is about what the PROCESS does at boot and on a request,
// and asserts each step with a known-positive beside it:
//
//   - PostgreSQL's premise is checked, not assumed: cleat_app does not bypass RLS, and an unscoped read of
//     tenant_domains as cleat_app fails. Without that the test could pass against the old code.
//   - With NO domains the worker still refuses, and with the check's own message (`tenant_domains is empty`),
//     not with a database error. The fix must not weaken the check, so the check has to be seen refusing.
//   - With a domain registered ONLY for a second, suspended tenant the worker boots: the count enumerates
//     tenants, it does not look at the default one alone.
//   - A request with the key and the registered Host is served, and with an unregistered Host, or another
//     tenant's Host, is answered 404: the runtime lookup is scoped too, and still refuses.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const defaultTenantForHostTest = "00000000-0000-0000-0000-000000000000"

func TestRequireHostMatchBootsAsTheAppRoleAndServesOnEveryDialect(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the worker binary")
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

	for _, c := range []deployDialect{
		{"postgres", "CLEAT_TEST_POSTGRES", "postgres"},
		{"mysql", "CLEAT_TEST_MYSQL", "mysql"},
		{"mssql", "CLEAT_TEST_MSSQL", "sqlserver"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.admin() == "" {
				t.Skipf("%s not set, skipping %s", c.env, c.name)
			}
			ownerDSN, owner := deployScratch(t, c)
			if code, out := runWorker(t, bin, nil, "--driver="+c.name, "--db="+ownerDSN, "--migrate-only"); code != 0 {
				t.Fatalf("--migrate-only exited %d:\n%s", code, out)
			}

			// The connection the worker serves on. PostgreSQL is the role that ships; the other two run
			// as the scratch database's login (SQL Server's security policy applies to it, MySQL has no
			// row-level policy and is a database per tenant).
			appDSN := ownerDSN
			if c.name == "postgres" {
				appDSN = pgAppRoleDSN(t, owner, ownerDSN)
			}
			args := func(extra ...string) []string {
				a := []string{"--driver=" + c.name, "--db=" + appDSN, "--migrate-db=" + ownerDSN,
					"--require-host-match", fmt.Sprintf("--api-addr=127.0.0.1:%d", freePort(t))}
				return append(a, extra...)
			}

			// 1. THE REFUSAL IS STILL THERE, and it is the check's own. No domain is registered.
			code, out := runWorker(t, bin, nil, args()...)
			if code == 0 {
				t.Fatalf("a worker with --require-host-match and no registered domain exited 0; the check must refuse")
			}
			if !strings.Contains(out, "tenant_domains is empty") {
				t.Errorf("with no domain registered the refusal must be the check's `tenant_domains is empty`, "+
					"not a database error (that was cleat#2258):\n%s", out)
			}
			if strings.Contains(out, "could not read tenant_domains") || strings.Contains(out, "P0001") {
				t.Errorf("the boot check could not READ tenant_domains as the app role:\n%s", out)
			}

			// 2. A domain registered only for a SECOND, SUSPENDED tenant: the count is across tenants.
			other, otherHost := "", "other.tenant.example.test"
			// The worker prints the key it generates ONCE, on the first boot that finds none, so keep it.
			var key string
			if c.name != "mysql" {
				other = "5a5a5a5a-0000-4000-8000-000000000258"
				seedTenantWithDomain(t, owner, c.name, other, "cleat-2258-suspended", otherHost, true)
				if ok, out := hostMatchServes(t, bin, args(), &key, nil); !ok {
					t.Fatalf("a worker whose only registered domain belongs to a suspended tenant did not boot: "+
						"the startup count sees only some tenants.\n%s", out)
				}
			}

			// 3. The default tenant's domain: boots, and the request path is scoped and still refuses.
			host := "app.tenant.example.test"
			execDomain(t, owner, c.name, host, defaultTenantForHostTest)
			ok, out2 := hostMatchServes(t, bin, args(), &key, func(base, key string) {
				status := func(h string) int {
					req, _ := http.NewRequest(http.MethodGet, base+"/api/workflows", nil)
					req.Host = h
					req.Header.Set("Authorization", "Bearer "+key)
					resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
					if err != nil {
						t.Fatalf("GET with Host %s: %v", h, err)
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					return resp.StatusCode
				}
				if got := status(host); got != http.StatusOK {
					t.Errorf("the default tenant's key with its own registered Host = %d, want 200: the request-time "+
						"lookup (TenantForHost) is not scoped as the app role", got)
				}
				if got := status("not-registered.example.test"); got != http.StatusNotFound {
					t.Errorf("an unregistered Host = %d, want 404", got)
				}
				if other != "" {
					if got := status(otherHost); got != http.StatusNotFound {
						t.Errorf("ANOTHER tenant's Host with the default tenant's key = %d, want 404 (the same "+
							"answer as an unregistered one: no oracle)", got)
					}
				}
			})
			if !ok {
				t.Fatalf("a worker with a registered domain did not boot as the app role:\n%s", out2)
			}
		})
	}
}

// pgAppRoleDSN gives cleat_app a login on the scratch database and returns its DSN, after checking the
// premise the whole test rests on: cleat_app does not bypass row-level security, and an unscoped read of
// tenant_domains as cleat_app fails.
func pgAppRoleDSN(t *testing.T, owner *sql.DB, ownerDSN string) string {
	t.Helper()
	if _, err := owner.Exec(`ALTER ROLE cleat_app LOGIN PASSWORD 'cleat-2258-pw'`); err != nil {
		t.Fatalf("giving cleat_app a login (is 005_app_role.sql applied?): %v", err)
	}
	u, err := url.Parse(ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("cleat_app", "cleat-2258-pw")
	dsn := u.String()
	app, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	var bypass bool
	if err := app.QueryRow(`SELECT rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil {
		t.Fatalf("connecting as cleat_app: %v", err)
	}
	if bypass {
		t.Fatal("cleat_app has BYPASSRLS, so this test cannot observe the failure it exists for")
	}
	var n int
	err = app.QueryRow(`SELECT count(*) FROM tenant_domains`).Scan(&n)
	if err == nil || !strings.Contains(err.Error(), "tenant_id is not set") {
		t.Fatalf("an unscoped read of tenant_domains as cleat_app should fail with `tenant_id is not set`, got n=%d err=%v: "+
			"the premise of cleat#2258 does not hold here, so a pass below would prove nothing", n, err)
	}
	return dsn
}

func seedTenantWithDomain(t *testing.T, owner *sql.DB, dialect, tenant, name, host string, suspended bool) {
	t.Helper()
	switch dialect {
	case "postgres":
		if _, err := owner.Exec(`INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES ($1, $2, $3)`, tenant, name, suspended); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	default:
		s := 0
		if suspended {
			s = 1
		}
		if _, err := owner.Exec(`INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES (@p1, @p2, @p3)`, tenant, name, s); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}
	execDomain(t, owner, dialect, host, tenant)
}

func execDomain(t *testing.T, owner *sql.DB, dialect, host, tenant string) {
	t.Helper()
	stmt := map[string]string{
		"postgres": `INSERT INTO tenant_domains (hostname, tenant_id) VALUES ($1, $2)`,
		"mysql":    `INSERT INTO tenant_domains (hostname, tenant_id) VALUES (?, ?)`,
		"mssql":    `INSERT INTO tenant_domains (hostname, tenant_id) VALUES (@p1, @p2)`,
	}[dialect]
	// SQL Server's policy has a BLOCK predicate: even the owner cannot insert a row for a tenant its
	// session is not scoped to. Set the context on the one connection that inserts.
	ctx := context.Background()
	conn, err := owner.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if dialect == "mssql" {
		if _, err := conn.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tenant); err != nil {
			t.Fatalf("scoping the seed connection to %s: %v", tenant, err)
		}
	}
	if _, err := conn.ExecContext(ctx, stmt, host, tenant); err != nil {
		t.Fatalf("register %s for %s: %v", host, tenant, err)
	}
}

var startupKeyRE = regexp.MustCompile(`cleat_sk_[A-Za-z0-9_-]+`)

// hostMatchServes starts the worker WITH authentication (--require-host-match only exists behind it, so
// startsHealthy's --require-auth=false would skip the check entirely), waits for /healthz, and runs probe
// with the base URL and the startup API key the worker printed (this boot's, or one
// remembered in *key from an earlier boot of the same database, since it is printed once). It reports whether the worker came up.
func hostMatchServes(t *testing.T, bin string, args []string, key *string, probe func(base, key string)) (bool, string) {
	t.Helper()
	var apiAddr string
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--api-addr="); ok {
			apiAddr = v
		}
	}
	cmd := exec.Command(bin, args...)
	var out syncBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	defer func() {
		_ = cmd.Process.Kill()
		<-exited
	}()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return false, out.String()
		default:
		}
		if resp, err := client.Get("http://" + apiAddr + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	if time.Now().After(deadline) {
		return false, out.String()
	}
	if k := startupKeyRE.FindString(out.String()); k != "" {
		*key = k
	}
	if probe != nil {
		if *key == "" {
			t.Fatalf("the worker did not print a startup API key, so the request checks cannot run:\n%s", out.String())
		}
		probe("http://"+apiAddr, *key)
	}
	return true, out.String()
}
