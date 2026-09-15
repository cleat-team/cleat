package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

	// The field being SET, not merely named: engine/egress_policy.go declares
	// it and describes it at length, and neither is the hazard.
	sets := regexp.MustCompile(`PluginHostExempt\s*:`)

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
		if sets.Match(body) {
			setters = append(setters, f)
		}
	}
	if checked == 0 {
		t.Fatal("no non-test Go files were read; the scan measured nothing")
	}

	const want = "cmd/cleat-worker/plugin_egress.go"
	if len(setters) != 1 || setters[0] != want {
		t.Errorf("PluginHostExempt is set in %v, want exactly [%s].\n"+
			"Every other guard serves guest code -- the workflow fetch path in "+
			"cmd/cleat-worker/setup.go and the embedded runner in cleat/embedded. "+
			"An operator's plugin exemption reaching one of those would let a "+
			"WORKFLOW reach the deployment's private network, which is the thing "+
			"engine/egress_policy.go exists to refuse.", setters, want)
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

func repoRootForExemptionScan(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

var _ = engine.EgressGuard{}
