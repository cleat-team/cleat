package featureflags

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
		Name:        "feature-flags",
		Version:     "0.1.0",
		Description: "Feature flag evaluation with rules and gradual rollout",
		Author:      "cleat",
	}, func() plugin.Plugin { return &Plugin{} })
}

// Plugin implements feature flag evaluation with targeting rules and
// gradual rollout for the cleat durable execution engine.
type Plugin struct {
	db      plugin.PluginDB
	mux     *http.ServeMux
	logger  *slog.Logger
	config  Config
	dialect plugin.Dialect
}

// Config controls feature flag plugin behavior.
type Config struct {
	DefaultRollout int `json:"default_rollout"` // default rollout percentage (0-100)
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "feature-flags",
		Version:     "0.1.0",
		Description: "Feature flag evaluation with rules and gradual rollout",
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
	p.dialect = env.Dialect

	// Parse config. If no config provided, use safe defaults.
	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &p.config); err != nil {
			return fmt.Errorf("feature-flags: invalid config: %w", err)
		}
	}

	p.logger.Info("feature-flags: initialized",
		"default_rollout", p.config.DefaultRollout,
	)
	return nil
}
