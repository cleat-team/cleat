package webhookingest

import (
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// removeLeakedSourceSecrets deletes the signing secrets this package's route
// tests created through handleCreateSource on the shared database. cleat#3474.
// Defer it AFTER `defer be.Cleanup()`. The prefix is taken from
// WebhookIngestSecretName itself so a rename cannot leave this deleting nothing.
func removeLeakedSourceSecrets(t *testing.T, be testutil.PluginTestBackend) {
	t.Helper()
	plugintest.RemoveTenantSecretsWithPrefix(t, be.DB, be.Dialect, engine.DefaultTenantUUID, secretPrefix())
}

// secretPrefix is what every secret this package's routes create starts with.
func secretPrefix() string {
	return strings.TrimSuffix(WebhookIngestSecretName(uuid.Nil), uuid.Nil.String())
}

// TestMain fails the package if any of those secrets is left on a shared
// database after its tests ran, whatever order the packages run in
// (cleat#3484). The per-test cleanup above is what keeps this quiet; this is
// what says so when a new test forgets it.
func TestMain(m *testing.M) {
	os.Exit(plugintest.RunAndCheckTenantSecretLeaks(m, secretPrefix()))
}
