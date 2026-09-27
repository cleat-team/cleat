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
//
// exemptible says whether an OPERATOR may name a host in this range as a
// deliberate exception (see PluginHostExempt). It is a property of the RANGE,
// not of any configuration: some of these ranges can legitimately hold a
// service an operator runs on purpose, and some cannot hold anything an
// operator would ever mean to reach.
//
// The split is the whole safety of the mechanism, so it is a field with a
// reason rather than a rule applied at the call site. cleat#1627.
type deniedRange struct {
	prefix netip.Prefix
	why    string

	// exemptible is TRUE for ranges where a real service may live -- a model
	// server on loopback, an internal API on RFC1918 -- and FALSE for ranges
	// where reaching anything at all is the failure this file exists to
	// prevent. whyNotExemptible says which for every false entry.
	exemptible       bool
	whyNotExemptible string
}

// deniedRanges is the floor. Every entry is refused for a guest-initiated
// request regardless of tenant configuration.
//
// IPv4-mapped IPv6 forms are NOT listed separately: Unmap() is applied before
// the comparison, so ::ffff:10.0.0.1 is tested as 10.0.0.1. Listing both would
// invite an entry being added to one list and not the other, which is the
// divergence the accompanying test exists to catch.
var deniedRanges = []deniedRange{
	// EXEMPTIBLE: an operator can legitimately run a service here and mean to
	// reach it. A self-hosted model server is the motivating case -- ollama's
	// own default is http://localhost:11434 (cleat#1627).
	{prefix: netip.MustParsePrefix("127.0.0.0/8"), why: "loopback: the worker's own API and admin surface", exemptible: true},
	{prefix: netip.MustParsePrefix("::1/128"), why: "loopback, IPv6", exemptible: true},
	{prefix: netip.MustParsePrefix("10.0.0.0/8"), why: "RFC1918 private: the network the worker sits in", exemptible: true},
	{prefix: netip.MustParsePrefix("172.16.0.0/12"), why: "RFC1918 private", exemptible: true},
	{prefix: netip.MustParsePrefix("192.168.0.0/16"), why: "RFC1918 private", exemptible: true},
	{prefix: netip.MustParsePrefix("100.64.0.0/10"), why: "RFC6598 carrier-grade NAT: not public, and routable inside many hosts", exemptible: true},
	{prefix: netip.MustParsePrefix("fc00::/7"), why: "IPv6 unique local, the RFC1918 equivalent", exemptible: true},

	// NOT EXEMPTIBLE. Each of these refuses something no operator configuring
	// a plugin endpoint would ever mean, and the first is the address this
	// whole file was written for.
	{prefix: netip.MustParsePrefix("169.254.0.0/16"), why: "link-local, and 169.254.169.254 is cloud instance metadata -- often credentials",
		whyNotExemptible: "the metadata endpoint is the target the floor exists to refuse; an exemption here would hand out cloud credentials to whatever supplied the endpoint"},
	{prefix: netip.MustParsePrefix("fe80::/10"), why: "link-local, IPv6",
		whyNotExemptible: "same as 169.254.0.0/16, and reachable by the same mistake"},
	{prefix: netip.MustParsePrefix("0.0.0.0/8"), why: "unspecified / this-network: 0.0.0.0 reaches loopback on several stacks",
		whyNotExemptible: "not an address anyone configures on purpose; it is what a blank or malformed endpoint parses to"},
	{prefix: netip.MustParsePrefix("::/128"), why: "unspecified, IPv6",
		whyNotExemptible: "same as 0.0.0.0/8"},
	{prefix: netip.MustParsePrefix("224.0.0.0/4"), why: "multicast: not a meaningful fetch target, and reaches local segments",
		whyNotExemptible: "a unicast HTTP endpoint is never multicast; naming one means the endpoint is wrong"},
	{prefix: netip.MustParsePrefix("ff00::/8"), why: "multicast, IPv6",
		whyNotExemptible: "same as 224.0.0.0/4"},
	{prefix: netip.MustParsePrefix("192.0.0.0/24"), why: "IETF protocol assignments, including NAT64 and DS-Lite endpoints",
		whyNotExemptible: "protocol infrastructure, not a service an operator runs"},
	{prefix: netip.MustParsePrefix("192.0.2.0/24"), why: "TEST-NET-1: not routable, so reaching it means something local answered",
		whyNotExemptible: "reaching it at all means something local answered for it, which is the case worth refusing"},
	{prefix: netip.MustParsePrefix("198.18.0.0/15"), why: "benchmarking range, routable inside some networks",
		whyNotExemptible: "same as TEST-NET-1"},
	{prefix: netip.MustParsePrefix("240.0.0.0/4"), why: "reserved: no legitimate fetch target, and treated as local by some stacks",
		whyNotExemptible: "reserved space; a service here is a misconfiguration whichever way it is reached"},
}

