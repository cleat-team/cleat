// Package stamponlyb is the control for stampclash: it calls only stampb's Client.Stamp, which makes no host
// call, so the build must link no cleat_now for it. A build that matched methods by name alone would link one.
package stamponlyb

import (
	"strconv"

	"github.com/cleat-team/cleat/cleat"
	"github.com/cleat-team/cleat/testdata/sdkhelpers/stampa"
	"github.com/cleat-team/cleat/testdata/sdkhelpers/stampb"
)

//cleat:entry
func Run(h cleat.HostCalls, input string) (string, error) {
	_ = stampa.NewClient()
	return strconv.FormatInt(stampb.NewClient().Stamp(h), 10), nil
}
