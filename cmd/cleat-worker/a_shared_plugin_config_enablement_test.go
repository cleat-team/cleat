package main

// cleat#1992 part 1, cleat-review's #2202 pass, the BROKEN finding: every
// plugin's Init receives the SAME raw --plugin-config bytes (main.go builds
// one Environment and copies it per plugin, Config included) -- there is no
// per-plugin section. Before the fix, email-notify read ANY non-empty config
// as "configured", so a worker set up only for llm -- or any other plugin --
// made len(env.Config) == 0 false and email read itself as enabled, then
// refused the whole worker at startup over a missing
// email.sendgrid_api_key nobody asked for.
//
// a_required_deployment_secret_startup_check_test.go already covers
// checkRequiredDeploymentSecrets in isolation, against hand-built fake
// plugins -- exactly the gap cleat-review named: "your startup test builds
// plugins individually, so it misses this". THIS test instead runs the REAL,
// registered email-notify plugin's own Init (discovered via plugin.Discover(),
// the same registry main.go's plugin loop iterates), so it fails if the fix
// is ever narrowed to only the hand-built case above.

import (
	"context"
	"errors"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// findPlugin returns a fresh instance of the registered plugin named name,
// or fails the test -- so a rename of "email-notify" or an import block that
// stops linking it fails this test by name instead of the loop below running
// zero iterations and passing vacuously.
func findPlugin(t *testing.T, loaded []*plugin.LoadedPlugin, name string) plugin.Plugin {
	t.Helper()
	for _, lp := range loaded {
		if lp.Plugin.Info().Name == name {
			return lp.Plugin
		}
	}
	t.Fatalf("plugin %q is not registered -- plugin.Discover() returned %d plugins, "+
		"none named %q; is it still blank-imported in main.go?", name, len(loaded), name)
	return nil
}

// TestASharedPluginConfigMeantForAnotherPluginDoesNotEnableEmail is the
// direct regression test: an llm-only config, handed to the REAL
// email-notify plugin's Init exactly as main.go's shared envCopy.Config
// does, must report plugin.ErrNotConfigured -- not read config presence
// alone as "this is for me".
func TestASharedPluginConfigMeantForAnotherPluginDoesNotEnableEmail(t *testing.T) {
	loaded, err := plugin.Discover()
	if err != nil {
		t.Fatalf("discovering registered plugins: %v", err)
	}
	email := findPlugin(t, loaded, "email-notify")

	llmOnlyConfig := []byte(`{"providers":{"openai":{"enabled":true}}}`)
	env := &plugin.Environment{Config: llmOnlyConfig}

	err = email.Init(context.Background(), env)
	if err == nil {
		t.Fatal("email-notify.Init() succeeded against a config section meant for llm; " +
			"it should read plugin.ErrNotConfigured, the same as no config at all")
	}
	if !errors.Is(err, plugin.ErrNotConfigured) {
		t.Errorf("email-notify.Init() against an llm-only config: got %v, want errors.Is(err, plugin.ErrNotConfigured) "+
			"-- any OTHER error marks the plugin unhealthy without the quiet-disable main.go's Init loop "+
			"gives ErrNotConfigured, which is the wrong outcome for a config that simply is not this plugin's", err)
	}
}

// TestASharedPluginConfigNamingEmailEnabledDoesEnableEmail is the positive
// control for the test above: a config carrying BOTH llm's section and
// email_enabled must still enable email, proving the refusal above is about
// the missing field, not about the presence of another plugin's section.
func TestASharedPluginConfigNamingEmailEnabledDoesEnableEmail(t *testing.T) {
	loaded, err := plugin.Discover()
	if err != nil {
		t.Fatalf("discovering registered plugins: %v", err)
	}
	email := findPlugin(t, loaded, "email-notify")

	mixedConfig := []byte(`{"providers":{"openai":{"enabled":true}},"email_enabled":true}`)
	env := &plugin.Environment{Config: mixedConfig}

	if err := email.Init(context.Background(), env); err != nil {
		t.Fatalf("email-notify.Init() against a config naming both llm's section and "+
			"email_enabled: %v, want nil", err)
	}
}
