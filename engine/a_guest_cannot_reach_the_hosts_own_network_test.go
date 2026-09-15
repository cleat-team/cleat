package engine

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cleat#1565. The floor: addresses a guest-initiated fetch may never reach,
// whatever a tenant's allowlist says.
func TestTheEgressFloorRefusesTheHostsOwnNetwork(t *testing.T) {
	// Named rather than generated from deniedRanges, deliberately. A table
	// derived from the thing under test agrees with it by construction and
	// would pass with every entry deleted. These are the addresses that must
	// be refused; if a range is removed, one of these goes green and the test
	// goes red.
	for _, tc := range []struct{ addr, why string }{
		{"169.254.169.254", "cloud instance metadata -- the reason this issue is ranked first"},
		{"127.0.0.1", "the worker's own API and admin surface"},
		{"::1", "loopback, IPv6"},
		{"10.1.2.3", "RFC1918"},
		{"172.16.0.1", "RFC1918, low end"},
		{"172.31.255.254", "RFC1918, high end -- 172.16/12 is easy to write as 172.16/16"},
		{"192.168.1.1", "RFC1918"},
		{"100.64.0.1", "carrier-grade NAT"},
		{"fd00::1", "IPv6 unique local"},
		{"fe80::1", "link-local, IPv6"},
		{"0.0.0.0", "unspecified reaches loopback on several stacks"},
		{"::ffff:10.0.0.1", "IPv4-mapped IPv6: the same private address wearing a v6 costume"},
		{"::ffff:169.254.169.254", "metadata, mapped -- the bypass if Unmap() were dropped"},
		{"224.0.0.1", "multicast"},
		{"192.0.2.1", "TEST-NET-1: unroutable, so an answer means something local replied"},
		{"198.18.0.1", "benchmarking range"},
		{"240.0.0.1", "reserved"},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			ip, err := netip.ParseAddr(tc.addr)
			if err != nil {
				t.Fatalf("bad test address: %v", err)
			}
			if err := (&EgressGuard{}).checkAddr("probe.example", ip); err == nil {
				t.Errorf("%s is allowed; it must not be (%s)", tc.addr, tc.why)
			}
		})
	}
}

// The other direction. A floor that refuses everything is safe and useless, and
// would pass the test above.
func TestTheEgressFloorAllowsOrdinaryPublicAddresses(t *testing.T) {
	for _, addr := range []string{
		"93.184.216.34", // example.com
		"1.1.1.1",
		"8.8.8.8",
		"172.15.255.255", // just BELOW 172.16/12
		"172.32.0.1",     // just ABOVE 172.16/12
		"100.63.255.255", // just below 100.64/10
		"2606:2800:220:1:248:1893:25c8:1946",
	} {
		t.Run(addr, func(t *testing.T) {
			ip, err := netip.ParseAddr(addr)
			if err != nil {
				t.Fatalf("bad test address: %v", err)
			}
			if err := (&EgressGuard{}).checkAddr("probe.example", ip); err != nil {
				t.Errorf("%s is refused and should not be: %v", addr, err)
			}
		})
	}
}

// Every entry must carry a reason. A range with no stated why is one nobody can
// safely remove later, which is how a floor becomes folklore.
func TestEveryDeniedRangeSaysWhyItIsThere(t *testing.T) {
	if len(deniedRanges) == 0 {
		t.Fatal("deniedRanges is empty; the floor enforces nothing")
	}
	seen := map[string]bool{}
	for _, d := range deniedRanges {
		if strings.TrimSpace(d.why) == "" {
			t.Errorf("%s has no reason recorded", d.prefix)
		}
		if seen[d.prefix.String()] {
			t.Errorf("%s appears twice", d.prefix)
		}
		seen[d.prefix.String()] = true
	}
}

