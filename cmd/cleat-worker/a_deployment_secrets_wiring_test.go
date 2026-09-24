package main

// GAP 2 and GAP 3 from cleat-review's #2202 re-check.
//
// GAP 2: the prefix-scoping adapter (plugin.HasDeploymentSecretPrefix) was
// optional but PERMISSIVE -- a plugin that declared no prefix got the one
// UNSCOPED adapter every plugin shared, exactly as before the interface
// existed. deploymentSecretsForPlugin (main.go) is now default-DENY: a
// plugin that declares no prefix gets nil.
//
// GAP 3: none of that was under test. Deleting the main.go block that built
// the scoped adapter, or deleting either email's or llm's
// DeploymentSecretPrefix method, left every test green -- the scoped-adapter
// TYPE itself was tested (engine/scoped_deployment_secrets_test.go), but
// nothing tested that a real plugin's declared prefix actually reaches it.
// These tests run against the REAL registered plugins via plugin.Discover(),
// not hand-built fakes, so deleting either method changes what
// deploymentSecretsForPlugin returns for that plugin and fails the
// corresponding test here.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// fakeUnscopedDeploymentSecrets implements plugin.DeploymentSecrets (Get),
// distinct from fakeDeploymentSecretGetter in
// a_required_deployment_secret_startup_check_test.go, which implements
// checkRequiredDeploymentSecrets' own deploymentSecretGetter interface
// (GetDeploymentSecret) -- deploymentSecretsForPlugin wraps the former, the
// one every plugin's Environment.DeploymentSecrets actually is.
type fakeUnscopedDeploymentSecrets struct {
	values map[string]string
}

func (f *fakeUnscopedDeploymentSecrets) Get(ctx context.Context, name string) (string, error) {
	if v, ok := f.values[name]; ok {
		return v, nil
	}
	return "", errors.New("deployment secret not found")
}

// TestDeploymentSecretsForPluginIsScopedByDeclaredPrefix proves email, llm
// and scheduled-backup -- the plugins that declare a deployment-secret prefix
// today -- each get a DeploymentSecrets that resolves their OWN prefix and
// refuses the others'. Deleting any one plugin's DeploymentSecretPrefix
// method makes the corresponding block of this test fail: the plugin stops
// implementing plugin.HasDeploymentSecretPrefix, so deploymentSecretsForPlugin
// returns nil instead of a scoped adapter.
//
// scheduled-backup's own case is the one cleat-review's #2236 GAP flagged:
// it does not implement plugin.HasRequiredDeploymentSecrets (deliberately --
// see legacyScheduledBackupConfig's doc comment, plugins/scheduledbackup),
// so nothing at boot ever fails if DeploymentSecretPrefix went missing --
// backupDSN would just always error with "no deployment secret store
// configured" in production, exactly as if p.deploymentSecrets were nil,
// and every one of scheduledbackup's own package-level tests would stay
// green regardless, since they construct Plugin directly rather than going
// through this wiring. This test is what actually exercises it.
func TestDeploymentSecretsForPluginIsScopedByDeclaredPrefix(t *testing.T) {
	loaded, err := plugin.Discover()
	if err != nil {
		t.Fatalf("discovering registered plugins: %v", err)
	}
	email := findPlugin(t, loaded, "email-notify")
	llm := findPlugin(t, loaded, "llm")
	scheduledBackup := findPlugin(t, loaded, "scheduled-backup")

	unscoped := &fakeUnscopedDeploymentSecrets{values: map[string]string{
		"email.sendgrid_api_key":       "sg-real",
		"llm.providers.openai.api_key": "sk-real",
		"scheduledbackup.dsn":          "postgres://real",
	}}

	got := deploymentSecretsForPlugin(email, unscoped)
	if got == nil {
		t.Fatal("email-notify declares plugin.HasDeploymentSecretPrefix; " +
			"deploymentSecretsForPlugin returned nil instead of a scoped adapter -- " +
			"either the wiring in main.go or email's DeploymentSecretPrefix method is gone")
	}
	if _, err := got.Get(context.Background(), "llm.providers.openai.api_key"); err == nil {
		t.Error("email's scoped DeploymentSecrets let it read llm's own key -- " +
			"prefix scoping is not actually enforced")
	}
	if v, err := got.Get(context.Background(), "email.sendgrid_api_key"); err != nil || v != "sg-real" {
		t.Errorf("email's own prefix should still resolve: got %q, %v", v, err)
	}

	got2 := deploymentSecretsForPlugin(llm, unscoped)
	if got2 == nil {
		t.Fatal("llm declares plugin.HasDeploymentSecretPrefix; " +
			"deploymentSecretsForPlugin returned nil instead of a scoped adapter -- " +
			"either the wiring in main.go or llm's DeploymentSecretPrefix method is gone")
	}
	if _, err := got2.Get(context.Background(), "email.sendgrid_api_key"); err == nil {
		t.Error("llm's scoped DeploymentSecrets let it read email's own key -- " +
			"prefix scoping is not actually enforced")
	}
	if v, err := got2.Get(context.Background(), "llm.providers.openai.api_key"); err != nil || v != "sk-real" {
		t.Errorf("llm's own prefix should still resolve: got %q, %v", v, err)
	}

	got3 := deploymentSecretsForPlugin(scheduledBackup, unscoped)
	if got3 == nil {
		t.Fatal("scheduled-backup declares plugin.HasDeploymentSecretPrefix; " +
			"deploymentSecretsForPlugin returned nil instead of a scoped adapter -- " +
			"either the wiring in main.go or scheduledbackup's DeploymentSecretPrefix method is gone")
	}
	if _, err := got3.Get(context.Background(), "email.sendgrid_api_key"); err == nil {
		t.Error("scheduled-backup's scoped DeploymentSecrets let it read email's own key -- " +
			"prefix scoping is not actually enforced")
	}
	if v, err := got3.Get(context.Background(), "scheduledbackup.dsn"); err != nil || v != "postgres://real" {
		t.Errorf("scheduled-backup's own prefix should still resolve: got %q, %v", v, err)
	}
}

