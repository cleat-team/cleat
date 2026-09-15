package engine

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

// cleat#1565, owner decision 2026-09-14: an empty list denies everything.
func TestAnEmptyAllowlistPermitsNothing(t *testing.T) {
	for _, l := range []*HostAllowlist{nil, NewHostAllowlist(), NewHostAllowlist(""), NewHostAllowlist("   ")} {
		for _, h := range []string{"example.com", "api.example.com", "", "93.184.216.34"} {
			if l.Permits(h) {
				t.Errorf("an empty list permitted %q; absence of a policy is not permission", h)
			}
		}
	}
}

func TestTheAllowlistMatchesExactlyAndBySuffix(t *testing.T) {
	l := NewHostAllowlist("example.com", ".corp.example", "API.Example.NET")
	for _, tc := range []struct {
		host string
		want bool
		why  string
	}{
		{"example.com", true, "exact"},
		{"EXAMPLE.COM", true, "exact, case-insensitive"},
		{"example.com.", true, "a trailing dot is the same name"},
		{"api.example.com", false, "a subdomain is NOT granted by an exact entry"},
		{"notexample.com", false, "suffix of the string is not suffix of the name"},
		{"a.corp.example", true, "the .suffix form"},
		{"a.b.corp.example", true, "the .suffix form, deeper"},
		{"corp.example", false, "the APEX is excluded from the .suffix form on purpose -- " +
			"an operator writing the narrower-looking entry must not get the wider grant"},
		{"evilcorp.example", false, "must not match without the dot boundary"},
		{"api.example.net", true, "entries are lowercased when the list is built"},
		{"", false, "the empty host matches nothing"},
	} {
		if got := l.Permits(tc.host); got != tc.want {
			t.Errorf("Permits(%q) = %v, want %v -- %s", tc.host, got, tc.want, tc.why)
		}
	}
}

// The allowlist NARROWS. It can never reopen what the floor refuses, which is
// what makes the floor non-tenant-overridable rather than merely a default.
func TestAnAllowlistCannotReopenTheFloor(t *testing.T) {
	g := &EgressGuard{
		AllowHost: NewHostAllowlist("metadata.internal", "169.254.169.254").AllowHostFunc(),
		Dial: func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("dialled a floor-denied address because it was allowlisted")
			return nil, nil
		},
	}
	g.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
	}
	_, err := g.DialContext(context.Background(), "tcp", "metadata.internal:80")
	var denied *EgressDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("got %v, want an EgressDeniedError", err)
	}
	if !strings.Contains(denied.Reason, "metadata") {
		t.Errorf("refused, but not by the floor -- reason was %q", denied.Reason)
	}
}
