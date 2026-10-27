package notifications

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

// removeLeakedWebhookSecrets deletes the signing secrets this package's route
// tests created through handleCreateWebhook on the shared database. cleat#3474.
// Defer it AFTER `defer be.Cleanup()`. The prefix is taken from
// WebhookSecretName itself so a rename cannot leave this deleting nothing.
func removeLeakedWebhookSecrets(t *testing.T, be testutil.PluginTestBackend) {
	t.Helper()
	prefix := strings.TrimSuffix(WebhookSecretName(uuid.Nil), uuid.Nil.String())
	plugintest.RemoveTenantSecretsWithPrefix(t, be.DB, be.Dialect, engine.DefaultTenantUUID, prefix)
}