// TestDeploymentSecretsForPluginDefaultsToNilForAPluginThatDeclaresNoPrefix
// is GAP 2 directly: a plugin that never implements
// plugin.HasDeploymentSecretPrefix must get NO deployment-secret access at
// all, not the unscoped adapter every plugin used to share. scheduler is
// used because it implements neither HasRequiredDeploymentSecrets nor
// HasDeploymentSecretPrefix today -- the ordinary case, most plugins.
func TestDeploymentSecretsForPluginDefaultsToNilForAPluginThatDeclaresNoPrefix(t *testing.T) {
	loaded, err := plugin.Discover()
	if err != nil {
		t.Fatalf("discovering registered plugins: %v", err)
	}
	scheduler := findPlugin(t, loaded, "scheduler")

	unscoped := &fakeUnscopedDeploymentSecrets{values: map[string]string{
		"email.sendgrid_api_key": "sg-real",
	}}

	if got := deploymentSecretsForPlugin(scheduler, unscoped); got != nil {
		t.Error("a plugin that declares no deployment-secret prefix must get nil " +
			"DeploymentSecrets (default-deny), not the unscoped adapter every plugin " +
			"used to share regardless of whether it read deployment secrets at all")
	}
}

// TestMainWiresDeploymentSecretsForPlugin is GAP 3's other half: the two
// tests above prove deploymentSecretsForPlugin itself behaves correctly, but
// prove nothing about whether main()'s per-plugin Init loop actually CALLS
// it -- reverting that one line back to
// "envCopy.DeploymentSecrets = pluginEnv.DeploymentSecrets" (the original
// unscoped-for-everyone bug) would leave both tests above green, since
// neither one exercises main() itself. main() runs a full worker startup
// (real flags, a real database) and is not practical to unit test directly,
// so this asserts the one fact that matters textually: the call site is
// still there. Anchored on the function name, not a line number, so it
// survives unrelated edits to the surrounding loop.
func TestMainWiresDeploymentSecretsForPlugin(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}
	if !strings.Contains(string(src), "envCopy.DeploymentSecrets = deploymentSecretsForPlugin(") {
		t.Error("main.go no longer calls deploymentSecretsForPlugin to build each plugin's " +
			"Environment.DeploymentSecrets -- deploymentSecretsForPlugin's own tests pass " +
			"whether or not anything actually calls it, so this checks the call site directly")
	}
}
