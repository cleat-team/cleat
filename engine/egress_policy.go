package engine

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// cleat#1565: a workflow can ask the worker to fetch a URL, and until this file
// nothing inspected where that request went. `169.254.169.254` -- cloud
// instance metadata, and so frequently cloud credentials -- was reachable, as
// was any RFC1918 address the worker could route to and the worker's own admin
// port on loopback.
//
// The bar for a platform that runs code it did not write is not "configurable",
// it is safe by default, so this floor is NOT tenant-overridable. A per-tenant
// allowlist (cleat#1565 open questions 1 and 3) narrows what a tenant may reach
// on top of this; it cannot widen it back to the host's own network position.
//
// Shaped after engine/wasi_policy.go, as the issue asks: an explicit table, one
// entry per denied range, each carrying the reason it is there, and a test that
// fails if the table and the enforced set diverge in either direction.

// deniedRange is one CIDR the floor refuses, with the reason it exists.
type deniedRange struct {
	prefix netip.Prefix
	why    string
}

// deniedRanges is the floor. Every entry is refused for a guest-initiated
// request regardless of tenant configuration.
//
// IPv4-mapped IPv6 forms are NOT listed separately: Unmap() is applied before
// the comparison, so ::ffff:10.0.0.1 is tested as 10.0.0.1. Listing both would
// invite an entry being added to one list and not the other, which is the
// divergence the accompanying test exists to catch.
var deniedRanges = []deniedRange{
	{netip.MustParsePrefix("127.0.0.0/8"), "loopback: the worker's own API and admin surface"},
	{netip.MustParsePrefix("::1/128"), "loopback, IPv6"},
	{netip.MustParsePrefix("169.254.0.0/16"), "link-local, and 169.254.169.254 is cloud instance metadata -- often credentials"},
	{netip.MustParsePrefix("fe80::/10"), "link-local, IPv6"},
	{netip.MustParsePrefix("10.0.0.0/8"), "RFC1918 private: the network the worker sits in"},
	{netip.MustParsePrefix("172.16.0.0/12"), "RFC1918 private"},
	{netip.MustParsePrefix("192.168.0.0/16"), "RFC1918 private"},
	{netip.MustParsePrefix("100.64.0.0/10"), "RFC6598 carrier-grade NAT: not public, and routable inside many hosts"},
	{netip.MustParsePrefix("fc00::/7"), "IPv6 unique local, the RFC1918 equivalent"},
	{netip.MustParsePrefix("0.0.0.0/8"), "unspecified / this-network: 0.0.0.0 reaches loopback on several stacks"},
	{netip.MustParsePrefix("::/128"), "unspecified, IPv6"},
	{netip.MustParsePrefix("224.0.0.0/4"), "multicast: not a meaningful fetch target, and reaches local segments"},
	{netip.MustParsePrefix("ff00::/8"), "multicast, IPv6"},
	{netip.MustParsePrefix("192.0.0.0/24"), "IETF protocol assignments, including NAT64 and DS-Lite endpoints"},
	{netip.MustParsePrefix("192.0.2.0/24"), "TEST-NET-1: not routable, so reaching it means something local answered"},
	{netip.MustParsePrefix("198.18.0.0/15"), "benchmarking range, routable inside some networks"},
	{netip.MustParsePrefix("240.0.0.0/4"), "reserved: no legitimate fetch target, and treated as local by some stacks"},
}

// EgressDeniedError says a destination was refused and why.
//
// A distinct type because the caller has to turn it into a PERMANENT failure.
// The existing http.fetch path returns TransientError for a failed request, and
// a refusal that retries is a refusal that burns the attempt budget against an
// answer that will not change. cleat#1565 open question 5.
type EgressDeniedError struct {
	Host   string
	IP     string
	Reason string
}

func (e *EgressDeniedError) Error() string {
	where := e.Host
	if e.IP != "" && e.IP != e.Host {
		where = fmt.Sprintf("%s (%s)", e.Host, e.IP)
	}
	return fmt.Sprintf("egress to %s is refused by cleat's network policy: %s", where, e.Reason)
}

// checkAddr applies the floor to one resolved address.
func checkAddr(host string, addr netip.Addr) error {
	a := addr.Unmap()
	for _, d := range deniedRanges {
		// A v4 prefix cannot contain a v6 address and vice versa; Contains
		// already answers false, so no explicit family check is needed.
		if d.prefix.Contains(a) {
			return &EgressDeniedError{Host: host, IP: a.String(), Reason: d.why}
		}
	}
	return nil
}

