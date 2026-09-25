package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// probeRouter builds a pluginBodyLimitRouter with defaultLimit and a single
// route registered via register, then fires a request of size n bytes at
// path and returns the response code and body.
func probeRouter(t *testing.T, defaultLimit int64, pattern, path string,
	register func(r *pluginBodyLimitRouter), n int) (int, string) {
	t.Helper()
	plugMux := http.NewServeMux()
	router := &pluginBodyLimitRouter{mux: plugMux, defaultLimit: defaultLimit}
	register(router)

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(strings.Repeat("x", n)))
	w := httptest.NewRecorder()
	plugMux.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func handlerReadsBody() func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := plugin.ReadBody(w, r); !ok {
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// TestMaxBodyEffectiveLimitIsTheMinimumWithTheFlag is cleat#2273's guard
// rail 2, for plugin.MaxBody specifically: the adapter must compute
// min(declared, defaultLimit) in BOTH directions, and the 413 must always
// name --plugin-max-body-size regardless of which value actually bound --
// this is also the mutation-coverage case for "MaxBodyLimit ignored" (a
// mutant that never reads plugin.MaxBodyLimit would pass the
// flag-below-declared case at the wrong limit).
func TestMaxBodyEffectiveLimitIsTheMinimumWithTheFlag(t *testing.T) {
	const pattern = "POST /x"
	const path = "/x"
	const declared = 100

	register := func(r *pluginBodyLimitRouter) {
		r.Handle(pattern, plugin.MaxBody(declared, handlerReadsBody()))
	}

	for _, tc := range []struct {
		name          string
		defaultLimit  int64
		wantEffective int64
	}{
		{"flag below declared: flag wins", 40, 40},
		{"flag above declared: declared wins", 400, declared},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Control: a body one byte under the effective limit succeeds.
			if code, body := probeRouter(t, tc.defaultLimit, pattern, path, register, int(tc.wantEffective)-1); code != http.StatusOK {
				t.Fatalf("control: %d-byte body got %d, want 200 (body: %s)", tc.wantEffective-1, code, body)
			}
			// A body one byte over the effective limit is refused, naming
			// the effective limit and --plugin-max-body-size -- never
			// "declared" unconditionally, and never a bare default that
			// ignores the route's own tighter cap.
			code, body := probeRouter(t, tc.defaultLimit, pattern, path, register, int(tc.wantEffective)+1)
			if code != http.StatusRequestEntityTooLarge {
				t.Fatalf("%d-byte body got %d, want 413 (body: %s)", tc.wantEffective+1, code, body)
			}
			if want := fmt.Sprintf("%d bytes", tc.wantEffective); !strings.Contains(body, want) {
				t.Errorf("413 body %q does not name the effective limit %s", body, want)
			}
			if !strings.Contains(body, "--plugin-max-body-size") {
				t.Errorf("413 body %q does not name --plugin-max-body-size", body)
			}
		})
	}
}

// TestMaxBodyFromConfigIgnoresTheFlagInBothDirections is cleat#2273's guard
// rail 2 for plugin.MaxBodyFromConfig: the effective limit is the declared
// value unconditionally, whether the flag is set below or above it, and the
// 413 names the plugin's own knob -- never --plugin-max-body-size, which
// would send an operator to the wrong setting.
func TestMaxBodyFromConfigIgnoresTheFlagInBothDirections(t *testing.T) {
	const pattern = "POST /x" // not one of pluginAuthExemptPatterns
	const path = "/x"
	const declared = 100
	const knob = "some_plugin_setting"

	register := func(r *pluginBodyLimitRouter) {
		r.Handle(pattern, plugin.MaxBodyFromConfig(declared, knob, handlerReadsBody()))
	}

	for _, tc := range []struct {
		name         string
		defaultLimit int64
	}{
		{"flag below declared", 40},
		{"flag above declared", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := probeRouter(t, tc.defaultLimit, pattern, path, register, declared-1); code != http.StatusOK {
				t.Fatalf("control: %d-byte body got %d, want 200 (body: %s)", declared-1, code, body)
			}
			code, body := probeRouter(t, tc.defaultLimit, pattern, path, register, declared+1)
			if code != http.StatusRequestEntityTooLarge {
				t.Fatalf("%d-byte body got %d, want 413 (body: %s) -- the flag must not move this route's ceiling",
					declared+1, code, body)
			}
			if want := fmt.Sprintf("%d bytes", declared); !strings.Contains(body, want) {
				t.Errorf("413 body %q does not name the declared limit %s, want it unmoved by the flag", body, want)
			}
			if !strings.Contains(body, knob) {
				t.Errorf("413 body %q does not name %q", body, knob)
			}
			if strings.Contains(body, "--plugin-max-body-size") {
				t.Errorf("413 body %q names --plugin-max-body-size, which is not the knob that moves this route's limit", body)
			}
		})
	}
}

