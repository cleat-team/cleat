package plugin

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

var (
	registryMu sync.Mutex
	registry   = make(map[string]func() Plugin)
)

// Register registers a plugin constructor. Call from init().
func Register(constructor func() Plugin) {
	registryMu.Lock()
	defer registryMu.Unlock()

	p := constructor()
	info := p.Info()
	if info.Name == "" {
		panic("plugin registered with empty name")
	}
	if _, exists := registry[info.Name]; exists {
		panic(fmt.Sprintf("plugin %q registered twice", info.Name))
	}
	registry[info.Name] = constructor
}

// LoadedPlugin wraps a plugin instance with its current state.
type LoadedPlugin struct {
	Plugin  Plugin
	Healthy bool
	Error   error
}

// LoadAll instantiates all registered plugins in dependency order,
// calls Init on each, and returns the successfully loaded plugins.
// A plugin that panics during Init is disabled and reported.
func LoadAll(ctx context.Context, env *Environment) ([]*LoadedPlugin, error) {
	registryMu.Lock()
	constructors := make(map[string]func() Plugin, len(registry))
	for name, ctor := range registry {
		constructors[name] = ctor
	}
	registryMu.Unlock()

	// Build dependency graph and topologically sort.
	ordered, err := topologicalSort(constructors)
	if err != nil {
		return nil, err
	}

	var loaded []*LoadedPlugin
	for _, name := range ordered {
		ctor := constructors[name]
		lp := &LoadedPlugin{Healthy: true}

		func() {
			defer func() {
				if r := recover(); r != nil {
					lp.Healthy = false
					lp.Error = fmt.Errorf("panic during Init: %v", r)
					if env.Logger != nil {
						env.Logger.Error("plugin init panicked", "plugin", name, "panic", r)
					}
				}
			}()

			p := ctor()
			lp.Plugin = p

			if err := p.Init(ctx, env); err != nil {
				lp.Healthy = false
				lp.Error = err
				if env.Logger != nil {
					env.Logger.Error("plugin init failed", "plugin", name, "error", err)
				}
			}
		}()

		loaded = append(loaded, lp)
	}

	return loaded, nil
}

// List returns metadata for all registered plugins.
func List() []PluginInfo {
	registryMu.Lock()
	defer registryMu.Unlock()

	var infos []PluginInfo
	for _, ctor := range registry {
		infos = append(infos, ctor().Info())
	}
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].Name < infos[j].Name
	})
	return infos
}

// topologicalSort orders plugins by their Requires dependencies using
// Kahn's algorithm.
func topologicalSort(constructors map[string]func() Plugin) ([]string, error) {
	inDegree := make(map[string]int)
	graph := make(map[string][]string)

	for name, ctor := range constructors {
		info := ctor().Info()
		inDegree[name] = len(info.Requires)
		for _, dep := range info.Requires {
			if _, exists := constructors[dep]; !exists {
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

	if len(sorted) != len(constructors) {
		return nil, fmt.Errorf("circular dependency detected among plugins")
	}

	return sorted, nil
}
