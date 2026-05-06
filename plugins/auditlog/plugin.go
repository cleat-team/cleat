// Package auditlog provides a comprehensive audit trail of all API access.
// It records every HTTP request, creates queryable audit events, and has
// a background retention cleanup.
package auditlog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/rcownie/durable/internal/plugin"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "audit-log",
		Version:     "0.1.0",
		Description: "Comprehensive audit trail of all API access",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// Plugin implements audit trail recording and querying.
type Plugin struct {
	db     *sql.DB
	mux    *http.ServeMux
	logger *slog.Logger
	config Config
}

// Config controls audit-log behaviour.
type Config struct {
	RetentionDays int `json:"retention_days"` // default 90
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "audit-log",
		Version:     "0.1.0",
		Description: "Comprehensive audit trail of all API access",
		Author:      "cleat",
	}
}

// Init initialises the plugin with the given environment.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	p.db = env.DB
	p.mux = env.Mux

	// Parse config. If no config provided, use safe defaults.
	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &p.config); err != nil {
			return fmt.Errorf("audit-log: invalid config: %w", err)
		}
	}
	if p.config.RetentionDays <= 0 {
		p.config.RetentionDays = 90
	}

	p.logger.Info("audit-log: initialized",
		"retention_days", p.config.RetentionDays,
	)
	return nil
}

// RegisterRoutes registers HTTP routes for querying audit events.
func (p *Plugin) RegisterRoutes(mux *http.ServeMux) error {
	if mux == nil {
		return fmt.Errorf("audit-log: nil mux")
	}
	mux.HandleFunc("GET /audit/events", p.handleQueryEvents)
	return nil
}
