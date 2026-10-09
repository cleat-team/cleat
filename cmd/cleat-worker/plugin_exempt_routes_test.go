package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// TestIsPluginAuthExemptPatternCatchesEquivalentSpellings is cleat#2279 item
// 1: an exact string compare against pluginAuthExemptPatterns missed every
// pattern that would serve the same anonymous traffic as a canonical entry
// but is spelled differently. These four are the examples the issue gave,
// none of which the pre-fix exact-match check flagged.
func TestIsPluginAuthExemptPatternCatchesEquivalentSpellings(t *testing.T) {
	for _, pattern := range []string{
		"POST /ingest/{sid}",                  // same shape, differently-named wildcard
		"/ingest/{source_id}",                 // no method -- matches POST along with everything else
		"POST /ingest/{source_id}/",           // subtree (trailing slash)
		"POST example.com/ingest/{source_id}", // host-qualified
	} {
		t.Run(pattern, func(t *testing.T) {
			if !isPluginAuthExemptPattern(pattern) {
				t.Errorf("isPluginAuthExemptPattern(%q) = false, want true -- it overlaps "+
					"%q and must be flagged so plugin.MaxBodyFromConfig refuses it", pattern,
					"POST /ingest/{source_id}")
			}
		})
	}
}

// TestIsPluginAuthExemptPatternControl is the negative control for the test
// above: routes with no real relationship to any exempt pattern must not be
// flagged, or every route in the tree would be forced onto plugin.MaxBody
// and MaxBodyFromConfig would be useless.
func TestIsPluginAuthExemptPatternControl(t *testing.T) {
	for _, pattern := range []string{
		"PUT /blobs/{key...}",   // blobstore's real route -- different path entirely
		"GET /oauth/providers",  // literal path one segment shorter than the oauth patterns
		"POST /webhooks/stripe", // unrelated plugin route
		"GET /healthz",
	} {
		t.Run(pattern, func(t *testing.T) {
			if isPluginAuthExemptPattern(pattern) {
				t.Errorf("isPluginAuthExemptPattern(%q) = true, want false -- it has no real "+
					"overlap with any entry in pluginAuthExemptPatterns", pattern)
			}
		})
	}
}

// TestIsPluginAuthExemptPatternMatchesEveryCanonicalEntry is the sanity
// check that overlap detection still recognizes each canonical pattern
// against itself -- a regression here would mean the rewrite from exact
// string equality to overlap-based matching lost the exact-match case it
// started from.
func TestIsPluginAuthExemptPatternMatchesEveryCanonicalEntry(t *testing.T) {
	for _, pattern := range pluginAuthExemptPatterns {
		t.Run(pattern, func(t *testing.T) {
			if !isPluginAuthExemptPattern(pattern) {
				t.Errorf("isPluginAuthExemptPattern(%q) = false, want true -- a canonical "+
					"entry must always match itself", pattern)
			}
		})
	}
}

// TestMaxBodyFromConfigPanicNamesTheRegisteringPlugin is cleat#2279 item 1's
// other half: the panic must name which plugin to go fix, not just the
// pattern -- main.go's RegisterRoutes loop registers many plugins through
// the SAME pluginBodyLimitRouter instance, so without this, a reader has no
// way to tell which plugin's RegisterRoutes call is the offending one short
// of bisecting plugList.
func TestMaxBodyFromConfigPanicNamesTheRegisteringPlugin(t *testing.T) {
	const pluginName = "some-offending-plugin"
	const pattern = "POST /ingest/{source_id}"

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Handle did not panic")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, pluginName) {
			t.Errorf("panic message %q does not name the registering plugin %q", msg, pluginName)
		}
	}()

	router := &pluginBodyLimitRouter{defaultLimit: 64, currentPlugin: pluginName}
	router.mux = http.NewServeMux()
	router.Handle(pattern, plugin.MaxBodyFromConfig(1024, "some_setting", handlerReadsBody()))
	t.Fatal("unreachable: Handle should have panicked before returning")
}

// TestMaxBodyFromConfigPanicWithNoCurrentPluginSetIsStillInformative covers
// currentPlugin's zero value -- a test or an unwired call site must not get
// a panic message that looks like it successfully named an empty plugin.
func TestMaxBodyFromConfigPanicWithNoCurrentPluginSetIsStillInformative(t *testing.T) {
	const pattern = "POST /ingest/{source_id}"

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Handle did not panic")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "unknown") {
			t.Errorf("panic message %q does not say the plugin name is unknown", msg)
		}
	}()

	router := &pluginBodyLimitRouter{defaultLimit: 64}
	router.mux = http.NewServeMux()
	router.Handle(pattern, plugin.MaxBodyFromConfig(1024, "some_setting", handlerReadsBody()))
	t.Fatal("unreachable: Handle should have panicked before returning")
}
