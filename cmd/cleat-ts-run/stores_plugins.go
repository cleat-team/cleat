// A scripted PluginRegistry for cleat-ts-run (cleat#3827, group I: plugin calls).
//
// WHY. A TypeScript workflow that calls a plugin reaches `plugin_call` and `plugin_call_streaming`, and the engine
// answers "no such plugin function" unless a registry holds one. That answer is indistinguishable from a plugin that
// genuinely failed, so a test written against an empty engine can pass while proving nothing about the binding.
//
// WHAT IT IS NOT. It says nothing about how the deployed worker registers plugins (plugins/*/plugin.go does that, with
// real effects). This answers from the test's own script: one entry per `plugin/function`, either a result, an error,
// or a chunk list for the streaming form. Where that differs from a deployed registry in a way a workflow could
// observe, the difference belongs in the PR body's divergence list, beside the other harness ones.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
)

// pluginScript is one scripted answer, keyed `plugin/function`.
//
// The three shapes are exclusive, and saying so is the point: a fixture with both a result and an error would
// otherwise silently prefer one, and a test asserting the other would fail for a reason the fixture hid.
type pluginScript struct {
	Result json.RawMessage   `json:"result,omitempty"`
	Error  string            `json:"error,omitempty"`
	Chunks []json.RawMessage `json:"chunks,omitempty"`
}

// validate refuses a script that answers two ways at once, and requires that it answers at all.
func (s pluginScript) validate(key string) error {
	n := 0
	if len(s.Result) > 0 {
		n++
	}
	if s.Error != "" {
		n++
	}
	if len(s.Chunks) > 0 {
		n++
	}
	if n == 0 {
		return fmt.Errorf("plugin script %q answers nothing: give it a result, an error or chunks", key)
	}
	if n > 1 {
		return fmt.Errorf("plugin script %q answers %d ways at once: a result, an error and chunks are exclusive", key, n)
	}
	return nil
}

// loadPluginScript reads the plugin fixture: {"<plugin>/<function>": {"result"|"error"|"chunks": ...}}.
func loadPluginScript(path string) (map[string]pluginScript, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the plugin script: %w", err)
	}
	var out map[string]pluginScript
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("the plugin script is not a JSON object of plugin/function answers: %w", err)
	}
	for key, s := range out {
		if !strings.Contains(key, "/") {
			return nil, fmt.Errorf("plugin script key %q is not <plugin>/<function>", key)
		}
		if err := s.validate(key); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the plugin script is empty: a run with no scripted plugin would fail at the call, not at what it proves")
	}
	return out, nil
}

// scriptedPlugins answers plugin calls from the fixture and records every call it received.
//
// The recording is not decoration. `extras.pluginCalls` is how a test asserts that a call reached the registry at all
// -- an unscripted call still has to appear, or a workflow that swallowed its own failure would look identical to one
// that never called.
type scriptedPlugins struct {
	scripts map[string]pluginScript

	// Kept so a test can reach the registries the option installs. Without them the ONLY way to exercise this store
	// would be a full wasm run, which is the slowest and least direct way to ask whether a scripted function answers.
	pr  *engine.PluginRegistry
	psr *engine.PluginStreamRegistry

	mu    sync.Mutex
	calls []string
}

func (p *scriptedPlugins) record(pluginName, functionName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, pluginName+"/"+functionName)
}

func (p *scriptedPlugins) callList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := append([]string(nil), p.calls...)
	sort.Strings(out)
	return out
}

// install registers every scripted function on both registries, and returns the single EngineOption that installs
// them.
//
// ONE OPTION, TWO REGISTRIES, because storeSpec carries one option and this store needs both: `plugin_call` reads the
// PluginRegistry and `plugin_call_streaming` reads the PluginStreamRegistry, and a group that could only install one
// of them would bind half its calls and silently answer "not registered" for the other half.
func (p *scriptedPlugins) install() (engine.EngineOption, error) {
	pr := engine.NewPluginRegistry()
	psr := engine.NewPluginStreamRegistry()
	p.pr, p.psr = pr, psr

	for key, s := range p.scripts {
		pluginName, functionName, _ := strings.Cut(key, "/")
		s := s
		if len(s.Chunks) > 0 {
			// A streaming function is registered ONLY on the stream registry: registering a stub on the plain one as
			// well would answer plugin_call for a function whose scripted answer is a chunk list, which no fixture
			// asked for.
			if err := psr.Register(pluginName, functionName, func(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {
				p.record(pluginName, functionName)
				ch := make(chan plugin.StreamEvent, len(s.Chunks))
				for i, raw := range s.Chunks {
					ch <- plugin.StreamEvent{Index: i, Content: strings.Trim(string(raw), `"`), Finish: i == len(s.Chunks)-1}
				}
				close(ch)
				return ch, nil
			}); err != nil {
				return nil, fmt.Errorf("registering streamed %q: %w", key, err)
			}
			continue
		}
		if err := pr.Register(pluginName, functionName, func(ctx context.Context, inputJSON string) (string, error) {
			p.record(pluginName, functionName)
			if s.Error != "" {
				return "", fmt.Errorf("%s", s.Error)
			}
			return string(s.Result), nil
		}); err != nil {
			return nil, fmt.Errorf("registering %q: %w", key, err)
		}
	}

	return func(e *engine.Engine) {
		engine.WithPluginRegistry(pr)(e)
		engine.WithPluginStreamRegistry(psr)(e)
	}, nil
}

// newScriptedPlugins is the registry factory for `-script plugins=file`.
func newScriptedPlugins(path string) (storeSpec, error) {
	scripts, err := loadPluginScript(path)
	if err != nil {
		return storeSpec{}, err
	}
	p := &scriptedPlugins{scripts: scripts}
	opt, err := p.install()
	if err != nil {
		return storeSpec{}, err
	}
	return storeSpec{
		Option: opt,
		// Read after the run, like every Extra: what the test wants is the calls the workflow made.
		Extra: func() (string, any) { return "pluginCalls", p.callList() },
	}, nil
}

func init() { registerStore("plugins", newScriptedPlugins) }
