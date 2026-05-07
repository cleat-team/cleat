// Package slacknotify provides Slack webhook integration for workflows.
// It lets workflows send Slack messages via incoming webhooks, and exposes
// HTTP CRUD routes for managing Slack configs (tenant-scoped).
package slacknotify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/rcownie/cleat/internal/plugin"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "slack-notify",
		Version:     "0.1.0",
		Description: "Send Slack messages from workflows",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// Plugin implements Slack webhook message sending for workflows.
type Plugin struct {
	db         *sql.DB
	logger     *slog.Logger
	httpClient *http.Client
	config     Config
}

// Config holds optional configuration for the slack-notify plugin.
type Config struct{}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "slack-notify",
		Version:     "0.1.0",
		Description: "Send Slack messages from workflows",
		Author:      "cleat",
	}
}

// Init initializes the plugin with the given environment. It creates an HTTP
// client with a 10-second timeout for Slack webhook requests.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.db = env.DB
	p.httpClient = &http.Client{
		Timeout: 10 * time.Second,
	}

	// Parse optional config.
	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &p.config); err != nil {
			return fmt.Errorf("slack-notify: invalid config: %w", err)
		}
	}

	p.logger.Info("slack-notify: initialized")
	return nil
}