// NonExemptibleRanges reports the floor ranges no operator exemption can reach,
// each with the floor's OWN reason string.
//
// It exists so that anything telling an operator what cannot be exempted --
// a startup log, a doc generator -- quotes the table rather than paraphrasing
// it. A paraphrase drifts: it is written once against the table as it was, and
// nothing fails when a range is added or its reasoning changes. WS-1's review
// of cleat#1630 made this point about their own FloorReadmissions and it is the
// better construction, so it is here too.
func NonExemptibleRanges() []string {
	out := make([]string, 0, len(deniedRanges))
	for _, d := range deniedRanges {
		if d.exemptible {
			continue
		}
		out = append(out, d.prefix.String()+" ("+d.why+")")
	}
	return out
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
func (g *EgressGuard) checkAddr(host string, addr netip.Addr) error {
	a := addr.Unmap()
	for _, d := range deniedRanges {
		// A v4 prefix cannot contain a v6 address and vice versa; Contains
		// already answers false, so no explicit family check is needed.
		if !d.prefix.Contains(a) {
			continue
		}
		// An operator exemption applies only to ranges the table marks
		// exemptible. The check is on the RANGE first and the host second, so
		// naming a host that resolves into the metadata range cannot become a
		// grant by being named.
		if d.exemptible && g.PluginHostExempt != nil && g.PluginHostExempt(host) {
			return nil
		}
		reason := d.why
		if !d.exemptible && g.PluginHostExempt != nil && g.PluginHostExempt(host) {
			// Named, and refused anyway. Say so, because the operator has
			// evidence they configured this and would otherwise read the bare
			// floor message as the exemption not being applied.
			reason = d.why + " -- and this range cannot be exempted: " + d.whyNotExemptible
		}
		return &EgressDeniedError{Host: host, IP: a.String(), Reason: reason}
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
	// AllowHost reports whether this TENANT may reach the requested host. Nil
	// denies, because an absent tenant policy is not permission.
	AllowHost func(ctx context.Context, host string) (bool, error)

	// OperatorAllows reports whether this DEPLOYMENT may reach the host at all.
	//
	// EGRESS NEEDS BOTH. A destination is reachable only when the operator
	// permits it AND the tenant permits it; either saying no is a refusal, and
	// the refusal names which one. Owner decision 2026-09-15, correcting a
	// model in which a tenant's list was the only thing consulted and an
	// operator had no say in what their own deployment could reach.
	//
	// Nil means UNSET, which permits every public host -- owner decision, and
	// the reason it is safe to default open here and not on the tenant side:
	// the floor still applies underneath, so "all public" really is all
	// public. An operator who wants to narrow sets a list; a tenant can only
	// ever narrow further within it.
	//
	// It is consulted for egress that has no tenant at all -- plugin
	// background sweeps, and anything on an auth-exempt route -- which is the
	// case that previously had nowhere to look and grew a flag of its own.
	OperatorAllows func(ctx context.Context, host string) (bool, error)

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

	// PluginHostExempt reports whether the operator has named this host as a
	// deliberate exception to the floor. Nil means none, which is the default
	// and the only value any guest-facing guard ever has.
	//
	// UNLIKE AllowLoopback this is a production mechanism, and the difference
	// between them is the point. AllowLoopback is a blanket "loopback is fine"
	// for in-process tests. This is per-HOST, operator-only, and cannot reach
	// the ranges marked non-exemptible in deniedRanges -- the metadata address
	// above all. cleat#1627.
	//
	// It exists because a plugin endpoint is OPERATOR configuration, the same
	// category plugin_egress.go already gives the deployment allowlist for
	// tenant-less sweeps. A self-hosted model server is the motivating case:
	// plugins/llm's ollama provider defaults to http://localhost:11434 and was
	// unreachable with no way to permit it.
	//
	// It is NOT wired into the guest fetch path or the embedded runner, and a
	// test asserts that: a workflow is code cleat did not write, and nothing a
	// guest supplies should reach a private address whatever the operator has
	// configured for plugins.
	PluginHostExempt func(host string) bool

	// TenantOptional reports whether THIS call legitimately has no tenant, in
	// which case the operator layer governs it alone.
	//
	// A PREDICATE rather than a bool, because the answer differs per call on
	// one transport. A plugin builds its http.Client once at Init and uses it
	// for both host-function calls (a tenant is in context) and background
	// sweeps (none is), so a static flag would have to be wrong for one of
	// them. Nil means "a tenant is always required", which is what guest
	// egress wants.
	//
	// Explicit rather than inferred from "AllowHost returned false", because
	// those states must not be confused: a missing tenant on a call that
	// SHOULD have one is a bug that has to deny, and treating it as
	// "tenant-less, so operator only" turns that bug into an open gate. The
	// test for exactly that is
	// TestTenantlessEgressAnswersToTheOperatorAlone/a_MISSING_tenant_hook.
	TenantOptional func(ctx context.Context) bool

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

	// The test seam, checked first and narrowly: an httptest server's URL is
	// always a loopback IP LITERAL, so this needs no resolution and cannot be
	// reached by a hostname. See AllowLoopback.
	if g.AllowLoopback {
		if ip, err := netip.ParseAddr(host); err == nil && ip.Unmap().IsLoopback() {
			return g.dialer()(ctx, network, address)
		}
	}

	// An IP LITERAL in a denied range is refused here, above both allowlists,
	// because its refusal is the one nobody can act on.
	//
	// The order below reports the FIRST gate that says no, and the floor is
	// last, so a destination refused by every gate was reported as an
	// allowlist failure -- which names a list the operator can edit. They
	// edit it. `cleatctl egress-allow add 127.0.0.1` is accepted, `list`
	// shows it afterwards, and the next call fails anyway, now citing the
	// floor. cleat#1627: that round trip is the defect, not the refusal.
	//
	// This is NOT a reordering of the checks and does not touch the DNS-oracle
	// property below. It applies only when the host is already an address, so
	// no resolution happens and nothing is learned that the caller did not
	// supply. A NAME that resolves into a denied range still reports the
	// allowlist first, deliberately: finding out otherwise would require
	// resolving it, which is exactly what must not happen before the
	// allowlists have spoken.
	if ip, perr := netip.ParseAddr(host); perr == nil {
		if err := g.checkAddr(host, ip); err != nil {
			return nil, err
		}
	}

	// BOTH LAYERS, operator first, and both BEFORE resolving -- a denied name
	// must not be a DNS oracle. A guest that cannot reach a host should not be
	// able to learn whether it exists, or make the worker emit a lookup for a
	// name of its choosing.
	//
	// Operator first because it is the outer bound: if the deployment may not
	// reach a host, whose behalf the request is on does not matter, and the
	// refusal should say so rather than blaming a tenant's list.
	if g.OperatorAllows != nil {
		ok, err := g.OperatorAllows(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("egress: checking the operator allowlist for %q: %w", host, err)
		}
		if !ok {
			return nil, &EgressDeniedError{
				Host:   host,
				Reason: "host is not on this deployment's egress allowlist (operator policy)",
			}
		}
	}

	// A nil AllowHost is the absence of a TENANT policy, and a platform that
	// runs code it did not write must not read that as permission. Unlike the
	// operator layer, which defaults open, this defaults closed. cleat#1565.
	//
	// Skipped entirely when there is no tenant to have a policy -- a plugin
	// background sweep, or an auth-exempt route. Those are governed by the
	// operator layer alone, which is why that layer had to become general
	// rather than a plugin-shaped flag.
	if !g.tenantOptional(ctx) {
		if g.AllowHost == nil {
			return nil, &EgressDeniedError{
				Host:   host,
				Reason: "no egress allowlist is configured for this tenant, and an empty list permits nothing",
			}
		}
		ok, err := g.AllowHost(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("egress: checking the allowlist for %q: %w", host, err)
		}
		if !ok {
			return nil, &EgressDeniedError{Host: host, Reason: "host is not on this tenant's egress allowlist"}
		}
	}

	// The FLOOR is below both layers and is checked after them, on the
	// RESOLVED addresses -- neither an operator's list nor a tenant's can
	// admit a private address, because a hostname on either list that resolves
	// into private space is the rebinding case rather than a grant.

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
		if err := g.checkAddr(host, ip); err != nil {
			return nil, err
		}
	}

	dial := g.dialer()
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

// dialer is g.Dial or a plain net.Dialer.
func (g *EgressGuard) dialer() func(ctx context.Context, network, address string) (net.Conn, error) {
	if g.Dial != nil {
		return g.Dial
	}
	d := &net.Dialer{}
	return d.DialContext
}

// tenantOptional reports whether this call may proceed without a tenant.
func (g *EgressGuard) tenantOptional(ctx context.Context) bool {
	return g.TenantOptional != nil && g.TenantOptional(ctx)
}
