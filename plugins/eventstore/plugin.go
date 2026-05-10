// Package eventstore provides append-only event streams with Server-Sent Events
// (SSE) support. It demonstrates all plugin API patterns: HTTP routes, database
// migrations, and background workers.
//
// Events are stored in the event_stream table with tenant isolation. Each event
// is a JSONB blob with an auto-incrementing sequence number per (tenant, stream).
package eventstore

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
		Name:        "eventstore",
		Version:     "0.1.0",
		Description: "Append-only event streams with SSE",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// Plugin implements append-only event streams with tenant isolation.
type Plugin struct {
db     plugin.PluginDB
	mux    *http.ServeMux
	logger *slog.Logger
	config Config
	dialect plugin.Dialect
}

// Config controls eventstore parameters.
type Config struct {
	MaxEventSize  int `json:"max_event_size"`  // max event body bytes; default 1 MB
	RetentionDays int `json:"retention_days"`  // days to keep events; <=0 disables cleanup, default 30
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "eventstore",
		Version:     "0.1.0",
		Description: "Append-only event streams with SSE",
		Author:      "cleat",
	}
}

// Init initializes the plugin with the given environment. It parses optional
// configuration and sets safe defaults for dev/testing.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.db = env.DB
	p.mux = env.Mux
	p.dialect = env.Dialect

	// Parse config. If no config provided, use safe defaults.
	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &p.config); err != nil {
			return fmt.Errorf("eventstore: invalid config: %w", err)
		}
	}
	if p.config.MaxEventSize == 0 {
		p.config.MaxEventSize = 1 * 1024 * 1024 // 1 MB default
	}

	p.logger.Info("eventstore: initialized",
		"max_event_size", p.config.MaxEventSize,
	)
	return nil
}