// The two properties the DIALER exists for. A URL check would pass the floor
// tests above and still leave both of these open.
func TestTheGuardChecksEveryAnswerAndDialsTheOneItChecked(t *testing.T) {
	ctx := context.Background()

	t.Run("a name answering both public and private is refused", func(t *testing.T) {
		// Rebinding in progress. Dialling the public answer and proceeding
		// would make the policy depend on resolver ordering.
		// Explicitly permitted, so what this subtest measures is the FLOOR
		// overriding an allowlist entry -- not the allowlist refusing first.
		g := &EgressGuard{
			AllowHost: NewHostAllowlist("rebind.example").AllowHostFunc(),
			Dial: func(context.Context, string, string) (net.Conn, error) {
				t.Fatal("dialled despite a private answer among the results")
				return nil, nil
			},
		}
		g.lookup = func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{
				netip.MustParseAddr("93.184.216.34"),
				netip.MustParseAddr("169.254.169.254"),
			}, nil
		}
		_, err := g.DialContext(ctx, "tcp", "rebind.example:80")
		var denied *EgressDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("got %v, want an EgressDeniedError", err)
		}
		if !strings.Contains(denied.Error(), "169.254.169.254") {
			t.Errorf("the refusal does not name the offending address: %v", denied)
		}
	})

	t.Run("the dial target is the checked IP, not the hostname", func(t *testing.T) {
		// This is what closes the rebinding window: handing the NAME back to
		// the dialer lets it resolve a second time, and the second answer is
		// the attacker's.
		var dialled string
		g := &EgressGuard{
			AllowHost: NewHostAllowlist("public.example").AllowHostFunc(),
			Dial: func(_ context.Context, _, address string) (net.Conn, error) {
				dialled = address
				return nil, errors.New("stop here; the address is what is under test")
			},
		}
		g.lookup = func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		}
		_, _ = g.DialContext(ctx, "tcp", "public.example:443")
		if dialled != "93.184.216.34:443" {
			t.Errorf("dialled %q, want the checked address literal 93.184.216.34:443. "+
				"Dialling the name would resolve again and defeat the check", dialled)
		}
	})

	t.Run("the per-tenant allowlist narrows and is consulted before resolving", func(t *testing.T) {
		resolved := false
		g := &EgressGuard{
			AllowHost: func(_ context.Context, host string) (bool, error) { return host == "allowed.example", nil },
			Dial: func(context.Context, string, string) (net.Conn, error) {
				t.Fatal("dialled a host that is not on the allowlist")
				return nil, nil
			},
		}
		g.lookup = func(context.Context, string) ([]netip.Addr, error) {
			resolved = true
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		}
		_, err := g.DialContext(ctx, "tcp", "denied.example:443")
		var denied *EgressDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("got %v, want an EgressDeniedError", err)
		}
		if resolved {
			t.Error("a host off the allowlist was resolved; refusing before the lookup " +
				"keeps a denied name from being a DNS oracle")
		}
	})
}

func TestOnlyHTTPSchemesAreAllowed(t *testing.T) {
	for _, s := range []string{"http", "https", "HTTP", "HTTPS"} {
		if err := CheckScheme(s); err != nil {
			t.Errorf("scheme %q refused: %v", s, err)
		}
	}
	for _, s := range []string{"file", "gopher", "ftp", "data", ""} {
		if err := CheckScheme(s); err == nil {
			t.Errorf("scheme %q allowed; the list is an allowlist for a reason", s)
		}
	}
}

