// Package pluginapi is a compatibility shim that re-exports types from
// github.com/cleat-team/cleat/plugin. It exists so that plugins written
// against the old pluginapi import path continue to compile after the
// internal/plugin → plugin promotion.
package pluginapi

import "github.com/cleat-team/cleat/plugin"

// Types
type (
	PluginInfo     = plugin.PluginInfo
	Plugin         = plugin.Plugin
	Environment    = plugin.Environment
	Migration      = plugin.Migration
	PluginDB       = plugin.PluginDB
	DatabaseAccess = plugin.DatabaseAccess
)

// Constants
const (
	DatabaseAccessNone      = plugin.DatabaseAccessNone
	DatabaseAccessReadOnly  = plugin.DatabaseAccessReadOnly
	DatabaseAccessReadWrite = plugin.DatabaseAccessReadWrite
)

// Functions
var Register = plugin.Register