// TestMaxBodyFromConfigOnAnExemptPatternPanicsAtRegistration is cleat#2273's
// guard rail 1, per cleat-review's final call: a plugin registering
// MaxBodyFromConfig on one of pluginAuthExemptPatterns is a plugin bug, and
// the host refuses at registration time (panic; RegisterRoutes' caller in
// main.go has no recover() around it, so this refuses to boot) rather than
// silently downgrading to the default ceiling. A silent downgrade was tried
// first and rejected on review: it protects the operator's flag either way,
// but nothing would ever tell a plugin author their MaxBodyFromConfig call
// was quietly ignored.
func TestMaxBodyFromConfigOnAnExemptPatternPanicsAtRegistration(t *testing.T) {
	const pattern = "POST /ingest/{source_id}" // IS one of pluginAuthExemptPatterns

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Handle did not panic for MaxBodyFromConfig registered on an auth-exempt pattern")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, pattern) {
			t.Errorf("panic message %q does not name the offending pattern", msg)
		}
		if !strings.Contains(msg, "MaxBody") {
			t.Errorf("panic message %q does not say which constructor to use instead", msg)
		}
	}()

	plugMux := http.NewServeMux()
	router := &pluginBodyLimitRouter{mux: plugMux, defaultLimit: 64}
	router.Handle(pattern, plugin.MaxBodyFromConfig(10*1024*1024, "some_plugin_setting", handlerReadsBody()))
	t.Fatal("unreachable: Handle should have panicked before returning")
}

// TestMaxBodyFromConfigOnANonExemptPatternDoesNotPanic is the control for the
// test above: MaxBodyFromConfig is exactly what a legitimate route (like
// blobstore's PUT) is meant to use, and registering it on any pattern that
// is NOT auth-exempt must not panic.
func TestMaxBodyFromConfigOnANonExemptPatternDoesNotPanic(t *testing.T) {
	const pattern = "PUT /blobs/{key...}" // not one of pluginAuthExemptPatterns
	plugMux := http.NewServeMux()
	router := &pluginBodyLimitRouter{mux: plugMux, defaultLimit: 64}
	router.Handle(pattern, plugin.MaxBodyFromConfig(10*1024*1024, "max_blob_size", handlerReadsBody()))
}

// TestPluginBodyLimitRouterDefaultLimitComesFromThePluginFlag is the mutation
// case for "defaultLimit wired to the wrong flag": the value main.go passes
// as pluginBodyLimitRouter.defaultLimit must be *pluginMaxBodySize, not the
// core API's own *maxBodySize -- both exist in this package, and the two are
// meant to be independently configurable (see pluginMaxBodySize's own flag
// doc). This is a source check, not a behavioural one, for the same reason
// a_slack_interactive_route_is_exempt_test.go is: the wiring lives inside
// main()'s own body, unreachable from any test that does not literally run
// the binary.
func TestPluginBodyLimitRouterDefaultLimitComesFromThePluginFlag(t *testing.T) {
	src := readMainGoSource(t)
	if !strings.Contains(src, "defaultLimit: *pluginMaxBodySize") {
		t.Fatal("main.go no longer constructs pluginBodyLimitRouter with defaultLimit: *pluginMaxBodySize -- " +
			"either the field was renamed (update this check) or it was wired to the wrong flag, which would " +
			"make --plugin-max-body-size silently do nothing")
	}
}

func readMainGoSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}
	return string(src)
}
