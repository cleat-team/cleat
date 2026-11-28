// Package sagarun is a fixture for cleat#3803 / cleat#3805: cleat.Saga.Run, which makes its host calls only
// through another SDK method.
//
// Run delegates to RunWithResult, and RunWithResult is what calls LogKV and SetQueryState. The function the
// workflow names reaches them one call away, so the build has to follow calls inside the SDK, not only read the
// first body.
package sagarun

import "github.com/cleat-team/cleat/cleat"

//cleat:entry
func Run(h cleat.HostCalls, input string) (string, error) {
	err := cleat.NewSaga().
		AddStep("only", func(cleat.HostCalls) (string, error) { return "done", nil }, nil).
		Run(h)
	if err != nil {
		return "", err
	}
	return "ok", nil
}
