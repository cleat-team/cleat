package main

import (
	"sort"

	"github.com/cleat-team/cleat/plugin"
)

// loadedPluginNames is the set of plugin names a worker publishes at registration
// (cleat#3600), sorted so a registration is reproducible. Names only: the version a plugin
// reports is checked at claim time, against the claiming worker's own set.
func loadedPluginNames(list []*plugin.LoadedPlugin) []string {
	seen := make(map[string]bool, len(list))
	names := make([]string, 0, len(list))
	for _, lp := range list {
		if lp == nil || lp.Plugin == nil {
			continue
		}
		n := lp.Plugin.Info().Name
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
