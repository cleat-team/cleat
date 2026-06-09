// Package datadogexport exports workflow metrics to Datadog. It manages
// per-tenant Datadog API configurations and periodically queries workflow
// statistics (count by status) and sends them as gauge metrics to the
// Datadog Metrics API.
package datadogexport

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "datadog-export",
		Version:     "0.1.0",
		Description: "Export workflow metrics to Datadog",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// New creates a new Plugin instance.
func New() plugin.Plugin {
	return &Plugin{}
}

// Plugin exports workflow metrics to Datadog.
type Plugin struct {
	db         plugin.PluginDB
	logger     *slog.Logger
	httpClient *http.Client
	config     Config
	dialect    plugin.Dialect
	workerID   string
}

// Config holds optional configuration for the datadog-export plugin.
type Config struct{}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "datadog-export",
		Version:     "0.1.0",
		Description: "Export workflow metrics to Datadog",
		Author:      "cleat",
	}
}

// Init initializes the plugin with the given environment. It creates an HTTP
// client with a 30-second timeout for Datadog API requests.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.db = env.DB
	p.dialect = env.Dialect
	p.workerID = uuid.New().String()
	p.httpClient = &http.Client{
		Timeout: 30 * time.Second,
	}

	// Parse optional config.
	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &p.config); err != nil {
			return fmt.Errorf("datadog-export: invalid config: %w", err)
		}
	}

	p.logger.Info("datadog-export: initialized")
	return nil
}
