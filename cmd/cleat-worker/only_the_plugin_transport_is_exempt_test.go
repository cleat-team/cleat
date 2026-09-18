package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#1627. The private-address exemption is for PLUGIN endpoints, which are
// operator configuration. A workflow is code cleat did not write, so nothing a
// guest supplies may reach a private address whatever the operator configured
// for plugins.
//
// That is a claim about wiring, and wiring is not something a behavioural test
// can cover exhaustively -- a future caller could pass the exemption to a
// fourth guard and every existing test would still pass. So this scans the
// tree, in the same shape as TestNoProductionCodeAllowsLoopbackEgress, which
// exists for the neighbouring field and for the same reason.
func TestOnlyThePluginTransportCarriesThePrivateHostExemption(t *testing.T) {
	root := repoRootForExemptionScan(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Fields(string(out))
	// A scan that found nothing reports a clean tree, which reads identically
	// to success.
	if len(files) < 100 {
		t.Fatalf("git ls-files matched %d Go files; the scan did not see the repo", len(files))
	}

	var setters []string
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(root, f))
		if rerr != nil {
			continue
		}
		checked++
		if setsPluginHostExempt(string(body)) {
			setters = append(setters, f)
		}
	}
	if checked == 0 {
		t.Fatal("no non-test Go files were read; the scan measured nothing")
	}

	// TWO TRANSPORTS, both operator-destination. The second was added when the
	// service forwarder moved behind the guard, and the test is widened rather
	// than worked around: the assignment could have been hidden in this file's
	// blessed neighbour and the scan would have passed while the wiring changed,
	// which is the failure this scan exists to make visible.
	//
	// The principle #1627 draws is not "plugins are special". It is WHO CHOSE
	// THE HOST. A plugin endpoint is operator configuration; so is a service
	// endpoint, registered by the same kind of flag. A guest reaches both --
	// a workflow calls an llm host function, and a workflow names a service --
	// but in neither case does the guest supply the DESTINATION. It selects
	// among destinations the operator registered. That is the line, and the
	// guest fetch path is on the other side of it: there the guest supplies the
	// URL itself, so no exemption may ever appear there.
	want := map[string]string{
		"cmd/cleat-worker/plugin_egress.go": "plugin endpoints, cleat#1627 -- " +
			"a self-hosted model server on loopback",
		"cmd/cleat-worker/service_egress.go": "service endpoints -- a sidecar on " +
			"loopback or payments.svc.cluster.local, which is RFC1918 by construction. " +
			"Its exempt set is the registered endpoint URLs themselves, so it has no " +
			"flag with which to name a host the deployment does not already call",
	}
	for _, f := range setters {
		if _, ok := want[f]; !ok {
			t.Errorf("PluginHostExempt is set in %s, which is not a transport allowed to carry it.\n"+
				"The permitted files are %v. Every other guard serves guest-CHOSEN "+
				"destinations -- the workflow fetch path in cmd/cleat-worker/setup.go and "+
				"the embedded runner in cleat/embedded. An operator's exemption reaching "+
				"one of those would let a WORKFLOW reach the deployment's private network "+
				"with a host of its own choosing, which is the thing engine/egress_policy.go "+
				"exists to refuse.", f, keysOf(want))
		}
	}
	// And the other direction: a file dropping the exemption silently is a
	// behaviour change too, and one that would show up as a broken deployment
	// rather than as a test failure.
	for f := range want {
		if !slices.Contains(setters, f) {
			t.Errorf("%s no longer sets PluginHostExempt; it is one of the two transports "+
				"that must carry it, because %s", f, want[f])
		}
	}
}

// And the behavioural half, because the scan above is about text: the guard the
// guest fetch path actually builds has no exemption in it.
func TestTheGuestFetchGuardHasNoPrivateHostExemption(t *testing.T) {
	c := &dbServiceCaller{}
	g := c.egressGuard(context.Background())
	if g.PluginHostExempt != nil {
		t.Error("the guest fetch guard carries a private-host exemption; a workflow " +
			"could then reach whatever the operator opened for plugins")
	}
}

