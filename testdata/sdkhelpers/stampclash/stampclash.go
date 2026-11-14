// Package stampclash imports BOTH stampa and stampb and calls only stampa's Client.Stamp. It writes no HostCalls
// method of its own, so cleat_now is linked only if the build tells the two same-named methods apart.
package stampclash

import (
	"strconv"

	"github.com/cleat-team/cleat/cleat"
	"github.com/cleat-team/cleat/testdata/sdkhelpers/stampa"
	"github.com/cleat-team/cleat/testdata/sdkhelpers/stampb"
)

//cleat:entry
func Run(h cleat.HostCalls, input string) (string, error) {
	_ = stampb.NewClient()
	return strconv.FormatInt(stampa.NewClient().Stamp(h), 10), nil
}
