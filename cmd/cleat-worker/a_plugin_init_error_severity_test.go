package main

// cleat-review's #2202 re-check, third pass: main()'s per-plugin Init loop
// switched on `errors.Is` inline, so the FATAL case -- os.Exit(1) -- had no
// call site a test could exercise without actually killing the test
// process. Mutating `case errors.Is(err, plugin.ErrFatalMisconfiguration):`
// to `case false:` left every test green: the mutated build still compiled
// and ran, the fatal case fell to the default arm (an ordinary disable, ERROR
// log, no os.Exit), and nothing asserted on that outcome. A worker with a
// leftover sendgrid_api_key and no email_enabled would then BOOT with email
// off -- exactly the silent regression cleat#1992 part 1's fail-closed check
// exists to prevent, and exactly what the CHANGELOG's upgrade note says
// cannot happen.
//
// classifyPluginInitError (main.go) is the decision extracted out of that
// switch so it has a call site: TestClassifyPluginInitError exercises it
// directly, and TestMainSwitchesOnClassifyPluginInitError proves main()'s
// loop still switches on its result rather than reimplementing the
// `errors.Is` chain inline -- the same two-part shape (extracted decision +
// wiring proof) as deploymentSecretsForPlugin's tests in
// a_deployment_secrets_wiring_test.go, and for the same reason: neither
// alone proves the other.

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

func TestClassifyPluginInitError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want pluginInitSeverity
	}{
		{
			name: "not configured",
			err:  fmt.Errorf("email: %w", plugin.ErrNotConfigured),
			want: pluginInitDisabledQuiet,
		},
		{
			// The case the mutation above collapses into the default arm.
			name: "fatal misconfiguration",
			err:  fmt.Errorf("email: leftover key: %w", plugin.ErrFatalMisconfiguration),
			want: pluginInitFatal,
		},
		{
			name: "ordinary error",
			err:  errors.New("dial tcp: connection refused"),
			want: pluginInitDisabledLoud,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPluginInitError(tc.err); got != tc.want {
				t.Errorf("classifyPluginInitError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestMainSwitchesOnClassifyPluginInitError is the wiring half:
// TestClassifyPluginInitError above proves the function classifies
// correctly, but proves nothing about whether main()'s Init loop actually
// calls it -- reverting the switch back to an inline `errors.Is` chain would
// leave that test green, since it never touches main(). Anchored on the
// three case labels rather than a line number, so it survives unrelated
// edits to the surrounding loop.
func TestMainSwitchesOnClassifyPluginInitError(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}
	s := string(src)
	if !strings.Contains(s, "switch classifyPluginInitError(err) {") {
		t.Error("main.go's Init loop no longer switches on classifyPluginInitError(err) -- " +
			"classifyPluginInitError's own tests pass whether or not anything actually " +
			"calls it, so this checks the call site directly")
	}
	for _, want := range []string{"case pluginInitDisabledQuiet:", "case pluginInitFatal:"} {
		if !strings.Contains(s, want) {
			t.Errorf("main.go's Init loop no longer has a %q case", want)
		}
	}
}
