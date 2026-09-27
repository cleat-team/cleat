package main

// cleat-review's should-fix on #2318: TestCheckPluginRouteSignatures and
// TestCheckPluginRouteSignaturesIsCalledFromMain prove, respectively, that
// checkPluginRouteSignatures classifies a stale signature correctly and that
// main.go's Init loop calls it -- but neither proves main() actually EXITS
// on that error rather than logging and continuing. Removing the os.Exit(1)
// that follows the call in main.go (cleat#2277's whole point) leaves both of
// those tests green, because neither one runs main() itself.
//
// This runs the REAL main() -- in a subprocess, because os.Exit(1) would
// otherwise kill the test binary -- with a plugin registered under the
// pre-cleat#2232 RegisterRoutes(mux *http.ServeMux) error signature, and
// asserts the process exits non-zero and names the plugin. The subprocess is
// the compiled TEST binary re-executed as itself (TestMain below intercepts
// before any test runs and calls main() directly), not a separate `go build
// ./cmd/cleat-worker`: only a _test.go file can register the stale-signature
// fixture plugin without shipping it in the production binary.
//
// a_migration_is_a_deploy_step_test.go and its neighbours in this package
// already boot the real binary against a live postgres, so this DB
// dependency is not new to the package -- CLEAT_TEST_POSTGRES, with
// CLEAT_TEST_DB as the fallback the test-go/commands job actually sets.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/cleat-team/cleat/plugin"
)

const (
	envBootRefusalSubprocess     = "CLEAT_2277_BOOT_REFUSAL_SUBPROCESS"
	envBootRefusalInjectStale    = "CLEAT_2277_BOOT_REFUSAL_INJECT_STALE_PLUGIN"
	envBootRefusalInjectRouteErr = "CLEAT_2277_BOOT_REFUSAL_INJECT_ROUTE_ERR_PLUGIN"
	staleRouteFixtureName        = "cleat-2277-boot-refusal-stale-route-fixture"
	routeErrFixtureName          = "cleat-2277-boot-refusal-route-err-fixture"
)

// staleRouteSignatureFixturePlugin has a method literally named
// RegisterRoutes but with the PRE-cleat#2232 signature
// (RegisterRoutes(mux *http.ServeMux) error, not RegisterRoutes(mux
// plugin.Router) error), so it does NOT satisfy plugin.HasRoutes -- the exact
// shape checkPluginRouteSignatures exists to catch.
type staleRouteSignatureFixturePlugin struct{}

func (staleRouteSignatureFixturePlugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{Name: staleRouteFixtureName}
}
func (staleRouteSignatureFixturePlugin) Init(ctx context.Context, env *plugin.Environment) error {
	return nil
}
func (staleRouteSignatureFixturePlugin) RegisterRoutes(mux *http.ServeMux) error { return nil }

// routeErrFixturePlugin satisfies plugin.HasRoutes with the CURRENT
// signature -- checkPluginRouteSignatures has nothing to flag -- but its
// RegisterRoutes itself fails. cleat-review's should-fix on #2318: main.go's
// other os.Exit(1), the one after `p.RegisterRoutes(pluginRouter)` fails
// (main.go:1666-ish, right below checkPluginRouteSignatures's), was
// untested. TestMainRefusesToStartOnAStaleRouteSignature only ever drove the
// signature-mismatch refusal, so removing this second os.Exit(1) would leave
// every test in this package green while the worker started and served with
// that plugin's routes silently absent.
type routeErrFixturePlugin struct{}

func (routeErrFixturePlugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{Name: routeErrFixtureName}
}
func (routeErrFixturePlugin) Init(ctx context.Context, env *plugin.Environment) error {
	return nil
}
func (routeErrFixturePlugin) RegisterRoutes(mux plugin.Router) error {
	return fmt.Errorf("%s: deliberate RegisterRoutes failure, injected for TestMainRefusesToStartWhenRegisterRoutesFails", routeErrFixtureName)
}

// Registration is gated on an env var so neither fixture is present in an
// ordinary test run of this package (it would otherwise make EVERY test that
// calls plugin.Discover() -- including TestPluginAuthExemptPatternsAreRealRoutes
// above -- see a plugin that does not really exist).
func init() {
	if os.Getenv(envBootRefusalInjectStale) == "1" {
		plugin.Register(plugin.PluginInfo{Name: staleRouteFixtureName},
			func() plugin.Plugin { return staleRouteSignatureFixturePlugin{} })
	}
	if os.Getenv(envBootRefusalInjectRouteErr) == "1" {
		plugin.Register(plugin.PluginInfo{Name: routeErrFixtureName},
			func() plugin.Plugin { return routeErrFixturePlugin{} })
	}
}

