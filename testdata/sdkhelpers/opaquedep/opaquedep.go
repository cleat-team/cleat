// Package opaquedep is the dependency half of testdata/sdkhelpers/opaqueuser (cleat#3803).
//
// Do is a function that hands the host-call runtime to a method on an INTERFACE. Which implementation runs is
// not knowable from this source, so the build cannot say which host calls it makes, and has to refuse a
// workflow that reaches it rather than guess "none".
package opaquedep

import "github.com/cleat-team/cleat/cleat"

// Runner is implemented by something the build cannot see.
type Runner interface {
	Run(h cleat.HostCalls) error
}

// Do runs r with the host-call runtime.
func Do(h cleat.HostCalls, r Runner) error {
	return r.Run(h)
}
