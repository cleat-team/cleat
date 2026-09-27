package engine

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

// cleat#1627. plugins/llm's ollama provider defaults to
// http://localhost:11434 -- its own doc comment says it "calls a local Ollama
// instance" -- and the floor refused it with no way to permit it. An operator
// running a model server on their own machine is describing their deployment,
// not granting anyone anything, so the exception is operator-scoped and
// per-host.

func TestAnExemptedHostReachesAPrivateAddress(t *testing.T) {
	for _, tc := range []struct {
		host string
		ip   string
		what string
	}{
		{"localhost", "127.0.0.1", "the ollama default"},
		{"localhost", "::1", "the same name over IPv6"},
		{"models.internal", "10.4.5.6", "RFC1918, a model server on the LAN"},
		{"models.internal", "192.168.1.9", "RFC1918, the other common shape"},
		{"tailnet-box", "100.100.1.2", "CGNAT, which is what a tailnet hands out"},
	} {
		dialled := ""
		g := &EgressGuard{
			OperatorAllows:   func(context.Context, string) (bool, error) { return true, nil },
			AllowHost:        func(context.Context, string) (bool, error) { return true, nil },
			PluginHostExempt: func(h string) bool { return h == tc.host },
			lookup: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr(tc.ip)}, nil
			},
			Dial: func(_ context.Context, _, addr string) (net.Conn, error) {
				dialled = addr
				return nil, errExemptDialProbe
			},
		}
		_, err := g.DialContext(context.Background(), "tcp", net.JoinHostPort(tc.host, "11434"))
		if !errors.Is(err, errExemptDialProbe) {
			t.Errorf("%s (%s): not dialled -- %v", tc.what, tc.ip, err)
			continue
		}
		if want := net.JoinHostPort(tc.ip, "11434"); dialled != want {
			t.Errorf("%s: dialled %q, want the CHECKED address %q", tc.what, dialled, want)
		}
	}
}

// The half that makes the mechanism safe rather than merely configurable.
//
// If an exemption could reach link-local, naming a host that resolves to
// 169.254.169.254 would hand cloud instance credentials to whatever supplied
// the endpoint -- which is the single thing engine/egress_policy.go was written
// to prevent. The exemption is checked against the RANGE first and the host
// second, so being named is not a grant.
func TestAnExemptionCannotReachTheRangesThatMatterMost(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		want string
	}{
		{"169.254.169.254", "metadata"},
		{"169.254.1.1", "link-local"},
		{"fe80::1", "link-local"},
		{"0.0.0.0", "unspecified"},
		{"::", "unspecified"},
		{"224.0.0.1", "multicast"},
		{"ff02::1", "multicast"},
		{"192.0.2.5", "TEST-NET-1"},
		{"198.18.0.1", "benchmarking"},
		{"240.0.0.1", "reserved"},
		{"192.0.0.1", "IETF protocol assignments"},
	} {
		g := &EgressGuard{
			OperatorAllows: func(context.Context, string) (bool, error) { return true, nil },
			AllowHost:      func(context.Context, string) (bool, error) { return true, nil },
			// Named by the operator, and refused anyway.
			PluginHostExempt: func(string) bool { return true },
			lookup: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr(tc.ip)}, nil
			},
			Dial: func(context.Context, string, string) (net.Conn, error) {
				t.Errorf("%s: dialled a non-exemptible range because the host was named", tc.ip)
				return nil, nil
			},
		}
		_, err := g.DialContext(context.Background(), "tcp", "named.example:80")
		var denied *EgressDeniedError
		if !errors.As(err, &denied) {
			t.Errorf("%s: got %v, want an EgressDeniedError", tc.ip, err)
			continue
		}
		if !strings.Contains(denied.Reason, tc.want) {
			t.Errorf("%s: reason %q does not name the rule (%q)", tc.ip, denied.Reason, tc.want)
		}
		// The operator configured this and it did not apply. The message has to
		// say that, or they will read the bare floor text as the flag not
		// arriving and go looking in the wrong place.
		if !strings.Contains(denied.Reason, "cannot be exempted") {
			t.Errorf("%s: reason %q does not tell the operator their exemption was "+
				"refused rather than ignored", tc.ip, denied.Reason)
		}
	}
}

// A guard with no exemption behaves exactly as before. Without this the tests
// above could pass against a build that exempted everything.
func TestWithoutAnExemptionThePrivateAddressIsStillRefused(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.4.5.6", "192.168.1.9", "100.100.1.2", "::1"} {
		g := &EgressGuard{
			OperatorAllows: func(context.Context, string) (bool, error) { return true, nil },
			AllowHost:      func(context.Context, string) (bool, error) { return true, nil },
			lookup: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr(ip)}, nil
			},
			Dial: func(context.Context, string, string) (net.Conn, error) {
				t.Errorf("%s: dialled with no exemption configured", ip)
				return nil, nil
			},
		}
		_, err := g.DialContext(context.Background(), "tcp", "unnamed.example:80")
		var denied *EgressDeniedError
		if !errors.As(err, &denied) {
			t.Errorf("%s: got %v, want an EgressDeniedError", ip, err)
		}
	}
}

