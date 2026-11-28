// Package promisetyped is a fixture for cleat#3803 / cleat#3805: the documented typed promise, cleat.NewPromiseTyped and Promise.Await.
//
// It calls ONE SDK API and writes no HostCalls method of its own, so every import the build links for it comes
// from the SDK function's own body. A fixture that also wrote h.X itself could not tell "the SDK function was
// seen" from "the workflow happened to name the same call".
package promisetyped

import (
	"encoding/json"
	"time"

	"github.com/cleat-team/cleat/cleat"
)

// Approval is what the promise is resolved with. A struct rather than a bare string, deliberately: a string
// result is a JSON scalar, and the store hands the guest a scalar with its quotes stripped.
type Approval struct {
	By string `json:"by"`
}

//cleat:entry
func Run(h cleat.HostCalls, input string) (string, error) {
	p, err := cleat.NewPromiseTyped[Approval](h, "approval")
	if err != nil {
		return "", err
	}
	v, timedOut, err := p.Await(5 * time.Second)
	if err != nil {
		return "", err
	}
	if timedOut {
		return `{"timed_out":true}`, nil
	}
	out, err := json.Marshal(map[string]string{"approved_by": v.By})
	return string(out), err
}
