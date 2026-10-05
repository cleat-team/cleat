package main

// cleat#3139. The --pprof-addr and --internal-addr listeners logged the
// *configured* address, so `":0"` logged `addr=:0` -- an address no caller can
// connect to -- while the kernel had already chosen the real port and the only
// record of it was a socket nobody read. Same defect cleat#3136 fixed for
// the --api-addr listener; this pins the same property for the two that bind
// inline in main.go beside it.
//
// Both directions are pinned for EACH listener, for the reason the API-listener
// test (cleat#3136, PR #3137) gives: "logs a bound address" is also satisfied
// by a mutant that logs any non-zero number, so the FIXED-port case is what ties
// the logged value to the configured one.
//
//  1. a FIXED port is reported as exactly that port;
//  2. ":0" is reported as something OTHER than ":0", with a non-zero port a real
//     client can connect to -- pprof answers 200 on /debug/pprof/, and the
//     internal-holds route answers 401 to an unauthenticated request, which
//     proves the handler ran rather than merely that something accepted the TCP
//     connection.
//
// Run against the pre-fix tree, each listener's ":0" leg is the one that goes
// red with `addr="127.0.0.1:0"`.
//
// Postgres only: the property is the listeners' logging in main.go, which does
// not vary by database -- the same single-dialect scope this package's other
// boot-the-real-binary tests use for a dialect-independent property.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

func TestThePprofAndInternalHoldsListenersLogTheAddressTheyBound(t *testing.T) {
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
	// CLEAT_INTERNAL_AUTH_KEY is required before --internal-addr will start: with
	// it unset the worker logs an error and exits 1, which would turn every
	// assertion below into a boot failure rather than a test of a log line.
	secret := base64.StdEncoding.EncodeToString(key)
	env := []string{
		"CLEAT_SECRET_MASTER_KEY=" + secret,
		"CLEAT_INTERNAL_AUTH_KEY=" + secret,
	}

	// Direction 1: a FIXED port is reported as exactly that port.
	//
	// freePort releases each port before the worker binds it, so there is a small
	// window for another process to take it; this package's other boot tests live
	// with the same window, and a takeover fails loudly here (the worker logs a
	// listen error and never reports) rather than silently.
	fixedPprof := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	fixedInternal := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	gotPprof, gotInternal, stopFixed := startListenerWorker(t, bin, env, dsn, ownerDSN, fixedPprof, fixedInternal)
	if gotPprof != fixedPprof {
		stopFixed()
		t.Fatalf("--pprof-addr %s logged addr=%q, want %q -- the log must name the address the socket bound", fixedPprof, gotPprof, fixedPprof)
	}
	if gotInternal != fixedInternal {
		stopFixed()
		t.Fatalf("--internal-addr %s logged addr=%q, want %q -- the log must name the address the socket bound", fixedInternal, gotInternal, fixedInternal)
	}
	if code := getStatus(t, pprofProbeURL(gotPprof)); code != http.StatusOK {
		stopFixed()
		t.Fatalf("GET %s = %d, want 200: the reported fixed pprof address did not serve", pprofProbeURL(gotPprof), code)
	}
	if code := getStatus(t, internalHoldsProbeURL(gotInternal)); code != http.StatusUnauthorized {
		stopFixed()
		t.Fatalf("GET %s = %d, want 401: the reported fixed internal-holds address did not serve", internalHoldsProbeURL(gotInternal), code)
	}
	stopFixed()

	// Direction 2: an EPHEMERAL port is reported as the port the kernel chose.
	gotPprof, gotInternal, stopEphemeral := startListenerWorker(t, bin, env, dsn, ownerDSN, "127.0.0.1:0", "127.0.0.1:0")
	defer stopEphemeral()
	for _, l := range []struct {
		flag string
		got  string
	}{
		{"--pprof-addr", gotPprof},
		{"--internal-addr", gotInternal},
	} {
		if l.got == "127.0.0.1:0" || strings.HasSuffix(l.got, ":0") {
			stopEphemeral()
			t.Fatalf("%s 127.0.0.1:0 logged addr=%q: that is the CONFIGURED value, not the bound one -- "+
				"no caller can connect to an ephemeral-port listener", l.flag, l.got)
		}
		_, portStr, err := net.SplitHostPort(l.got)
		if err != nil {
			stopEphemeral()
			t.Fatalf("%s logged addr=%q is not host:port: %v", l.flag, l.got, err)
		}
		if port, err := strconv.Atoi(portStr); err != nil || port == 0 {
			stopEphemeral()
			t.Fatalf("%s logged addr=%q has port %q, want a non-zero kernel-assigned port", l.flag, l.got, portStr)
		}
	}
	if code := getStatus(t, pprofProbeURL(gotPprof)); code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: the logged ephemeral pprof address is not connectable", pprofProbeURL(gotPprof), code)
	}
	if code := getStatus(t, internalHoldsProbeURL(gotInternal)); code != http.StatusUnauthorized {
		t.Fatalf("GET %s = %d, want 401: the logged ephemeral internal-holds address is not connectable", internalHoldsProbeURL(gotInternal), code)
	}
}

// startListenerWorker boots the real binary with both opt-in listeners and
// returns the addresses their "pprof listening" and "internal holds listening"
// lines report, plus a stop function that kills it. It fails the test if the
// worker exits before reporting BOTH, which is what a pre-fix bind failure or a
// refused boot looks like.
func startListenerWorker(t *testing.T, bin string, env []string, dsn, ownerDSN, pprofAddr, internalAddr string) (string, string, func()) {
	t.Helper()
	cmd := exec.Command(bin,
		"--driver=postgres", "--db="+dsn, "--migrate-db="+ownerDSN,
		"--pprof-addr="+pprofAddr, "--internal-addr="+internalAddr)
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
			t.Fatalf("worker exited before reporting both bound listener addresses:\n%s", out.String())
		default:
		}
		p := boundAddrFor(out.String(), "pprof listening")
		i := boundAddrFor(out.String(), "internal holds listening")
		if p != "" && i != "" {
			return p, i, stop
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	t.Fatalf(`no "pprof listening" and "internal holds listening" lines within 90s:`+"\n%s", out.String())
	return "", "", stop
}

// boundAddrFor returns the addr field of the first log line in s whose msg is
// exactly want, or "" if no such line has been written yet. The worker logs JSON
// on stderr (slog.NewJSONHandler), so a line that does not parse, or whose msg
// is something else, is skipped rather than guessed at.
func boundAddrFor(s, want string) string {
	for _, line := range strings.Split(s, "\n") {
		var rec struct {
			Msg  string `json:"msg"`
			Addr string `json:"addr"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == want {
			return rec.Addr
		}
	}
	return ""
}

// pprofProbeURL is a route DefaultServeMux answers with 200 once the pprof
// listener is bound.
func pprofProbeURL(addr string) string {
	return "http://" + addr + "/debug/pprof/"
}

// internalHoldsProbeURL builds a URL that reaches the internal-holds handler
// WITHOUT a credential. The handler checks auth before reading the path, so a
// 401 proves the listener accepted the connection and ran the handler -- which a
// bare dial would not distinguish from any other socket on that port.
func internalHoldsProbeURL(addr string) string {
	return "http://" + addr + "/internal/holds/probe?generation=1"
}
