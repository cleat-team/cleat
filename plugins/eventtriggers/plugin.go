// Package eventtriggers provides event-driven workflow triggers.
// It lets workflows subscribe to domain events via HTTP API,
// evaluates filter expressions, and automatically starts workflow
// instances when matching events are published.
package eventtriggers

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/rcownie/durable/internal/plugin"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "event-triggers",
		Version:     "0.1.0",
		Description: "Event-driven workflow triggers — subscribe workflows to domain events",
		Author:      "cleat",
	}, func() plugin.Plugin { return &Plugin{} })
}

// Plugin implements event-driven workflow triggers with tenant-isolated
// event subscriptions, idempotent event ingestion, and filter expressions.
type Plugin struct {
	db     *sql.DB
	mux    *http.ServeMux
	logger *slog.Logger
	env    *plugin.Environment
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "event-triggers",
		Version:     "0.1.0",
		Description: "Event-driven workflow triggers — subscribe workflows to domain events",
		Author:      "cleat",
	}
}

// Init initializes the plugin with the given environment.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.db = env.DB
	p.mux = env.Mux
	p.env = env

	p.logger.Info("event-triggers: initialized")
	return nil
}
