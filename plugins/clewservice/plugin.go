package clewservice

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/cleat-team/cleat/pluginapi"
)

func init() {
	pluginapi.Register(pluginapi.PluginInfo{
		Name:           "clew-service",
		Version:        "0.1.0",
		Description:    "Clew dashboard API — project/task CRUD, agent dispatch, file-based state",
		Author:         "cleat",
		DatabaseAccess: pluginapi.DatabaseAccessNone,
	}, func() pluginapi.Plugin {
		return &Plugin{}
	})
}

// Config holds plugin configuration from env.Config JSON.
type Config struct {
	ProjectRoot   string `json:"project_root"`
	NewTaskScript string `json:"new_task_script"`
}

// Plugin implements the clew-service HTTP plugin.
type Plugin struct {
	projectRoot   string
	newTaskScript string
	tenantID      string
	logger        *slog.Logger
	mu            sync.Mutex
}

// Info returns plugin metadata.
func (p *Plugin) Info() pluginapi.PluginInfo {
	return pluginapi.PluginInfo{
		Name:           "clew-service",
		Version:        "0.1.0",
		Description:    "Clew dashboard API — project/task CRUD, agent dispatch, file-based state",
		Author:         "cleat",
		DatabaseAccess: pluginapi.DatabaseAccessNone,
	}
}

// Init initializes the plugin from environment.
func (p *Plugin) Init(ctx context.Context, env *pluginapi.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}

	var cfg Config
	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &cfg); err != nil {
			return fmt.Errorf("clewservice: invalid config: %w", err)
		}
	}

	p.projectRoot = cfg.ProjectRoot
	if p.projectRoot == "" {
		return fmt.Errorf("clewservice: project_root is required in config")
	}

	p.newTaskScript = cfg.NewTaskScript
	if p.newTaskScript == "" {
		p.newTaskScript = p.projectRoot + "/src/new-task.sh"
	}

	p.tenantID = env.TenantID

	p.logger.Info("clewservice: initialized",
		"project_root", p.projectRoot,
		"new_task_script", p.newTaskScript,
	)
	return nil
}

// InitStandalone initializes the plugin for standalone binary use (no cleat
// plugin system). Sets projectRoot and defaults.
func (p *Plugin) InitStandalone(projectRoot string) error {
	p.projectRoot = projectRoot
	p.newTaskScript = projectRoot + "/src/new-task.sh"
	p.logger = slog.Default()
	p.logger.Info("clewservice: initialized (standalone)",
		"project_root", p.projectRoot,
	)
	return nil
}
