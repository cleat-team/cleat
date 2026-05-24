// Package pluginapi re-exports types from internal/plugin for external plugin authors.
// Plugin modules outside the github.com/cleat-team/cleat module tree must import this
// package instead of internal/plugin.
package pluginapi

import "github.com/cleat-team/cleat/internal/plugin"

// Types re-exported for plugin authors.
type (
	PluginInfo     = plugin.PluginInfo
	Plugin         = plugin.Plugin
	Environment    = plugin.Environment
	DatabaseAccess = plugin.DatabaseAccess
)

// Register re-exports the plugin registration function.
var Register = plugin.Register

// Database access levels.
const (
	DatabaseAccessNone      = plugin.DatabaseAccessNone
	DatabaseAccessReadOnly  = plugin.DatabaseAccessReadOnly
	DatabaseAccessReadWrite = plugin.DatabaseAccessReadWrite
)
