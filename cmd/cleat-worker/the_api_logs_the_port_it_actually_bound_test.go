package main

// cleat#3136. The HTTP API logged the *configured* --api-addr, so a worker
// asked for an ephemeral port logged `addr=:0` -- an address nothing can
// connect to, while the kernel had already chosen the real port and the only
// record of it was a socket nobody read. A caller that wants the kernel to pick
// (the quick-start tests, cleat#3131) could not read the port back, so it had
// to pre-choose a number before the bind, which is the race those tests had.
//
// The fix binds explicitly (net.Listen) and logs ln.Addr(), which is the
// address the socket ACTUALLY bound.
//
// This test pins BOTH directions, and it is deliberately not just the ":0"
// case, because "the log names a bound address" is satisfied by a mutant that
// logs any non-zero number:
//
//  1. a FIXED port is reported as exactly that port -- this is what ties the
//     logged value to the configured one, so it cannot be arithmetic on it;
//  2. an EPHEMERAL port (":0") is reported as something OTHER than ":0", with
//     a non-zero port that a real client can actually connect to.
//
// Before the fix, (2) logged the literal ":0"; a mutant that logged a wrong
// non-zero number fails (1), and one that logged a plausible-but-wrong port
// fails (2)'s connectability check. Run against the pre-fix tree, the ":0"
// leg is the one that goes red with `addr="127.0.0.1:0"`.
//
// Postgres only: the property is the HTTP listener's logging in main.go, which
// does not vary by database -- the same single-dialect scope this package's
// other boot-the-real-binary tests use for a dialect-independent property.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTheHTTPAPILogsThePortItActuallyBound(t *testing.T) {
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

	ownerDSN, owner := deployScratch(t, c)
	if code, out := runWorker(t, bin, nil, "--driver=postgres", "--db="+ownerDSN, "--migrate-only"); code != 0 {
		t.Fatalf("--migrate-only exited %d:\n%s", code, out)
	}
	dsn := pgAppRoleDSN(t, owner, ownerDSN)

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	env := []string{"CLEAT_SECRET_MASTER_KEY=" + base64.StdEncoding.EncodeToString(key)}

	// Direction 1: a FIXED port is reported as exactly that port.
	//
	// freePort releases the port before the worker binds it, so there is a
	// small window for another process to take it; this package's other
	// boot tests live with the same window, and a takeover fails loudly here
	// (the worker logs a listen error and never reports) rather than silently.
	fixed := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	gotFixed, stopFixed := startAPIWorker(t, bin, env, dsn, ownerDSN, fixed)
	if gotFixed != fixed {
		stopFixed()
		t.Fatalf("--api-addr %s logged addr=%q, want %q -- the log must name the address the socket bound", fixed, gotFixed, fixed)
	}
	if code := getStatus(t, "http://"+gotFixed+"/healthz"); code != http.StatusOK {
		stopFixed()
		t.Fatalf("GET http://%s/healthz = %d, want 200: the reported fixed address did not serve", gotFixed, code)
	}
	stopFixed()

	// Direction 2: an EPHEMERAL port is reported as the port the kernel chose.
	gotEphemeral, stopEphemeral := startAPIWorker(t, bin, env, dsn, ownerDSN, "127.0.0.1:0")
	defer stopEphemeral()
	if gotEphemeral == "127.0.0.1:0" || strings.HasSuffix(gotEphemeral, ":0") {
		t.Fatalf("--api-addr 127.0.0.1:0 logged addr=%q: that is the CONFIGURED value, not the bound one -- "+
			"no caller can connect to an ephemeral-port worker", gotEphemeral)
	}
	_, portStr, err := net.SplitHostPort(gotEphemeral)
	if err != nil {
		t.Fatalf("logged addr=%q is not host:port: %v", gotEphemeral, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port == 0 {
		t.Fatalf("logged addr=%q has port %q, want a non-zero kernel-assigned port", gotEphemeral, portStr)
	}
	if code := getStatus(t, "http://"+gotEphemeral+"/healthz"); code != http.StatusOK {
		t.Fatalf("GET http://%s/healthz = %d, want 200: the logged ephemeral address is not connectable", gotEphemeral, code)
	}
}

// startAPIWorker boots the real binary on apiAddr and returns the address its
// "HTTP API listening" line reports, plus a stop function that kills it. It
// fails the test if the worker exits before reporting one, which is what a
// pre-fix bind failure or a refused boot looks like.
func startAPIWorker(t *testing.T, bin string, env []string, dsn, ownerDSN, apiAddr string) (string, func()) {
	t.Helper()
	cmd := exec.Command(bin,
		"--driver=postgres", "--db="+dsn, "--migrate-db="+ownerDSN, "--api-addr="+apiAddr)
	cmd.Env = append(os.Environ(), env...)
	var out syncBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	stop := func() {
		_ = cmd.Process.Kill()
		<-exited
	}

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			t.Fatalf("worker exited before reporting a bound API address:\n%s", out.String())
		default:
		}
		if addr := boundAPIAddr(out.String()); addr != "" {
			return addr, stop
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	t.Fatalf(`no "HTTP API listening" line within 90s:`+"\n%s", out.String())
	return "", stop
}

// boundAPIAddr returns the addr field of the first "HTTP API listening" JSON
// log line in s, or "" if no such line has been written yet. The worker logs
// JSON on stderr (slog.NewJSONHandler), so a line that does not parse, or whose
// msg is something else, is skipped rather than guessed at.
func boundAPIAddr(s string) string {
	for _, line := range strings.Split(s, "\n") {
		var rec struct {
			Msg  string `json:"msg"`
			Addr string `json:"addr"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "HTTP API listening" {
			return rec.Addr
		}
	}
	return ""
}

// getStatus GETs url and returns the status code, or 0 if the request failed.
func getStatus(t *testing.T, url string) int {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
