// Package kvstore provides a versioned JSONB key-value store with optimistic
// concurrency control. It is a thin HTTP API over a kv_store table with tenant
// isolation.
package kvstore

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
		Name:        "kvstore",
		Version:     "0.1.0",
		Description: "Versioned JSONB key-value store with optimistic concurrency",
		Author:      "cleat",
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// Plugin implements a versioned JSONB key-value store with tenant isolation.
type Plugin struct {
db     plugin.DB
	mux    *http.ServeMux
	logger *slog.Logger
	config Config
}

// Config controls kvstore behaviour.
type Config struct {
	MaxValueSize int `json:"max_value_size"` // max JSON value bytes; default 1 MB
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "kvstore",
		Version:     "0.1.0",
		Description: "Versioned JSONB key-value store with optimistic concurrency",
		Author:      "cleat",
	}
}

// Init initializes the plugin with the given environment. It parses optional
// configuration and sets safe defaults.
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
			return fmt.Errorf("kvstore: invalid config: %w", err)
		}
	}
	if p.config.MaxValueSize == 0 {
		p.config.MaxValueSize = 1_048_576 // 1 MB default
	}

	p.logger.Info("kvstore: initialized",
		"max_value_size", p.config.MaxValueSize,
	)
	return nil
}
