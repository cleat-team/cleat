// Package globalhhelper is a fixture for cleat#3803 / cleat#3805: a helper that reaches an SDK function through the package-level h.
//
// It calls ONE SDK API and writes no HostCalls method of its own, so every import the build links for it comes
// from the SDK function's own body. A fixture that also wrote h.X itself could not tell "the SDK function was
// seen" from "the workflow happened to name the same call".
package globalhhelper

import (
	"github.com/cleat-team/cleat/cleat"
)

// h is the package-level context object (see testdata/autothread). The helper below never names a HostCalls
// method: it only hands h to an SDK function. If the build does not count that as a host call, the helper is
// not in the cleat closure, h is never threaded into it, and every run dies on a nil dereference.
var h cleat.HostCalls

func newApproval() (string, error) {
	p, err := cleat.NewPromiseTyped[string](h, "approval")
	if err != nil {
		return "", err
	}
	return p.ID, nil
}

//cleat:entry
func Run(h cleat.HostCalls, input string) (string, error) {
	id, err := newApproval()
	if err != nil {
		return "", err
	}
	return "created " + id, nil
}