// TestMain intercepts before go test's own flag parsing / test selection, so
// the subprocess this test launches can hand main() a plain worker argv
// (--driver=..., --db=..., ...) with no `-test.*` flags in the way.
func TestMain(m *testing.M) {
	if os.Getenv(envBootRefusalSubprocess) == "1" {
		main() // expected to os.Exit(1) via checkPluginRouteSignatures's refusal
		// Reachable only if main() returned instead of exiting -- e.g. it
		// booted and started serving. Exit with a distinct code so the
		// parent can tell that apart from a real refusal (exit 1) and a
		// clean exit (0).
		os.Exit(42)
	}
	os.Exit(m.Run())
}

func TestMainRefusesToStartOnAStaleRouteSignature(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the real worker against a live database")
	}
	admin := os.Getenv("CLEAT_TEST_POSTGRES")
	if admin == "" {
		admin = os.Getenv("CLEAT_TEST_DB")
	}
	if admin == "" {
		t.Skip("CLEAT_TEST_POSTGRES/CLEAT_TEST_DB not set, skipping")
	}

	adb, err := sql.Open("postgres", admin)
	if err != nil {
		t.Fatal(err)
	}
	defer adb.Close()
	if err := adb.Ping(); err != nil {
		t.Fatalf("%s is set but unreachable: %v", admin, err)
	}
	name := fmt.Sprintf("cleat_2277_boot_refusal_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := adb.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, err := sql.Open("postgres", admin)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = a.Exec(`DROP DATABASE IF EXISTS ` + name)
	})
	ownerDSN := replaceDBName(t, admin, name)

	// Migration is a deploy step (cleat#2117): a plain boot does not migrate
	// on start, so the schema has to be applied first or every subprocess
	// below refuses on "the database has no schema_migrations table" before
	// ever reaching checkPluginRouteSignatures.
	if code, out := runBootSubprocess(t, []string{"--driver=postgres", "--db=" + ownerDSN, "--migrate-only"}, ""); code != 0 {
		t.Fatalf("--migrate-only exited %d:\n%s", code, out)
	}

	owner, err := sql.Open("postgres", ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	// --db as the superuser admin connection refuses BEFORE reaching
	// checkPluginRouteSignatures at all ("this connection is not subject to
	// row-level security"), which is cleat#2258's check firing on a
	// completely different problem -- so this needs the app role, exactly as
	// a_require_host_match_boots_as_the_app_role_test.go's real deployment
	// does, not the migrate/owner connection.
	dsn := pgAppRoleDSN(t, owner, ownerDSN)

	// KNOWN-POSITIVE FIRST: the same subprocess, same fresh database, with
	// the fixture plugin NOT registered, must boot cleanly (killed at the
	// deadline while serving, not refused) -- otherwise a failure below
	// could just as well be this test's own setup being broken, not the
	// boot-refusal check actually firing.
	t.Run("control: no stale plugin registered, worker starts serving", func(t *testing.T) {
		code, out := runBootRefusalSubprocess(t, dsn, ownerDSN, "")
		if code != -1 {
			t.Fatalf("worker with no stale plugin registered exited %d instead of being killed while "+
				"serving -- something else refused to start, which means the assertion below would not "+
				"be testing what it claims to:\n%s", code, out)
		}
	})

	t.Run("worker refuses to start with the stale-signature plugin registered", func(t *testing.T) {
		code, out := runBootRefusalSubprocess(t, dsn, ownerDSN, envBootRefusalInjectStale)
		if code == -1 {
			t.Fatal("worker with the stale-signature plugin registered started serving instead of " +
				"refusing -- removing the os.Exit(1) after checkPluginRouteSignatures in main.go would " +
				"produce exactly this")
		}
		if code == 42 {
			t.Fatalf("main() returned instead of exiting -- checkPluginRouteSignatures's error must reach "+
				"an os.Exit(1), not merely be logged:\n%s", out)
		}
		if code == 0 {
			t.Fatalf("worker exited 0 with the stale-signature plugin registered, want a fatal refusal (non-zero):\n%s", out)
		}
		if !strings.Contains(out, staleRouteFixtureName) {
			t.Errorf("refusal message does not name the offending plugin %q:\n%s", staleRouteFixtureName, out)
		}
		if !strings.Contains(out, "RegisterRoutes") {
			t.Errorf("refusal message does not mention RegisterRoutes, so a reader would not know what to fix:\n%s", out)
		}
	})

	// cleat-review's should-fix: this is main.go's OTHER refusal on this
	// path -- the plugin's RegisterRoutes signature is current (so
	// checkPluginRouteSignatures above has nothing to flag), but the CALL
	// itself returns an error. That is a separate os.Exit(1) a few lines
	// below the one the subtest above pins; nothing else in this package
	// drives it, so removing it silently kept ./cmd/cleat-worker green.
	t.Run("worker refuses to start when a plugin's RegisterRoutes returns an error", func(t *testing.T) {
		code, out := runBootRefusalSubprocess(t, dsn, ownerDSN, envBootRefusalInjectRouteErr)
		if code == -1 {
			t.Fatal("worker with the route-err plugin registered started serving instead of refusing -- " +
				"removing the os.Exit(1) after a failed RegisterRoutes in main.go would produce exactly this")
		}
		if code == 42 {
			t.Fatalf("main() returned instead of exiting -- a failed RegisterRoutes must reach an "+
				"os.Exit(1), not merely be logged:\n%s", out)
		}
		if code == 0 {
			t.Fatalf("worker exited 0 with the route-err plugin registered, want a fatal refusal (non-zero):\n%s", out)
		}
		if !strings.Contains(out, routeErrFixtureName) {
			t.Errorf("refusal message does not name the offending plugin %q:\n%s", routeErrFixtureName, out)
		}
		if !strings.Contains(out, "RegisterRoutes") {
			t.Errorf("refusal message does not mention RegisterRoutes, so a reader would not know what to fix:\n%s", out)
		}
	})
}

