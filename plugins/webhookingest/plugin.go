// Package webhookingest receives inbound webhooks from external services
// (GitHub, Stripe, etc.) and delivers them as workflow signals. It manages
// webhook sources and events with tenant isolation, HMAC signature
// verification, and a workflow-callable await_webhook host function.
package webhookingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/rcownie/cleat/internal/plugin"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "webhook-ingest",
		Version:     "0.1.0",
		Description: "Receive inbound webhooks and deliver as workflow signals",
		Author:      "cleat",
		Requires:    []string{"event-triggers"},
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// Plugin implements inbound webhook ingestion with source management,
// HMAC verification, and workflow-accessible event polling.
type Plugin struct {
db     plugin.DB
	mux    *http.ServeMux
	logger *slog.Logger
	config Config
	env    *plugin.Environment
}

// Config controls webhook-ingest plugin behaviour.
type Config struct {
	// No specific configuration options currently.
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "webhook-ingest",
		Version:     "0.1.0",
		Description: "Receive inbound webhooks and deliver as workflow signals",
		Author:      "cleat",
	}
}

// Init initializes the plugin with the given environment. It parses optional
// configuration and sets up internal state.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.db = env.DB
	p.mux = env.Mux
	p.env = env

	// Parse optional config.
	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &p.config); err != nil {
			return fmt.Errorf("webhook-ingest: invalid config: %w", err)
		}
	}

	p.logger.Info("webhook-ingest: initialized")
	return nil
}
