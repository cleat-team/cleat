// Package opaqueuser reaches opaquedep.Do and writes no HostCalls method of its own: the build must REFUSE it
// (cleat#3803), not emit a module that may fail at run time.
package opaqueuser

import (
	"github.com/cleat-team/cleat/cleat"
	"github.com/cleat-team/cleat/testdata/sdkhelpers/opaquedep"
)

type noop struct{}

func (noop) Run(h cleat.HostCalls) error { return nil }

//cleat:entry
func Run(h cleat.HostCalls, input string) (string, error) {
	return input, opaquedep.Do(h, noop{})
}
