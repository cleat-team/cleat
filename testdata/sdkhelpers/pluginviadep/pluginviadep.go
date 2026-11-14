// Package pluginviadep reaches a plugin call only through plugindep.Notify (cleat#3803): it writes no HostCalls
// method and no plugin call of its own, so plugin_call is linked only if the build reads Notify's body.
package pluginviadep

import (
	"github.com/cleat-team/cleat/cleat"
	"github.com/cleat-team/cleat/testdata/sdkhelpers/plugindep"
)

//cleat:entry
func Run(h cleat.HostCalls, input string) (string, error) {
	return plugindep.Notify(h, input)
}
