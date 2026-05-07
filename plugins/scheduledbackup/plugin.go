// Package scheduledbackup provides scheduled PostgreSQL backups with pg_dump.
// It supports cron-based scheduling, manual backup/restore via HTTP API and CLI
// commands, and records backup history in PostgreSQL.
package scheduledbackup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/rcownie/cleat/internal/plugin"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "scheduled-backup",
		Version:     "0.1.0",
		Description: "Scheduled PostgreSQL backups to S3",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// Plugin implements scheduled PostgreSQL backups with tenant isolation.
type Plugin struct {
	db     *sql.DB
	mux    *http.ServeMux
	logger *slog.Logger
	config Config
}

// Config controls backup storage and pg_dump output location.
type Config struct {
	DSN     string `json:"dsn"`      // PostgreSQL DSN passed to pg_dump
	DumpDir string `json:"dump_dir"` // Directory for dump output files
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "scheduled-backup",
		Version:     "0.1.0",
		Description: "Scheduled PostgreSQL backups to S3",
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

	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &p.config); err != nil {
			return fmt.Errorf("scheduledbackup: invalid config: %w", err)
		}
	}

	if p.config.DumpDir == "" {
		p.config.DumpDir = "/tmp/cleat-backups"
	}

	if err := os.MkdirAll(p.config.DumpDir, 0755); err != nil {
		return fmt.Errorf("scheduledbackup: create dump dir: %w", err)
	}

	p.logger.Info("scheduledbackup: initialized",
		"dump_dir", p.config.DumpDir,
	)
	return nil
}
