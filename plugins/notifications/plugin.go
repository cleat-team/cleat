// Package notifications provides webhook delivery with retry and delivery
// tracking. It demonstrates all plugin API patterns: host functions, HTTP
// routes, database migrations, and background workers.
//
// Webhooks are HTTP POST deliveries with HMAC-SHA256 signing. Retries use
// exponential backoff (1m, 5m, 15m, 1h) up to 10 attempts, at which point
// the delivery is marked as failed.
package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/cleat-team/cleat/plugin"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "notifications",
		Version:     "0.1.0",
		Description: "Webhook delivery with retry and delivery tracking",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// New creates a new Plugin instance.
func New() plugin.Plugin {
	return &Plugin{}
}

// Plugin implements webhook delivery with retry and delivery tracking.
type Plugin struct {
	db         plugin.PluginDB
	logger     *slog.Logger
	httpClient *http.Client
	config     Config
	dialect    plugin.Dialect
}

// Config controls notifications plugin behaviour.
type Config struct {
	// No specific configuration options currently.
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "notifications",
		Version:     "0.1.0",
		Description: "Webhook delivery with retry and delivery tracking",
		Author:      "cleat",
	}
}

// Init initializes the plugin with the given environment. It parses optional
// configuration and creates an HTTP client with a 30-second timeout.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.db = env.DB
	p.dialect = env.Dialect
	p.httpClient = &http.Client{
		Timeout: 30 * time.Second,
	}

	// Parse optional config.
	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &p.config); err != nil {
			return fmt.Errorf("notifications: invalid config: %w", err)
		}
	}

	p.logger.Info("notifications: initialized")
	return nil
}