// Naming one host does not name another, and the match is on the host rather
// than the address -- so exempting "localhost" does not exempt every name that
// happens to resolve to loopback.
func TestAnExemptionAppliesToTheHostItNames(t *testing.T) {
	g := &EgressGuard{
		OperatorAllows:   func(context.Context, string) (bool, error) { return true, nil },
		AllowHost:        func(context.Context, string) (bool, error) { return true, nil },
		PluginHostExempt: func(h string) bool { return h == "localhost" },
		lookup: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		Dial: func(context.Context, string, string) (net.Conn, error) { return nil, errExemptDialProbe },
	}
	if _, err := g.DialContext(context.Background(), "tcp", "localhost:11434"); !errors.Is(err, errExemptDialProbe) {
		t.Errorf("the named host was refused: %v", err)
	}
	_, err := g.DialContext(context.Background(), "tcp", "other.example:11434")
	var denied *EgressDeniedError
	if !errors.As(err, &denied) {
		t.Errorf("an UNNAMED host resolving to the same loopback address was permitted (%v); "+
			"the exemption is per-host, not per-address", err)
	}
}

// Every non-exemptible entry carries the reason it cannot be exempted, so the
// split is reviewable in the table rather than inferred from behaviour. Same
// shape as TestEveryDeniedRangeSaysWhyItIsThere, which this complements.
func TestEveryNonExemptibleRangeSaysWhyItCannotBeExempted(t *testing.T) {
	if len(deniedRanges) == 0 {
		t.Fatal("the floor table is empty; this test measured nothing")
	}
	exemptible := 0
	for _, d := range deniedRanges {
		if d.exemptible {
			exemptible++
			if d.whyNotExemptible != "" {
				t.Errorf("%s is exemptible but carries a whyNotExemptible (%q); one of the "+
					"two is wrong and a reader cannot tell which", d.prefix, d.whyNotExemptible)
			}
			continue
		}
		if strings.TrimSpace(d.whyNotExemptible) == "" {
			t.Errorf("%s cannot be exempted and does not say why. The reason is what a "+
				"reviewer checks the split against.", d.prefix)
		}
	}
	// A floor on the floor: if a change made every range exemptible the tests
	// above would still pass, because they name their own addresses.
	if exemptible == 0 {
		t.Error("no range is exemptible; the mechanism cannot do anything")
	}
	if exemptible == len(deniedRanges) {
		t.Error("EVERY range is exemptible, including link-local -- the split that makes " +
			"this mechanism safe has been lost")
	}
}

// Containment, adapted from WS-1's review of cleat#1630.
//
// Their point was about prefixes: re-admitting one range must not widen
// another. Keyed by HOST the same question is sharper and is the weakest spot
// in this design, because a name does not tell you what it resolves to. A host
// an operator exempted for a loopback model server can later resolve to a
// second address as well -- which is the rebinding shape -- and EVERY answer
// has to be checked, not just the one that would be dialled.
//
// So: an exempt host resolving to a permitted private address AND a
// non-exemptible one must be refused outright, not dialled on the good answer.
func TestAnExemptHostIsRefusedIfANYAnswerIsNonExemptible(t *testing.T) {
	for _, tc := range []struct {
		ips []string
		why string
	}{
		{[]string{"127.0.0.1", "169.254.169.254"}, "the exempted answer first"},
		{[]string{"169.254.169.254", "127.0.0.1"}, "the metadata answer first -- resolver " +
			"ordering must not decide the policy"},
		{[]string{"10.1.2.3", "fe80::1"}, "RFC1918 alongside IPv6 link-local"},
	} {
		addrs := make([]netip.Addr, 0, len(tc.ips))
		for _, ip := range tc.ips {
			addrs = append(addrs, netip.MustParseAddr(ip))
		}
		g := &EgressGuard{
			OperatorAllows:   func(context.Context, string) (bool, error) { return true, nil },
			AllowHost:        func(context.Context, string) (bool, error) { return true, nil },
			PluginHostExempt: func(string) bool { return true },
			lookup:           func(context.Context, string) ([]netip.Addr, error) { return addrs, nil },
			Dial: func(context.Context, string, string) (net.Conn, error) {
				t.Errorf("%s: dialled a host with a non-exemptible answer among %v", tc.why, tc.ips)
				return nil, nil
			},
		}
		_, err := g.DialContext(context.Background(), "tcp", "models.example:11434")
		var denied *EgressDeniedError
		if !errors.As(err, &denied) {
			t.Errorf("%s: got %v, want an EgressDeniedError", tc.why, err)
		}
	}
}

// And the control it needs: the same shape with every answer exemptible IS
// dialled, so the test above is not passing because multi-answer hosts are
// refused in general.
func TestAnExemptHostWithOnlyExemptibleAnswersIsDialled(t *testing.T) {
	g := &EgressGuard{
		OperatorAllows:   func(context.Context, string) (bool, error) { return true, nil },
		AllowHost:        func(context.Context, string) (bool, error) { return true, nil },
		PluginHostExempt: func(string) bool { return true },
		lookup: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("10.1.2.3")}, nil
		},
		Dial: func(context.Context, string, string) (net.Conn, error) { return nil, errExemptDialProbe },
	}
	if _, err := g.DialContext(context.Background(), "tcp", "models.example:11434"); !errors.Is(err, errExemptDialProbe) {
		t.Errorf("a host whose every answer is exemptible was refused: %v", err)
	}
}

var errExemptDialProbe = errors.New("dial reached")
