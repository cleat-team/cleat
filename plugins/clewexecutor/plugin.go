package clewexecutor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/cleat-team/cleat/internal/plugin"
)

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:           "clew-executor",
		Version:        "0.1.0",
		Description:    "Launch Claude Code subprocess for Clew task phases",
		Author:         "cleat",
		DatabaseAccess: plugin.DatabaseAccessNone,
	}, func() plugin.Plugin {
		return &Plugin{}
	})
}

// clewConfig holds optional plugin configuration.
type clewConfig struct {
	AgentBin string `json:"agent_bin,omitempty"` // default "claude"
}

// Plugin implements the clew-executor plugin.
type Plugin struct {
	logger   *slog.Logger
	agentBin string
}

// Info returns plugin metadata.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:           "clew-executor",
		Version:        "0.1.0",
		Description:    "Launch Claude Code subprocess for Clew task phases",
		Author:         "cleat",
		DatabaseAccess: plugin.DatabaseAccessNone,
	}
}

// Init initializes the plugin.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}
	p.agentBin = "claude"
	if len(env.Config) > 0 {
		var cfg clewConfig
		if err := json.Unmarshal(env.Config, &cfg); err != nil {
			return fmt.Errorf("clew-executor: invalid config: %w", err)
		}
		if cfg.AgentBin != "" {
			p.agentBin = cfg.AgentBin
		}
	}
	p.logger.Info("clew-executor: initialized", "agent_bin", p.agentBin)
	return nil
}

func (p *Plugin) validateFiles(ctx context.Context, inputJSON string) (string, error) {
	return `{"missing":[]}`, nil
}

func (p *Plugin) readFile(ctx context.Context, inputJSON string) (string, error) {
	return "", fmt.Errorf("clew-executor: read_file not implemented")
}

func (p *Plugin) createTask(ctx context.Context, inputJSON string) (string, error) {
	return "", fmt.Errorf("clew-executor: create_task not implemented")
}
