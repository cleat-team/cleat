package main

import (
	"context"
	"net/url"

	"github.com/cleat-team/cleat/engine"
)

// serviceEgressGuard is the policy for a destination the OPERATOR named, as
// opposed to egressGuard, which is the policy for one a GUEST named. The
// difference is who chose the host, and it decides both of the questions this
// file answers: whether the per-tenant allowlist has anything to say, and
// whether the destination may be a private address.
//
// A SEPARATE FILE, and that is not tidiness. TestOnlyThePluginTransport-
// CarriesThePrivateHostExemption scans the tree per FILE for anything setting
// PluginHostExempt, and setup.go holds the guest fetch guard as well as this
// one. Naming setup.go in that scan's allowed set would stop it catching an
// exemption added to the guest path -- the exact hazard its failure message
// describes. One guard per file keeps the scan as sharp as it was.
//
// WHO NAMED THE HOST, for both questions:
//
//   - THE TENANT LIST. egressGuard is built for http.fetch, where the URL comes
//     from the guest, so no tenant allowlist means no permission: AllowHost
//     stays nil and the guard denies, deliberately rather than as a nil-check
//     that opens the gate. Here the operator named the host and the tenant
//     chose nothing, so consulting a per-tenant list refuses every deployment
//     that has not built a tenant egress table -- which is every deployment
//     using this path today. Measured before TenantOptional was set: "no egress
//     allowlist is configured for this tenant, and an empty list permits
//     nothing", on a --bench-svc-url the operator had set explicitly.
//   - THE FLOOR. A service endpoint is almost always a private address: a
//     sidecar on loopback, or payments.svc.cluster.local, which is RFC1918 by
//     construction. An earlier revision of this change left the floor absolute
//     here and said so in a comment, as though that were a tolerable status
//     quo. It is not, and the repo said so: five tests in tests/crash point a
//     service endpoint at an httptest server on 127.0.0.1 and every one of them
//     failed with "egress to 127.0.0.1 is refused ... loopback: the worker's
//     own API and admin surface". Guarding a path must not mean deleting the
//     feature that uses it.
//
// THE EXEMPT SET IS THE ENDPOINT BEING DIALLED, which is narrower than the
// mechanism it borrows and narrower than reading the config would be. plugins
// get PluginHostExempt from a flag (--plugin-egress-allow-private) that is
// separate from the endpoint config, so an operator can name a host they never
// configured. Here there is no flag and no set to maintain: the caller passes
// the base URL it is about to POST to, and the grant is exactly that host.
//
// A PARAMETER RATHER THAN A READ OF c.benchSvcURL, which is the difference
// between a rule and a rule that stays true. A second way to register an
// endpoint is already in flight (--service-endpoints, #1844), and a guard that
// exempted only the host in benchSvcURL would refuse every destination reached
// through the new flag -- this exact defect, reintroduced for the newer config
// and invisible until someone deployed it. Taking the URL from the call site
// means a forwarder cannot dial an endpoint it did not also name here.
//
// c.privateHosts -- the plugin set -- is deliberately NOT consulted. An
// operator permitting ollama on localhost for plugins/llm has said nothing
// about which hosts a workflow's service calls may reach, and quietly reading
// one flag as the other is how a narrow grant becomes a wide one.
//
// WHAT THIS STILL REFUSES, which is the reason it is an exemption and not a
// bypass. The floor's non-exemptible ranges hold whatever is registered --
// 169.254.169.254 above all -- because checkAddr tests the RANGE before it
// tests the host. A registered endpoint that resolves to cloud instance
// metadata is refused, and refused with the reason that it cannot be exempted.
// That is the case the dialer exists for: the operator registers a public
// hostname, an attacker who controls its DNS answers with the metadata address,
// and a URL check made before the dial would never see it.
func (c *dbServiceCaller) serviceEgressGuard(ctx context.Context, endpoint string) *engine.EgressGuard {
	if c.egress != nil {
		return c.egress // a test supplied one
	}
	return &engine.EgressGuard{
		OperatorAllows: operatorAllowFunc(c.operatorEgress),

		PluginHostExempt: serviceEndpointHosts(endpoint).permits,

		TenantOptional: func(context.Context) bool { return true },
	}
}

// serviceEndpointHosts is the exempt set: the host of the endpoint being
// dialled, and nothing else. Variadic because the caller may one day pass more
// than one; today it is always exactly the URL of the call in hand.
//
// It reuses pluginPrivateHosts for the set itself -- exact match, lowercased,
// trailing dot dropped, nil-safe -- rather than growing a second spelling of
// "is this host named". The type's name says plugin because that was its first
// caller; what it implements is an operator's set of hosts permitted into
// private space, which is exactly what this is.
//
// An unparseable URL contributes NOTHING rather than failing: this runs on the
// call path, and a URL that does not parse will fail at the request build a few
// lines later with a better message than an egress denial would give. Boot-time
// validation of endpoint config is a separate concern and lives with the flag.
func serviceEndpointHosts(raw ...string) *pluginPrivateHosts {
	var hosts []string
	for _, r := range raw {
		if r == "" {
			continue
		}
		u, err := url.Parse(r)
		if err != nil || u.Hostname() == "" {
			continue
		}
		hosts = append(hosts, u.Hostname())
	}
	return newPluginPrivateHosts(hosts)
}
