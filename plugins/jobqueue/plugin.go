// Package jobqueue provides a standalone job queue. Jobs are enqueued via CLI
// and executed as workflows. The plugin adds a task_queue table, HTTP endpoints
// for job CRUD, a CLI enqueue command, and a background worker that polls for
// pending jobs and logs dispatch events (actual workflow dispatch comes later).
package jobqueue

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/rcownie/durable/internal/plugin"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "jobqueue",
		Version:     "0.1.0",
		Description: "Standalone job queue",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// Plugin implements a standalone job queue with tenant isolation.
type Plugin struct {
	db     *sql.DB
	mux    *http.ServeMux
	logger *slog.Logger
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "jobqueue",
		Version:     "0.1.0",
		Description: "Standalone job queue",
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

	p.logger.Info("jobqueue: initialized")
	return nil
}
