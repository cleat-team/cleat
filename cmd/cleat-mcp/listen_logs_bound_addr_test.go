package main

import (
	"bytes"
	"log"
	"net"
	"regexp"
	"testing"
)

// TestListenAndLogBoundAddrLogsWhatItActuallyBound is the regression test for
// cleat#3155: cleat-mcp started on an ephemeral `:0` port must log the REAL
// port a caller can connect to, not the configured ":0" -- the same gap
// cleat#3136/#3137 closed for cleat-worker. Asserting on the log line is the
// point: `listenAndLogBoundAddr` returning a working listener is not enough,
// because the old defect's listener worked fine too -- ListenAndServe binds
// correctly, it just never told anyone which port it chose.
func TestListenAndLogBoundAddrLogsWhatItActuallyBound(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	ln := listenAndLogBoundAddr("127.0.0.1:0", "http://example.invalid")
	t.Cleanup(func() { ln.Close() })

	gotAddr := ln.Addr().(*net.TCPAddr)
	if gotAddr.Port == 0 {
		t.Fatalf("listener's own Addr() reports port 0 -- the kernel did not allocate one")
	}

	logged := buf.String()
	re := regexp.MustCompile(`serving MCP on (\S+)`)
	m := re.FindStringSubmatch(logged)
	if m == nil {
		t.Fatalf("log line does not contain %q: %s", "serving MCP on <addr>", logged)
	}
	loggedAddr, err := net.ResolveTCPAddr("tcp", m[1])
	if err != nil {
		t.Fatalf("logged address %q does not parse: %v", m[1], err)
	}

	// THE ASSERTION THE OLD DEFECT WOULD HAVE FAILED: it logged the
	// configured "127.0.0.1:0" verbatim, so this would read 0, not the real
	// port the listener above is actually reachable on.
	if loggedAddr.Port != gotAddr.Port {
		t.Errorf("logged port %d, want the bound port %d (logged line: %q)", loggedAddr.Port, gotAddr.Port, logged)
	}
	if loggedAddr.Port == 0 {
		t.Errorf("logged port is 0 -- the configured address was logged instead of the bound one")
	}
}

// TestListenAndLogBoundAddrFixedAddrStillMatches covers the ordinary case,
// where the configured address already names a specific port: the bound and
// logged addresses must still agree, so this isn't only exercised through the
// ephemeral-port path.
func TestListenAndLogBoundAddrFixedAddrStillMatches(t *testing.T) {
	// Pick a free port the same way net.Listen itself would, then reuse that
	// exact number as the FIXED address under test.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	fixedAddr := probe.Addr().String()
	probe.Close()

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	ln := listenAndLogBoundAddr(fixedAddr, "http://example.invalid")
	t.Cleanup(func() { ln.Close() })

	if ln.Addr().String() != fixedAddr {
		t.Fatalf("bound %s, want the fixed %s", ln.Addr(), fixedAddr)
	}
	if got := buf.String(); !regexp.MustCompile(regexp.QuoteMeta(fixedAddr)).MatchString(got) {
		t.Errorf("log line does not contain the fixed address %q: %s", fixedAddr, got)
	}
}
