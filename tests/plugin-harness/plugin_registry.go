package pluginharness

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
)

// BuildPluginRegistry discovers, initialises, and registers all plugins. It
// returns the host-level PluginRegistry, PluginStreamRegistry, and the list of
// loaded plugins.
//
// For each plugin that implements HasHostFunctions, the adapter created by this
// function bridges the plugin's FuncRegistry to the host's PluginRegistry so
// that workflow-callable host functions are registered.
//
// The provided config is serialised to JSON and placed into Environment.Config.
// Provide nil config to skip setting any custom configuration.
func BuildPluginRegistry(
	t *testing.T,
	ctx context.Context,
	pluginDB plugin.PluginDB,
	dialect plugin.Dialect,
	mockServers *MockServers,
	config map[string]interface{},
) (*engine.PluginRegistry, *engine.PluginStreamRegistry, []*plugin.LoadedPlugin) {
	t.Helper()

	// 1. Discover all registered plugins.
	loadedPlugins, err := plugin.Discover()
	if err != nil {
		t.Fatalf("BuildPluginRegistry: Discover: %v", err)
	}

	// 2. Build environment.
	var configBytes []byte
	if config != nil {
		configBytes, err = json.Marshal(config)
		if err != nil {
			t.Fatalf("BuildPluginRegistry: marshal config: %v", err)
		}
	}

	env := &plugin.Environment{
		DB:      pluginDB,
		Dialect: dialect,
		Config:  configBytes,
	}

	// 3. Initialise each plugin.
	plugin.InitAll(ctx, env, loadedPlugins)

	// 4. Create host registries.
	pr := engine.NewPluginRegistry()
	psr := engine.NewPluginStreamRegistry()

	// 5. Register host functions for plugins that implement HasHostFunctions.
	for _, lp := range loadedPlugins {
		if !lp.Healthy {
			continue
		}

		pluginName := lp.Plugin.Info().Name

		// Register regular (non-streaming) host functions.
		if hf, ok := lp.Plugin.(plugin.HasHostFunctions); ok {
			adapter := &hostFuncAdapter{
				pluginName: pluginName,
				registry:   pr,
			}
			if err := hf.RegisterHostFunctions(adapter); err != nil {
				t.Fatalf("BuildPluginRegistry: %s RegisterHostFunctions: %v", pluginName, err)
			}
		}

		// Register streaming host functions if available.
		// Note: streaming host functions are declared via the same
		// HasHostFunctions interface, but we check for StreamFuncRegistry
		// separately since not every plugin that has regular functions
		// also has streaming functions.
		if sf, ok := lp.Plugin.(pluginStreamPlugin); ok {
			streamAdapter := &streamFuncAdapter{
				pluginName: pluginName,
				registry:   psr,
			}
			if err := sf.RegisterStreamHostFunctions(streamAdapter); err != nil {
				t.Fatalf("BuildPluginRegistry: %s RegisterStreamHostFunctions: %v", pluginName, err)
			}
		}
	}

	return pr, psr, loadedPlugins
}

// pluginStreamPlugin is an optional interface that plugins can implement to
// register streaming host functions.
type pluginStreamPlugin interface {
	plugin.Plugin
	RegisterStreamHostFunctions(scope plugin.StreamFuncRegistry) error
}

// hostFuncAdapter adapts the plugin-level FuncRegistry to the host-level
// PluginRegistry. It captures the plugin name at creation time so the caller
// does not need to pass it on every Register call.
type hostFuncAdapter struct {
	pluginName string
	registry   *engine.PluginRegistry
}

// Register adds a plugin host function to the host registry. The plugin name
// is the one captured at adapter creation time.
func (a *hostFuncAdapter) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	return a.registry.Register(a.pluginName, opts.Name, fn)
}

// streamFuncAdapter adapts the plugin-level StreamFuncRegistry to the
// host-level PluginStreamRegistry.
type streamFuncAdapter struct {
	pluginName string
	registry   *engine.PluginStreamRegistry
}

// RegisterStream adds a streaming plugin host function to the host registry.
func (a *streamFuncAdapter) RegisterStream(opts plugin.FuncOptions, fn plugin.PluginStreamFunc) error {
	return a.registry.RegisterStream(a.pluginName, opts, fn)
}
