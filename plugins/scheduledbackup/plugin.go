// Package scheduledbackup provides scheduled PostgreSQL backups with pg_dump.
// It supports cron-based scheduling, manual backup/restore via HTTP API and CLI
// commands, and records backup history in PostgreSQL.
package scheduledbackup

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/cleat-team/cleat/plugin"
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

// New creates a new Plugin instance.
func New() plugin.Plugin {
	return &Plugin{}
}

// Plugin implements scheduled PostgreSQL backups with tenant isolation.
type Plugin struct {
	db      plugin.PluginDB
	mux     *http.ServeMux
	logger  *slog.Logger
	dialect plugin.Dialect
	config  Config
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
	p.dialect = env.Dialect
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

	if err := p.checkDBVersion(ctx); err != nil {
		return err
	}

	p.logger.Info("scheduledbackup: initialized",
		"dump_dir", p.config.DumpDir,
	)
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
		return fmt.Errorf("scheduledbackup: failed to check MySQL version: %w", err)
	}
	major := 0
	for _, part := range strings.Split(version, ".") {
		if n, err := fmt.Sscanf(part, "%d", &major); err == nil && n == 1 {
			break
		}
	}
	if major < 8 {
		return fmt.Errorf("scheduledbackup: MySQL 8.0+ is required (found %s). FOR UPDATE SKIP LOCKED is not available in older MySQL versions", version)
	}
	return nil
}
