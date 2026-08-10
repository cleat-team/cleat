package plugin

import (
	"context"
	"testing"
)

// Test plugin that does nothing.
type noopPlugin struct{}

func (p *noopPlugin) Info() PluginInfo {
	return PluginInfo{Name: "noop", Version: "0.1.0", Description: "does nothing"}
}

func (p *noopPlugin) Init(ctx context.Context, env *Environment) error {
	return nil
}

func init() {
	Register(PluginInfo{Name: "noop", Version: "0.1.0", Description: "does nothing"}, func() Plugin { return &noopPlugin{} })
}

func TestRegistration(t *testing.T) {
	infos := List()
	found := false
	for _, info := range infos {
		if info.Name == "noop" {
			found = true
			break
		}
	}
	if !found {
		t.Error("noop plugin not found in registry")
	}
}

func TestTopologicalSort(t *testing.T) {
	entries := map[string]registryEntry{
		"a": {info: PluginInfo{Name: "a", Requires: []string{"b"}}, ctor: func() Plugin { return &testPlugin{info: PluginInfo{Name: "a", Requires: []string{"b"}}} }},
		"b": {info: PluginInfo{Name: "b"}, ctor: func() Plugin { return &testPlugin{info: PluginInfo{Name: "b"}} }},
	}
	sorted, err := topologicalSort(entries)
	if err != nil {
		t.Fatal(err)
	}
	if sorted[0] != "b" || sorted[1] != "a" {
		t.Errorf("expected [b a], got %v", sorted)
	}
}

func TestCircularDependency(t *testing.T) {
	entries := map[string]registryEntry{
		"a": {info: PluginInfo{Name: "a", Requires: []string{"b"}}, ctor: func() Plugin { return &testPlugin{info: PluginInfo{Name: "a", Requires: []string{"b"}}} }},
		"b": {info: PluginInfo{Name: "b", Requires: []string{"a"}}, ctor: func() Plugin { return &testPlugin{info: PluginInfo{Name: "b", Requires: []string{"a"}}} }},
	}
	_, err := topologicalSort(entries)
	if err == nil {
		t.Error("expected error for circular dependency")
	}
}

func TestMissingDependency(t *testing.T) {
	entries := map[string]registryEntry{
		"a": {info: PluginInfo{Name: "a", Requires: []string{"nonexistent"}}, ctor: func() Plugin { return &testPlugin{info: PluginInfo{Name: "a", Requires: []string{"nonexistent"}}} }},
	}
	_, err := topologicalSort(entries)
	if err == nil {
		t.Error("expected error for missing dependency")
	}
}

func TestLoadAll(t *testing.T) {
	// Load all registered plugins (including the noop test plugin).
	env := &Environment{Logger: nil}
	plugins, err := LoadAll(context.Background(), env)
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}
	if len(plugins) == 0 {
		t.Fatal("expected at least the noop plugin to be loaded")
	}
	// All loaded plugins must be healthy (no init failures).
	for _, lp := range plugins {
		if !lp.Healthy {
			t.Errorf("plugin %s not healthy: %v", lp.Plugin.Info().Name, lp.Error)
		}
	}
}

func TestPanickingPlugin(t *testing.T) {
	// Register a plugin that panics during Init.
	Register(PluginInfo{Name: "panic-plugin", Version: "0.1.0"}, func() Plugin {
		return &testPlugin{
			info: PluginInfo{Name: "panic-plugin", Version: "0.1.0"},
			init: func(ctx context.Context, env *Environment) error {
				panic("omg")
			},
		}
	})

	env := &Environment{Logger: nil}
	plugins, err := LoadAll(context.Background(), env)
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}

	found := false
	for _, lp := range plugins {
		if lp.Plugin.Info().Name == "panic-plugin" {
			found = true
			if lp.Healthy {
				t.Error("expected panic-plugin to be unhealthy")
			}
			if lp.Error == nil {
				t.Error("expected panic-plugin to have an error")
			}
		}
	}
	if !found {
		t.Error("panic-plugin not found in loaded plugins")
	}
}

func TestFailingInitPlugin(t *testing.T) {
	Register(PluginInfo{Name: "fail-plugin", Version: "0.1.0"}, func() Plugin {
		return &testPlugin{
			info: PluginInfo{Name: "fail-plugin", Version: "0.1.0"},
			init: func(ctx context.Context, env *Environment) error {
				return context.Canceled
			},
		}
	})

	env := &Environment{Logger: nil}
	plugins, err := LoadAll(context.Background(), env)
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}

	found := false
	for _, lp := range plugins {
		if lp.Plugin.Info().Name == "fail-plugin" {
			found = true
			if lp.Healthy {
				t.Error("expected fail-plugin to be unhealthy")
			}
			if lp.Error == nil {
				t.Error("expected fail-plugin to have an error")
			}
		}
	}
	if !found {
		t.Error("fail-plugin not found in loaded plugins")
	}
}

func TestConstructorCalledOnce(t *testing.T) {
	// Register a plugin with a constructor that tracks invocations.
	callCount := 0
	Register(PluginInfo{Name: "ctor-count-test", Version: "0.1.0"}, func() Plugin {
		callCount++
		return &testPlugin{
			info: PluginInfo{Name: "ctor-count-test", Version: "0.1.0"},
		}
	})

	// Discover should call the constructor exactly once.
	plugins, err := Discover()
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}

	// Verify constructor was called exactly once.
	if callCount != 1 {
		t.Errorf("constructor called %d times, want 1", callCount)
	}

	// Verify the plugin was loaded.
	found := false
	for _, lp := range plugins {
		if lp.Plugin.Info().Name == "ctor-count-test" {
			found = true
			break
		}
	}
	if !found {
		t.Error("ctor-count-test plugin not found in discovered plugins")
	}

	// Verify a second Discover() calls the constructor again.
	callCount = 0
	_, err = Discover()
	if err != nil {
		t.Fatalf("second Discover failed: %v", err)
	}
	if callCount != 1 {
		t.Errorf("second Discover: constructor called %d times, want 1", callCount)
	}
}

type testPlugin struct {
	info PluginInfo
	init func(ctx context.Context, env *Environment) error
}

func (p *testPlugin) Info() PluginInfo { return p.info }
func (p *testPlugin) Init(ctx context.Context, env *Environment) error {
	if p.init != nil {
		return p.init(ctx, env)
	}
	return nil
}
