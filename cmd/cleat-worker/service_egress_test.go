package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// The scan in only_the_plugin_transport_is_exempt_test.go proves WHERE the
// exemption is set. These prove what it does, which is the half a text scan
// cannot reach: that registering an endpoint permits that host and nothing
// else, and that no registration reaches the ranges the floor will not exempt.
func TestAServiceEndpointIsExemptOnlyForTheHostItRegistered(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		host     string
		want     bool
		why      string
	}{
		{"the registered host", "http://127.0.0.1:8080", "127.0.0.1", true,
			"a sidecar on loopback is the common deployment and the case that " +
				"failed five tests in tests/crash before this"},
		{"a different private host", "http://127.0.0.1:8080", "10.1.2.3", false,
			"registering one endpoint is not a grant over private space"},
		{"port is not part of the host", "http://127.0.0.1:8080", "127.0.0.1:9999", false,
			"the set matches Hostname(), so a port never appears in it"},
		{"nothing registered", "", "127.0.0.1", false,
			"no endpoint means no exemption; absence of a policy is not permission"},
		{"an unparseable endpoint", "://nonsense", "nonsense", false,
			"a URL that does not parse contributes no grant"},
		{"a scheme with no host", "file:///etc/passwd", "", false,
			"Hostname() is empty, and the empty host must not become a set entry " +
				"that matches every hostless lookup"},
		{"case and trailing dot", "http://Payments.Internal./x", "payments.internal", true,
			"the same name written differently is the same name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := serviceEndpointHosts(tc.endpoint).permits(tc.host); got != tc.want {
				t.Errorf("endpoint %q permits(%q) = %v, want %v -- %s",
					tc.endpoint, tc.host, got, tc.want, tc.why)
			}
		})
	}
}

// The non-exemptible ranges hold whatever is registered. This is the reason the
// exemption is safe to give at all, and it is asserted through the REAL guard
// rather than through the host set, because the order of the two checks is what
// makes it true: checkAddr tests the range before it tests the host.
func TestARegisteredEndpointStillCannotReachInstanceMetadata(t *testing.T) {
	c := &dbServiceCaller{}
	g := c.serviceEgressGuard(context.Background(), "http://169.254.169.254")

	_, err := g.DialContext(context.Background(), "tcp", "169.254.169.254:80")
	if err == nil {
		t.Fatal("a registered endpoint reached the cloud metadata address; " +
			"registering a host must never be able to grant this range")
	}
	var denied *engine.EgressDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("refused with %v, want an EgressDeniedError -- a connection error "+
			"would mean the floor never ran and only the dial failed", err)
	}
	// The operator configured this and needs to know the exemption was seen and
	// overruled, not silently absent.
	if !strings.Contains(denied.Reason, "cannot be exempted") {
		t.Errorf("reason was %q, want it to say the range cannot be exempted; an "+
			"operator who registered this endpoint would otherwise read the bare "+
			"floor message as their configuration not having arrived", denied.Reason)
	}
}

// The neighbouring guard must not have gained any of this. The guest supplies
// the URL on that path, which is the whole reason the exemption is confined.
func TestTheGuestFetchGuardStillHasNoExemptionAfterTheServiceGuardGainedOne(t *testing.T) {
	c := &dbServiceCaller{benchSvcURL: "http://127.0.0.1:8080"}
	if g := c.egressGuard(context.Background()); g.PluginHostExempt != nil {
		t.Error("registering a service endpoint put an exemption on the GUEST fetch " +
			"guard; a workflow could then name that host in http.fetch")
	}
	if g := c.serviceEgressGuard(context.Background(), c.benchSvcURL); g.PluginHostExempt == nil {
		t.Error("the service guard has no exemption, so every private endpoint is " +
			"refused -- the regression this pair of tests was written for")
	}
}

// The parameter is the point: the grant follows the URL the caller is about to
// POST to, not whatever happens to be in the caller's config. A guard that read
// c.benchSvcURL would refuse every endpoint registered through the newer
// --service-endpoints flag (#1844), which is the defect this whole commit fixes,
// reintroduced one config at a time.
func TestTheExemptionFollowsTheEndpointBeingDialledNotTheConfig(t *testing.T) {
	c := &dbServiceCaller{benchSvcURL: "http://127.0.0.1:1111"}

	// Dialling a DIFFERENT registered endpoint exempts that one...
	g := c.serviceEgressGuard(context.Background(), "http://127.0.0.2:2222")
	if !g.PluginHostExempt("127.0.0.2") {
		t.Error("the endpoint passed to the guard was not exempt; a forwarder " +
			"reaching a second registered service would be refused")
	}
	// ...and not the one the caller merely has configured.
	if g.PluginHostExempt("127.0.0.1") {
		t.Error("the guard exempted a host from config rather than the endpoint " +
			"it was given; the grant must be no wider than the call in hand")
	}
}