// runBootRefusalSubprocess re-executes this test binary as a worker (via
// TestMain's interception) against dsn (the app-role connection --db serves
// requests through) and ownerDSN (the schema-owning connection --migrate-db
// uses for its startup catch-up check). inject is "" for no fixture plugin,
// or one of envBootRefusalInjectStale/envBootRefusalInjectRouteErr to
// register the matching fixture. Returns -1 if the worker was still running
// (and was killed) when the deadline elapsed -- i.e. it started serving
// rather than exiting either way.
func runBootRefusalSubprocess(t *testing.T, dsn, ownerDSN, inject string) (code int, output string) {
	t.Helper()
	args := []string{
		"--driver=postgres",
		"--db=" + dsn,
		"--migrate-db=" + ownerDSN,
		fmt.Sprintf("--api-addr=127.0.0.1:%d", freePort(t)),
	}
	return runBootSubprocess(t, args, inject)
}

// runBootSubprocess re-executes this test binary as the worker (via
// TestMain's interception) with args. inject is "" for no fixture plugin, or
// one of envBootRefusalInjectStale/envBootRefusalInjectRouteErr to register
// the matching fixture. Returns -1 if the process was still running (and was
// killed) when the deadline elapsed.
func runBootSubprocess(t *testing.T, args []string, inject string) (code int, output string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := os.Environ()
	env = append(env, envBootRefusalSubprocess+"=1")
	if inject != "" {
		env = append(env, inject+"=1")
	}

	const bound = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Env = env
	out, runErr := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return -1, string(out)
	}
	if runErr == nil {
		return 0, string(out)
	}
	if ee, ok := runErr.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("run %s %v: %v", self, args, runErr)
	return -2, ""
}

// replaceDBName swaps the database name in a postgres DSN/URL for name,
// however it is spelled (URL form or libpq keyword form).
func replaceDBName(t *testing.T, dsn, name string) string {
	t.Helper()
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parsing DSN: %v", err)
		}
		u.Path = "/" + name
		return u.String()
	}
	// libpq keyword form: replace or append dbname=.
	fields := strings.Fields(dsn)
	found := false
	for i, f := range fields {
		if strings.HasPrefix(f, "dbname=") {
			fields[i] = "dbname=" + name
			found = true
		}
	}
	if !found {
		fields = append(fields, "dbname="+name)
	}
	return strings.Join(fields, " ")
}