// EgressGuard is the enforcement point: a DialContext that resolves a name,
// refuses any address the floor denies, and then dials THE ADDRESS IT CHECKED.
//
// WHY A DIALER AND NOT A URL CHECK, which is the whole design and the reason
// this is not three lines in handleHTTPFetch:
//
//   - REDIRECTS. A stock http.Client follows up to ten. Checking the URL the
//     guest supplied says nothing about hop two, and hop two is where a
//     cooperating public host sends you to 169.254.169.254.
//   - DNS REBINDING. Checking a hostname resolves it once; the client then
//     resolves it again when it dials. A name that answers public on the first
//     lookup and private on the second defeats any check that does not dial the
//     address it approved. This resolves once and dials the approved IP, so
//     there is no second lookup to poison.
//
// AllowHost, when non-nil, is consulted after the floor passes and narrows
// further -- the per-tenant allowlist. It cannot widen: the floor has already
// refused, and this is not reached.
type EgressGuard struct {
	// AllowHost reports whether this tenant may reach the requested host. Nil
	// means no per-tenant list is configured and only the floor applies.
	AllowHost func(ctx context.Context, host string) (bool, error)

	// AllowLoopback permits 127.0.0.0/8 and ::1 ONLY. It exists so that tests
	// driving the real fetch path against an httptest server -- which always
	// binds loopback -- can pin behaviour that has nothing to do with egress,
	// such as whether a failed connection stays retryable.
	//
	// TEST ONLY, and enforced as such rather than asserted:
	// TestNoProductionCodeAllowsLoopbackEgress fails if any non-test file sets
	// it. Narrow to loopback on purpose -- a general AllowPrivate would also
	// open RFC1918 and link-local, and the metadata address is the one thing
	// this whole file exists to refuse.
	AllowLoopback bool

	// Resolver is overridable by callers. Nil means net.DefaultResolver.
	Resolver *net.Resolver

	// Dial is overridable by callers. Nil means a plain net.Dialer.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)

	// lookup is the resolution seam, unexported because only tests set it.
	// A test that had to stand up real DNS to check the rebinding case would
	// be asserting on the network rather than on this policy.
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
}

// DialContext resolves, checks every answer, and dials the first allowed one.
//
// EVERY answer is checked, not just the one dialled. A name resolving to one
// public and one private address is a rebinding attempt in progress, and
// picking the public one and proceeding would mean the policy depends on
// resolver ordering.
func (g *EgressGuard) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("egress: cannot parse address %q: %w", address, err)
	}

	if g.AllowHost != nil {
		ok, err := g.AllowHost(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("egress: checking the allowlist for %q: %w", host, err)
		}
		if !ok {
			return nil, &EgressDeniedError{Host: host, Reason: "host is not on this tenant's egress allowlist"}
		}
	}

	lookup := g.lookup
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			resolver := g.Resolver
			if resolver == nil {
				resolver = net.DefaultResolver
			}
			return resolver.LookupNetIP(ctx, "ip", host)
		}
	}
	ips, err := lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("egress: resolving %q: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("egress: %q resolved to no addresses", host)
	}
	for _, ip := range ips {
		if g.AllowLoopback && ip.Unmap().IsLoopback() {
			continue
		}
		if err := checkAddr(host, ip); err != nil {
			return nil, err
		}
	}

	dial := g.Dial
	if dial == nil {
		d := &net.Dialer{}
		dial = d.DialContext
	}
	// Dial the checked address literal rather than the name. This is the line
	// that closes the rebinding window -- handing `host` back to the dialer
	// would let it resolve again, and the second answer is the attacker's.
	return dial(ctx, network, net.JoinHostPort(ips[0].Unmap().String(), port))
}

// AllowedSchemes are the only schemes a guest-initiated fetch may use.
//
// Not a denylist: a denylist of file/gopher/ftp is a list of the ones somebody
// thought of, and http.Transport can be extended with more.
var AllowedSchemes = []string{"http", "https"}

// CheckScheme refuses anything outside AllowedSchemes.
func CheckScheme(scheme string) error {
	s := strings.ToLower(scheme)
	for _, a := range AllowedSchemes {
		if s == a {
			return nil
		}
	}
	return &EgressDeniedError{
		Host:   "",
		Reason: fmt.Sprintf("scheme %q is not one of %s", scheme, strings.Join(AllowedSchemes, ", ")),
	}
}
