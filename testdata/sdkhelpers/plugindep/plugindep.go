// Package plugindep is the dependency half of testdata/sdkhelpers/pluginviadep (cleat#3803).
//
// Notify calls a plugin through a cleat.PluginFunc, a PluginCaller, from inside a function the workflow only
// invokes. Nothing in the workflow's own source mentions a plugin call.
package plugindep

import "github.com/cleat-team/cleat/cleat"

var notify = cleat.NewPluginFunc[string, string]("some-plugin", "notify")

// Notify sends msg through the plugin.
func Notify(h cleat.HostCalls, msg string) (string, error) {
	return notify.Call(h, msg)
}
