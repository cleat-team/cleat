package main

import (
	"context"

	"github.com/cleat-team/cleat/monitoring/prometheus"
)

// pluginEventsLostHook is what plugin.Environment.EventsLost is set to (cleat#2168). A plugin that
// buffers events reports each one it gives up on here, and it becomes
// cleat_plugin_events_lost_total{plugin,reason}.
//
// The context is Background, not the worker's: a loss during shutdown is exactly one that has to be
// counted. A nil Metrics reports nothing, which is what a plugin's own log line is for.
func pluginEventsLostHook(m *prometheus.Metrics) func(pluginName, reason string, n int64) {
	return func(pluginName, reason string, n int64) {
		if m == nil {
			return
		}
		m.RecordPluginEventsLost(context.Background(), pluginName, reason, n)
	}
}