// The exemption set itself: an unset flag and an empty entry are both "nothing",
// and a host is matched as written rather than by any wildcard.
func TestThePrivateHostSetMatchesExactlyAndPermitsNothingByDefault(t *testing.T) {
	if newPluginPrivateHosts(nil).permits("localhost") {
		t.Error("an unconfigured set permitted a host; absence of a policy is not permission")
	}
	if newPluginPrivateHosts([]string{"", "   "}).permits("localhost") {
		t.Error("a set of blank entries permitted a host")
	}
	var nilSet *pluginPrivateHosts
	if nilSet.permits("localhost") {
		t.Error("a nil set permitted a host; the guard calls through this receiver")
	}

	p := newPluginPrivateHosts([]string{"localhost", "Models.Internal."})
	for _, tc := range []struct {
		host string
		want bool
		why  string
	}{
		{"localhost", true, "exact"},
		{"LOCALHOST", true, "case-insensitive, like the operator allowlist"},
		{"models.internal", true, "entries are lowercased and the trailing dot dropped"},
		{"models.internal.", true, "a trailing dot is the same name"},
		{"127.0.0.1", false, "the ADDRESS localhost resolves to is a different entry, " +
			"because the match is on the host as the endpoint URL writes it"},
		{"sub.models.internal", false, "no suffix form: a wildcard over private space " +
			"would be a far larger grant than the motivating case needs"},
		{"", false, "the empty host matches nothing"},
	} {
		if got := p.permits(tc.host); got != tc.want {
			t.Errorf("permits(%q) = %v, want %v -- %s", tc.host, got, tc.want, tc.why)
		}
	}
}

// setsPluginHostExempt reports whether this source SETS the field, as opposed to
// declaring it or writing about it.
//
// Deliberately the same shape as setsAllowLoopback in
// engine/a_guest_cannot_reach_the_hosts_own_network_test.go, which is the guard
// for the neighbouring field. The first version of this scan was a regex for
// `PluginHostExempt\s*:` -- a struct literal only -- and BOTH guest guards
// assign with `=`:
//
//	cmd/cleat-worker/setup.go:286   g.AllowHost = func(...)
//	cleat/embedded/runner.go:474    guard.AllowHost = ...
//
// So the scan was blind to the exact idiom a violation would use, in the exact
// two files its own failure message names. Caught in review by WS-1. The
// correct matcher was already in the tree, next to mine, written for the same
// purpose -- which is the more useful lesson than the regex itself.
//
// Comments are stripped first, because a search cannot tell a line of code from
// a sentence about one, and egress_policy.go has twenty lines of sentences.
func setsPluginHostExempt(src string) bool {
	const field = "PluginHostExempt"
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		i := strings.Index(line, field)
		if i < 0 {
			continue
		}
		rest := strings.TrimSpace(line[i+len(field):])
		// `PluginHostExempt: fn` in a literal, or `g.PluginHostExempt = fn`.
		// The declaration `PluginHostExempt func(host string) bool` is neither.
		if strings.HasPrefix(rest, ":") || strings.HasPrefix(rest, "=") {
			return true
		}
	}
	return false
}

// TestTheExemptionScanSeesBothWaysAGuardFieldIsSet is the KNOWN-POSITIVE the
// scan above did not have.
//
// It had a negative control -- the two floor assertions -- and no demonstration
// that it could report a tree that was genuinely broken. A scan that quietly
// stops matching reports a clean tree, which reads identically to success, and
// that is precisely what happened: it passed while being unable to see an
// assignment.
//
// The declaration and prose cases are here too, because a matcher loose enough
// to catch every assignment can be loose enough to fire on the field's own
// definition, and then the guard is useless in the other direction.
func TestTheExemptionScanSeesBothWaysAGuardFieldIsSet(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want bool
		why  string
	}{
		{"\tguard := &engine.EgressGuard{\n\t\tPluginHostExempt: private.permits,\n\t}", true,
			"a struct literal, which is how plugin_egress.go sets it"},
		{"\tg.PluginHostExempt = private.permits", true,
			"an ASSIGNMENT -- the idiom both guest guards use, and the one the first " +
				"version of this scan could not see"},
		{"\tg.PluginHostExempt=private.permits", true, "no spaces"},
		{"\tg.PluginHostExempt  =  nil", true,
			"assigning nil is still setting it; a reviewer must see the line either way"},
		{"\tPluginHostExempt func(host string) bool", false,
			"the DECLARATION in engine/egress_policy.go is not a hazard"},
		{"\t// PluginHostExempt: nil means none, which is the default", false,
			"a sentence about the field, which the twenty lines of prose above it are " +
				"full of"},
		{"\tAllowHost = fn", false, "a different field"},
	} {
		if got := setsPluginHostExempt(tc.src); got != tc.want {
			t.Errorf("setsPluginHostExempt(%q) = %v, want %v -- %s", tc.src, got, tc.want, tc.why)
		}
	}
}

func repoRootForExemptionScan(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

var _ = engine.EgressGuard{}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
