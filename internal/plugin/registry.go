package plugin

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"
)

var (
	registryMu sync.Mutex
	registry   = make(map[string]registryEntry)
)

// registryEntry stores plugin metadata alongside the constructor so that
// Info is available without instantiating the plugin again.
type registryEntry struct {
	info PluginInfo
	ctor func() Plugin
}

// Register registers a plugin constructor with its PluginInfo. Call from init().
func Register(info PluginInfo, ctor func() Plugin) {
	registryMu.Lock()
	defer registryMu.Unlock()

	if info.Name == "" {
		panic("plugin registered with empty name")
	}
	if _, exists := registry[info.Name]; exists {
		panic(fmt.Sprintf("plugin %q registered twice", info.Name))
	}
	registry[info.Name] = registryEntry{info: info, ctor: ctor}
}

// LoadedPlugin wraps a plugin instance with its current state.
type LoadedPlugin struct {
	Plugin  Plugin
	Healthy bool
	Error   error
}

// Discover instantiates all registered plugins in dependency order without
// calling Init. The caller should call RunMigrations followed by InitAll
// to complete plugin initialization.
func Discover() ([]*LoadedPlugin, error) {
	registryMu.Lock()
	entries := make(map[string]registryEntry, len(registry))
	for name, entry := range registry {
		entries[name] = entry
	}
	registryMu.Unlock()

	ordered, err := topologicalSort(entries)
	if err != nil {
		return nil, err
	}

	var loaded []*LoadedPlugin
	for _, name := range ordered {
		entry := entries[name]
		lp := &LoadedPlugin{Healthy: true}
		lp.Plugin = entry.ctor()
		loaded = append(loaded, lp)
	}

	return loaded, nil
}

// InitAll calls Init on each loaded plugin in order. Plugins that panic
// or return an error during Init are marked unhealthy but do not halt
// initialization of remaining plugins.
//
// Each plugin receives an Environment whose DB field is restricted based
// on the plugin's declared DatabaseAccess: None gets nil, ReadOnly gets
// a ReadOnlyDB wrapper, ReadWrite gets the raw *sql.DB.
func InitAll(ctx context.Context, env *Environment, plugins []*LoadedPlugin) {
	for _, lp := range plugins {
		if !lp.Healthy {
			continue
		}

		// Create a per-plugin environment with database access restricted
		// to the level declared by the plugin.
		pluginEnv := env
		access := lp.Plugin.Info().DatabaseAccess
		if access == DatabaseAccessNone || access == "" {
			pluginEnv = env.withDB(nil)
		} else if access == DatabaseAccessReadOnly {
			if db, ok := env.DB.(*sql.DB); ok {
				pluginEnv = env.withDB(NewReadOnlyDB(db))
			} else {
				pluginEnv = env.withDB(nil)
			}
		}
		// ReadWrite: use the raw Environment as-is.

		func() {
			defer func() {
				if r := recover(); r != nil {
					lp.Healthy = false
					lp.Error = fmt.Errorf("panic during Init: %v", r)
					if env != nil && env.Logger != nil {
						env.Logger.Error("plugin init panicked", "plugin", lp.Plugin.Info().Name, "panic", r)
					}
				}
			}()

			if err := lp.Plugin.Init(ctx, pluginEnv); err != nil {
				lp.Healthy = false
				lp.Error = err
				if env != nil && env.Logger != nil {
					env.Logger.Error("plugin init failed", "plugin", lp.Plugin.Info().Name, "error", err)
				}
			}
		}()
	}
}

// withDB returns a shallow copy of env with DB replaced by db.
func (env *Environment) withDB(db DB) *Environment {
	copy := *env
	copy.DB = db
	return &copy
}

// LoadAll instantiates all registered plugins in dependency order,
// calls Init on each, and returns the successfully loaded plugins.
// A plugin that panics during Init is disabled and reported.
func LoadAll(ctx context.Context, env *Environment) ([]*LoadedPlugin, error) {
	plugins, err := Discover()
	if err != nil {
		return nil, err
	}
	InitAll(ctx, env, plugins)
	return plugins, nil
}

// List returns metadata for all registered plugins.
func List() []PluginInfo {
	registryMu.Lock()
	defer registryMu.Unlock()

	var infos []PluginInfo
	for _, entry := range registry {
		infos = append(infos, entry.info)
	}
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].Name < infos[j].Name
	})
	return infos
}

// topologicalSort orders plugins by their Requires dependencies using
// Kahn's algorithm.
func topologicalSort(entries map[string]registryEntry) ([]string, error) {
	inDegree := make(map[string]int)
	graph := make(map[string][]string)

	for name, entry := range entries {
		info := entry.info
		inDegree[name] = len(info.Requires)
		for _, dep := range info.Requires {
			if _, exists := entries[dep]; !exists {
				return nil, fmt.Errorf("plugin %q requires %q which is not registered", name, dep)
			}
			graph[dep] = append(graph[dep], name)
		}
	}

	// Kahn's algorithm.
	var queue []string
	for name, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, name)
		}
	}

	var sorted []string
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		sorted = append(sorted, name)
		for _, dependent := range graph[name] {
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}

	if len(sorted) != len(entries) {
		return nil, fmt.Errorf("circular dependency detected among plugins")
	}

	return sorted, nil
}
