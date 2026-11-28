package main

// cleat#3654. oauthprovider.Middleware answered 401 {"error":"invalid session"} to a session-shaped Bearer
// (64 lowercase hex) on the routes core auth exempts by pattern, POST /ingest/{source_id} and
// POST /slack/interactive, while the same request with no Authorization header, or with a Bearer of any
// other shape, reached the handler. core auth waives those routes because their callers cannot hold a cleat
// credential, so a header on them is not the plugin's to judge.
//
// Run through the REAL binary under the default --require-auth, for the reason
// an_anonymous_oauth_login_is_not_401_test.go gives: the chain is built inside main(). The unit tests next
// to the change (auth, plugins/oauthprovider) pin the flag; this one pins that the worker wires it.
//
// Single dialect (Postgres): HTTP middleware wiring does not vary by database.
//
// THE CONTROLS: a request with no credential reaches each handler (the exemption applies), and the same
// bearer on an AUTHENTICATED route is still refused by core auth, with core auth's own body. The test
// compares the bearer's answer to the credential-less answer of the same route, so it asserts that the
// header changed nothing rather than hardcoding what each handler happens to say.

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func TestASessionShapedBearerOnAnExemptRouteIsNotRefusedByTheOAuthPlugin(t *testing.T) {
	c := deployDialect{"postgres", "CLEAT_TEST_POSTGRES", "postgres"}
	if c.admin() == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "cleat-worker")
	build := exec.Command("go", "build", "-o", bin, "./cmd/cleat-worker")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	ownerDSN, owner := deployScratch(t, c)
	if code, out := runWorker(t, bin, nil, "--driver=postgres", "--db="+ownerDSN, "--migrate-only"); code != 0 {
		t.Fatalf("--migrate-only exited %d:\n%s", code, out)
	}
	dsn := pgAppRoleDSN(t, owner, ownerDSN)

	cmd := exec.Command(bin, "--driver=postgres", "--db="+dsn, "--migrate-db="+ownerDSN, "--api-addr=127.0.0.1:0")
	cmd.Env = os.Environ()
	var out syncBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	defer func() { _ = cmd.Process.Kill(); <-exited }()

	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var base string
	healthy := false
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline) && !healthy; {
		select {
		case <-exited:
			t.Fatalf("worker exited:\n%s", out.String())
		default:
		}
		if a := boundAPIAddr(out.String()); a != "" {
			base = "http://" + a
		}
		if base != "" {
			if resp, err := client.Get(base + "/healthz"); err == nil {
				resp.Body.Close()
				healthy = resp.StatusCode == http.StatusOK
			}
		}
		if !healthy {
			time.Sleep(300 * time.Millisecond)
		}
	}
	if !healthy {
		t.Fatalf("not healthy:\n%s", out.String())
	}

	hex64 := strings.Repeat("ab", 32)
	type answer struct {
		code int
		body string
	}
	do := func(method, path, bearer string) answer {
		t.Helper()
		req, err := http.NewRequest(method, base+path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return answer{resp.StatusCode, strings.TrimSpace(string(b))}
	}

	for _, rt := range []struct{ method, path string }{
		{"POST", "/ingest/probe-source"},
		{"POST", "/slack/interactive"},
	} {
		anon := do(rt.method, rt.path, "")
		if strings.Contains(anon.body, "invalid session") || strings.Contains(anon.body, "authentication required") {
			t.Fatalf("UNMEASURED: %s %s with no credential = %d %q, so the exemption did not apply and the comparison below says nothing",
				rt.method, rt.path, anon.code, anon.body)
		}
		if got := do(rt.method, rt.path, hex64); got != anon {
			t.Errorf("%s %s with a session-shaped bearer = %d %q; with no credential = %d %q. "+
				"The header on an exempt route must change nothing", rt.method, rt.path, got.code, got.body, anon.code, anon.body)
		}
	}

	// The same bearer on an authenticated route is still refused, by CORE auth, as an API key it is not.
	if got := do("GET", "/api/workflows", hex64); got.code != http.StatusUnauthorized || !strings.Contains(got.body, "invalid or revoked API key") {
		t.Errorf("GET /api/workflows with a session-shaped bearer = %d %q; want 401 from core auth (invalid or revoked API key)", got.code, got.body)
	}
}
