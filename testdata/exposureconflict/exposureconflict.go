// Package exposureconflict is the Go fixture for cleat#3524: a workflow whose
// package declares two different exposure classes, which `cleat build` refuses
// and `cleat vet` used to print OK for.
//
// It is deliberately broken. A package may declare at most one class
// (wasm.UsageInfo.ExposureConflict), and these two files' worth of intent are
// stated in one file so the fixture needs nothing else.
//
//cleat:exposure internal
//cleat:exposure public
package exposureconflict

import (
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

// Handle is the workflow's entry point; nothing here is about the host ABI.
func Handle(h cleat.HostCalls, input string) (string, error) {
	return fmt.Sprintf(`{"handled":%q}`, input), nil
}
