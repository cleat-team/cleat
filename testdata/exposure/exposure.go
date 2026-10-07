// Package exposure is the Go fixture for cleat#1986 slice 2c: a workflow that
// declares its exposure class in SOURCE with the //cleat:exposure directive.
//
// It carries the directive and nothing exotic, because two separate things need
// proving and they are proved separately: that the directive reaches
// wasm.UsageInfo from a real package loaded the way the build loads one (this
// fixture, through analyzer.LoadPackages), and what the extraction does with the
// value once it has it (wasm/exposure_is_declared_by_a_directive_test.go, whose
// unit cases parse their own sources, so they can cover what a fixture cannot --
// no directive at all, a conflicting one, and prose that merely mentions the
// directive).
//
// The class here is `internal` rather than `auth`. `auth` is what a workflow
// gets with no declaration at all, so a fixture declaring it would pass both
// with the directive read and with it silently ignored. `internal` is reachable
// only by having been read.
//
//cleat:exposure internal
package exposure

import (
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

// Handle is the workflow's entry point. It makes no DurableCall: this fixture is
// about the metadata stamped beside the module, not about the host ABI, so
// keeping its verification result independent of the host ABI is the point.
func Handle(h cleat.HostCalls, input string) (string, error) {
	if input == "" {
		input = "nothing"
	}
	return fmt.Sprintf(`{"handled":%q}`, input), nil
}
