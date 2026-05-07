// Package scheduler provides user-managed cron schedules that trigger workflow
// executions. It generalizes the built-in cron with a schedules table, HTTP API,
// CLI commands, and a background ticker that evaluates due schedules every 60s.
package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/rcownie/cleat/internal/plugin"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "scheduler",
		Version:     "0.1.0",
		Description: "User-managed cron schedules for workflow execution",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// Plugin implements user-managed cron schedules with tenant isolation.
type Plugin struct {
	db     *sql.DB
	env    *plugin.Environment
	mux    *http.ServeMux
	logger *slog.Logger
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "scheduler",
		Version:     "0.1.0",
		Description: "User-managed cron schedules for workflow execution",
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

	p.env = env
	p.db = env.DB
	p.mux = env.Mux

	if len(env.Config) > 0 {
		var cfg struct{}
		if err := json.Unmarshal(env.Config, &cfg); err != nil {
			return fmt.Errorf("scheduler: invalid config: %w", err)
		}
	}

	p.logger.Info("scheduler: initialized")
	return nil
}
