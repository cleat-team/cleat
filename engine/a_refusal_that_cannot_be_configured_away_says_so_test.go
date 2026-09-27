package engine

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

// cleat#1627. The gates are checked operator -> tenant -> floor and the refusal
// names the FIRST one that said no, so a private literal -- refused by every
// gate -- was reported as an allowlist failure. That names a list the operator
// can edit, and editing it changes nothing: `cleatctl egress-allow add
// 127.0.0.1` is accepted, `list` shows it afterwards, and the next call cites
// the floor instead.
//
// The fix is not a reordering. It refuses a denied IP LITERAL above the
// allowlists, where no resolution is needed, and leaves the name path exactly
// as it was. Both halves are pinned below, because the one that is NOT fixed is
// the one a later reader would otherwise "finish".

func TestADeniedLiteralIsRefusedByTheFloorNotTheAllowlist(t *testing.T) {
	for _, tc := range []struct {
		host string
		want string // a distinctive fragment of the floor's reason
	}{
		{"127.0.0.1", "loopback"},
		{"169.254.169.254", "metadata"},
		{"10.1.2.3", "RFC1918"},
		{"192.168.1.1", "RFC1918"},
		{"::1", "loopback"},
	} {
		g := &EgressGuard{
			// Every layer above the floor REFUSES as well, which is the
			// situation that produced the misleading message: without this the
			// test would pass for the trivial reason that nothing else ran.
			OperatorAllows: func(context.Context, string) (bool, error) { return false, nil },
			AllowHost:      func(context.Context, string) (bool, error) { return false, nil },
			lookup: func(context.Context, string) ([]netip.Addr, error) {
				t.Errorf("%s: resolved a literal address; the floor check must need no lookup", tc.host)
				return nil, nil
			},
			Dial: func(context.Context, string, string) (net.Conn, error) {
				t.Errorf("%s: dialled a floor-denied address", tc.host)
				return nil, nil
			},
		}
		_, err := g.DialContext(context.Background(), "tcp", net.JoinHostPort(tc.host, "80"))
		var denied *EgressDeniedError
		if !errors.As(err, &denied) {
			t.Errorf("%s: got %v, want an EgressDeniedError", tc.host, err)
			continue
		}
		if strings.Contains(denied.Reason, "allowlist") {
			t.Errorf("%s: refused by an allowlist (%q). This is the defect: the operator "+
				"reads that, adds the host to the list, and the call fails anyway.",
				tc.host, denied.Reason)
		}
		if !strings.Contains(denied.Reason, tc.want) {
			t.Errorf("%s: reason %q does not name the floor rule (%q) that refused it",
				tc.host, denied.Reason, tc.want)
		}
	}
}

// The limitation, asserted rather than left to be discovered.
//
// A NAME that resolves into a denied range still reports the allowlist first,
// and that is deliberate: finding out otherwise means resolving it, which is
// exactly what must not happen before the allowlists have spoken -- a guest
// that cannot reach a host must not be able to make the worker look up a name
// of its choosing.
//
// So `localhost` keeps the old message. This test exists so that a later reader
// who notices the inconsistency finds the reason attached to it rather than
// "fixing" it and quietly turning the guard into a DNS oracle.
func TestANameResolvingIntoADeniedRangeStillReportsTheAllowlistFirst(t *testing.T) {
	resolved := false
	g := &EgressGuard{
		AllowHost: func(context.Context, string) (bool, error) { return false, nil },
		lookup: func(context.Context, string) ([]netip.Addr, error) {
			resolved = true
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
	}
	_, err := g.DialContext(context.Background(), "tcp", "localhost:11434")
	var denied *EgressDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("got %v, want an EgressDeniedError", err)
	}
	if !strings.Contains(denied.Reason, "allowlist") {
		t.Errorf("reason was %q; a NAME must still be refused by the allowlist before "+
			"anything resolves it", denied.Reason)
	}
	if resolved {
		t.Error("the name was resolved despite the allowlist refusing it -- that is the " +
			"DNS-oracle property this ordering exists to hold")
	}
}

// The change must move a MESSAGE and nothing else. A destination that was
// reachable before is still reachable, and one that was refused is still
// refused -- only the stated reason differs, and only for literals.
func TestReportingTheFloorFirstDoesNotChangeWhatIsReachable(t *testing.T) {
	dialled := ""
	g := &EgressGuard{
		OperatorAllows: func(context.Context, string) (bool, error) { return true, nil },
		AllowHost:      func(context.Context, string) (bool, error) { return true, nil },
		lookup: func(_ context.Context, h string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
		Dial: func(_ context.Context, _, addr string) (net.Conn, error) {
			dialled = addr
			return nil, errDialProbe
		},
	}

	// A permitted PUBLIC literal still dials.
	_, err := g.DialContext(context.Background(), "tcp", "93.184.216.34:443")
	if !errors.Is(err, errDialProbe) {
		t.Errorf("a permitted public literal was not dialled: %v", err)
	}
	if dialled != "93.184.216.34:443" {
		t.Errorf("dialled %q, want the checked literal", dialled)
	}

	// A permitted NAME still dials, through the resolution path.
	dialled = ""
	_, err = g.DialContext(context.Background(), "tcp", "example.com:443")
	if !errors.Is(err, errDialProbe) {
		t.Errorf("a permitted name was not dialled: %v", err)
	}
	if dialled != "93.184.216.34:443" {
		t.Errorf("dialled %q, want the RESOLVED address rather than the name -- that is "+
			"the line that closes the rebinding window", dialled)
	}
}

var errDialProbe = errors.New("dial reached")
