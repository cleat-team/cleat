// Package pluginfunc is a fixture for cleat#3803 / cleat#3805: cleat.PluginFunc.Call, a method on a generic type.
//
// It calls ONE SDK API and writes no HostCalls method of its own, so every import the build links for it comes
// from the SDK function's own body. A fixture that also wrote h.X itself could not tell "the SDK function was
// seen" from "the workflow happened to name the same call".
package pluginfunc

import (
	"github.com/cleat-team/cleat/cleat"
)

var echo = cleat.NewPluginFunc[string, string]("some-plugin", "echo")

//cleat:entry
func Run(h cleat.HostCalls, input string) (string, error) {
	return echo.Call(h, input)
}
