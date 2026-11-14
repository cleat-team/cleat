// Package funcvalue is a fixture for cleat#3803 / cleat#3805: an SDK function used as a VALUE, not called where
// it is named.
//
// `mk := cleat.NewPromiseTyped[string]` makes the same host call later, at `mk(h, ...)`, and nothing at that
// second site names the SDK function. A scan that looked only at call expressions would miss it.
package funcvalue

import "github.com/cleat-team/cleat/cleat"

//cleat:entry
func Run(h cleat.HostCalls, input string) (string, error) {
	mk := cleat.NewPromiseTyped[string]
	p, err := mk(h, "approval")
	if err != nil {
		return "", err
	}
	return p.ID, nil
}
