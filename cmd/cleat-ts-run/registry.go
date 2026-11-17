// A store registry, so adding a store is one new FILE and no edit to main.go (cleat#3827 F1).
//
// WHY. `buildStores` takes its five fixture paths positionally, and three of them were added one at a
// time; a sixth store changes the signature and every caller, which makes ten groups written in parallel
// conflict on one line. The registry turns that into a fan-out: a group adds `stores_<group>.go` with an
// `init()` that registers itself, and this file does not change.
//
// WHAT IT DOES NOT REPLACE. The five flags and `buildStores` stay exactly as they are. They work, they are
// tested, and rewriting them in the same change as the mechanism that supersedes them would leave two
// things to doubt instead of one. A group's store is reached through `-script name=file`; the old flags
// keep meaning what they meant.
package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/cleat-team/cleat/engine"
)

// storeSpec is what a registered store contributes to a run.
type storeSpec struct {
	// Option is handed to the engine. Required.
	Option engine.EngineOption

	// Extra, when set, contributes one entry to the envelope's `extras` map -- the generic place a store
	// reports what IT saw, so a test can assert on the store rather than only on the workflow's output.
	// Optional: a store with nothing to report leaves it nil.
	Extra func() (key string, value any)
}

// storeFactory builds a store from its fixture file.
type storeFactory func(path string) (storeSpec, error)

// storeRegistry maps a `-script` name to its factory. Populated by init() in stores_<group>.go.
var storeRegistry = map[string]storeFactory{}

// registerStore is called from a group's init(). It panics on a duplicate name, because two stores
// answering to one name is a programming error at load time and there is no run to attach it to yet.
func registerStore(name string, f storeFactory) {
	if name == "" {
		panic("registerStore: empty name")
	}
	if _, dup := storeRegistry[name]; dup {
		panic("registerStore: duplicate store name " + name)
	}
	storeRegistry[name] = f
}

// scriptFlag collects repeated `-script name=file` values.
type scriptFlag struct{ pairs []string }

func (s *scriptFlag) String() string { return strings.Join(s.pairs, ",") }

func (s *scriptFlag) Set(v string) error {
	if !strings.Contains(v, "=") {
		return fmt.Errorf("want <name>=<file>, got %q", v)
	}
	s.pairs = append(s.pairs, v)
	return nil
}

// registeredNames is sorted so an error message is stable across runs.
func registeredNames() []string {
	out := make([]string, 0, len(storeRegistry))
	for n := range storeRegistry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// resolveScripts turns the `-script` pairs into engine options and extras.
//
// AN UNKNOWN NAME IS FATAL, not ignored. A typo in a store name would otherwise configure nothing and the
// run would proceed with that store absent -- which for the update store means the engine reports no
// pending update and an update test passes having delivered nothing (engine/updater.go:98). The error
// lists the known names, so the fix is visible without reading this file.
func resolveScripts(pairs []string) ([]engine.EngineOption, map[string]any, error) {
	var opts []engine.EngineOption
	extras := map[string]any{}
	seen := map[string]string{}
	for _, p := range pairs {
		name, path, _ := strings.Cut(p, "=")
		if path == "" {
			return nil, nil, fmt.Errorf("-script %q has no file", name)
		}
		if prev, dup := seen[name]; dup {
			return nil, nil, fmt.Errorf("-script %q given twice (%s and %s): one store, one fixture", name, prev, path)
		}
		factory, ok := storeRegistry[name]
		if !ok {
			return nil, nil, fmt.Errorf("-script %q is not a registered store; known: %s",
				name, strings.Join(registeredNames(), ", "))
		}
		spec, err := factory(path)
		if err != nil {
			return nil, nil, fmt.Errorf("-script %q: %w", name, err)
		}
		seen[name] = path
		opts = append(opts, spec.Option)
		if spec.Extra != nil {
			k, v := spec.Extra()
			extras[k] = v
		}
	}
	if len(extras) == 0 {
		extras = nil
	}
	return opts, extras, nil
}
