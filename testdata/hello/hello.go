// Package hello is the workflow README.md's first "try it" command runs.
//
// cleat#1967: that snippet used to point at testdata/basic's PlaceOrder,
// which makes a DurableCall to a "catalog" service nothing in this repo
// provides -- so the very first command a reader runs fails unconditionally
// with a connection-refused error. Greet makes no DurableCall at all, so it
// completes under `cleat dev` with nothing else running.
package hello

import (
	"encoding/json"

	"github.com/cleat-team/cleat/cleat"
)

// Greet returns a greeting for name, defaulting to "world" when name is
// empty. No external dependency: this is the whole workflow.
func Greet(h cleat.HostCalls, name string) (string, error) {
	if name == "" {
		name = "world"
	}
	out, err := json.Marshal(struct {
		Greeting string `json:"greeting"`
	}{Greeting: "Hello, " + name + "!"})
	if err != nil {
		return "", err
	}
	return string(out), nil
}
