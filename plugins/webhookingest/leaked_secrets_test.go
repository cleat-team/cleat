package webhookingest

import (
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
	prefix := strings.TrimSuffix(WebhookIngestSecretName(uuid.Nil), uuid.Nil.String())
	plugintest.RemoveTenantSecretsWithPrefix(t, be.DB, be.Dialect, engine.DefaultTenantUUID, prefix)
}