// BOTH implementations must be guarded, which is the warning cleat#1565 leads
// with: "There is a second implementation with the same shape at
// cleat/embedded/runner.go:417. Whatever is decided has to cover both, or the
// embedded path becomes the way around the worker's policy."
//
// The worker's path has a behavioural test (cmd/cleat-worker). This one is a
// source check, and it is weaker on purpose rather than by neglect: building an
// embedded runner here would drag the wasm host into engine's tests to assert
// one field. The thing it can still catch is the failure that actually happens
// -- a new HTTP client added to that file without a guarded Transport.
func TestBothHTTPFetchImplementationsUseTheEgressGuard(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "cleat", "embedded", "runner.go"))
	if err != nil {
		t.Fatalf("read the embedded runner: %v", err)
	}
	var code []string
	for _, l := range strings.Split(string(src), "\n") {
		if i := strings.Index(l, "//"); i >= 0 {
			l = l[:i]
		}
		code = append(code, l)
	}
	body := strings.Join(code, "\n")

	clients := strings.Count(body, "http.Client{")
	if clients == 0 {
		t.Fatal("no http.Client in the embedded runner at all -- this check is looking at " +
			"the wrong file, which reads identically to a clean result")
	}
	// Counted by ROLE rather than by one spelling. The first version matched
	// the literal `EgressGuard{}).DialContext`, and went red the moment the
	// guard was built into a variable instead of inlined -- a false positive
	// on a file that was correct, which is the worst kind of guard to have.
	transports := strings.Count(body, "Transport:")
	if transports < clients {
		t.Errorf("the embedded runner builds %d http.Client(s) and gives %d of them a "+
			"Transport. A client on the default transport does not go through the egress "+
			"guard (cleat#1565)", clients, transports)
	}
	if !strings.Contains(body, "EgressGuard") {
		t.Error("the embedded runner names no EgressGuard, so whatever its Transport is, " +
			"it is not this policy (cleat#1565)")
	}
}

// EgressGuard.AllowLoopback is documented as test-only. That is a promise, and
// a promise in a doc comment is the thing this repository keeps paying for, so
// it is enforced here instead.
//
// git ls-files rather than a filesystem walk: a walk descends into
// .claude/worktrees/, a whole second copy of the repo, and would attribute a
// scratch checkout's code to this one. It also covers every module, which
// `go vet ./...` does not -- cleat/ is a separate module and the second
// http.fetch implementation lives there.
func TestNoProductionCodeAllowsLoopbackEgress(t *testing.T) {
	root := repoRootForEgressScan(t)
	// -C root, because git ls-files run from a subdirectory lists only that
	// subdirectory and prints paths relative to it. Without this the scan read
	// nothing and the floor below caught it -- which is the only reason this
	// comment exists rather than a silent pass.
	out, err := exec.Command("git", "-C", root, "ls-files", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Fields(string(out))
	// A scan that found nothing reports a clean tree, which reads identically
	// to success. The floor belongs on the success path.
	if len(files) < 100 {
		t.Fatalf("git ls-files matched %d Go files; the scan did not see the repo", len(files))
	}

	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			continue // a file listed but absent is not this test's business
		}
		checked++
		// Look for the field being SET, not merely named: this file's own
		// declaration (`AllowLoopback bool`) and the prose around it mention
		// it and are not the hazard. Comments are stripped first, because a
		// search cannot tell a line of code from a sentence about one.
		if setsAllowLoopback(string(body)) {
			t.Errorf("%s is not a test and mentions AllowLoopback. It opens loopback to "+
				"guest-initiated fetches -- the worker's own API and admin surface "+
				"(cleat#1565). If a production path genuinely needs it, it needs a "+
				"different mechanism and a decision, not this field.", f)
		}
	}
	if checked == 0 {
		t.Fatal("no non-test Go files were read; the scan measured nothing")
	}
}

func repoRootForEgressScan(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// setsAllowLoopback reports whether src assigns the field, ignoring comments
// and the declaration itself.
func setsAllowLoopback(src string) bool {
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		i := strings.Index(line, "AllowLoopback")
		if i < 0 {
			continue
		}
		rest := strings.TrimSpace(line[i+len("AllowLoopback"):])
		// `AllowLoopback: true` in a literal, or `x.AllowLoopback = true`.
		if strings.HasPrefix(rest, ":") || strings.HasPrefix(rest, "=") {
			return true
		}
	}
	return false
}
