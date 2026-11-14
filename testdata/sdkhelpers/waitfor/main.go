// Package waitfor is a fixture for cleat#3803: cleat.PluginFunc.WaitFor (cleat#3794), a method on a generic type
// that makes h.WaitForEvent on the workflow's behalf.
//
// It was named in both hand lists the build used to consult. The derivation has to produce every import that row
// carried, so the fixture asks for all seven.
package waitfor

import (
	"time"

	"github.com/cleat-team/cleat/cleat"
)

var wait = cleat.NewPluginFunc[string, string]("some-plugin", "await").
	WithWake(func(s string) []string { return []string{"wake-" + s} })

//cleat:entry
func Run(h cleat.HostCalls, input string) (string, error) {
	return wait.WaitFor(h, input, time.Minute)
}
