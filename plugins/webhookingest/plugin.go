// Package webhookingest receives inbound webhooks from external services
// (GitHub, Stripe, etc.), storing them for a workflow to claim. It manages
// webhook sources and events with tenant isolation, HMAC signature
// verification, and a workflow-callable await_webhook host function that
// claims a matching event through eventtriggers' correlated key-slot
// mechanism (cleat#2649). There is no push-delivery path -- retired in
// cleat#2689, see migrations.go v10 and CHANGELOG's UPGRADE NOTES.
package webhookingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/cleat-team/cleat/plugin"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "webhook-ingest",
		Version:     "0.1.0",
		Description: "Receive inbound webhooks for a workflow to claim",
		Author:      "cleat",
		Requires:    []string{"event-triggers"},
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// New creates a new Plugin instance.
func New() plugin.Plugin {
	return &Plugin{}
}

// Plugin implements inbound webhook ingestion with source management,
// HMAC verification, and workflow-accessible event polling.
type Plugin struct {
	db      plugin.PluginDB
	mux     plugin.Dispatcher
	logger  *slog.Logger
	dialect plugin.Dialect
	config  Config
	env     *plugin.Environment
	secrets plugin.Secrets
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
		Description: "Receive inbound webhooks for a workflow to claim",
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
	p.dialect = env.Dialect
	p.secrets = env.Secrets

	// Parse optional config.
	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &p.config); err != nil {
			return fmt.Errorf("webhook-ingest: invalid config: %w", err)
		}
	}

	p.logger.Info("webhook-ingest: initialized")
	return nil
}
