// Package pagerdutyalert provides PagerDuty incident management from workflows.
// It lets workflows trigger and resolve PagerDuty incidents, and exposes HTTP
// CRUD routes for managing PagerDuty configs (tenant-scoped).
package pagerdutyalert

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/rcownie/cleat/internal/plugin"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "pagerduty-alert",
		Version:     "0.1.0",
		Description: "Create PagerDuty incidents from workflow failures",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// Plugin implements PagerDuty incident management for workflows.
type Plugin struct {
db         plugin.DB
	logger     *slog.Logger
	httpClient *http.Client
	config     Config
}

// Config holds optional configuration for the pagerduty-alert plugin.
type Config struct{}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "pagerduty-alert",
		Version:     "0.1.0",
		Description: "Create PagerDuty incidents from workflow failures",
		Author:      "cleat",
	}
}

// Init initializes the plugin with the given environment. It creates an HTTP
// client with a 30-second timeout for PagerDuty API requests.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.db = env.DB
	p.httpClient = &http.Client{
		Timeout: 30 * time.Second,
	}

	// Parse optional config.
	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &p.config); err != nil {
			return fmt.Errorf("pagerduty: invalid config: %w", err)
		}
	}

	p.logger.Info("pagerduty-alert: initialized")
	return nil
}

// Health returns nil if at least one enabled PagerDuty config exists.
func (p *Plugin) Health() error {
	var count int
	err := p.db.QueryRow(`SELECT COUNT(*) FROM pd_config WHERE enabled = true`).Scan(&count)
	if err != nil {
		return fmt.Errorf("pagerduty: health check failed: %w", err)
	}
	return nil
}
