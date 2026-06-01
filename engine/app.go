package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/cleat-team/cleat/plugin"
)

// AppConfig configures a cleat application.
type AppConfig struct {
	// Runtime is the WASM runtime for executing workflows. Required.
	Runtime *Runtime

	// StoreFactory creates workflow stores for database access. Required.
	StoreFactory StoreFactory

	// ServiceCaller makes external API calls on behalf of workflows. Required.
	ServiceCaller ServiceCaller

	// Plugins is the list of plugins to load at startup.
	Plugins []plugin.Plugin

	// PluginConfigs maps plugin name to JSON configuration.
	// The config is passed to the plugin via Environment.Config.
	PluginConfigs map[string]json.RawMessage

	// Logger is the root logger for the application. If nil, a default
	// logger is created.
	Logger *slog.Logger

	// Mux is the HTTP serve mux for plugin route registration.
	Mux *http.ServeMux

	// Registry is the plugin function registry. If nil, a new one is created.
	Registry *PluginRegistry

	// StreamRegistry is the stream plugin function registry. If nil, a new one is created.
	StreamRegistry *PluginStreamRegistry
}

// App manages the lifecycle of a cleat application and its plugins.
type App struct {
	config         AppConfig
	registry       *PluginRegistry
	streamRegistry *PluginStreamRegistry
	plugins        []plugin.Plugin
	logger         *slog.Logger
}

// NewApp creates a new cleat application, initializing plugins in order.
// Each plugin's Init is called with an Environment that includes the plugin's
// JSON configuration (from PluginConfigs) and the shared HTTP mux and logger.
// If a plugin implements HasHostFunctions, its host functions are registered
// automatically.
func NewApp(ctx context.Context, config AppConfig) (*App, error) {
	if config.Runtime == nil {
		return nil, fmt.Errorf("AppConfig.Runtime is required")
	}
	if config.StoreFactory == nil {
		return nil, fmt.Errorf("AppConfig.StoreFactory is required")
	}
	if config.ServiceCaller == nil {
		return nil, fmt.Errorf("AppConfig.ServiceCaller is required")
	}

	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	registry := config.Registry
	if registry == nil {
		registry = NewPluginRegistry()
	}

	streamRegistry := config.StreamRegistry
	if streamRegistry == nil {
		streamRegistry = NewPluginStreamRegistry()
	}
	// Share a single health tracker between both registries.
	streamRegistry.SetHealthTracker(registry.healthTracker)

	mux := config.Mux
	if mux == nil {
		mux = http.NewServeMux()
	}

	a := &App{
		config:         config,
		registry:       registry,
		streamRegistry: streamRegistry,
		plugins:        make([]plugin.Plugin, 0, len(config.Plugins)),
		logger:         logger,
	}

	// Initialize each plugin in order.
	for _, p := range config.Plugins {
		info := p.Info()
		pluginConfig := config.PluginConfigs[info.Name]

		env := &plugin.Environment{
			Mux:    mux,
			Config: pluginConfig,
			Logger: logger.With("plugin", info.Name),
		}

		if err := p.Init(ctx, env); err != nil {
			_ = a.closePlugins()
			return nil, fmt.Errorf("plugin %s init: %w", info.Name, err)
		}

		// Register host functions if the plugin supports them.
		if hf, ok := p.(plugin.HasHostFunctions); ok {
			scope := &appFuncRegistry{
				pluginName: info.Name,
				registry:   registry,
			}
			if err := hf.RegisterHostFunctions(scope); err != nil {
				_ = a.closePlugins()
				return nil, fmt.Errorf("plugin %s register host functions: %w", info.Name, err)
			}
		}

		a.plugins = append(a.plugins, p)
		logger.Info("plugin initialized", "plugin", info.Name)
	}

	return a, nil
}

// CloseablePlugin is an optional interface for plugins that need cleanup.
type CloseablePlugin interface {
	Close() error
}

// Close shuts down all plugins in reverse initialization order.
// Plugins that implement CloseablePlugin have their Close method called.
func (a *App) Close() error {
	return a.closePlugins()
}

func (a *App) closePlugins() error {
	var lastErr error
	for i := len(a.plugins) - 1; i >= 0; i-- {
		if c, ok := a.plugins[i].(CloseablePlugin); ok {
			if err := c.Close(); err != nil {
				lastErr = err
			}
		}
	}
	return lastErr
}

// Registry returns the plugin function registry.
func (a *App) Registry() *PluginRegistry {
	return a.registry
}

// StreamRegistry returns the stream plugin function registry.
func (a *App) StreamRegistry() *PluginStreamRegistry {
	return a.streamRegistry
}

// Logger returns the application logger.
func (a *App) Logger() *slog.Logger {
	return a.logger
}

// Mux returns the HTTP serve mux used by plugins.
func (a *App) Mux() *http.ServeMux {
	return a.config.Mux
}

// appFuncRegistry adapts the engine PluginRegistry to the plugin.FuncRegistry interface.
type appFuncRegistry struct {
	pluginName string
	registry   *PluginRegistry
}

func (s *appFuncRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	if opts.Idempotent {
		return s.registry.RegisterIdempotent(s.pluginName, opts.Name, fn)
	}
	return s.registry.Register(s.pluginName, opts.Name, fn)
}
