// Package scheduler provides user-managed cron schedules that trigger workflow
// executions. It generalizes the built-in cron with a schedules table, HTTP API,
// CLI commands, and a background ticker that evaluates due schedules every 60s.
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/cleat-team/cleat/plugin"
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

// New creates a new Plugin instance.
func New() plugin.Plugin {
	return &Plugin{}
}

// Plugin implements user-managed cron schedules with tenant isolation.
type Plugin struct {
db     plugin.PluginDB
	env    *plugin.Environment
	mux    *http.ServeMux
	logger *slog.Logger
	dialect plugin.Dialect
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
	p.dialect = env.Dialect
	p.mux = env.Mux

	if len(env.Config) > 0 {
		var cfg struct{}
		if err := json.Unmarshal(env.Config, &cfg); err != nil {
			return fmt.Errorf("scheduler: invalid config: %w", err)
		}
	}

	if err := p.checkDBVersion(ctx); err != nil {
		return err
	}

	p.logger.Info("scheduler: initialized")
	return nil
}

// checkDBVersion verifies the database meets minimum requirements.
// MySQL 8.0+ is required for FOR UPDATE SKIP LOCKED.
func (p *Plugin) checkDBVersion(ctx context.Context) error {
	if p.db == nil || p.dialect != plugin.DialectMySQL {
		return nil
	}
	var version string
	if err := p.db.QueryRow(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return fmt.Errorf("scheduler: failed to check MySQL version: %w", err)
	}
	major := 0
	for _, part := range strings.Split(version, ".") {
		if n, err := fmt.Sscanf(part, "%d", &major); err == nil && n == 1 {
			break
		}
	}
	if major < 8 {
		return fmt.Errorf("scheduler: MySQL 8.0+ is required (found %s). FOR UPDATE SKIP LOCKED is not available in older MySQL versions", version)
	}
	return nil
}
